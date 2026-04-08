package codegen

// extern.go - helpers for mapping Tin types to C-compatible LLVM types,
// declaring extern C functions, and wrapping/unwrapping fat-pointer arguments.

import (
	"fmt"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// tinTypeToExternLLVM returns the C-compatible LLVM type for a Tin type.
// Fat-pointer types (string, atom, dynamic arrays) are unwrapped to their
// underlying raw pointer when used as parameters.
// Named Tin structs are converted to C-native layout (no type_id / vtable).
// When forReturn is true (return-type context), dynamic arrays keep their
// full fat-ptr type so the C function can return the struct directly.
func (cg *CodeGen) tinTypeToExternLLVM(te ast.TypeExpr, forReturn bool) (irtypes.Type, error) {
	if te == nil {
		return irtypes.Void, nil
	}
	// string / atom -> i8*
	if st, ok := te.(*ast.SimpleType); ok {
		if st.Name == "string" || st.Name == "atom" {
			return irtypes.I8Ptr, nil
		}
		// Named Tin struct: strip type_id and vtable pointers for C ABI.
		// Small all-integer structs are further coerced to an integer register
		// type to match x86-64 SysV ABI (clang coerces { i8, i8, i8, i8 } -> i32).
		if _, isStruct := cg.structFieldLLVMTypes[st.Name]; isStruct {
			native, err := cg.tinStructNativeLLVM(st.Name)
			if err != nil {
				return nil, err
			}

			if coerced := coerceNativeStructForABI(native); coerced != nil && cg.targetIsAMD64() {
				return coerced, nil
			}

			return native, nil
		}
	}
	// *S where S is a named struct: pointer to C-native layout.
	if pt, ok := te.(*ast.PointerType); ok {
		if st, ok2 := pt.Elem.(*ast.SimpleType); ok2 {
			if _, isStruct := cg.structFieldLLVMTypes[st.Name]; isStruct {
				native, err := cg.tinStructNativeLLVM(st.Name)
				if err != nil {
					return nil, err
				}

				return irtypes.NewPointer(native), nil
			}
		}
	}
	// []T (dynamic array):
	//   - as parameter: unwrap to *T (C receives the data pointer)
	//   - as return type: keep the full fat-ptr {*T, i64} so C returns the struct
	if at, ok := te.(*ast.ArrayType); ok && at.Size < 0 {
		if forReturn {
			return cg.tinTypeToLLVM(te)
		}

		elem, err := cg.tinTypeToLLVM(at.Elem)
		if err != nil {
			return nil, err
		}

		return irtypes.NewPointer(elem), nil
	}

	return cg.tinTypeToLLVM(te)
}

// tinStructNativeLLVM returns (creating if needed) the C-compatible LLVM struct
// type for the named Tin struct: only user fields, no type_id or vtable pointers.
// The type is named "<structName>.native" in the LLVM module.
func (cg *CodeGen) tinStructNativeLLVM(structName string) (*irtypes.StructType, error) {
	nativeName := structName + ".native"
	// Return cached version if available.
	if st, ok := cg.structTypes[nativeName]; ok {
		return st, nil
	}

	userFields, ok := cg.structFieldLLVMTypes[structName]
	if !ok {
		return nil, fmt.Errorf("tinStructNativeLLVM: unknown struct %q", structName)
	}

	nativeFields := make([]irtypes.Type, len(userFields))
	for i, ft := range userFields {
		// Recursively convert nested named struct fields.
		if innerSt, ok2 := ft.(*irtypes.StructType); ok2 && innerSt.Name() != "" {
			inner, err := cg.tinStructNativeLLVM(innerSt.Name())
			if err != nil {
				return nil, err
			}

			nativeFields[i] = inner
		} else {
			nativeFields[i] = ft
		}
	}

	st := irtypes.NewStruct(nativeFields...)
	st.SetName(nativeName)
	cg.structTypes[nativeName] = st
	cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)

	return st, nil
}

// wrapStructToExtern converts a Tin struct value (with type_id + vtable fields)
// into a C-native struct value (user fields only) suitable for passing to extern C.
func (cg *CodeGen) wrapStructToExtern(block *ir.Block, val value.Value, structName string) (value.Value, error) {
	tinSt, ok := val.Type().(*irtypes.StructType)
	if !ok {
		return val, nil // not a struct; pass through
	}

	nativeSt, err := cg.tinStructNativeLLVM(structName)
	if err != nil {
		return nil, err
	}

	offset := int64(cg.userFieldOffset(structName))
	userFields := cg.structFieldLLVMTypes[structName]

	tinAlloca := block.NewAlloca(tinSt)
	block.NewStore(val, tinAlloca)
	nativeAlloca := block.NewAlloca(nativeSt)

	for i, ft := range userFields {
		srcGep := block.NewGetElementPtr(tinSt, tinAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, offset+int64(i)))

		var fv value.Value = block.NewLoad(ft, srcGep)
		// Recursively convert nested struct fields.
		if innerSt, ok2 := ft.(*irtypes.StructType); ok2 && innerSt.Name() != "" {
			fv2, err2 := cg.wrapStructToExtern(block, fv, innerSt.Name())
			if err2 != nil {
				return nil, err2
			}

			fv = fv2
		}

		dstGep := block.NewGetElementPtr(nativeSt, nativeAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
		block.NewStore(fv, dstGep)
	}

	return block.NewLoad(nativeSt, nativeAlloca), nil
}

// wrapNativeStructToTin converts a C-native struct (user fields only) into the
// full Tin struct layout (type_id + vtable + user fields).
// For cLayoutStructs, uses the wrapper+native RC allocation so c_data_ptr is valid.
func (cg *CodeGen) wrapNativeStructToTin(block *ir.Block, val value.Value, structName string) (value.Value, error) {
	nativeSt, ok := val.Type().(*irtypes.StructType)
	if !ok {
		return val, nil
	}

	tinSt, tinOk := cg.structTypes[structName]
	if !tinOk {
		return val, nil
	}

	// cLayoutStructs: allocate wrapper + native overflow in one RC block (handover
	// semantics - Tin owns this copy). c_data_ptr points to the overflow area.
	if cg.cLayoutStructs[structName] {
		nativeSize := cg.llvmSizeOf(block, nativeSt)
		wrapperSize := cg.llvmSizeOf(block, tinSt)
		totalSize := block.NewAdd(wrapperSize, nativeSize)
		rcRaw := block.NewCall(cg.ensureRCAlloc(), totalSize)
		tinPtr := block.NewBitCast(rcRaw, irtypes.NewPointer(tinSt))

		typeID := cg.structTypeIDs[structName]
		typeIDGep := block.NewGetElementPtr(tinSt, tinPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		block.NewStore(constant.NewInt(irtypes.I32, int64(typeID)), typeIDGep)

		offset := int64(cg.userFieldOffset(structName))
		for v := int64(1); v < offset; v++ {
			vtGep := block.NewGetElementPtr(tinSt, tinPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, v))
			fieldType := tinSt.Fields[v]
			block.NewStore(constant.NewNull(fieldType.(*irtypes.PointerType)), vtGep)
		}

		// Copy native data into overflow area (GEP+1 past wrapper).
		overflowGEP := block.NewGetElementPtr(tinSt, tinPtr, constant.NewInt(irtypes.I64, 1))
		overflowI8 := block.NewBitCast(overflowGEP, irtypes.I8Ptr)
		nativeAlloca := block.NewAlloca(nativeSt)
		block.NewStore(val, nativeAlloca)
		srcI8 := block.NewBitCast(nativeAlloca, irtypes.I8Ptr)
		block.NewCall(cg.ensureMemcpy(), overflowI8, srcI8, nativeSize, constant.NewInt(irtypes.I1, 0))

		cDataIdx := int64(cg.cDataPtrIndex(structName))
		cDataGep := block.NewGetElementPtr(tinSt, tinPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, cDataIdx))
		block.NewStore(overflowI8, cDataGep)

		return block.NewLoad(tinSt, tinPtr), nil
	}

	userFields := cg.structFieldLLVMTypes[structName]
	offset := int64(cg.userFieldOffset(structName))

	nativeAlloca := block.NewAlloca(nativeSt)
	block.NewStore(val, nativeAlloca)
	tinAlloca := block.NewAlloca(tinSt)

	typeID := cg.structTypeIDs[structName]
	typeIDGep := block.NewGetElementPtr(tinSt, tinAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(constant.NewInt(irtypes.I32, int64(typeID)), typeIDGep)

	// Zero vtable pointer fields (fields 1..offset-1).
	for v := int64(1); v < offset; v++ {
		vtGep := block.NewGetElementPtr(tinSt, tinAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, v))
		fieldType := tinSt.Fields[v]
		block.NewStore(constant.NewNull(fieldType.(*irtypes.PointerType)), vtGep)
	}

	// Copy user fields from native to Tin.
	for i := range userFields {
		// Native struct field types may differ from Tin user field types for nested
		// structs (native uses .native layout). Load from the native field type.
		nativeFieldType := nativeSt.Fields[i]
		srcGep := block.NewGetElementPtr(nativeSt, nativeAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))

		var fv value.Value = block.NewLoad(nativeFieldType, srcGep)
		// Recursively reconstruct nested struct fields.
		if nativeName, ok2 := nativeFieldType.(*irtypes.StructType); ok2 {
			innerName := nativeName.Name()
			if strings.HasSuffix(innerName, ".native") {
				innerTinName := strings.TrimSuffix(innerName, ".native")

				fv2, err2 := cg.wrapNativeStructToTin(block, fv, innerTinName)
				if err2 != nil {
					return nil, err2
				}

				fv = fv2
			}
		}

		dstGep := block.NewGetElementPtr(tinSt, tinAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, offset+int64(i)))
		block.NewStore(fv, dstGep)
	}

	return block.NewLoad(tinSt, tinAlloca), nil
}

// isNamedTinStruct reports whether the TypeExpr is a named Tin struct.
func (cg *CodeGen) isNamedTinStruct(te ast.TypeExpr) (string, bool) {
	if st, ok := te.(*ast.SimpleType); ok {
		if _, isStruct := cg.structFieldLLVMTypes[st.Name]; isStruct {
			return st.Name, true
		}
	}

	return "", false
}

// scanExternPtrStructs scans all nodes for extern function declarations that
// use *StructName pointer types. Those struct names are recorded in
// cg.cLayoutStructs so their type layout omits the i32 type_id prefix,
// matching C ABI for raw pointer round-trips.
//
// After the initial scan, it propagates C-layout to nested struct field types:
// if outer_t is C-layout and contains inner_t fields, inner_t must also be
// C-layout so its Tin layout matches C.
func (cg *CodeGen) scanExternPtrStructs(stmts []ast.Node) {
	// Collect struct declarations for propagation.
	structDecls := map[string]*ast.StructDecl{}

	for _, node := range stmts {
		if sd, ok := node.(*ast.StructDecl); ok {
			structDecls[sd.Name] = sd
		}
	}

	// Phase 1: mark structs used as *S in extern signatures.
	for _, node := range stmts {
		switch n := node.(type) {
		case *ast.FuncDecl:
			if n.IsExtern == "" {
				continue
			}

			for _, p := range n.Params {
				cg.markPtrStructCLayout(p.Type)
			}

			cg.markPtrStructCLayout(n.RetType)

		case *ast.UseDecl:
			if !n.IsExtern {
				continue
			}

			for _, imp := range n.Imports {
				if ft, ok := imp.Type.(*ast.FuncType); ok {
					for _, p := range ft.Params {
						cg.markPtrStructCLayout(p)
					}

					cg.markPtrStructCLayout(ft.RetType)
				}
			}
		}
	}

	// Phase 2: propagate C-layout to nested struct fields.
	changed := true
	for changed {
		changed = false

		for name := range cg.cLayoutStructs {
			sd, ok := structDecls[name]
			if !ok {
				continue
			}

			for _, f := range sd.Fields {
				if st, ok2 := f.Type.(*ast.SimpleType); ok2 {
					if _, isStruct := structDecls[st.Name]; isStruct && !cg.cLayoutStructs[st.Name] {
						cg.cLayoutStructs[st.Name] = true
						changed = true
					}
				}
			}
		}
	}
}

// extractCSrcPtr loads the c_data_ptr field from a cLayoutStruct wrapper and
// returns it cast to cTargetType. This is the pointer that C sees - for non-handover
// returns it is the original C pointer; for handover/literals it points to the
// inline fields within the wrapper allocation.
func (cg *CodeGen) extractCSrcPtr(block *ir.Block, tinPtr value.Value, te ast.TypeExpr, cTargetType irtypes.Type) value.Value {
	pt := te.(*ast.PointerType)
	st := pt.Elem.(*ast.SimpleType)

	idx := cg.cDataPtrIndex(st.Name)
	if idx < 0 {
		return block.NewBitCast(tinPtr, cTargetType)
	}

	tinSt := cg.structTypes[st.Name]
	gep := block.NewGetElementPtr(tinSt, tinPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(idx)))
	cPtr := block.NewLoad(irtypes.I8Ptr, gep)

	return block.NewBitCast(cPtr, cTargetType)
}

// isExternPtrParam checks if te is *StructName where StructName has a hidden
// C source pointer field (used for extern pointer round-trips).
func (cg *CodeGen) isExternPtrParam(te ast.TypeExpr) bool {
	if pt, ok := te.(*ast.PointerType); ok {
		if st, ok2 := pt.Elem.(*ast.SimpleType); ok2 {
			return cg.cLayoutStructs[st.Name]
		}
	}

	return false
}

// markPtrStructCLayout marks the struct name inside a *StructName type as
// needing a hidden C source pointer field. Only acts on pointer-to-struct
// types (skips primitives like *void, *i8, etc.).
func (cg *CodeGen) markPtrStructCLayout(te ast.TypeExpr) {
	if pt, ok := te.(*ast.PointerType); ok {
		if st, ok2 := pt.Elem.(*ast.SimpleType); ok2 {
			// Only mark if it's a registered struct type (from preregister pass 1).
			if _, isStruct := cg.structTypes[st.Name]; isStruct {
				cg.cLayoutStructs[st.Name] = true
			}
		}
	}
}

// nativeStructByteSize computes the byte size of a native struct type by
// summing the sizes of its fields (recursively for nested structs).
// This is used to decide whether to use byval (> 16 bytes) or register passing.
func nativeStructByteSize(st *irtypes.StructType) int {
	total := 0

	for _, field := range st.Fields {
		switch f := field.(type) {
		case *irtypes.StructType:
			total += nativeStructByteSize(f)
		case *irtypes.FloatType:
			switch f.Kind {
			case irtypes.FloatKindHalf:
				total += 2
			case irtypes.FloatKindFloat:
				total += 4
			case irtypes.FloatKindDouble, irtypes.FloatKindFP128, irtypes.FloatKindX86_FP80, irtypes.FloatKindPPC_FP128:
				total += 8
			}
		case *irtypes.IntType:
			total += int((f.BitSize + 7) / 8)
		default:
			total += 8 // pointer or other 8-byte type
		}
	}

	return total
}

// nativeStructNeedsByval reports whether the native struct type needs the
// byval attribute for C calling convention on AMD64 (size > 16 bytes).
func nativeStructNeedsByval(st *irtypes.StructType) bool {
	return nativeStructByteSize(st) > 16
}

// nativeStructAllInteger reports whether every field in the native struct is
// an integer type (i8, i16, i32, i64, i128). Nested structs are checked
// recursively. Used for x86-64 SysV ABI coercion: small all-integer structs
// that fit in a single eightbyte are passed as an integer register.
func nativeStructAllInteger(st *irtypes.StructType) bool {
	for _, f := range st.Fields {
		switch ft := f.(type) {
		case *irtypes.IntType:
			// OK
		case *irtypes.StructType:
			if !nativeStructAllInteger(ft) {
				return false
			}
		default:
			_ = ft

			return false
		}
	}

	return true
}

// coerceNativeStructForABI returns an integer type (iN) for small all-integer
// native structs that the x86-64 SysV ABI passes in a single integer register.
// Returns nil if the struct does not need coercion (e.g. contains floats,
// spans multiple eightbytes, etc.).
func coerceNativeStructForABI(st *irtypes.StructType) irtypes.Type {
	size := nativeStructByteSize(st)
	if size > 8 || size == 0 {
		return nil
	}

	if !nativeStructAllInteger(st) {
		return nil
	}

	return irtypes.NewInt(uint64(size * 8))
}

// ensureExternDecl returns (or creates) a bare LLVM function declaration for a
// C extern symbol. Re-uses an existing declaration if one with a matching
// signature already exists.
func (cg *CodeGen) ensureExternDecl(cName string, retType irtypes.Type, params []*ir.Param, variadic bool) *ir.Func {
	for _, f := range cg.mod.Funcs {
		if f.Name() == cName {
			return f
		}
	}

	f := cg.mod.NewFunc(cName, retType, params...)
	f.Sig.Variadic = variadic
	f.Blocks = nil
	// Track that this IR name is a C extern symbol so that Tin user functions
	// with the same name can be mangled to avoid redefinition conflicts.
	if cg.externIRNames == nil {
		cg.externIRNames = map[string]bool{}
	}

	cg.externIRNames[cName] = true

	return f
}

// ensureExternTLSVar returns (or creates) an extern thread-local global variable
// declaration in the IR. Used to read runtime TLS state (e.g. _current_pid)
// without a function call, enabling the compiler to inline the TLS load.
// Uses localexec TLS model for the most efficient single-instruction access
// in standalone (non-shared-library) executables.
func (cg *CodeGen) ensureExternTLSVar(name string, typ irtypes.Type) *ir.Global {
	if g, ok := cg.externTLSVars[name]; ok {
		return g
	}

	g := cg.mod.NewGlobal(name, typ)
	g.Linkage = enum.LinkageExternal
	g.TLSModel = enum.TLSModelLocalExec
	cg.externTLSVars[name] = g

	return g
}

// ensureStrlenDecl lazily creates the bare `declare i64 @strlen(i8*)` for use
// inside wrapFromExtern when a C function returns a char* that we wrap into a
// Tin string fat-pointer.
func (cg *CodeGen) ensureStrlenDecl() *ir.Func {
	return cg.ensureExternDecl("strlen", irtypes.I64,
		[]*ir.Param{ir.NewParam("s", irtypes.I8Ptr)}, false)
}

// extractFatPtrData extracts field 0 (the raw data pointer) from a fat-pointer
// struct value. Works whether the value is a struct value or a pointer to one.
func (cg *CodeGen) extractFatPtrData(block *ir.Block, val value.Value, st *irtypes.StructType) value.Value {
	alloca := block.NewAlloca(st)
	block.NewStore(val, alloca)
	gep := block.NewGetElementPtr(st, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))

	return block.NewLoad(st.Fields[0], gep)
}

// wrapFromExtern wraps a raw C return value into a Tin fat-pointer or atom.
// For char* -> string, it calls strlen to obtain the length.
// For char* -> atom, it calls __tin_string_to_atom.
//
// When handover is true the function was annotated {#handover}: ownership of
// the raw pointer is transferred to Tin.
//   - char* -> atom:         frees the char* after atom lookup.
//   - char* -> string:       RC-ifies the char* and builds a fat-ptr.
//   - native_struct* -> *T:  loads native data, frees original, adds type_id,
//     stores into a fresh RC allocation.
//   - any other T* -> T*:    calls _tin_ptr_handover to RC-ify.
func (cg *CodeGen) wrapFromExtern(block *ir.Block, val value.Value, target irtypes.Type, handover bool) value.Value {
	src := val.Type()

	// #handover: take ownership of pointer returns before any type conversion.
	if handover {
		if _, ok := src.(*irtypes.PointerType); ok {
			// char* → atom: handover variant frees the input string.
			if isAtomType(target) {
				return block.NewCall(cg.ensureStringToAtomHandover(), val)
			}
			// char* → fat-ptr string: _tin_string_handover + fat-ptr.
			if tgtSt, ok2 := target.(*irtypes.StructType); ok2 && isFatPtrType(target) {
				handoverFn := cg.ensureExternDecl("_tin_string_handover", irtypes.I8Ptr,
					[]*ir.Param{ir.NewParam("src", irtypes.I8Ptr)}, false)

				i8Ptr := val
				if !src.Equal(irtypes.I8Ptr) {
					i8Ptr = block.NewBitCast(val, irtypes.I8Ptr)
				}

				var ptr value.Value = block.NewCall(handoverFn, i8Ptr)
				if !tgtSt.Fields[0].Equal(irtypes.I8Ptr) {
					ptr = block.NewBitCast(ptr, tgtSt.Fields[0])
				}

				rawI8Ptr := ptr
				if !ptr.Type().Equal(irtypes.I8Ptr) {
					rawI8Ptr = block.NewBitCast(ptr, irtypes.I8Ptr)
				}

				length := block.NewCall(cg.ensureStrlenDecl(), rawI8Ptr)
				alloca := block.NewAlloca(tgtSt)
				block.NewStore(ptr, block.NewGetElementPtr(tgtSt, alloca,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0)))
				block.NewStore(length, block.NewGetElementPtr(tgtSt, alloca,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1)))

				return block.NewLoad(tgtSt, alloca)
			}
			// native_struct* → *TinStruct: load, free original, convert layout, RC-alloc.
			if tgtPt, ok2 := target.(*irtypes.PointerType); ok2 {
				tgtName := cg.typeNameOf(tgtPt.ElemType)
				if tgtName != "" && !strings.HasSuffix(tgtName, ".native") {
					return cg.emitStructPtrHandover(block, val, tgtPt, tgtName)
				}
			}
			// All other pointer types: generic _tin_ptr_handover.
			return cg.emitGenericPtrHandover(block, val, target)
		}
	}

	if src.Equal(target) {
		return val
	}
	// *native_struct -> *TinStruct (non-handover): copy into an immortal RC block
	// with correct Tin layout. Store the original C pointer in the hidden trailing
	// field so it can be extracted when passing the pointer back to C.
	if srcPt, ok := src.(*irtypes.PointerType); ok {
		if tgtPt, ok2 := target.(*irtypes.PointerType); ok2 {
			tgtName := cg.typeNameOf(tgtPt.ElemType)
			if tgtName != "" && !strings.HasSuffix(tgtName, ".native") {
				if srcSt, ok3 := srcPt.ElemType.(*irtypes.StructType); ok3 && strings.HasSuffix(srcSt.Name(), ".native") {
					return cg.emitStructPtrBorrow(block, val, tgtPt, tgtName)
				}
			}
		}
	}
	// i8* -> %__atom: find atom in table via strcmp.
	if _, ok := src.(*irtypes.PointerType); ok {
		if isAtomType(target) {
			return block.NewCall(cg.ensureStringToAtom(), val)
		}
	}
	// raw pointer -> fat-pointer (non-handover string return): build {ptr, len}.
	// Copy string data into an RC allocation so Tin can release it correctly.
	// C retains ownership of the original; Tin has an independent RC=1 copy.
	if _, ok := src.(*irtypes.PointerType); ok {
		if tgtSt, ok2 := target.(*irtypes.StructType); ok2 && isFatPtrType(target) {
			strlenFn := cg.ensureStrlenDecl()

			rawI8Ptr := val
			if !src.Equal(irtypes.I8Ptr) {
				rawI8Ptr = block.NewBitCast(val, irtypes.I8Ptr)
			}

			// Compute length and copy into RC-managed allocation (+1 for null terminator).
			length := block.NewCall(strlenFn, rawI8Ptr)
			allocSize := block.NewAdd(length, constant.NewInt(irtypes.I64, 1))
			rcRaw := block.NewCall(cg.ensureRCAlloc(), allocSize)
			block.NewCall(cg.ensureMemcpy(), rcRaw, rawI8Ptr, allocSize, constant.NewInt(irtypes.I1, 0))

			// Coerce RC pointer to the type expected by fat-ptr field 0.
			var ptr value.Value = rcRaw
			if !rcRaw.Type().Equal(tgtSt.Fields[0]) {
				ptr = block.NewBitCast(rcRaw, tgtSt.Fields[0])
			}

			alloca := block.NewAlloca(tgtSt)
			gep0 := block.NewGetElementPtr(tgtSt, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
			block.NewStore(ptr, gep0)
			gep1 := block.NewGetElementPtr(tgtSt, alloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
			block.NewStore(length, gep1)

			return block.NewLoad(tgtSt, alloca)
		}
	}

	return val
}

// emitCLayoutFieldPtr returns an IR pointer to field fieldIdx of a cLayoutStruct,
// going through the c_data_ptr field. All cLayoutStruct field accesses must use
// this helper so that non-handover mutations by C are observable.
func (cg *CodeGen) emitCLayoutFieldPtr(block *ir.Block, wrapperPtr value.Value, structName string, fieldIdx int) value.Value {
	wrapperType := cg.structTypes[structName]
	nativeType := cg.nativeStructTypes[structName]

	// Load c_data_ptr (at userFieldOffset index)
	cDataGEP := block.NewGetElementPtr(wrapperType, wrapperPtr,
		constant.NewInt(irtypes.I32, 0),
		constant.NewInt(irtypes.I32, int64(cg.cDataPtrIndex(structName))))
	cDataPtr := block.NewLoad(irtypes.NewPointer(irtypes.I8), cDataGEP)

	// Cast to *S.native, GEP to field
	nativePtr := block.NewBitCast(cDataPtr, irtypes.NewPointer(nativeType))

	return block.NewGetElementPtr(nativeType, nativePtr,
		constant.NewInt(irtypes.I32, 0),
		constant.NewInt(irtypes.I32, int64(fieldIdx)))
}

// emitFieldGEP returns a pointer to the named field of a struct.
// structPtr must be a pointer to the struct (for cLayoutStructs: *%S.wrapper).
// For cLayoutStructs: loads c_data_ptr, casts to *%S.native, GEPs at native field idx.
// For regular structs: direct GEP at wrapper field idx.
// Returns nil if structName or fieldName is not found.
func (cg *CodeGen) emitFieldGEP(block *ir.Block, structPtr value.Value, structName, fieldName string) value.Value {
	if cg.cLayoutStructs[structName] {
		fieldIdx := cg.nativeFieldIndex(structName, fieldName)
		if fieldIdx < 0 {
			return nil
		}

		return cg.emitCLayoutFieldPtr(block, structPtr, structName, fieldIdx)
	}

	fieldIdx := cg.fieldIndex(structName, fieldName)
	if fieldIdx < 0 {
		return nil
	}

	structType := cg.structTypes[structName]
	if structType == nil {
		return nil
	}

	return block.NewGetElementPtr(structType, structPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fieldIdx)))
}

// emitStructPtrBorrow handles non-handover extern *StructType returns.
// Allocates a Tin wrapper with RC=1, sets type_id/vtable stubs, and stores
// the raw C pointer in the c_data_ptr field. No data is copied, so C mutations
// to the original struct are immediately visible through the wrapper.
func (cg *CodeGen) emitStructPtrBorrow(block *ir.Block, src value.Value, tgtPt *irtypes.PointerType, structName string) value.Value {
	tinSt := tgtPt.ElemType.(*irtypes.StructType)
	// 1. RC-allocate the wrapper (inline fields are zero-init, unused for non-handover).
	tinSize := cg.llvmSizeOf(block, tinSt)
	rcRaw := block.NewCall(cg.ensureRCAlloc(), tinSize)
	tinPtr := block.NewBitCast(rcRaw, tgtPt)
	// 2. Set type_id (field 0).
	typeID := cg.structTypeIDs[structName]
	typeIDGep := block.NewGetElementPtr(tinSt, tinPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(constant.NewInt(irtypes.I32, int64(typeID)), typeIDGep)
	// 3. Zero vtable pointer fields (fields 1..vtableOffset).
	offset := cg.userFieldOffset(structName)
	for v := int64(1); v < int64(offset); v++ {
		vtGep := block.NewGetElementPtr(tinSt, tinPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, v))
		fieldType := tinSt.Fields[v]
		block.NewStore(constant.NewNull(fieldType.(*irtypes.PointerType)), vtGep)
	}
	// 4. Store raw C pointer in c_data_ptr field.
	cDataIdx := int64(cg.cDataPtrIndex(structName))
	cDataGep := block.NewGetElementPtr(tinSt, tinPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, cDataIdx))

	i8Src := src
	if !src.Type().Equal(irtypes.I8Ptr) {
		i8Src = block.NewBitCast(src, irtypes.I8Ptr)
	}

	block.NewStore(i8Src, cDataGep)
	// Inline fields left zero-init; they are not used for non-handover borrows.

	return tinPtr
}

// emitStructPtrHandover handles {#handover} for a C native struct pointer return.
// Allocates a Tin wrapper + native data (sizeof(wrapper) + sizeof(native) bytes).
// The native data area sits immediately after the wrapper (GEP+1 overflow area).
// c_data_ptr points into this overflow area; the original C pointer is freed.
// Tin fully owns the data; no separate allocation needed.
func (cg *CodeGen) emitStructPtrHandover(block *ir.Block, src value.Value, tgtPt *irtypes.PointerType, structName string) value.Value {
	tinSt := tgtPt.ElemType.(*irtypes.StructType)

	nativeSt := cg.nativeStructTypes[structName]
	if nativeSt == nil {
		// Fallback: use old generic handover path.
		return cg.emitGenericPtrHandover(block, src, tgtPt)
	}
	// 1. Allocate wrapper + native data in one RC block.
	wrapperSize := cg.llvmSizeOf(block, tinSt)
	nativeSize := cg.llvmSizeOf(block, nativeSt)
	totalSize := block.NewAdd(wrapperSize, nativeSize)
	rcRaw := block.NewCall(cg.ensureRCAlloc(), totalSize)
	tinPtr := block.NewBitCast(rcRaw, tgtPt)
	// 2. Set type_id.
	typeID := cg.structTypeIDs[structName]
	typeIDGep := block.NewGetElementPtr(tinSt, tinPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(constant.NewInt(irtypes.I32, int64(typeID)), typeIDGep)
	// 3. Zero vtable pointer fields.
	offset := cg.userFieldOffset(structName)
	for v := int64(1); v < int64(offset); v++ {
		vtGep := block.NewGetElementPtr(tinSt, tinPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, v))
		fieldType := tinSt.Fields[v]
		block.NewStore(constant.NewNull(fieldType.(*irtypes.PointerType)), vtGep)
	}
	// 4. Compute the overflow area (native data sits after the wrapper struct).
	// GEP+1 on the wrapper pointer advances by sizeof(%S.wrapper) bytes.
	overflowGEP := block.NewGetElementPtr(tinSt, tinPtr, constant.NewInt(irtypes.I64, 1))
	overflowI8 := block.NewBitCast(overflowGEP, irtypes.I8Ptr)
	// 5. Memcpy C struct data into overflow area.
	i8Src := src
	if !src.Type().Equal(irtypes.I8Ptr) {
		i8Src = block.NewBitCast(src, irtypes.I8Ptr)
	}

	block.NewCall(cg.ensureMemcpy(), overflowI8, i8Src, nativeSize, constant.NewInt(irtypes.I1, 0))
	// 6. Set c_data_ptr to point to the overflow area.
	cDataIdx := int64(cg.cDataPtrIndex(structName))
	cDataGep := block.NewGetElementPtr(tinSt, tinPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, cDataIdx))
	block.NewStore(overflowI8, cDataGep)
	// 7. Free original C pointer (data now lives in the Tin-owned overflow area).
	block.NewCall(cg.ensureHandoverFree(), i8Src)

	return tinPtr
}

// emitGenericPtrHandover handles {#handover} for any other pointer type by
// calling _tin_ptr_handover(src, elem_size), which copies into an RC allocation
// and frees the original if it was malloc'd.
func (cg *CodeGen) emitGenericPtrHandover(block *ir.Block, src value.Value, target irtypes.Type) value.Value {
	handoverFn := cg.ensureExternDecl("_tin_ptr_handover", irtypes.I8Ptr,
		[]*ir.Param{
			ir.NewParam("src", irtypes.I8Ptr),
			ir.NewParam("elem_size", irtypes.I64),
		}, false)

	// Determine element size from target type.
	// Use 0 for void* / i8* targets (unknown size; relies on malloc_usable_size).
	var elemSize value.Value
	if tgtPt, ok := target.(*irtypes.PointerType); ok &&
		tgtPt.ElemType != nil &&
		!irtypes.IsVoid(tgtPt.ElemType) &&
		!tgtPt.ElemType.Equal(irtypes.I8) {
		elemSize = cg.llvmSizeOf(block, tgtPt.ElemType)
	} else {
		elemSize = constant.NewInt(irtypes.I64, 0)
	}

	// Cast source to i8* for _tin_ptr_handover.
	i8Ptr := src
	if !src.Type().Equal(irtypes.I8Ptr) {
		i8Ptr = block.NewBitCast(src, irtypes.I8Ptr)
	}

	result := block.NewCall(handoverFn, i8Ptr, elemSize)

	// Cast result to the target pointer type.
	if target.Equal(irtypes.I8Ptr) {
		return result
	}

	return block.NewBitCast(result, target)
}

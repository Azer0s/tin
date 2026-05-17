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
		// Resolve type alias first (e.g. "Color" -> "raylib__Color") so that
		// package-qualified canonical names are found in structFieldLLVMTypes.
		structNameForExtern := st.Name
		if alias := cg.aliasTypeFor(CanonKey(structNameForExtern)); alias != nil {
			if simple, ok3 := alias.(*ast.SimpleType); ok3 {
				structNameForExtern = simple.Name
			}
		}

		if _, isStruct := cg.structFieldLLVMTypes[structNameForExtern]; isStruct {
			native, err := cg.tinStructNativeLLVM(structNameForExtern)
			if err != nil {
				return nil, err
			}

			if coerced := coerceNativeStructForABI(native); coerced != nil {
				if cg.targetIsAMD64() {
					// x86-64 SysV ABI: coerce to i(size*8).
					return coerced, nil
				}

				if cg.targetIsARM64() {
					// AAPCS64 (ARM64/Apple Silicon): all small integer structs
					// (<=8 bytes) are zero-extended to a 64-bit register (i64).
					return irtypes.I64, nil
				}
			}

			return native, nil
		}
	}
	// *...*S (any depth) where S is a named struct: build chain of native pointers.
	// *S -> *S.native, **S -> **S.native, ***S -> ***S.native, etc.
	if _, ok := te.(*ast.PointerType); ok {
		cur := te
		depth := 0

		for {
			pt, ok2 := cur.(*ast.PointerType)
			if !ok2 {
				break
			}

			if st, ok3 := pt.Elem.(*ast.SimpleType); ok3 {
				ptrStructName := st.Name
				if alias := cg.aliasTypeFor(CanonKey(ptrStructName)); alias != nil {
					if simple, ok5 := alias.(*ast.SimpleType); ok5 {
						ptrStructName = simple.Name
					}
				}

				_, isStruct := cg.structFieldLLVMTypes[ptrStructName]
				if isStruct {
					native, err := cg.tinStructNativeLLVM(ptrStructName)
					if err != nil {
						return nil, err
					}
					// Build depth+1 levels of pointer to native struct.
					var t irtypes.Type = native
					for i := 0; i <= depth; i++ {
						t = irtypes.NewPointer(t)
					}

					return t, nil
				}

				break // *primitive - not a struct pointer chain
			}

			depth++
			cur = pt.Elem
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
	// fn(...) T: at the C ABI boundary, a Tin fn value (fat-fn-ptr
	// `{fn(i8* env, params...) ret, i8* env}`) is incompatible with a
	// raw C function pointer (which has no env arg).  Lower to `i8*`;
	// the call site wraps Tin fn values via `tin_make_trampoline` to
	// produce a C-callable thunk, and the return path wraps incoming
	// raw C ptrs back into a fat-fn-ptr.  Without this case, externs
	// declared with fn-typed params received the fat-ptr struct as the
	// LLVM arg type, and the segfaults at every callback call site
	// (the env shifted every C param).
	if _, ok := te.(*ast.FuncType); ok {
		return irtypes.I8Ptr, nil
	}
	// *fn(...) T: pointer-to-fn extern param.  The Tin source shape
	// is `&cb` where cb is a fat-fn-ptr local; the C side expects
	// `fn_t *cbp` (pointer to a raw fn ptr).  Lower to `i8**`; the
	// call site builds a trampoline for the underlying fat-fn-ptr,
	// stores it in a fresh stack slot, and passes the slot address.
	if pt, ok := te.(*ast.PointerType); ok {
		if _, isFn := pt.Elem.(*ast.FuncType); isFn {
			return irtypes.NewPointer(irtypes.I8Ptr), nil
		}
	}

	return cg.tinTypeToLLVM(te)
}

// tinStructNativeLLVM returns (creating if needed) the C-compatible LLVM struct
// type for the named Tin struct: only user fields, no type_id or vtable pointers.
// The type is named "<structName>.native" in the LLVM module.
func (cg *CodeGen) tinStructNativeLLVM(structName string) (*irtypes.StructType, error) {
	nativeName := structName + ".native"
	// Return cached version if available.
	if st := cg.structTypeFor(CanonKey(nativeName)); st != nil {
		return st, nil
	}

	userFields, ok := cg.structFieldLLVMTypes[structName]
	if !ok {
		return nil, fmt.Errorf("tinStructNativeLLVM: unknown struct %q", structName)
	}

	tinFieldTypes := cg.structFieldTinTypes[structName]

	nativeFields := make([]irtypes.Type, len(userFields))
	for i, ft := range userFields {
		// Recursively convert nested named struct fields.
		if innerSt, ok2 := ft.(*irtypes.StructType); ok2 && innerSt.Name() != "" {
			inner, err := cg.tinStructNativeLLVM(innerSt.Name())
			if err != nil {
				return nil, err
			}

			nativeFields[i] = inner
		} else if i < len(tinFieldTypes) {
			// Fn-typed fields are lowered to raw C fn ptrs at the
			// boundary, matching `tinTypeToExternLLVM`.  Without this,
			// a Tin struct passed by value to C would carry the
			// fat-fn-ptr `{fn(...), env}` struct in the native layout
			// -- the C side would have to know Tin's internal layout
			// to call the callback.  The struct->native conversion in
			// wrapStructToExtern populates the i8* field with a
			// trampoline; this side just declares the slot type.
			if _, isFn := tinFieldTypes[i].(*ast.FuncType); isFn {
				nativeFields[i] = irtypes.I8Ptr
			} else if pt, isPt := tinFieldTypes[i].(*ast.PointerType); isPt {
				if _, innerFn := pt.Elem.(*ast.FuncType); innerFn {
					nativeFields[i] = irtypes.NewPointer(irtypes.I8Ptr)
				} else {
					nativeFields[i] = ft
				}
			} else {
				nativeFields[i] = ft
			}
		} else {
			nativeFields[i] = ft
		}
	}

	st := irtypes.NewStruct(nativeFields...)
	st.SetName(nativeName)
	cg.recordLLVM(CanonKey(nativeName), st)
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
	tinFieldTypes := cg.structFieldTinTypes[structName]

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
		// Fat-fn-ptr field (`f fn(...) T` in source).  At the C
		// boundary the native struct holds the field as `i8*` (raw
		// fn ptr).  Build a trampoline from the {fn, env} pair so C
		// can call through the slot directly; the trampoline bakes
		// in env via the runtime register-stash mechanism.
		if i < len(tinFieldTypes) {
			if ftAst, ok2 := tinFieldTypes[i].(*ast.FuncType); ok2 && isFatFnPtr(fv.Type()) {
				disp, dispErr := cg.getOrCreateClosureDispatcher(ftAst)
				if dispErr != nil {
					return nil, dispErr
				}

				// Non-colored sync variant (slot 0) -- C trampolines run synchronously.
				fnRaw := block.NewExtractValue(fv, 0)
				envRaw := block.NewExtractValue(fv, 3)
				fnI8 := block.NewBitCast(fnRaw, irtypes.I8Ptr)
				dispI8 := block.NewBitCast(disp, irtypes.I8Ptr)
				// Retain env: the trampoline transfers one ref; the
				// source struct field still owns the original.
				block.NewCall(cg.ensureRetain(), envRaw)
				fv = block.NewCall(cg.ensureMakeTrampoline(), fnI8, envRaw, dispI8)
			}
		}
		// *fn(...) field: source is a `*{fn, env}` (pointer to a
		// local fat-fn-ptr).  Native field is `i8**`.  Load the
		// fat-ptr, build trampoline, store in a fresh stack slot,
		// and use the slot's address as the native field value.
		if i < len(tinFieldTypes) {
			if ptAst, ok2 := tinFieldTypes[i].(*ast.PointerType); ok2 {
				if ftAst, isFn := ptAst.Elem.(*ast.FuncType); isFn {
					if argPt, isPtr := fv.Type().(*irtypes.PointerType); isPtr && isFatFnPtr(argPt.ElemType) {
						disp, dispErr := cg.getOrCreateClosureDispatcher(ftAst)
						if dispErr != nil {
							return nil, dispErr
						}

						fatVal := block.NewLoad(argPt.ElemType, fv)
						// Non-colored sync variant (slot 0) -- C trampolines run synchronously.
						fnRaw := block.NewExtractValue(fatVal, 0)
						envRaw := block.NewExtractValue(fatVal, 3)
						fnI8 := block.NewBitCast(fnRaw, irtypes.I8Ptr)
						dispI8 := block.NewBitCast(disp, irtypes.I8Ptr)
						// See sibling `*FuncType` field branch above.
						block.NewCall(cg.ensureRetain(), envRaw)
						tramp := block.NewCall(cg.ensureMakeTrampoline(), fnI8, envRaw, dispI8)
						slot := block.NewAlloca(irtypes.I8Ptr)
						block.NewStore(tramp, slot)
						fv = slot
					}
				}
			}
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

	tinSt := cg.structTypeFor(CanonKey(structName))
	if tinSt == nil {
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
		name := st.Name
		// Resolve type alias so package-qualified canonical names are found
		// (e.g. "Color" -> "raylib__Color" after `use raylib`).
		if alias := cg.aliasTypeFor(CanonKey(name)); alias != nil {
			if simple, ok3 := alias.(*ast.SimpleType); ok3 {
				name = simple.Name
			}
		}

		if _, isStruct := cg.structFieldLLVMTypes[name]; isStruct {
			return name, true
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

	tinSt := cg.structTypeFor(CanonKey(st.Name))
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

// isExternOutPtrParam checks if te is N*StructName (N >= 2) where StructName
// is a cLayoutStruct. Returns the struct name, total depth N, and true when
// matched. Used for C output-parameter patterns (C writes (N-1)*S into *out).
func (cg *CodeGen) isExternOutPtrParam(te ast.TypeExpr) (string, int, bool) {
	depth := 0
	cur := te

	for {
		pt, ok := cur.(*ast.PointerType)
		if !ok {
			return "", 0, false
		}

		depth++

		if st, ok2 := pt.Elem.(*ast.SimpleType); ok2 {
			if cg.cLayoutStructs[st.Name] && depth >= 2 {
				return st.Name, depth, true
			}

			return "", 0, false
		}

		cur = pt.Elem
	}
}

// markPtrStructCLayout marks the struct at the base of a *...*StructName type
// (any depth) as needing a hidden C source pointer field. Handles *S, **S,
// ***S, etc. Skips primitives like *void, *i8.
func (cg *CodeGen) markPtrStructCLayout(te ast.TypeExpr) {
	cur := te

	for {
		pt, ok := cur.(*ast.PointerType)
		if !ok {
			return
		}

		if st, ok2 := pt.Elem.(*ast.SimpleType); ok2 {
			if cg.structTypeFor(CanonKey(st.Name)) != nil {
				cg.cLayoutStructs[st.Name] = true
			}

			return
		}

		cur = pt.Elem
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

// isNativeStructHFA checks whether the native struct is a Homogeneous
// Floating-point Aggregate (HFA) as defined by AAPCS64: all leaf fields are
// the same base floating-point type and there are between 1 and 4 such fields.
// HFAs are passed in VFP registers (d0-d3 for doubles) on ARM64, regardless
// of total size, so they must NOT be treated as byval or indirect pointer args.
// Returns (true, count) when the struct is an HFA, (false, 0) otherwise.
func isNativeStructHFA(st *irtypes.StructType) bool {
	var baseKind irtypes.FloatKind

	count := 0

	if !collectHFALeaves(st, &baseKind, &count, true) {
		return false
	}

	return count >= 1 && count <= 4
}

// collectHFALeaves recursively collects leaf float types in a struct.
// All leaves must be the same float kind; any non-float leaf returns false.
// isFirst indicates whether we haven't seen any float yet (to set baseKind).
func collectHFALeaves(t irtypes.Type, baseKind *irtypes.FloatKind, count *int, isFirst bool) bool {
	switch ft := t.(type) {
	case *irtypes.FloatType:
		if *count == 0 {
			*baseKind = ft.Kind
		} else if ft.Kind != *baseKind {
			return false // mixed float kinds
		}

		*count++

		return true

	case *irtypes.StructType:
		for _, f := range ft.Fields {
			if !collectHFALeaves(f, baseKind, count, false) {
				return false
			}
		}

		return true

	default:
		_ = isFirst

		return false // integer or pointer leaf -> not an HFA
	}
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

// coerceNativeStructForABI2Reg checks whether a native struct (9-16 bytes,
// all-integer) should be split into two i64 parameters for the x86-64 SysV
// ABI (matching clang's (i64, i64) coercion). Returns true for such structs.
func coerceNativeStructForABI2Reg(st *irtypes.StructType) bool {
	size := nativeStructByteSize(st)
	if size <= 8 || size > 16 {
		return false
	}

	return nativeStructAllInteger(st)
}

// ensureExternDecl returns (or creates) a bare LLVM function declaration for a
// C extern symbol. Re-uses an existing declaration if one with a matching
// signature already exists.
func (cg *CodeGen) ensureExternDecl(cName string, retType irtypes.Type, params []*ir.Param, variadic bool) *ir.Func {
	for _, f := range cg.allFuncs() {
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

// ensureCStrLenDecl lazily declares `_tin_extern_cstr_len`, a NULL-safe
// strlen used when wrapping C `char*` returns into Tin fat-string fat ptrs.
// Returns 0 for a NULL input; defers to strlen otherwise.  Defined in
// runtime.c.
func (cg *CodeGen) ensureCStrLenDecl() *ir.Func {
	return cg.ensureExternDecl("_tin_extern_cstr_len", irtypes.I64,
		[]*ir.Param{ir.NewParam("s", irtypes.I8Ptr)}, false)
}

// extractFatPtrData extracts field 0 (the raw data pointer) from a fat-pointer
// struct value. Works whether the value is a struct value or a pointer to one.
func (cg *CodeGen) extractFatPtrData(block *ir.Block, val value.Value, st *irtypes.StructType) value.Value {
	return block.NewExtractValue(val, 0)
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
			// char* -> atom: handover variant frees the input string.
			if isAtomType(target) {
				return block.NewCall(cg.ensureStringToAtomHandover(), val)
			}
			// char* -> fat-ptr string: _tin_string_handover + fat-ptr.
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
			// native_struct* -> *TinStruct: load, free original, convert layout, RC-alloc.
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
	// i8* -> Tin fat-fn-ptr {sync*, colored*, coro*, i8* env}:
	// the extern returned a raw C function pointer; wrap it into a
	// Tin fat-fn-ptr value.  Env (slot 3) points at a small RC block
	// laid out as {i8* dtor=null, i8* c_fn_ptr} -- the standard
	// fat-fn-ptr release path (_tin_release_closure) frees it when
	// the wrapped value's last reference is dropped.  Slots 0 and 1
	// are a per-signature shim that loads c_fn_ptr from env+8 and
	// calls through it.  Slot 2 (coro ramp) is a synth wrapper so
	// `spawn ret_of_c_fn(args)` works.  Symmetric counterpart to
	// wrapFatFnPtrAsCCallback in exprs_call.go (Tin-fn -> C-arg).
	if src.Equal(irtypes.I8Ptr) {
		if tgtSt, ok := target.(*irtypes.StructType); ok && isFatFnPtr(tgtSt) {
			shim := cg.getOrCreateCFnShimFromLLVM(tgtSt)
			// Allocate the env block via _tin_rc_alloc(16) so
			// _tin_release_closure can free it safely.
			envI8 := block.NewCall(cg.ensureRCAlloc(),
				constant.NewInt(irtypes.I64, 16))
			i8PtrPtr := irtypes.NewPointer(irtypes.I8Ptr)
			// Slot 0: dtor (null -- no captured RC values to release).
			dtorSlot := block.NewBitCast(envI8, i8PtrPtr)
			block.NewStore(constant.NewNull(irtypes.I8Ptr), dtorSlot)
			// Slot 1: the raw C fn ptr (env+8).
			cFnPtrSlot := block.NewGetElementPtr(irtypes.I8, envI8,
				constant.NewInt(irtypes.I32, 8))
			cFnPtrSlotTyped := block.NewBitCast(cFnPtrSlot, i8PtrPtr)
			block.NewStore(val, cFnPtrSlotTyped)

			return cg.buildFatFnPtrValue(block, shim, envI8)
		}
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
	//
	// Use _tin_extern_cstr_len (NULL-safe strlen wrapper) instead of bare
	// strlen so that C APIs returning NULL for absent values -- getenv
	// missing var, ttyname not a tty, readline EOF -- don't segfault when
	// we wrap their return.  When len is 0 (input was NULL or empty C
	// string) we materialize an empty fat string {NULL, 0} rather than
	// rc_alloc'ing a 1-byte buffer just to copy the lone NUL terminator.
	if _, ok := src.(*irtypes.PointerType); ok {
		if tgtSt, ok2 := target.(*irtypes.StructType); ok2 && isFatPtrType(target) {
			cstrLenFn := cg.ensureCStrLenDecl()

			rawI8Ptr := val
			if !src.Equal(irtypes.I8Ptr) {
				rawI8Ptr = block.NewBitCast(val, irtypes.I8Ptr)
			}

			// length = 0 when rawI8Ptr is NULL (NULL-safe wrapper).
			length := block.NewCall(cstrLenFn, rawI8Ptr)
			allocSize := block.NewAdd(length, constant.NewInt(irtypes.I64, 1))

			// Always allocate (size = len + 1).  When length == 0 (NULL or
			// empty input) the alloc is just 1 byte -- a writable NUL
			// terminator -- so we can safely memcpy with len bytes (zero
			// bytes from NULL is a no-op; libc treats memcpy(_, NULL, 0)
			// as valid by convention).  Avoid the bare 0-byte alloc path
			// since RC headers need a real allocation to hang off of.
			rcRaw := block.NewCall(cg.ensureRCAlloc(), allocSize)

			// Avoid memcpy when input is NULL: undefined behavior on some
			// libc impls.  Branch on length-as-proxy-for-non-null since we
			// only get length > 0 on a valid non-empty C string.
			isEmpty := block.NewICmp(enum.IPredEQ, length, constant.NewInt(irtypes.I64, 0))
			// Materialize a non-null source for the memcpy path; the
			// `select` keeps the branch off the IR critical path -- when
			// isEmpty is true the dst gets one byte (the NUL we'll write
			// below).
			memcpyLen := block.NewSelect(isEmpty, constant.NewInt(irtypes.I64, 0), length)
			block.NewCall(cg.ensureMemcpy(), rcRaw, rawI8Ptr, memcpyLen, constant.NewInt(irtypes.I1, 0))

			// Write the trailing NUL ourselves so the RC buffer is a
			// valid C string regardless of which path produced it.
			nulGep := block.NewGetElementPtr(irtypes.I8, rcRaw, length)
			block.NewStore(constant.NewInt(irtypes.I8, 0), nulGep)

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
	wrapperType := cg.structTypeFor(CanonKey(structName))
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

	structType := cg.structTypeFor(CanonKey(structName))
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

// emitWrapNativeChain recursively converts a C native pointer chain into a Tin
// wrapper chain. nativeVal has type (depth)*S.native. Returns the Tin
// (depth)*S.wrapper pointer value.
//
//	depth=1: nativeVal is *S.native  -> returns *S.wrapper (borrow wrapper, heap RC=1)
//	depth=2: nativeVal is **S.native -> loads inner *S.native, wraps depth-1,
//	         allocates 8-byte RC block to hold inner wrapper ptr, returns **S.wrapper
//	depth=3: nativeVal is ***S.native -> analogous recursion, returns ***S.wrapper
//
// For depths > 1, each intermediate pointer level is an RC-allocated heap block
// holding the inner wrapper pointer. The scope release must free the chain via
// ensureHeapChainReleaseFn.
func (cg *CodeGen) emitWrapNativeChain(block *ir.Block, nativeVal value.Value, structName string, depth int) (value.Value, error) {
	wrapperSt := cg.structTypeFor(CanonKey(structName))
	if wrapperSt == nil {
		return nil, fmt.Errorf("emitWrapNativeChain: unknown struct %q", structName)
	}

	if depth == 1 {
		wrapperPtrType := irtypes.NewPointer(wrapperSt)

		return cg.emitStructPtrBorrow(block, nativeVal, wrapperPtrType, structName), nil
	}

	// depth > 1: load the inner (depth-1)*S.native from nativeVal
	innerType := nativeVal.Type().(*irtypes.PointerType).ElemType
	innerNativeVal := block.NewLoad(innerType, nativeVal)

	// Recursively build the inner wrapper chain
	innerWrapper, err := cg.emitWrapNativeChain(block, innerNativeVal, structName, depth-1)
	if err != nil {
		return nil, err
	}

	// Allocate a 8-byte RC block to hold the inner wrapper pointer.
	ptrSize := constant.NewInt(irtypes.I64, 8)
	rcRaw := block.NewCall(cg.ensureRCAlloc(), ptrSize)
	outerPtrType := irtypes.NewPointer(innerWrapper.Type())
	outerBlockPtr := block.NewBitCast(rcRaw, outerPtrType)
	block.NewStore(innerWrapper, outerBlockPtr)

	return outerBlockPtr, nil
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

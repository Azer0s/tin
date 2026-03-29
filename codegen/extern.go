package codegen

// extern.go - helpers for mapping Tin types to C-compatible LLVM types,
// declaring extern C functions, and wrapping/unwrapping fat-pointer arguments.

import (
	"fmt"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
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
		if _, isStruct := cg.structFieldLLVMTypes[st.Name]; isStruct {
			return cg.tinStructNativeLLVM(st.Name)
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
	// Index offset: 1 (type_id) + vtableCount
	offset := int64(1 + cg.vtableOffset(structName))
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
func (cg *CodeGen) wrapNativeStructToTin(block *ir.Block, val value.Value, structName string) (value.Value, error) {
	nativeSt, ok := val.Type().(*irtypes.StructType)
	if !ok {
		return val, nil
	}
	tinSt, tinOk := cg.structTypes[structName]
	if !tinOk {
		return val, nil
	}
	userFields := cg.structFieldLLVMTypes[structName]
	typeID := cg.structTypeIDs[structName]
	offset := int64(1 + cg.vtableOffset(structName))

	nativeAlloca := block.NewAlloca(nativeSt)
	block.NewStore(val, nativeAlloca)
	tinAlloca := block.NewAlloca(tinSt)

	// Set type_id (field 0).
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
func (cg *CodeGen) wrapFromExtern(block *ir.Block, val value.Value, target irtypes.Type) value.Value {
	src := val.Type()
	if src.Equal(target) {
		return val
	}
	// i8* -> %__atom: find atom in table via strcmp.
	if _, ok := src.(*irtypes.PointerType); ok {
		if isAtomType(target) {
			return block.NewCall(cg.ensureStringToAtom(), val)
		}
	}
	// raw pointer -> fat-pointer: build {ptr, len}
	if _, ok := src.(*irtypes.PointerType); ok {
		if tgtSt, ok2 := target.(*irtypes.StructType); ok2 && isFatPtrType(target) {
			// Coerce pointer to the type expected by field 0
			var ptr value.Value
			if src.Equal(tgtSt.Fields[0]) {
				ptr = val
			} else {
				ptr = block.NewBitCast(val, tgtSt.Fields[0])
			}
			// Use strlen to get the length (treat as a null-terminated string)
			strlenFn := cg.ensureStrlenDecl()
			rawI8Ptr := ptr
			if !src.Equal(irtypes.I8Ptr) {
				rawI8Ptr = block.NewBitCast(val, irtypes.I8Ptr)
			}
			length := block.NewCall(strlenFn, rawI8Ptr)
			// Build the fat-pointer struct
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

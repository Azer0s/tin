package codegen

// types.go — LLVM type mapping, type helpers, and type query utilities.

import (
	"github.com/Azer0s/tin/ast"
	irtypes "github.com/llir/llvm/ir/types"
)

// -- Type mapping

// tinTypeToLLVM converts an ast.TypeExpr to an LLVM type.
func (cg *CodeGen) tinTypeToLLVM(te ast.TypeExpr) (irtypes.Type, error) {
	if te == nil {
		return irtypes.Void, nil
	}
	switch t := te.(type) {
	case *ast.SimpleType:
		return cg.resolveSimpleType(t.Name)
	case *ast.VoidType:
		return irtypes.Void, nil
	case *ast.PointerType:
		// *void is invalid in LLVM IR — use i8* (opaque pointer convention)
		if st, ok := t.Elem.(*ast.SimpleType); ok && st.Name == "void" {
			return irtypes.I8Ptr, nil
		}
		inner, err := cg.tinTypeToLLVM(t.Elem)
		if err != nil {
			return nil, err
		}
		return irtypes.NewPointer(inner), nil
	case *ast.ArrayType:
		elem, err := cg.tinTypeToLLVM(t.Elem)
		if err != nil {
			return nil, err
		}
		if t.Size < 0 {
			// Dynamic array: {elem*, i64}
			return irtypes.NewStruct(irtypes.NewPointer(elem), irtypes.I64), nil
		}
		return irtypes.NewArray(uint64(t.Size), elem), nil
	case *ast.FuncType:
		// Function values are fat pointers: { fn(i8* env, params...) ret *, i8* }
		// The i8* env carries the closure environment; non-capturing lambdas use null
		llParams := []irtypes.Type{irtypes.I8Ptr} // env is always first
		for _, p := range t.Params {
			pt, err := cg.tinTypeToLLVM(p)
			if err != nil {
				return nil, err
			}
			llParams = append(llParams, pt)
		}
		var ret irtypes.Type = irtypes.Void
		if t.RetType != nil {
			var err error
			ret, err = cg.tinTypeToLLVM(t.RetType)
			if err != nil {
				return nil, err
			}
		}
		ft := irtypes.NewFunc(ret, llParams...)
		ft.Variadic = t.IsVarArgs
		// Fat pointer struct: { fn_ptr*, i8* }
		return irtypes.NewStruct(irtypes.NewPointer(ft), irtypes.I8Ptr), nil
	case *ast.GenericType:
		// Handle known generic types
		if t.Name == "fn" && len(t.TypeParams) >= 1 {
			return cg.tinTypeToLLVM(&ast.FuncType{})
		}
		// Generic trait instantiation (e.g. iter[i64]) → fat pointer type
		if td, ok := cg.traits[t.Name]; ok {
			instKey := traitImplKey(t)
			typeSubst := map[string]irtypes.Type{}
			for i, tpName := range td.TypeParams {
				if i < len(t.TypeParams) {
					lt, err := cg.tinTypeToLLVM(t.TypeParams[i])
					if err != nil {
						return nil, err
					}
					typeSubst[tpName] = lt
				}
			}
			return cg.buildTraitFatPtrTypeInst(t.Name, instKey, typeSubst)
		}
		// Generic data type instantiation (e.g. maybe[string])
		if dd, ok := cg.genericDataDecls[t.Name]; ok {
			return cg.instantiateDataType(dd, t.TypeParams)
		}
		return cg.resolveSimpleType(t.Name)
	case *ast.UnionTypeExpr:
		// Simplified: use i64 for union types
		return irtypes.I64, nil
	}
	return irtypes.I64, nil
}

func (cg *CodeGen) resolveSimpleType(name string) (irtypes.Type, error) {
	switch name {
	case "void":
		return irtypes.Void, nil
	case "bool":
		return irtypes.I1, nil
	case "i8":
		return irtypes.I8, nil
	case "i16":
		return irtypes.I16, nil
	case "i32":
		return irtypes.I32, nil
	case "i64", "int":
		return irtypes.I64, nil
	case "u8", "char":
		return irtypes.I8, nil
	case "u16":
		return irtypes.I16, nil
	case "u32", "uint32":
		return irtypes.I32, nil
	case "u64", "uint", "size_t":
		return irtypes.I64, nil
	case "f32":
		return irtypes.Float, nil
	case "f64":
		return irtypes.Double, nil
	case "string":
		// fat pointer: {i8*, i64}
		return irtypes.NewStruct(irtypes.I8Ptr, irtypes.I64), nil
	case "atom":
		// Atoms are represented as string fat-pointers at runtime
		return irtypes.NewStruct(irtypes.I8Ptr, irtypes.I64), nil
	case "any":
		// fat pointer: {i8*, i32}  (type-tagged box)
		return anyFatPtrType(), nil
	}
	// Check trait types — represented as fat pointers {i8*, vtable*}
	if _, ok := cg.traits[name]; ok {
		fp, err := cg.buildTraitFatPtrType(name)
		if err != nil {
			return nil, err
		}
		return fp, nil
	}
	// Check struct types
	if st, ok := cg.structTypes[name]; ok {
		return st, nil
	}
	// Check enum types
	if et, ok := cg.enumTypes[name]; ok {
		return et, nil
	}
	// Check type aliases
	if alias, ok := cg.typeAliases[name]; ok {
		return cg.tinTypeToLLVM(alias)
	}
	// Default to i64
	return irtypes.I64, nil
}

// stringFatPtrType returns the {i8*, i64} type used for tin strings.
func stringFatPtrType() *irtypes.StructType {
	return irtypes.NewStruct(irtypes.I8Ptr, irtypes.I64)
}

// anyFatPtrType returns the {i32, i8*} type used for tin `any` values.
// Field 0: i32  – type tag (0=i64, 1=f64, 2=string, 3=bool, 4=ptr, 5+=user).
// Field 1: i8*  – pointer to the boxed value on the stack or heap.
// The type tag is always field 0 so that any pointer can be bitcast to
// *any and the type read from field 0.
func anyFatPtrType() *irtypes.StructType {
	return irtypes.NewStruct(irtypes.I32, irtypes.I8Ptr)
}

const (
	anyTagInt    = int32(0)
	anyTagFloat  = int32(1)
	anyTagString = int32(2)
	anyTagBool   = int32(3)
	anyTagPtr    = int32(4)
	anyTagFn     = int32(5) // closure / fat function pointer
)

// -- Type size helpers

// llvmTypeSize returns the byte size of an LLVM type (approximate, for data
// type payload sizing on a 64-bit target).
func llvmTypeSize(t irtypes.Type) uint64 {
	sz, _ := llvmTypeSizeAlign(t)
	return sz
}

// llvmTypeAlign returns the ABI alignment of t on a 64-bit target.
func llvmTypeAlign(t irtypes.Type) uint64 {
	_, al := llvmTypeSizeAlign(t)
	return al
}

// llvmTypeSizeAlign returns (size, alignment) for t on a 64-bit x86 target.
// It accounts for alignment padding so that malloc receives the correct size.
func llvmTypeSizeAlign(t irtypes.Type) (uint64, uint64) {
	switch ty := t.(type) {
	case *irtypes.IntType:
		b := (ty.BitSize + 7) / 8
		return b, b
	case *irtypes.FloatType:
		switch ty.Kind {
		case irtypes.FloatKindHalf:
			return 2, 2
		case irtypes.FloatKindFloat:
			return 4, 4
		case irtypes.FloatKindDouble:
			return 8, 8
		}
		return 8, 8
	case *irtypes.PointerType:
		return 8, 8
	case *irtypes.StructType:
		var offset, maxAlign uint64
		for _, f := range ty.Fields {
			fsz, fal := llvmTypeSizeAlign(f)
			if fal > maxAlign {
				maxAlign = fal
			}
			// Align current offset to field's alignment
			if fal > 0 {
				offset = (offset + fal - 1) &^ (fal - 1)
			}
			offset += fsz
		}
		if maxAlign == 0 {
			maxAlign = 1
		}
		// Pad struct to its own alignment
		offset = (offset + maxAlign - 1) &^ (maxAlign - 1)
		return offset, maxAlign
	case *irtypes.ArrayType:
		esz, eal := llvmTypeSizeAlign(ty.ElemType)
		return ty.Len * esz, eal
	}
	return 8, 8
}

// -- Type query helpers

// isFatPtrType returns true if t is a two-field struct whose first field
// is a pointer — i.e., a Tin fat-pointer (string, array, etc.).
func isFatPtrType(t irtypes.Type) bool {
	st, ok := t.(*irtypes.StructType)
	if !ok || len(st.Fields) != 2 {
		return false
	}
	_, isPtr := st.Fields[0].(*irtypes.PointerType)
	return isPtr && irtypes.IsInt(st.Fields[1])
}

// isStringType returns true if t is the tin string fat-pointer type {i8*, i64}.
// Named structs (user-defined) are never fat-pointers.
func isStringType(t irtypes.Type) bool {
	st, ok := t.(*irtypes.StructType)
	if !ok || st.Name() != "" || len(st.Fields) != 2 {
		return false
	}
	return st.Fields[0] == irtypes.I8Ptr && st.Fields[1].Equal(irtypes.I64)
}

// isAnyType returns true if t is the tin `any` fat-pointer type {i32, i8*}.
// Named structs (user-defined) are never fat-pointers.
func isAnyType(t irtypes.Type) bool {
	st, ok := t.(*irtypes.StructType)
	if !ok || st.Name() != "" || len(st.Fields) != 2 {
		return false
	}
	return st.Fields[0].Equal(irtypes.I32) && st.Fields[1] == irtypes.I8Ptr
}

// isFatArrayPtr returns true for anonymous {T*, i64} fat array pointer structs.
// Named structs (user-defined) are excluded to avoid false matches with
// structs that embed vtable pointers as their first field.
func isFatArrayPtr(t irtypes.Type) bool {
	st, ok := t.(*irtypes.StructType)
	if !ok || st.Name() != "" || len(st.Fields) != 2 {
		return false
	}
	if !irtypes.IsInt(st.Fields[1]) {
		return false
	}
	_, ok = st.Fields[0].(*irtypes.PointerType)
	return ok
}

// isFatFnPtr returns true when t is a closure fat pointer { fn(i8*,...)*, i8* }.
func isFatFnPtr(t irtypes.Type) bool {
	st, ok := t.(*irtypes.StructType)
	if !ok || len(st.Fields) != 2 {
		return false
	}
	if st.Fields[1] != irtypes.I8Ptr {
		return false
	}
	pt, ok := st.Fields[0].(*irtypes.PointerType)
	if !ok {
		return false
	}
	_, ok = pt.ElemType.(*irtypes.FuncType)
	return ok
}

// typeNameOf returns the type name for a struct or empty string.
func (cg *CodeGen) typeNameOf(t irtypes.Type) string {
	if st, ok := t.(*irtypes.StructType); ok {
		return st.Name()
	}
	return ""
}

// vtableOffset returns the number of vtable pointer fields prepended to the
// LLVM layout of structName (0 for structs that implement no traits).
func (cg *CodeGen) vtableOffset(structName string) int {
	return len(cg.structVtableOrder[structName])
}

// fieldIndex returns the LLVM field index for a named user field, accounting
// for the leading i32 type_id and vtable pointer fields at the front.
// Layout: [i32 type_id, vtable_0*, …, user_field_0, …]
// Returns -1 if not found.
func (cg *CodeGen) fieldIndex(structName, fieldName string) int {
	names, ok := cg.structFields[structName]
	if !ok {
		return -1
	}
	// +1 for the leading i32 type_id field
	offset := 1 + cg.vtableOffset(structName)
	for i, n := range names {
		if n == fieldName {
			return offset + i
		}
	}
	return -1
}

// resolveTypeWithSubst converts a TypeExpr to LLVM type, substituting any
// type parameter names found in subst.
func (cg *CodeGen) resolveTypeWithSubst(te ast.TypeExpr, subst map[string]irtypes.Type) (irtypes.Type, error) {
	if len(subst) == 0 {
		return cg.tinTypeToLLVM(te)
	}
	if st, ok := te.(*ast.SimpleType); ok {
		if lt, ok2 := subst[st.Name]; ok2 {
			return lt, nil
		}
	}
	return cg.tinTypeToLLVM(te)
}

// typeExprName returns a short string name for a TypeExpr (used in instance keys).
func typeExprName(te ast.TypeExpr) string {
	switch t := te.(type) {
	case *ast.SimpleType:
		return t.Name
	case *ast.GenericType:
		return t.Name
	}
	return "unknown"
}

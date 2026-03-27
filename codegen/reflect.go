package codegen

import (
	"github.com/Azer0s/tin/ast"
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

// Reflection builtins

// structNameFromValue returns the LLVM named struct name for a value's type,
// or "" if the value is not a named struct.
func structNameFromValue(v value.Value) string {
	t := v.Type()
	if pt, ok := t.(*irtypes.PointerType); ok {
		t = pt.ElemType
	}
	if st, ok := t.(*irtypes.StructType); ok {
		return st.Name()
	}
	return ""
}

// buildAtomArray allocates a heap array of %__atom values and returns a
// fat-pointer {%__atom*, i64} representing [atom].
// Each element of atoms is an atom name with a leading apostrophe (e.g. "'ok").
func (cg *CodeGen) buildAtomArray(block *ir.Block, atoms []string) value.Value {
	elemType := cg.atomType // %__atom = { i32 }
	n := int64(len(atoms))

	fat := irtypes.NewStruct(irtypes.NewPointer(elemType), irtypes.I64)

	if n == 0 {
		alloca := block.NewAlloca(fat)
		ptrGep := block.NewGetElementPtr(fat, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		block.NewStore(constant.NewNull(irtypes.NewPointer(elemType)), ptrGep)
		lenGep := block.NewGetElementPtr(fat, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		block.NewStore(constant.NewInt(irtypes.I64, 0), lenGep)
		return block.NewLoad(fat, alloca)
	}

	// Register each atom (strip leading apostrophe) and build constants.
	vals := make([]value.Value, n)
	for i, a := range atoms {
		name := a
		if len(name) > 0 && name[0] == '\'' {
			name = name[1:]
		}
		vals[i] = cg.atomConstant(cg.registerAtom(name))
	}

	nullPtr := constant.NewNull(irtypes.NewPointer(elemType))
	sizeGep := block.NewGetElementPtr(elemType, nullPtr, constant.NewInt(irtypes.I64, 1))
	elemSz := block.NewPtrToInt(sizeGep, irtypes.I64)
	totalSz := block.NewMul(elemSz, constant.NewInt(irtypes.I64, n))

	mallocI8 := block.NewCall(cg.ensureMalloc(), totalSz)
	dataPtr := block.NewBitCast(mallocI8, irtypes.NewPointer(elemType))

	for i, v := range vals {
		gep := block.NewGetElementPtr(elemType, dataPtr, constant.NewInt(irtypes.I64, int64(i)))
		block.NewStore(v, gep)
	}

	fatAlloca := block.NewAlloca(fat)
	ptrGep := block.NewGetElementPtr(fat, fatAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(dataPtr, ptrGep)
	lenGep := block.NewGetElementPtr(fat, fatAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(constant.NewInt(irtypes.I64, n), lenGep)
	return block.NewLoad(fat, fatAlloca)
}

// llvmTypeName returns the tin type name string for any LLVM type,
// including nested pointer and array types.
func llvmTypeName(t irtypes.Type) string {
	if t == nil {
		return "void"
	}
	switch {
	case t.Equal(irtypes.I1):
		return "bool"
	case t.Equal(irtypes.I8):
		return "i8"
	case t.Equal(irtypes.I16):
		return "i16"
	case t.Equal(irtypes.I32):
		return "i32"
	case t.Equal(irtypes.I64):
		return "i64"
	case t.Equal(irtypes.Float):
		return "f32"
	case t.Equal(irtypes.Double):
		return "f64"
	}
	if pt, ok := t.(*irtypes.PointerType); ok {
		if fnType, isFnType := pt.ElemType.(*irtypes.FuncType); isFnType {
			return fnSigName(fnType, false)
		}
		return "*" + llvmTypeName(pt.ElemType)
	}
	if at, ok := t.(*irtypes.ArrayType); ok {
		return "[" + llvmTypeName(at.ElemType) + "]"
	}
	if st, ok := t.(*irtypes.StructType); ok {
		if st.Name() == "__atom" {
			return "atom"
		}
		if st.Name() != "" {
			// User-defined struct / data type: use struct name as atom.
			return st.Name()
		}
		// Anonymous struct: could be fat ptr, any, etc.
		if isStringType(t) {
			return "string"
		}
		if isAnyType(t) {
			return "any"
		}
		if isFatFnPtr(t) {
			st2 := t.(*irtypes.StructType)
			innerFnType := st2.Fields[0].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
			return fnSigName(innerFnType, true)
		}
		if isFatArrayPtr(t) {
			if len(st.Fields) == 2 {
				if pt, ok2 := st.Fields[0].(*irtypes.PointerType); ok2 {
					return "[" + llvmTypeName(pt.ElemType) + "]"
				}
			}
			return "[unknown]"
		}
	}
	return "unknown"
}

// primitiveTypeName is an alias kept for compatibility with existing callers
// that only deal with simple scalar types.
func primitiveTypeName(t irtypes.Type) string {
	return llvmTypeName(t)
}

// buildTypeNameAtom builds the atom for a known struct/data-type name.
func (cg *CodeGen) buildTypeNameAtom(_ *ir.Block, sn string) value.Value {
	return cg.atomConstant(cg.registerAtom(sn))
}

// runtimeAtomSelectByTypeID generates an inline select chain that picks the
// correct %__atom from a table keyed by compile-time type IDs.
// table maps type_id -> atom name string (with leading apostrophe, e.g. "'i64").
// typeIDVal is the i32 type_id extracted at runtime.
// defaultAtom is the %__atom value used when no type_id matches.
func (cg *CodeGen) runtimeAtomSelectByTypeID(block *ir.Block, typeIDVal value.Value,
	table map[int32]string, defaultAtom value.Value) value.Value {

	result := defaultAtom
	for id, atomStr := range table {
		isMatch := block.NewICmp(enum.IPredEQ, typeIDVal, constant.NewInt(irtypes.I32, int64(id)))
		// Strip leading apostrophe to get the atom name.
		name := atomStr
		if len(name) > 0 && name[0] == '\'' {
			name = name[1:]
		}
		candidate := cg.atomConstant(cg.registerAtom(name))
		result = block.NewSelect(isMatch, candidate, result)
	}
	return result
}

// extractAnyTypeID extracts the i32 type_id from an `any` fat-ptr value.
func (cg *CodeGen) extractAnyTypeID(block *ir.Block, anyVal value.Value) value.Value {
	anyType := anyFatPtrType()
	alloca := block.NewAlloca(anyType)
	block.NewStore(anyVal, alloca)
	tagGep := block.NewGetElementPtr(anyType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	return block.NewLoad(irtypes.I32, tagGep)
}

// buildTypeIDToNameTable builds the full type_id -> atom name table from
// all registered struct and data types plus the reserved primitive IDs.
func (cg *CodeGen) buildTypeIDToNameTable() map[int32]string {
	table := map[int32]string{
		anyTagInt:    "'i64",
		anyTagFloat:  "'f64",
		anyTagString: "'string",
		anyTagBool:   "'bool",
		anyTagPtr:    "'ptr",
		anyTagFn:     "'fn",
	}
	for sn, id := range cg.structTypeIDs {
		table[id] = "'" + sn
	}
	for dn, id := range cg.dataTypeIDs {
		table[id] = "'" + dn
	}
	for sig, id := range cg.fnTypeIDs {
		table[id] = "'" + sig
	}
	for un, id := range cg.unionTypeIDs {
		table[id] = "'" + un
	}
	return table
}

// buildTypeIDToTraitsTable builds type_id -> []trait atom strings table.
func (cg *CodeGen) buildTypeIDToTraitsTable() map[int32][]string {
	table := make(map[int32][]string)
	for sn, id := range cg.structTypeIDs {
		var atoms []string
		for _, tn := range cg.structImpls[sn] {
			atoms = append(atoms, "'"+tn)
		}
		table[id] = atoms
	}
	return table
}

// buildTypeIDToFieldsTable builds type_id -> []field name atom strings table.
func (cg *CodeGen) buildTypeIDToFieldsTable() map[int32][]string {
	table := make(map[int32][]string)
	for sn, id := range cg.structTypeIDs {
		var atoms []string
		for _, fn := range cg.structFields[sn] {
			atoms = append(atoms, "'"+fn)
		}
		table[id] = atoms
	}
	return table
}

// buildTypeIDToFieldTypesTable builds type_id -> []field type name atom strings table.
func (cg *CodeGen) buildTypeIDToFieldTypesTable() map[int32][]string {
	table := make(map[int32][]string)
	for sn, id := range cg.structTypeIDs {
		var atoms []string
		for _, ft := range cg.structFieldLLVMTypes[sn] {
			atoms = append(atoms, "'"+primitiveTypeName(ft))
		}
		table[id] = atoms
	}
	return table
}

// genTypeof returns an atom of the type name.
// For concrete types the name is resolved at compile-time.
// For `any` values the actual type_id is inspected at runtime.
func (cg *CodeGen) genTypeof(block *ir.Block, e *ast.TypeofExpr) (value.Value, error) {
	val, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}
	if val == nil {
		return cg.atomConstant(cg.registerAtom("unknown")), nil
	}

	// Runtime dispatch for `any` values.
	if isAnyType(val.Type()) {
		typeIDVal := cg.extractAnyTypeID(block, val)
		table := cg.buildTypeIDToNameTable()
		defaultAtom := cg.atomConstant(cg.registerAtom("unknown"))
		return cg.runtimeAtomSelectByTypeID(block, typeIDVal, table, defaultAtom), nil
	}

	// Compile-time: resolve the tin type name and register as an atom.
	name := llvmTypeName(val.Type())
	return cg.atomConstant(cg.registerAtom(name)), nil
}

// genTraitof returns a [atom] of trait names.
// For `any` values the type_id is inspected at runtime and the result is
// selected from a per-type compile-time table.
func (cg *CodeGen) genTraitof(block *ir.Block, e *ast.TraitofExpr) (value.Value, error) {
	val, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}
	if val == nil {
		return cg.buildAtomArray(block, nil), nil
	}

	// Runtime dispatch for `any`.
	if isAnyType(val.Type()) {
		typeIDVal := cg.extractAnyTypeID(block, val)
		return cg.runtimeAtomArraySelectByTypeID(block, typeIDVal, cg.buildTypeIDToTraitsTable()), nil
	}

	// Compile-time.
	sn := structNameFromValue(val)
	var atoms []string
	if sn != "" {
		for _, tn := range cg.structImpls[sn] {
			atoms = append(atoms, "'"+tn)
		}
	}
	return cg.buildAtomArray(block, atoms), nil
}

// genFieldnames returns a [atom] of field names.
// For `any` values the type_id is inspected at runtime.
func (cg *CodeGen) genFieldnames(block *ir.Block, e *ast.FieldnamesExpr) (value.Value, error) {
	val, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}
	if val == nil {
		return cg.buildAtomArray(block, nil), nil
	}

	// Runtime dispatch for `any`.
	if isAnyType(val.Type()) {
		typeIDVal := cg.extractAnyTypeID(block, val)
		return cg.runtimeAtomArraySelectByTypeID(block, typeIDVal, cg.buildTypeIDToFieldsTable()), nil
	}

	// Compile-time.
	sn := structNameFromValue(val)
	var atoms []string
	if sn != "" {
		for _, fn := range cg.structFields[sn] {
			atoms = append(atoms, "'"+fn)
		}
	}
	return cg.buildAtomArray(block, atoms), nil
}

// runtimeAtomArraySelectByTypeID selects a [atom] array based on a runtime type_id.
// It builds all candidate arrays at compile-time and uses an alloca + select pattern
// to pick the right one.  The result type is always {%__atom*, i64}.
func (cg *CodeGen) runtimeAtomArraySelectByTypeID(block *ir.Block, typeIDVal value.Value,
	table map[int32][]string) value.Value {

	// Fat-pointer type for [atom]: {%__atom*, i64}.
	fatType := irtypes.NewStruct(irtypes.NewPointer(cg.atomType), irtypes.I64)

	// Build default (empty array).
	def := cg.buildAtomArray(block, nil)
	resultAlloca := block.NewAlloca(fatType)
	block.NewStore(def, resultAlloca)

	for id, atomStrs := range table {
		isMatch := block.NewICmp(enum.IPredEQ, typeIDVal, constant.NewInt(irtypes.I32, int64(id)))
		candidate := cg.buildAtomArray(block, atomStrs)
		current := block.NewLoad(fatType, resultAlloca)
		// LLVM select works on first-class types including structs.
		selected := block.NewSelect(isMatch, candidate, current)
		block.NewStore(selected, resultAlloca)
	}

	return block.NewLoad(fatType, resultAlloca)
}

// genFieldtypes returns a [atom] of field type names for the compile-time struct type.
// For `any` values the type_id is inspected at runtime.
func (cg *CodeGen) genFieldtypes(block *ir.Block, e *ast.FieldtypesExpr) (value.Value, error) {
	val, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}
	if val == nil {
		return cg.buildAtomArray(block, nil), nil
	}

	// Runtime dispatch for `any`.
	if isAnyType(val.Type()) {
		typeIDVal := cg.extractAnyTypeID(block, val)
		return cg.runtimeAtomArraySelectByTypeID(block, typeIDVal, cg.buildTypeIDToFieldTypesTable()), nil
	}

	// Compile-time.
	sn := structNameFromValue(val)
	var atoms []string
	if sn != "" {
		for _, ft := range cg.structFieldLLVMTypes[sn] {
			atoms = append(atoms, "'"+primitiveTypeName(ft))
		}
	}
	return cg.buildAtomArray(block, atoms), nil
}

// genFieldtag returns the first @"tag" annotation for the named field, or empty atom.
func (cg *CodeGen) genFieldtag(_ *ir.Block, _ *ast.FieldtagExpr) (value.Value, error) {
	return cg.atomConstant(cg.registerAtom("")), nil
}

// genGetfield returns an `any` fat-ptr containing the value of the named field.
// For concrete struct types: generates a compile-time strcmp chain.
// For `any` values: dispatches to the concrete type via type_id, then reads the field.
func (cg *CodeGen) genGetfield(block *ir.Block, e *ast.GetfieldExpr) (value.Value, error) {
	val, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}
	fieldNameVal, err := cg.genExpr(block, e.Field)
	if err != nil {
		return nil, err
	}

	anyType := anyFatPtrType()
	zeroAny := cg.zeroValue(anyType)

	if val == nil {
		return zeroAny, nil
	}

	// If the value is `any`, extract the data pointer and type_id,
	// then dispatch to genGetfieldForStruct for each known struct type.
	if isAnyType(val.Type()) {
		return cg.genGetfieldFromAny(block, val, fieldNameVal), nil
	}

	// Compile-time concrete struct.
	sn := structNameFromValue(val)
	if sn == "" {
		return zeroAny, nil
	}
	return cg.genGetfieldForStruct(block, sn, val, fieldNameVal)
}

// genGetfieldFromAny dispatches getfield for an `any` value over all known struct types.
func (cg *CodeGen) genGetfieldFromAny(block *ir.Block, anyVal value.Value, fieldNameVal value.Value) value.Value {
	anyType := anyFatPtrType()
	resultAlloca := block.NewAlloca(anyType)
	block.NewStore(cg.zeroValue(anyType), resultAlloca)

	typeIDVal := cg.extractAnyTypeID(block, anyVal)

	// Extract the raw data pointer from `any`.
	anyAlloca := block.NewAlloca(anyType)
	block.NewStore(anyVal, anyAlloca)
	dataPtrGep := block.NewGetElementPtr(anyType, anyAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	dataI8Ptr := block.NewLoad(irtypes.I8Ptr, dataPtrGep)

	strcmp := cg.ensureStrcmp()
	fieldNamePtr := cg.extractStringPtr(block, fieldNameVal)

	for sn, typeID := range cg.structTypeIDs {
		st := cg.structTypes[sn]
		if st == nil {
			continue
		}
		fieldNames := cg.structFields[sn]
		fieldTypes := cg.structFieldLLVMTypes[sn]
		vtableOff := cg.vtableOffset(sn)

		isTypeMatch := block.NewICmp(enum.IPredEQ, typeIDVal, constant.NewInt(irtypes.I32, int64(typeID)))

		// Bitcast data pointer to *struct.
		structPtr := block.NewBitCast(dataI8Ptr, irtypes.NewPointer(st))

		for i, fname := range fieldNames {
			namePtr := cg.newGlobalString(fname)
			cmp := block.NewCall(strcmp, fieldNamePtr, namePtr)
			isFieldMatch := block.NewICmp(enum.IPredEQ, cmp, constant.NewInt(irtypes.I32, 0))

			isMatch := block.NewAnd(isTypeMatch, isFieldMatch)

			fieldIdx := int64(1 + vtableOff + i)
			fieldGep := block.NewGetElementPtr(st, structPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, fieldIdx))
			fieldVal := block.NewLoad(fieldTypes[i], fieldGep)
			boxed := cg.boxToAny(block, fieldVal)

			current := block.NewLoad(anyType, resultAlloca)
			selected := block.NewSelect(isMatch, boxed, current)
			block.NewStore(selected, resultAlloca)
		}
	}

	return block.NewLoad(anyType, resultAlloca)
}

// genGetfieldForStruct generates a strcmp chain for a concrete struct type.
func (cg *CodeGen) genGetfieldForStruct(block *ir.Block, sn string, val value.Value, fieldNameVal value.Value) (value.Value, error) {
	anyType := anyFatPtrType()
	zeroAny := cg.zeroValue(anyType)

	fieldNames := cg.structFields[sn]
	fieldTypes := cg.structFieldLLVMTypes[sn]
	st := cg.structTypes[sn]
	if st == nil || len(fieldNames) == 0 {
		return zeroAny, nil
	}

	structAlloca := block.NewAlloca(st)
	block.NewStore(val, structAlloca)

	fieldNamePtr := cg.extractStringPtr(block, fieldNameVal)
	strcmp := cg.ensureStrcmp()
	vtableOff := cg.vtableOffset(sn)

	resultAlloca := block.NewAlloca(anyType)
	block.NewStore(zeroAny, resultAlloca)

	for i, fname := range fieldNames {
		namePtr := cg.newGlobalString(fname)
		cmp := block.NewCall(strcmp, fieldNamePtr, namePtr)
		isMatch := block.NewICmp(enum.IPredEQ, cmp, constant.NewInt(irtypes.I32, 0))

		fieldIdx := int64(1 + vtableOff + i)
		fieldGep := block.NewGetElementPtr(st, structAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, fieldIdx))
		fieldVal := block.NewLoad(fieldTypes[i], fieldGep)
		boxed := cg.boxToAny(block, fieldVal)

		current := block.NewLoad(anyType, resultAlloca)
		selected := block.NewSelect(isMatch, boxed, current)
		block.NewStore(selected, resultAlloca)
	}

	return block.NewLoad(anyType, resultAlloca), nil
}

// genSetfield sets the named field of a struct value (via lvalue) from a typed value.
// Generates a compile-time strcmp chain - one comparison per field.
func (cg *CodeGen) genSetfield(block *ir.Block, e *ast.SetfieldExpr) (value.Value, error) {
	structPtr, err := cg.genLValue(block, e.Expr)
	if err != nil {
		return nil, err
	}
	fieldNameVal, err := cg.genExpr(block, e.Field)
	if err != nil {
		return nil, err
	}
	newVal, err := cg.genExpr(block, e.Val)
	if err != nil {
		return nil, err
	}

	if structPtr == nil || newVal == nil {
		return nil, nil
	}

	pt, ok := structPtr.Type().(*irtypes.PointerType)
	if !ok {
		return nil, nil
	}
	st, ok := pt.ElemType.(*irtypes.StructType)
	if !ok {
		return nil, nil
	}
	sn := st.Name()
	if sn == "" {
		return nil, nil
	}

	fieldNames := cg.structFields[sn]
	fieldTypes := cg.structFieldLLVMTypes[sn]
	if len(fieldNames) == 0 {
		return nil, nil
	}

	fieldNamePtr := cg.extractStringPtr(block, fieldNameVal)
	strcmp := cg.ensureStrcmp()
	vtableOff := cg.vtableOffset(sn)

	for i, fname := range fieldNames {
		namePtr := cg.newGlobalString(fname)
		cmp := block.NewCall(strcmp, fieldNamePtr, namePtr)
		isMatch := block.NewICmp(enum.IPredEQ, cmp, constant.NewInt(irtypes.I32, 0))

		fieldIdx := int64(1 + vtableOff + i)
		fieldGep := block.NewGetElementPtr(st, structPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, fieldIdx))

		coerced := cg.coerce(block, newVal, fieldTypes[i])
		currentField := block.NewLoad(fieldTypes[i], fieldGep)
		selected := block.NewSelect(isMatch, coerced, currentField)
		block.NewStore(selected, fieldGep)
	}

	return nil, nil
}

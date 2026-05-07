package codegen

import (
	"fmt"
	"sort"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// Reflection builtins

// sortedStructTypeIDs returns the (struct name, type id) pairs of every
// registered struct, sorted by struct name. Use this anywhere IR emission
// would otherwise iterate `cg.structTypeIDs` directly - Go map iteration
// is randomized per process, which would make codegen output non-
// deterministic and break the byte-identical-IR property the
// content-addressed mono cache relies on.
type structTypeID struct {
	name string
	id   int32
}

func (cg *CodeGen) sortedStructTypeIDs() []structTypeID {
	out := make([]structTypeID, 0, len(cg.structTypeIDs))
	for sn, id := range cg.structTypeIDs {
		out = append(out, structTypeID{name: sn, id: id})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })

	return out
}

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

// buildAtomArray returns a fat-pointer {%__atom*, i64} representing a [atom]
// array whose elements are the given atom names (each with a leading apostrophe).
// The data is stored in an immortal global constant so that _tin_release is a
// no-op: the global is wrapped in { i64, [N x %__atom] } with an immortal ARC
// header (rc = -1) exactly like string literals, allowing the fat pointer to be
// freely copied/released without lifetime management.
func (cg *CodeGen) buildAtomArray(block *ir.Block, atoms []string) value.Value {
	elemType := cg.atomType // %__atom = { i32 }
	n := int64(len(atoms))
	fat := irtypes.NewStruct(irtypes.NewPointer(elemType), irtypes.I64)

	if n == 0 {
		// Empty array: null data pointer, length 0.
		alloca := block.NewAlloca(fat)
		ptrGep := block.NewGetElementPtr(fat, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		block.NewStore(constant.NewNull(irtypes.NewPointer(elemType)), ptrGep)
		lenGep := block.NewGetElementPtr(fat, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		block.NewStore(constant.NewInt(irtypes.I64, 0), lenGep)

		return block.NewLoad(fat, alloca)
	}

	// Register each atom and build element constants.
	elems := make([]constant.Constant, n)

	for i, a := range atoms {
		name := a
		if len(name) > 0 && name[0] == '\'' {
			name = name[1:]
		}

		code := cg.registerAtom(name)
		elems[i] = cg.atomConstant(code).(*constant.Struct)
	}

	// Build a global { i64, i64, [N x %__atom] } with an immortal RC header (-1, pad = 0).
	// _tin_release(ptr) reads ptr-16 for the RC: the first i64 == -1 -> no-op.
	// The second i64 is padding to match the 16-byte TinRCHdr layout.
	arrType := irtypes.NewArray(uint64(n), elemType)
	immortalRC := constant.NewInt(irtypes.I64, -1)
	pad := constant.NewInt(irtypes.I64, 0)
	atomArr := constant.NewArray(arrType, elems...)
	hdrStructType := irtypes.NewStruct(irtypes.I64, irtypes.I64, arrType)
	hdrConst := constant.NewStruct(hdrStructType, immortalRC, pad, atomArr)

	g := cg.mod.NewGlobalDef(fmt.Sprintf("atoms.%d", cg.strCount), hdrConst)
	g.Immutable = true
	g.Linkage = enum.LinkagePrivate
	g.UnnamedAddr = enum.UnnamedAddrUnnamedAddr
	cg.strCount++

	// GEP to skip the 16-byte ARC header: { i64, i64, [N x %__atom] }* -> %__atom*
	i32_0 := constant.NewInt(irtypes.I32, 0)
	i32_2 := constant.NewInt(irtypes.I32, 2)
	i64_0 := constant.NewInt(irtypes.I64, 0)
	dataGEP := constant.NewGetElementPtr(hdrStructType, g, i32_0, i32_2, i64_0)
	dataGEP.InBounds = true

	fatAlloca := block.NewAlloca(fat)
	ptrGep := block.NewGetElementPtr(fat, fatAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(dataGEP, ptrGep)
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
	case t.Equal(irtypes.I128):
		return "i128"
	case t.Equal(irtypes.Float):
		return "f32"
	case t.Equal(irtypes.Double):
		return "f64"
	case t.Equal(irtypes.FP128):
		return "f128"
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

	if vt, ok := t.(*irtypes.VectorType); ok {
		elemName := llvmTypeName(vt.ElemType)

		return fmt.Sprintf("%sx%d", elemName, vt.Len)
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

// runtimeAtomSelectByTypeID generates an inline select chain that picks the
// correct %__atom from a table keyed by compile-time type IDs.
// table maps type_id -> atom name string (with leading apostrophe, e.g. "'i64").
// typeIDVal is the i32 type_id extracted at runtime.
// defaultAtom is the %__atom value used when no type_id matches.
func (cg *CodeGen) runtimeAtomSelectByTypeID(block *ir.Block, typeIDVal value.Value,
	table map[int32]string, defaultAtom value.Value) value.Value {
	result := defaultAtom

	// Iterate by ascending type_id - map iteration is randomized in Go
	// and registerAtom calls in a different order shift the resulting
	// @str.NN / @atoms.NN globals around.
	ids := make([]int32, 0, len(table))
	for id := range table {
		ids = append(ids, id)
	}

	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	for _, id := range ids {
		atomStr := table[id]
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
		table[id] = "'" + cg.displayStructName(sn)
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
			atoms = append(atoms, "'"+cg.displayStructName(primitiveTypeName(ft)))
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
	name := cg.displayStructName(llvmTypeName(val.Type()))
	name = prettyTupleName(name)

	return cg.atomConstant(cg.registerAtom(name)), nil
}

// prettyTupleName rewrites the canonical `Tuple__T1__T2__...` form into the
// generic-display form `Tuple[T1, T2, ...]`, matching how other generic
// structs render in typeof output. Without this, tuple typeof leaks the
// internal `__` separator (e.g. `'Tuple__i64__i64`) which reads as
// compiler-internal noise instead of source syntax.
func prettyTupleName(s string) string {
	const prefix = "Tuple__"
	if !strings.HasPrefix(s, prefix) {
		return s
	}

	parts := strings.Split(s[len(prefix):], "__")
	if len(parts) < 2 {
		return s
	}

	return "Tuple[" + strings.Join(parts, ", ") + "]"
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

	// Iterate by ascending type_id so the emitted @atoms.NN globals land
	// in a deterministic order. Map iteration is randomized in Go and
	// would otherwise break the byte-identical-IR property.
	ids := make([]int32, 0, len(table))
	for id := range table {
		ids = append(ids, id)
	}

	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	for _, id := range ids {
		atomStrs := table[id]
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
			atoms = append(atoms, "'"+cg.displayStructName(primitiveTypeName(ft)))
		}
	}

	return cg.buildAtomArray(block, atoms), nil
}

// genFieldtag returns the first @"tag" annotation for the named field, or empty atom.
// For concrete struct types it generates a runtime strcmp chain over field names,
// returning the compile-time atom constant for the matching field's tag.
func (cg *CodeGen) genFieldtag(block *ir.Block, e *ast.FieldtagExpr) (value.Value, error) {
	val, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}

	fieldNameVal, err := cg.genExpr(block, e.Field)
	if err != nil {
		return nil, err
	}

	emptyAtom := cg.atomConstant(cg.registerAtom(""))

	sn := structNameFromValue(val)
	if sn == "" {
		return emptyAtom, nil
	}

	fieldNames := cg.structFields[sn]
	if len(fieldNames) == 0 {
		return emptyAtom, nil
	}

	// Fast path: compile-time string literal -> return atom constant directly.
	if strLit, ok := e.Field.(*ast.StringLit); ok {
		tags := cg.structFieldTags[sn]

		tag := ""
		if tags != nil {
			tag = tags[strLit.Value]
		}

		return cg.atomConstant(cg.registerAtom(tag)), nil
	}

	// Runtime path: strcmp chain over all fields, select the matching tag atom.
	atomType := cg.atomType
	resultAlloca := block.NewAlloca(atomType)
	block.NewStore(emptyAtom, resultAlloca)

	fieldNamePtr := cg.extractStringPtr(block, fieldNameVal)
	strcmp := cg.ensureStrcmp()
	tags := cg.structFieldTags[sn]

	for _, fname := range fieldNames {
		tag := ""
		if tags != nil {
			tag = tags[fname]
		}

		tagAtom := cg.atomConstant(cg.registerAtom(tag))

		namePtr := cg.newGlobalString(fname)
		cmp := block.NewCall(strcmp, fieldNamePtr, namePtr)
		isMatch := block.NewICmp(enum.IPredEQ, cmp, constant.NewInt(irtypes.I32, 0))

		current := block.NewLoad(atomType, resultAlloca)
		selected := block.NewSelect(isMatch, tagAtom, current)
		block.NewStore(selected, resultAlloca)
	}

	return block.NewLoad(atomType, resultAlloca), nil
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
//
// To avoid out-of-bounds reads when the `any` holds a smaller struct (e.g. triple/32 bytes)
// and code unconditionally GEPs into a larger struct layout (e.g. rect/40 bytes), each
// struct type gets its own stack-allocated dummy buffer.  A `select` chooses between the
// actual heap data pointer and the dummy based on the runtime type_id.  GEPs therefore
// always reach valid memory: heap data for the matching type, dummy stack memory otherwise.
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

	// Collect all boxes so we can release non-selected ones after selection.
	var allBoxes []value.Value

	for _, st0 := range cg.sortedStructTypeIDs() {
		sn := st0.name
		typeID := st0.id

		st := cg.structTypes[sn]
		if st == nil {
			continue
		}

		fieldNames := cg.structFields[sn]
		fieldTypes := cg.structFieldLLVMTypes[sn]
		userOff := cg.userFieldOffset(sn)

		isTypeMatch := block.NewICmp(enum.IPredEQ, typeIDVal, constant.NewInt(irtypes.I32, int64(typeID)))

		// Allocate a same-typed stack dummy so GEPs into it are always in-bounds
		// when the type_id does NOT match.  select(isTypeMatch, heapPtr, dummyPtr)
		// routes loads to the correct buffer without any branches.
		// Zero-initialize so pointer fields load as null - _tin_release(null) is a
		// no-op, preventing a crash when a garbage pointer would otherwise be released.
		dummyAlloca := block.NewAlloca(st)
		block.NewStore(cg.zeroValue(st), dummyAlloca)
		// For cLayoutStructs, the dummy must have a valid c_data_ptr (not NULL)
		// so emitCLayoutFieldPtr doesn't dereference a null pointer.
		if cg.cLayoutStructs[sn] {
			if nativeSt := cg.nativeStructTypes[sn]; nativeSt != nil {
				dummyNative := block.NewAlloca(nativeSt)
				block.NewStore(cg.zeroValue(nativeSt), dummyNative)
				cDataIdx := int64(cg.cDataPtrIndex(sn))
				cdGep := block.NewGetElementPtr(st, dummyAlloca,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, cDataIdx))
				block.NewStore(block.NewBitCast(dummyNative, irtypes.I8Ptr), cdGep)
			}
		}

		dummyI8Ptr := block.NewBitCast(dummyAlloca, irtypes.I8Ptr)
		safeI8Ptr := block.NewSelect(isTypeMatch, dataI8Ptr, dummyI8Ptr)

		// Bitcast the safe pointer to *struct.
		structPtr := block.NewBitCast(safeI8Ptr, irtypes.NewPointer(st))

		for i, fname := range fieldNames {
			namePtr := cg.newGlobalString(fname)
			cmp := block.NewCall(strcmp, fieldNamePtr, namePtr)
			isFieldMatch := block.NewICmp(enum.IPredEQ, cmp, constant.NewInt(irtypes.I32, 0))

			isMatch := block.NewAnd(isTypeMatch, isFieldMatch)

			var fieldGep value.Value
			if cg.cLayoutStructs[sn] {
				fieldGep = cg.emitCLayoutFieldPtr(block, structPtr, sn, i)
			} else {
				fieldIdx := int64(userOff + i)
				fieldGep = block.NewGetElementPtr(st, structPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, fieldIdx))
			}

			fieldVal := block.NewLoad(fieldTypes[i], fieldGep)
			boxed := cg.boxToAny(block, fieldVal)
			allBoxes = append(allBoxes, boxed)

			current := block.NewLoad(anyType, resultAlloca)
			selected := block.NewSelect(isMatch, boxed, current)
			block.NewStore(selected, resultAlloca)
		}
	}

	result := block.NewLoad(anyType, resultAlloca)
	// Retain the winner before releasing all boxes so it isn't freed underneath us.
	// _tin_retain(null) is a safe no-op when no field matched.
	cg.emitRetain(block, result)

	for _, box := range allBoxes {
		cg.emitRelease(block, box) // winner -> RC=1; non-selected -> freed
	}

	return result
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
	userOff := cg.userFieldOffset(sn)

	// For cLayoutStructs, ensure c_data_ptr is valid in structAlloca.
	// (It should already be set from the wrapper value; no extra setup needed
	//  as genExpr/genVarDecl always stores a valid wrapper.)

	resultAlloca := block.NewAlloca(anyType)
	block.NewStore(zeroAny, resultAlloca)

	boxes := make([]value.Value, 0, len(fieldNames))
	for i, fname := range fieldNames {
		namePtr := cg.newGlobalString(fname)
		cmp := block.NewCall(strcmp, fieldNamePtr, namePtr)
		isMatch := block.NewICmp(enum.IPredEQ, cmp, constant.NewInt(irtypes.I32, 0))

		var fieldGep value.Value
		if cg.cLayoutStructs[sn] {
			fieldGep = cg.emitCLayoutFieldPtr(block, structAlloca, sn, i)
		} else {
			fieldIdx := int64(userOff + i)
			fieldGep = block.NewGetElementPtr(st, structAlloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, fieldIdx))
		}

		fieldVal := block.NewLoad(fieldTypes[i], fieldGep)
		boxed := cg.boxToAny(block, fieldVal)
		boxes = append(boxes, boxed)

		current := block.NewLoad(anyType, resultAlloca)
		selected := block.NewSelect(isMatch, boxed, current)
		block.NewStore(selected, resultAlloca)
	}

	result := block.NewLoad(anyType, resultAlloca)
	// Retain the winner before releasing all boxes.
	// _tin_retain(null) is a safe no-op when no field matched.
	cg.emitRetain(block, result)

	for _, box := range boxes {
		cg.emitRelease(block, box) // winner -> RC=1; non-selected -> freed
	}

	return result, nil
}

// genSetfieldOnAny writes the named field of an any-boxed struct value in place.
// It mirrors genGetfieldFromAny but stores instead of loads.  The any fat-ptr
// alloca is modified directly: for each known struct type, a safe dummy-alloca
// pattern is used (same as genGetfieldFromAny) so GEPs are always in-bounds.
func (cg *CodeGen) genSetfieldOnAny(block *ir.Block, anyAlloca value.Value, fieldNameVal value.Value, newVal value.Value) {
	anyType := anyFatPtrType()

	typeIDVal := cg.extractAnyTypeID(block, block.NewLoad(anyType, anyAlloca))

	// Extract the raw i8* data pointer from the any fat-ptr.
	dataPtrGep := block.NewGetElementPtr(anyType, anyAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	dataI8Ptr := block.NewLoad(irtypes.I8Ptr, dataPtrGep)

	strcmp := cg.ensureStrcmp()
	fieldNamePtr := cg.extractStringPtr(block, fieldNameVal)

	for _, st0 := range cg.sortedStructTypeIDs() {
		sn := st0.name
		typeID := st0.id

		st := cg.structTypes[sn]
		if st == nil {
			continue
		}

		fieldNames := cg.structFields[sn]
		fieldTypes := cg.structFieldLLVMTypes[sn]
		userOff := cg.userFieldOffset(sn)

		isTypeMatch := block.NewICmp(enum.IPredEQ, typeIDVal, constant.NewInt(irtypes.I32, int64(typeID)))

		// Safe dummy alloca: zero-initialized so pointer fields load as null (no crash
		// when a garbage pointer would otherwise be released).
		dummyAlloca := block.NewAlloca(st)
		block.NewStore(cg.zeroValue(st), dummyAlloca)
		// For cLayoutStructs, wire a valid c_data_ptr in the dummy wrapper.
		if cg.cLayoutStructs[sn] {
			if nativeSt := cg.nativeStructTypes[sn]; nativeSt != nil {
				dummyNative := block.NewAlloca(nativeSt)
				block.NewStore(cg.zeroValue(nativeSt), dummyNative)
				cDataIdx := int64(cg.cDataPtrIndex(sn))
				cdGep := block.NewGetElementPtr(st, dummyAlloca,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, cDataIdx))
				block.NewStore(block.NewBitCast(dummyNative, irtypes.I8Ptr), cdGep)
			}
		}

		dummyI8Ptr := block.NewBitCast(dummyAlloca, irtypes.I8Ptr)
		safeI8Ptr := block.NewSelect(isTypeMatch, dataI8Ptr, dummyI8Ptr)

		structPtr := block.NewBitCast(safeI8Ptr, irtypes.NewPointer(st))

		for i, fname := range fieldNames {
			namePtr := cg.newGlobalString(fname)
			cmp := block.NewCall(strcmp, fieldNamePtr, namePtr)
			isFieldMatch := block.NewICmp(enum.IPredEQ, cmp, constant.NewInt(irtypes.I32, 0))
			isMatch := block.NewAnd(isTypeMatch, isFieldMatch)

			var fieldGep value.Value
			if cg.cLayoutStructs[sn] {
				fieldGep = cg.emitCLayoutFieldPtr(block, structPtr, sn, i)
			} else {
				fieldIdx := int64(userOff + i)
				fieldGep = block.NewGetElementPtr(st, structPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, fieldIdx))
			}

			coerced := cg.coerce(block, newVal, fieldTypes[i])
			if !coerced.Type().Equal(fieldTypes[i]) {
				continue
			}

			currentField := block.NewLoad(fieldTypes[i], fieldGep)
			selected := block.NewSelect(isMatch, coerced, currentField)
			block.NewStore(selected, fieldGep)
			// ARC: retain selected, release old - safe whether or not field matched.
			//   match:   retain(new) + release(old) -> correct ownership transfer
			//   !match:  retain(old) + release(old) -> net no-op
			if isStringType(fieldTypes[i]) || isFatArrayPtr(fieldTypes[i]) {
				cg.emitRetain(block, selected)
				cg.emitRelease(block, currentField)
			}
		}
	}
}

// genSetfield sets the named field of a struct value (via lvalue) from a typed value.
// Generates a compile-time strcmp chain - one comparison per field.
// Also handles any-typed first arguments via genSetfieldOnAny (runtime struct dispatch).
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
		// Anonymous struct: check if this is an any fat-ptr (setfield on any-boxed struct).
		if isAnyType(pt.ElemType) {
			cg.genSetfieldOnAny(block, structPtr, fieldNameVal, newVal)
		}

		return nil, nil
	}

	fieldNames := cg.structFields[sn]
	fieldTypes := cg.structFieldLLVMTypes[sn]

	if len(fieldNames) == 0 {
		return nil, nil
	}

	fieldNamePtr := cg.extractStringPtr(block, fieldNameVal)
	strcmp := cg.ensureStrcmp()
	userOff := cg.userFieldOffset(sn)

	for i, fname := range fieldNames {
		namePtr := cg.newGlobalString(fname)
		cmp := block.NewCall(strcmp, fieldNamePtr, namePtr)
		isMatch := block.NewICmp(enum.IPredEQ, cmp, constant.NewInt(irtypes.I32, 0))

		var fieldGep value.Value
		if cg.cLayoutStructs[sn] {
			fieldGep = cg.emitCLayoutFieldPtr(block, structPtr, sn, i)
		} else {
			fieldIdx := int64(userOff + i)
			fieldGep = block.NewGetElementPtr(st, structPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, fieldIdx))
		}

		coerced := cg.coerce(block, newVal, fieldTypes[i])
		// Skip fields where the value type cannot be coerced to the field type.
		// At runtime the name comparison will be false for these fields anyway,
		// but the LLVM NewSelect / NewStore require type-compatible operands.
		if !coerced.Type().Equal(fieldTypes[i]) {
			continue
		}

		currentField := block.NewLoad(fieldTypes[i], fieldGep)
		selected := block.NewSelect(isMatch, coerced, currentField)
		block.NewStore(selected, fieldGep)
		// ARC: for RC-tracked fields (strings, fat arrays), retain the selected
		// value and release the old value. This pattern is safe whether or not the
		// field matched (isMatch):
		//   if match:  retain(new) + release(old) -> correct ownership transfer
		//   if !match: retain(old) + release(old) -> net no-op
		if isStringType(fieldTypes[i]) || isFatArrayPtr(fieldTypes[i]) {
			cg.emitRetain(block, selected)
			cg.emitRelease(block, currentField)
		}
	}

	return nil, nil
}

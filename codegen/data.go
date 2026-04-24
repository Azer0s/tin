package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// dataVariantInfo holds the per-variant layout of an ADT variant.
// Tag is the ordinal index (declaration order). PayloadType is the LLVM struct
// packed from the variant's fields (empty struct for nullary variants).
type dataVariantInfo struct {
	Tag         int8
	PayloadType *irtypes.StructType
	Fields      []ast.StructField
}

// genDataDecl generates the LLVM layout for a `data Foo = V0 | V1(...)` decl.
// Runtime layout mirrors tagged unions: { i32 type_id, i8 tag, [N x i8] payload }.
// N is sized to the largest variant payload.
//
// Generic ADTs are registered but not emitted here - monomorphization happens
// on-demand when a concrete instance like `Option[i32]` is used.
func (cg *CodeGen) genDataDecl(n *ast.DataDecl) error {
	cg.dataDecls[n.Name] = n

	// For every variant, remember that its name resolves to this ADT.
	// Used later to resolve bare constructors like `Some(42)`.
	for _, v := range n.Variants {
		cg.dataVariantLookup[v.Name] = appendUnique(cg.dataVariantLookup[v.Name], n.Name)
	}

	if len(n.TypeParams) > 0 {
		// Generic ADTs are emitted on-demand during monomorphization.
		return nil
	}

	return cg.emitConcreteData(n.Name, n)
}

// emitConcreteData emits the outer tagged-union struct and the per-variant
// payload structs for a concrete (non-generic, or already-monomorphized) ADT.
func (cg *CodeGen) emitConcreteData(name string, n *ast.DataDecl) error {
	variants := make(map[string]*dataVariantInfo, len(n.Variants))

	var maxSize uint64

	for i, v := range n.Variants {
		payloadFields := make([]irtypes.Type, 0, len(v.Fields))

		for _, f := range v.Fields {
			ft, err := cg.tinTypeToLLVM(f.Type)
			if err != nil {
				return fmt.Errorf("data %s: variant %s: %w", name, v.Name, err)
			}

			payloadFields = append(payloadFields, ft)
		}

		payloadSt := irtypes.NewStruct(payloadFields...)

		if len(payloadFields) > 0 {
			if sz := llvmTypeSize(payloadSt); sz > maxSize {
				maxSize = sz
			}
		}

		variants[v.Name] = &dataVariantInfo{
			Tag:         int8(i),
			PayloadType: payloadSt,
			Fields:      v.Fields,
		}
	}

	if maxSize == 0 {
		maxSize = 1
	}

	payloadArr := irtypes.NewArray(maxSize, irtypes.I8)

	st := cg.structTypes[name]
	if st == nil {
		st = irtypes.NewStruct()
		st.SetName(name)
		cg.structTypes[name] = st
		cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)
	}

	st.Fields = []irtypes.Type{irtypes.I32, irtypes.I8, payloadArr}

	cg.dataVariants[name] = variants

	if _, ok := cg.dataTypeIDs[name]; !ok {
		cg.dataTypeIDs[name] = cg.nextTypeID
		cg.nextTypeID++
	}

	return nil
}

// wrapDataVariant constructs a value for an ADT variant: sets the type_id,
// writes the tag byte, and stores the packed payload struct into the payload
// buffer. Returns a loaded struct value (the ADT's outer type).
func (cg *CodeGen) wrapDataVariant(block *ir.Block, adtName, variantName string, args []value.Value) (value.Value, error) {
	vars := cg.dataVariants[adtName]
	if vars == nil {
		return nil, fmt.Errorf("data %s: not registered", adtName)
	}

	vi := vars[variantName]
	if vi == nil {
		return nil, fmt.Errorf("data %s: unknown variant %s", adtName, variantName)
	}

	if len(args) != len(vi.Fields) {
		return nil, fmt.Errorf("data %s: variant %s expects %d fields, got %d",
			adtName, variantName, len(vi.Fields), len(args))
	}

	outerSt := cg.structTypes[adtName]
	if outerSt == nil {
		return nil, fmt.Errorf("data %s: outer LLVM struct missing", adtName)
	}

	alloca := block.NewAlloca(outerSt)

	typeIDGEP := block.NewGetElementPtr(outerSt, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(constant.NewInt(irtypes.I32, int64(cg.dataTypeIDs[adtName])), typeIDGEP)

	tagGEP := block.NewGetElementPtr(outerSt, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(constant.NewInt(irtypes.I8, int64(vi.Tag)), tagGEP)

	if len(args) > 0 {
		payloadGEP := block.NewGetElementPtr(outerSt, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
		payloadPtr := block.NewBitCast(payloadGEP, irtypes.NewPointer(vi.PayloadType))

		for i, arg := range args {
			fieldPtr := block.NewGetElementPtr(vi.PayloadType, payloadPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
			block.NewStore(arg, fieldPtr)
		}
	}

	return block.NewLoad(outerSt, alloca), nil
}

// resolveVariantName finds which ADT declares a variant with the given name.
// Returns the single ADT name if unambiguous, an empty string if unknown, or
// an error if ambiguous.
func (cg *CodeGen) resolveVariantName(variantName string) (string, error) {
	adts := cg.dataVariantLookup[variantName]
	switch len(adts) {
	case 0:
		return "", nil
	case 1:
		return adts[0], nil
	default:
		return "", fmt.Errorf("constructor %q is ambiguous (declared by: %v) - qualify explicitly", variantName, adts)
	}
}

// isDataVariant returns true if variantName is known to be a constructor of
// any registered ADT.
func (cg *CodeGen) isDataVariant(variantName string) bool {
	return len(cg.dataVariantLookup[variantName]) > 0
}

// dataVariantInfoFor returns the variant info for an ADT/variant pair.
func (cg *CodeGen) dataVariantInfoFor(adtName, variantName string) *dataVariantInfo {
	if vars, ok := cg.dataVariants[adtName]; ok {
		return vars[variantName]
	}

	return nil
}

// genDataConstructorCall emits a call-style ADT constructor `Variant(args...)`.
// Returns the constructed ADT value (outer struct). Returns (nil, nil) when
// the variant cannot be resolved in the current context, so callers can fall
// through to the normal function-call dispatch.
func (cg *CodeGen) genDataConstructorCall(block *ir.Block, variantName string, args []ast.Node) (value.Value, error) {
	adt, err := cg.resolveVariantName(variantName)
	if err != nil {
		return nil, err
	}

	if adt == "" {
		return nil, nil
	}

	vi := cg.dataVariantInfoFor(adt, variantName)
	if vi == nil {
		return nil, nil
	}

	if len(args) != len(vi.Fields) {
		return nil, fmt.Errorf("data %s: variant %s expects %d argument(s), got %d",
			adt, variantName, len(vi.Fields), len(args))
	}

	argVals := make([]value.Value, len(args))

	for i, a := range args {
		v, err2 := cg.genExpr(block, a)
		if err2 != nil {
			return nil, err2
		}

		expected := vi.PayloadType.Fields[i]
		argVals[i] = cg.coerce(block, v, expected)
	}

	return cg.wrapDataVariant(block, adt, variantName, argVals)
}

// genDataNullaryConstructor emits a value expression for a nullary variant
// such as `None` or `Leaf`. Returns (nil, nil) when the identifier is not a
// known nullary variant, so callers can fall through to normal lookup.
func (cg *CodeGen) genDataNullaryConstructor(block *ir.Block, variantName string) (value.Value, error) {
	if !cg.isDataVariant(variantName) {
		return nil, nil
	}

	adt, err := cg.resolveVariantName(variantName)
	if err != nil {
		return nil, err
	}

	if adt == "" {
		return nil, nil
	}

	vi := cg.dataVariantInfoFor(adt, variantName)
	if vi == nil || len(vi.Fields) != 0 {
		return nil, nil
	}

	return cg.wrapDataVariant(block, adt, variantName, nil)
}

// isDataMatchPattern reports whether pat is an ADT match arm pattern:
// either a call `Ctor(bindings...)` on a known variant, or a bare
// identifier naming a nullary variant.
func (cg *CodeGen) isDataMatchPattern(pat ast.Node) bool {
	switch p := pat.(type) {
	case *ast.CallExpr:
		if id, ok := p.Func.(*ast.Identifier); ok {
			return cg.isDataVariant(id.Name)
		}

		return false
	case *ast.Identifier:
		// Bare identifier is an ADT pattern only if it names a nullary variant.
		if adts := cg.dataVariantLookup[p.Name]; len(adts) > 0 {
			if adt, err := cg.resolveVariantName(p.Name); err == nil && adt != "" {
				vi := cg.dataVariantInfoFor(adt, p.Name)
				if vi != nil && len(vi.Fields) == 0 {
					return true
				}
			}
		}

		return false
	}

	return false
}

// dataPatternVariantName returns the variant name referenced by a match
// pattern that isDataMatchPattern accepts.
func dataPatternVariantName(pat ast.Node) string {
	switch p := pat.(type) {
	case *ast.CallExpr:
		if id, ok := p.Func.(*ast.Identifier); ok {
			return id.Name
		}
	case *ast.Identifier:
		return p.Name
	}

	return ""
}

// isDataType returns true if the given LLVM struct type corresponds to a
// registered ADT.
func (cg *CodeGen) isDataType(t irtypes.Type) bool {
	st, ok := t.(*irtypes.StructType)
	if !ok {
		return false
	}

	_, ok = cg.dataVariants[st.Name()]

	return ok
}

// ensureDataPtrReleaseFn lazily generates a null-safe release function for a
// pointer to an ADT value. It:
//
//  1. Null-guards the pointer.
//  2. Loads the tag byte.
//  3. Switches on tag to a per-variant block.
//  4. In each variant block, bitcasts the payload to the variant's struct
//     layout and emits emitRelease on every RC-tracked or owning-pointer
//     field (the standard struct-field release machinery takes it from
//     there).
//  5. Calls _tin_release on the outer block.
//
// Weak fields are skipped (they're non-owning by construction).
func (cg *CodeGen) ensureDataPtrReleaseFn(adtName string, st *irtypes.StructType) *ir.Func {
	if fn, ok := cg.structPtrReleaseFns[adtName]; ok {
		return fn
	}

	variants := cg.dataVariants[adtName]
	if variants == nil {
		return nil
	}

	ptrType := irtypes.NewPointer(st)
	fnName := adtName + "__data_release_ptr"
	fn := cg.mod.NewFunc(fnName, irtypes.Void, ir.NewParam("ptr", ptrType))

	cg.structPtrReleaseFns[adtName] = fn

	entry := fn.NewBlock("entry")
	doRelease := fn.NewBlock("do_release")
	exit := fn.NewBlock("exit")

	isNull := entry.NewICmp(enum.IPredEQ, fn.Params[0], constant.NewNull(ptrType))
	entry.NewCondBr(isNull, exit, doRelease)

	// Load the full struct onto the stack BEFORE decrementing RC so that
	// payload reads remain valid even if _tin_release_struct frees the block.
	loadedVal := doRelease.NewLoad(st, fn.Params[0])
	stackCopy := doRelease.NewAlloca(st)
	doRelease.NewStore(loadedVal, stackCopy)

	tagGEP := doRelease.NewGetElementPtr(st, stackCopy,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	tagI8 := doRelease.NewLoad(irtypes.I8, tagGEP)
	tagI64 := doRelease.NewZExt(tagI8, irtypes.I64)

	payloadGEP := doRelease.NewGetElementPtr(st, stackCopy,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))

	// Decrement RC on the outer block; proceed to descend into children only
	// when we were the last reference (RC hit 0).
	ptrI8 := doRelease.NewBitCast(fn.Params[0], irtypes.I8Ptr)
	wasFreed := doRelease.NewCall(cg.ensureReleaseStruct(), ptrI8)
	isOne := doRelease.NewTrunc(wasFreed, irtypes.I1)

	dispatch := fn.NewBlock("dispatch")
	doRelease.NewCondBr(isOne, dispatch, exit)

	var switchCases []*ir.Case

	for variantName, vi := range variants {
		if !variantHasReleasableField(vi) {
			continue
		}

		caseBlock := fn.NewBlock("var_" + variantName)
		switchCases = append(switchCases, ir.NewCase(
			constant.NewInt(irtypes.I64, int64(vi.Tag)), caseBlock))

		payloadPtr := caseBlock.NewBitCast(payloadGEP, irtypes.NewPointer(vi.PayloadType))

		for fi, f := range vi.Fields {
			if f.IsWeak {
				continue
			}

			if !cg.fieldNeedsOwningRelease(vi.PayloadType.Fields[fi]) {
				continue
			}

			fieldPtr := caseBlock.NewGetElementPtr(vi.PayloadType, payloadPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fi)))
			fieldVal := caseBlock.NewLoad(vi.PayloadType.Fields[fi], fieldPtr)
			cg.emitRelease(caseBlock, fieldVal)
		}

		caseBlock.NewBr(exit)
	}

	dispatch.NewSwitch(tagI64, exit, switchCases...)

	exit.NewRet(nil)

	return fn
}

// variantHasReleasableField returns true if any of the variant's fields carry
// an owning reference that needs release (RC-tracked type or owning pointer
// to a registered struct/ADT).
func variantHasReleasableField(vi *dataVariantInfo) bool {
	for i, f := range vi.Fields {
		if f.IsWeak {
			continue
		}

		t := vi.PayloadType.Fields[i]
		if isRCTrackedType(t) {
			return true
		}

		if pt, ok := t.(*irtypes.PointerType); ok {
			if innerSt, ok2 := pt.ElemType.(*irtypes.StructType); ok2 && innerSt.Name() != "" {
				return true
			}
		}
	}

	return false
}

// fieldNeedsOwningRelease returns true when a payload field type represents an
// owning reference (RC-tracked fat type or pointer to a named struct).
func (cg *CodeGen) fieldNeedsOwningRelease(t irtypes.Type) bool {
	if isRCTrackedType(t) {
		return true
	}

	if pt, ok := t.(*irtypes.PointerType); ok {
		if innerSt, ok2 := pt.ElemType.(*irtypes.StructType); ok2 && innerSt.Name() != "" {
			return true
		}
	}

	return false
}

// emitDataValueRetain tag-dispatches retain over an ADT value's payload.
func (cg *CodeGen) emitDataValueRetain(block *ir.Block, val value.Value) {
	st, ok := val.Type().(*irtypes.StructType)
	if !ok {
		return
	}

	fn := cg.ensureDataValueRetainFn(st.Name(), st)
	if fn == nil {
		return
	}

	block.NewCall(fn, val)
}

// emitDataValueRelease releases the active variant's owning fields for an
// ADT value. Implemented as a single call to a per-ADT helper function so
// that the caller's basic block is not split.
func (cg *CodeGen) emitDataValueRelease(block *ir.Block, val value.Value) {
	st, ok := val.Type().(*irtypes.StructType)
	if !ok {
		return
	}

	fn := cg.ensureDataValueFieldFn(st.Name(), st,
		"__data_release_val", cg.dataValueReleaseFns,
		(*CodeGen).emitRelease)
	if fn == nil {
		return
	}

	block.NewCall(fn, val)
}

// ensureDataValueRetainFn generates a per-ADT helper that retains all owning
// fields of the active variant's payload. The releaser counterpart is inlined
// directly into emitDataValueRelease via ensureDataValueFieldFn.
func (cg *CodeGen) ensureDataValueRetainFn(adtName string, st *irtypes.StructType) *ir.Func {
	return cg.ensureDataValueFieldFn(adtName, st,
		"__data_retain_val", cg.dataValueRetainFns,
		(*CodeGen).emitStructFieldRetain)
}

// ensureDataValueFieldFn is the common skeleton: lookup cache, precompute the
// "any variant has a releasable field" short-circuit, emit the tag-dispatch
// switch, and for each releasable field in each variant call the supplied
// emitField method (a pointer-to-method so the caller can pick retain vs
// release). All owning fields are processed (pointer-to-struct and
// RC-tracked fat types); weak fields are skipped.
func (cg *CodeGen) ensureDataValueFieldFn(
	adtName string,
	st *irtypes.StructType,
	suffix string,
	cache map[string]*ir.Func,
	emitField func(*CodeGen, *ir.Block, value.Value),
) *ir.Func {
	if fn, ok := cache[adtName]; ok {
		return fn
	}

	variants := cg.dataVariants[adtName]
	if variants == nil {
		return nil
	}

	any := false

	for _, vi := range variants {
		if variantHasReleasableField(vi) {
			any = true

			break
		}
	}

	if !any {
		cache[adtName] = nil

		return nil
	}

	fnName := adtName + suffix
	fn := cg.mod.NewFunc(fnName, irtypes.Void, ir.NewParam("val", st))
	cache[adtName] = fn

	entry := fn.NewBlock("entry")
	exit := fn.NewBlock("exit")

	stackCopy := entry.NewAlloca(st)
	entry.NewStore(fn.Params[0], stackCopy)

	tagGEP := entry.NewGetElementPtr(st, stackCopy,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	tagI8 := entry.NewLoad(irtypes.I8, tagGEP)
	tagI64 := entry.NewZExt(tagI8, irtypes.I64)

	payloadGEP := entry.NewGetElementPtr(st, stackCopy,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))

	var switchCases []*ir.Case

	for variantName, vi := range variants {
		if !variantHasReleasableField(vi) {
			continue
		}

		caseBlock := fn.NewBlock("var_" + variantName)
		switchCases = append(switchCases, ir.NewCase(
			constant.NewInt(irtypes.I64, int64(vi.Tag)), caseBlock))

		payloadPtr := caseBlock.NewBitCast(payloadGEP, irtypes.NewPointer(vi.PayloadType))

		for fi, f := range vi.Fields {
			if f.IsWeak {
				continue
			}

			if !cg.fieldNeedsOwningRelease(vi.PayloadType.Fields[fi]) {
				continue
			}

			fieldPtr := caseBlock.NewGetElementPtr(vi.PayloadType, payloadPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fi)))
			fieldVal := caseBlock.NewLoad(vi.PayloadType.Fields[fi], fieldPtr)
			emitField(cg, caseBlock, fieldVal)
		}

		caseBlock.NewBr(exit)
	}

	entry.NewSwitch(tagI64, exit, switchCases...)

	exit.NewRet(nil)

	return fn
}

// genAdtIsExpr handles `x is Ctor(bindings...)` and `x is NullaryVariant` on
// an ADT-typed scrutinee. Returns (value, handled=true, err) when it
// recognizes the form; otherwise returns (nil, false, nil) so the caller can
// fall through to the existing union-is-check logic.
func (cg *CodeGen) genAdtIsExpr(block *ir.Block, scrut value.Value, e *ast.IsExpr) (value.Value, bool, error) {
	st, ok := scrut.Type().(*irtypes.StructType)
	if !ok {
		return nil, false, nil
	}

	adtName := st.Name()

	variants := cg.dataVariants[adtName]
	if variants == nil {
		return nil, false, nil
	}

	variantName := ""

	var binders []string

	// Explicit constructor pattern `is Ok(v)` stored in Pattern.
	if e.Pattern != nil {
		switch p := e.Pattern.(type) {
		case *ast.CallExpr:
			if id, ok2 := p.Func.(*ast.Identifier); ok2 {
				variantName = id.Name
				binders = dataPatternBinders(p)
			}
		case *ast.Identifier:
			variantName = p.Name
		}
	}

	// Nullary variant parsed through the Type path: `is None`, `is Leaf`.
	if variantName == "" && e.Type != nil {
		if simple, ok2 := e.Type.(*ast.SimpleType); ok2 {
			if _, isVariant := variants[simple.Name]; isVariant {
				variantName = simple.Name
			}
		}
	}

	if variantName == "" {
		return nil, false, nil
	}

	vi := variants[variantName]
	if vi == nil {
		return nil, true, fmt.Errorf("data %s: unknown variant %q in is-check", adtName, variantName)
	}

	if len(binders) != 0 && len(binders) != len(vi.Fields) {
		return nil, true, fmt.Errorf("data %s: variant %s expects %d binding(s), got %d",
			adtName, variantName, len(vi.Fields), len(binders))
	}

	alloca := block.NewAlloca(st)
	block.NewStore(scrut, alloca)

	tagGEP := block.NewGetElementPtr(st, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	tagVal := block.NewLoad(irtypes.I8, tagGEP)
	cmp := block.NewICmp(enum.IPredEQ, tagVal, constant.NewInt(irtypes.I8, int64(vi.Tag)))

	if len(vi.Fields) > 0 && len(binders) == len(vi.Fields) {
		payloadGEP := block.NewGetElementPtr(st, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
		payloadPtr := block.NewBitCast(payloadGEP, irtypes.NewPointer(vi.PayloadType))

		for fi := range vi.Fields {
			name := binders[fi]
			if name == "" || name == "_" {
				continue
			}

			fieldPtr := block.NewGetElementPtr(vi.PayloadType, payloadPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(fi)))
			fieldTy := vi.PayloadType.Fields[fi]
			fieldVal := block.NewLoad(fieldTy, fieldPtr)

			bindAlloca := block.NewAlloca(fieldTy)
			block.NewStore(fieldVal, bindAlloca)

			// IsExpr is side-effecting across all clauses of `where`/`if`
			// chains: the load runs even when the tag check fails. Any retain
			// here would operate on mis-interpreted bytes (another variant's
			// payload), so we bind as a borrow (noRelease: true, no retain),
			// matching the semantics of union type-check `if v is n i64`.
			//
			// Callers that need to own the payload should prefer `match`,
			// whose per-case scope only evaluates bindings after the tag has
			// already matched.
			cg.curScope.set(name, &scopeEntry{val: bindAlloca, isAlloc: true, noRelease: true})
		}
	}

	return cmp, true, nil
}

// isExhaustiveDataMatch returns true when every variant of the ADT named by
// the scrutinee is covered by some case arm (and no arm has a guard). Guards
// make exhaustiveness unprovable at compile time, so we conservatively return
// false when any arm is guarded.
func (cg *CodeGen) isExhaustiveDataMatch(s *ast.MatchStmt, adtName string) bool {
	if s.Default != nil {
		return true
	}

	if len(s.Cases) == 0 {
		return false
	}

	variants := cg.dataVariants[adtName]
	if variants == nil {
		return false
	}

	covered := make(map[string]bool, len(variants))

	for _, c := range s.Cases {
		if c.Guard != nil {
			return false
		}

		name := dataPatternVariantName(c.Pattern)
		if name == "" {
			return false
		}

		if _, ok := variants[name]; !ok {
			return false
		}

		covered[name] = true
	}

	for name := range variants {
		if !covered[name] {
			return false
		}
	}

	return true
}

// dataPatternBinders returns the list of binder names (from `case Ok(v):`
// the binders are ["v"]). For nullary patterns, returns nil. An identifier
// pattern is treated as "_" if it is "_" or "nil"; otherwise it is a
// fresh binding with that name.
func dataPatternBinders(pat ast.Node) []string {
	call, ok := pat.(*ast.CallExpr)
	if !ok {
		return nil
	}

	out := make([]string, len(call.Args))

	for i, a := range call.Args {
		switch v := a.(type) {
		case *ast.Identifier:
			out[i] = v.Name
		default:
			out[i] = "_"
		}
	}

	return out
}

func appendUnique(xs []string, s string) []string {
	for _, x := range xs {
		if x == s {
			return xs
		}
	}

	return append(xs, s)
}

package codegen

import (
	"fmt"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) wrapDataVariant(block *ir.Block, adtName, variantName string, args []value.Value, retainMask []bool) (value.Value, error) {
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

	outerSt := cg.structTypeFor(CanonKey(adtName))
	if outerSt == nil {
		return nil, fmt.Errorf("data %s: outer LLVM struct missing", adtName)
	}

	alloca := block.NewAlloca(outerSt)

	typeIDGEP := block.NewGetElementPtr(outerSt, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(constant.NewInt(irtypes.I32, int64(cg.dataTypeIDs[adtName])), typeIDGEP)

	tagGEP := block.NewGetElementPtr(outerSt, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(constant.NewInt(irtypes.I64, vi.Tag), tagGEP)

	if len(args) > 0 {
		payloadGEP := block.NewGetElementPtr(outerSt, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
		payloadPtr := block.NewBitCast(payloadGEP, irtypes.NewPointer(vi.PayloadType))

		for i, arg := range args {
			fieldPtr := block.NewGetElementPtr(vi.PayloadType, payloadPtr,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
			// Coerce + type-check FIRST so a mismatch errors out before
			// we emit the retain.  Otherwise the retain is leaked when
			// coerce can't bridge the types -- and on the success path
			// the retain would target the pre-coerce value, which is
			// wrong if coerce produced a fresh value (e.g. T -> any
			// boxing).
			fieldType := vi.PayloadType.Fields[i]
			arg = cg.coerce(block, arg, fieldType)

			if !arg.Type().Equal(fieldType) {
				return nil, fmt.Errorf(
					"variant %s field %d: cannot store %s where %s is expected",
					variantName, i,
					cg.tinTypeDisplay(arg.Type()),
					cg.tinTypeDisplay(fieldType))
			}

			f := vi.Fields[i]

			if !f.IsWeak && retainMask != nil && i < len(retainMask) && retainMask[i] {
				cg.emitStructFieldRetain(block, arg)
			}

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

// genDataScopeCtorCall handles `Adt::Variant(args)` and `Adt[T, U]::Variant(args)`
// style constructor calls routed through a ScopeAccess expression. Returns
// (value, handled=true, err) when the path matches a known ADT; otherwise
// (nil, false, nil) so the caller can fall through to struct/pkg dispatch.
func (cg *CodeGen) genDataScopeCtorCall(block *ir.Block, fn *ast.ScopeAccess, args []ast.Node) (value.Value, bool, error) {
	if len(fn.Path) < 2 {
		return nil, false, nil
	}

	variantName := fn.Path[len(fn.Path)-1]
	typePart := fn.Path[0]

	if len(fn.Path) > 2 {
		// 3-element paths (`pkg::Adt::Variant`) -- type is the
		// second-to-last component.
		typePart = fn.Path[len(fn.Path)-2]
	}

	typeParamStr := ""
	if i := strings.IndexByte(typePart, '['); i >= 0 {
		typeParamStr = typePart[i+1 : len(typePart)-1]
		typePart = typePart[:i]
	}

	adtName := typePart
	// Accept `pkg::Adt[...]::Variant` (e.g. `result::Result[i64, string]::Ok`)
	// by falling back to the bare name when the package-qualified form
	// is not registered under its qualified key.  ADTs live in a flat
	// namespace; qualification is a disambiguation hint, not a separate
	// declaration.
	if cg.dataDeclFor(CanonKey(adtName)) == nil {
		if idx := strings.LastIndex(adtName, "::"); idx >= 0 {
			bare := adtName[idx+2:]
			if cg.dataDeclFor(CanonKey(bare)) != nil {
				adtName = bare
			}
		}
	}

	if cg.dataDeclFor(CanonKey(adtName)) == nil {
		return nil, false, nil
	}

	// Concrete instance: Option[i32]::Some -> concrete Option__i32.
	if typeParamStr != "" {
		tmpl := cg.dataDeclFor(CanonKey(adtName))
		if tmpl == nil {
			return nil, true, fmt.Errorf("data %s: template not found", adtName)
		}

		rawParts := splitTopLevel(typeParamStr, ',')
		resolvedParts := make([]string, len(rawParts))
		resolvedTEs := make([]ast.TypeExpr, len(rawParts))

		for i, raw := range rawParts {
			resolvedTEs[i] = parseTypeParamStr(raw)
			// Resolve through typeAliases so names resolved in the same
			// way as the declared function signature types, keeping the
			// monomorphic name consistent (e.g. Value -> json__Value).
			resolvedParts[i] = cg.typeExprCanonicalKey(resolvedTEs[i])
		}

		concreteName := adtName + "__" + strings.Join(resolvedParts, "__")
		if cg.structTypeFor(CanonKey(concreteName)) == nil {
			if err := cg.monomorphizeDataDecl(tmpl, resolvedTEs, concreteName); err != nil {
				return nil, true, err
			}
		}

		adtName = concreteName
	}

	vi := cg.dataVariantInfoFor(adtName, variantName)
	if vi == nil {
		return nil, true, fmt.Errorf("data %s: unknown variant %q", adtName, variantName)
	}

	if len(args) != len(vi.Fields) {
		return nil, true, fmt.Errorf("data %s: variant %s expects %d argument(s), got %d",
			adtName, variantName, len(vi.Fields), len(args))
	}

	argVals := make([]value.Value, len(args))
	retainMask := make([]bool, len(args))

	for i, a := range args {
		v, err := cg.genExpr(block, a)
		if err != nil {
			return nil, true, err
		}
		// `cur || b`-style short-circuit operands evaluate across
		// several blocks and leave the IR insertion point parked on a
		// merge block; pick that up so the subsequent coerce / wrap
		// stores into the right block (otherwise we emit instructions
		// into the OLD block referencing values that only exist after
		// the merge -- a dominance error).
		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}

		argVals[i] = cg.coerce(block, v, vi.PayloadType.Fields[i])
		// retainMask: true means the arg is a borrow whose source still
		// owns the +1 RC, so the ADT needs its own retain to keep the
		// payload alive past the source's scope-exit.  Fresh allocations
		// (`as string`/`as Trait` casts that lower to a runtime call
		// returning rc=1, `_tin_bytes_from_buf` results) already own
		// their rc; a second retain here would leave them unbalanced
		// and leak by exactly 1 per construction.  Mirrors the
		// freshIface / freshCallResult exemptions in the let-binding
		// retain logic at the top of genVarDecl.
		retainMask[i] = isCopyExpr(a) && !cg.isFreshBytesAlloc(argVals[i]) && !cg.isFreshCallResult(argVals[i])
	}

	v, err := cg.wrapDataVariant(block, adtName, variantName, argVals, retainMask)

	return v, true, err
}

// splitTopLevel splits s on sep while respecting `[...]` nesting. Used to
// parse ADT generic arg lists where nested brackets are possible
// (e.g. `Option[Result[i32, string]]`).
func splitTopLevel(s string, sep byte) []string {
	var (
		out   []string
		start int
		depth int
	)

	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '[':
			depth++
		case ']':
			depth--
		case sep:
			if depth == 0 {
				out = append(out, strings.TrimSpace(s[start:i]))

				start = i + 1
			}
		}
	}

	out = append(out, strings.TrimSpace(s[start:]))

	return out
}

// genDataConstructorCall emits a call-style ADT constructor `Variant(args...)`.
// Returns the constructed ADT value (outer struct). Returns (nil, nil) when
// the variant cannot be resolved in the current context, so callers can fall
// through to the normal function-call dispatch.
//
// Resolution order:
//  1. When returnTypeHint is set to a known ADT struct type, prefer that ADT
//     (used for `let x Result[i32,e] = Ok(42)` and arg-position inference).
//  2. Otherwise, if the variant is uniquely owned by one ADT, use it.
//  3. If generic monomorphization hasn't run yet and the variant is still
//     ambiguous, give up (caller likely needs explicit path qualification).
func (cg *CodeGen) genDataConstructorCall(block *ir.Block, variantName string, args []ast.Node) (value.Value, error) {
	adt := cg.preferAdtFromHint(variantName)
	if adt == "" {
		var err error

		adt, err = cg.resolveVariantName(variantName)
		if err != nil {
			return nil, err
		}
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
	retainMask := make([]bool, len(args))

	for i, a := range args {
		v, err2 := cg.genExpr(block, a)
		if err2 != nil {
			return nil, err2
		}
		// Pick up any block advance caused by short-circuit operands
		// (`a || b`, `a && b`) before the subsequent coerce/wrap;
		// otherwise we store into the original block while the operand
		// values only exist after the merge, producing a dominance
		// error.  Same fix as in genDataScopeCtorCall.
		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}

		expected := vi.PayloadType.Fields[i]
		preCoerceType := v.Type()
		argVals[i] = cg.coerce(block, v, expected)
		// Same fresh-alloc exemption as the unqualified variant
		// constructor above: skip the retain when the arg already owns
		// its rc (e.g. `as string` slice cast, `_tin_bytes_from_buf`
		// result) so the construction doesn't push rc from 1 to 2 with
		// only one matching release ever fired.
		retainMask[i] = isCopyExpr(a) && !cg.isFreshBytesAlloc(argVals[i]) && !cg.isFreshCallResult(argVals[i])
		// Fresh trait fat-ptr iface in field position: either
		// (a) the implicit coerce widened a concrete value (preCoerceType
		// differs from the iface field), or
		// (b) the user wrote `X as Trait` and the AsExpr already invoked
		// coerceToTrait, so the value arriving here is already iface-typed
		// but came from a fresh _tin_rc_alloc.
		// Both produce an owned rc=1 iface backing block.  Retaining here
		// pushes rc to 2 with only one matching release at consumer drop,
		// leaking the backing block (e.g. `Err(EmptyInput)` widening a
		// JsonError, or `Err(X as errors::Err)` from jwt::verify).
		if retainMask[i] {
			if expSt, ok := expected.(*irtypes.StructType); ok && isTraitFatPtrShape(expSt) {
				if !preCoerceType.Equal(expected) {
					retainMask[i] = false
				} else if _, isAs := a.(*ast.AsExpr); isAs {
					retainMask[i] = false
				}
			}
		}
	}

	return cg.wrapDataVariant(block, adt, variantName, argVals, retainMask)
}

// genDataNullaryConstructor emits a value expression for a nullary variant
// such as `None` or `Leaf`. Returns (nil, nil) when the identifier is not a
// known nullary variant, so callers can fall through to normal lookup.
func (cg *CodeGen) genDataNullaryConstructor(block *ir.Block, variantName string) (value.Value, error) {
	if !cg.isDataVariant(variantName) {
		return nil, nil
	}

	adt := cg.preferAdtFromHint(variantName)
	if adt == "" {
		var err error

		adt, err = cg.resolveVariantName(variantName)
		if err != nil {
			return nil, err
		}
	}

	if adt == "" {
		return nil, nil
	}

	vi := cg.dataVariantInfoFor(adt, variantName)
	if vi == nil || len(vi.Fields) != 0 {
		return nil, nil
	}

	return cg.wrapDataVariant(block, adt, variantName, nil, nil)
}

// preferAdtFromHint picks the ADT name that owns variantName AND matches the
// current returnTypeHint. Used to disambiguate bare constructor calls when
// the expected target type is known (let-bindings with annotation, function
// arguments, return values).
func (cg *CodeGen) preferAdtFromHint(variantName string) string {
	if cg.returnTypeHint == nil {
		return ""
	}

	st, ok := cg.returnTypeHint.(*irtypes.StructType)
	if !ok {
		return ""
	}

	hintAdt := st.Name()
	if hintAdt == "" {
		return ""
	}

	for _, adt := range cg.dataVariantLookup[variantName] {
		if adt == hintAdt {
			return adt
		}
	}

	return ""
}

// isDataMatchPattern reports whether pat is an ADT match arm pattern:
// either a call `Ctor(bindings...)` on a known variant, or a bare
// identifier naming a nullary variant from some ADT. The concrete ADT is
// resolved later from the scrutinee's type; ambiguous variant names (e.g.
// `Empty` declared by both `Box[i64]` and `Box[string]`) are fine here.
func (cg *CodeGen) isDataMatchPattern(pat ast.Node) bool {
	switch p := pat.(type) {
	case *ast.CallExpr:
		if id, ok := p.Func.(*ast.Identifier); ok {
			return cg.isDataVariant(id.Name)
		}

		return false
	case *ast.Identifier:
		// Treat as a nullary-variant pattern only if at least one registered
		// ADT declares a nullary variant with this name.
		for _, adt := range cg.dataVariantLookup[p.Name] {
			if vi := cg.dataVariantInfoFor(adt, p.Name); vi != nil && len(vi.Fields) == 0 {
				return true
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

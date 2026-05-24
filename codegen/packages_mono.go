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

func (cg *CodeGen) monomorphizeFunc(tmpl *ast.FuncDecl, instKey string, typeSubst map[string]TypeName) (*ir.Func, error) {
	irName := tmpl.Name + "__" + instKey
	// Disambiguate generic free-fn overloads that share a base name + type
	// args but differ in arity / param shape (e.g. `unwrap[t](r)` vs
	// `unwrap[t](r, msg)`).  Without the suffix, both monomorphizations
	// collapse to the same IR symbol and the cache returns whichever was
	// compiled first, ignoring the caller's arg count.  Also fold in the
	// return-type's deep mangle so eager-vs-lazy curried overloads with
	// identical param shapes (`fn map[t,u](f) fn([t]) [u]` vs
	// `fn map[t,u](f) fn(*Seq[t]) *Seq[u]`) get distinct IR symbols.
	if overloads := cg.genericFuncOverloads[tmpl.Name]; len(overloads) > 1 {
		irName += "__" + funcParamSigDeep(tmpl.Params)
		if tmpl.RetType != nil {
			irName += "__" + typeExprMangleDeep(tmpl.RetType)
		}
	}

	if f, ok := cg.constrainedFuncInstances[irName]; ok {
		return f, nil // already compiled (or forward-declared for recursive generics)
	}

	// Validate that each concrete type satisfies its declared constraints, and
	// ensure default (non-virtual) trait methods are available for the concrete type.
	for _, c := range tmpl.Constraints {
		concreteTN, ok := typeSubst[c.TypeParam]
		if !ok {
			continue
		}

		concreteName := concreteTN.Canon

		if ok, witness := cg.typeBoundSatisfied(concreteName, c.Bound); !ok {
			return nil, fmt.Errorf("%d:%d: fn %s[%s]: type %q does not satisfy constraint \"where %s is %s\" (failing sub-check: \"%s\")",
				c.Pos.Line, c.Pos.Col, tmpl.Name, concreteName, concreteName,
				c.TypeParam, typeBoundString(c.Bound), typeBoundString(witness))
		}
		// Inject default (non-virtual) trait methods for every positive leaf
		// of the bound so the instantiation has the methods it needs.
		for _, traitExpr := range flattenPositiveTraits(c.Bound) {
			if err := cg.ensureDefaultTraitMethods(concreteName, traitExpr); err != nil {
				return nil, err
			}
		}
	}

	// Build ast.TypeExpr substitution map.  Most concrete names are
	// bare struct / primitive names that wrap straight into a
	// SimpleType, but inference for `[*Trait]` / `[*Struct]` element
	// types now records the pointer prefix in `concrete` (e.g.
	// `*Show`) so the substituted type carries the indirection.  Parse
	// the leading `*` runs into nested PointerType wrappers so the
	// resolver sees a well-formed `*T` type expression rather than a
	// SimpleType whose name starts with `*` (which the resolver would
	// flag as an unknown identifier).
	astSubst := make(map[string]ast.TypeExpr, len(typeSubst))
	for param, concrete := range typeSubst {
		astSubst[param] = parseConcreteSubstName(concrete.Canon)
	}

	// Substitute params.
	newParams := make([]ast.Param, len(tmpl.Params))
	for i, p := range tmpl.Params {
		newParams[i] = ast.Param{
			Name:      p.Name,
			Type:      substituteTypeInTypeExpr(p.Type, astSubst),
			IsVarArgs: p.IsVarArgs,
		}
	}

	// Substitute return type.
	newRet := substituteTypeInTypeExpr(tmpl.RetType, astSubst)

	// Build the concrete FuncDecl (no constraints, no type params).
	concrete := &ast.FuncDecl{
		Name:    irName,
		Params:  newParams,
		RetType: newRet,
		Body:    tmpl.Body,
		Tags:    tmpl.Tags,
	}

	// Save/restore scope so the monomorphization gets a fresh inner scope.
	// We root the new scope at the function's home scope (the package scope where
	// the template was declared), so that bare local names (e.g. `parse` inside
	// json.tin's decode[T]) resolve correctly. If no home scope is recorded, fall
	// back to moduleScope so that at least package-exported names are visible.
	prevScope := cg.curScope

	baseScope, hasHome := cg.genericFuncHomeScopes[tmpl.Name]
	if !hasHome || baseScope == nil {
		baseScope = cg.moduleScope
	}

	if baseScope == nil {
		baseScope = cg.curScope
		for baseScope.parent != nil {
			baseScope = baseScope.parent
		}
	}

	cg.curScope = newScope(baseScope)

	// Register type aliases so that body expressions referring to the type
	// param (e.g. as a variable type annotation) resolve to the concrete type.
	// Save previous values so they can be restored after compilation - stale
	// aliases from one monomorphization must not bleed into the next.
	type prevEntry struct {
		val    ast.TypeExpr
		hadVal bool
	}

	prevAliases := make(map[string]prevEntry, len(astSubst))

	for param, concreteTE := range astSubst {
		prev, had := cg.pushAlias(param, concreteTE)
		prevAliases[param] = prevEntry{val: prev, hadVal: had}
	}

	restoreAliases := func() {
		for param := range astSubst {
			entry := prevAliases[param]
			cg.popAlias(param, entry.val, entry.hadVal)
		}
	}

	// Pre-declare the function signature (no body yet) so that recursive calls
	// inside the body - e.g. a self-recursive generic like _encode_any[T] - can
	// resolve to a forward declaration rather than triggering recursive instantiation.
	if err := cg.predeclareFuncAs(concrete, irName); err != nil {
		cg.curScope = prevScope

		restoreAliases()

		return nil, err
	}
	// Register the forward declaration immediately in constrainedFuncInstances so
	// that any re-entrant monomorphizeFunc call for the same irName returns it.
	// Walk allFuncs() (cg.mod + per-pkg modules) because predeclareFuncAs routes
	// new fns through cg.activeModule(), which lands them in the per-pkg module
	// for the package currently being compiled.
	for _, f := range cg.allFuncs() {
		if f.Name() == irName {
			cg.constrainedFuncInstances[irName] = f

			break
		}
	}

	if err := cg.genFuncDeclAs(concrete, irName); err != nil {
		cg.curScope = prevScope

		restoreAliases()

		return nil, err
	}

	// Restore type aliases - must happen before restoring curScope so that any
	// scope-sensitive alias lookups during cleanup see the original state.
	restoreAliases()

	cg.curScope = prevScope

	// Find the compiled function (now has a body). Walk allFuncs() so we
	// see fns in per-pkg modules too - the body emit went via the same
	// activeModule() routing as the forward declaration above.
	var compiled *ir.Func

	for _, f := range cg.allFuncs() {
		if f.Name() == irName {
			compiled = f

			break
		}
	}

	if compiled == nil {
		return nil, fmt.Errorf("monomorphize %s: compiled function not found", irName)
	}

	cg.constrainedFuncInstances[irName] = compiled

	return compiled, nil
}

// inferTypeArgs maps type-parameter names to TypeName given the actual
// argument LLVM types at a call site.  The returned map uses TypeName so
// downstream consumers can pick the Canon (monomorph-key) or Pretty
// (diagnostic) form explicitly; before TypeName this returned bare
// strings and consumers were inconsistent about which form they
// interpreted them as.
func (cg *CodeGen) inferTypeArgs(tmpl *ast.FuncDecl, argVals []value.Value) map[string]TypeName {
	subst := make(map[string]TypeName)
	// Track whether each bound type param came from a constant (literal) argument.
	// Non-constant (runtime) bindings take priority over constant-derived ones.
	fromConst := make(map[string]bool)

	// Two-pass: first bind from runtime expressions, then fill gaps from constants.
	for pass := 0; pass < 2; pass++ {
		for i, p := range tmpl.Params {
			if i >= len(argVals) {
				break
			}

			_, isConst := argVals[i].(constant.Constant)

			if pass == 0 && isConst {
				continue // first pass: skip constants
			}

			if pass == 1 && !isConst {
				continue // second pass: skip non-constants
			}

			cg.inferTypeArgsFromParamPrio(p.Type, argVals[i].Type(), tmpl.TypeParams, subst, fromConst, isConst)
		}
	}

	// Pipe-context inference: when the call is the RHS of `a |> f(args)`
	// and f's declared return is `fn(<head>) <ret>`, unify <head> against
	// the LHS's LLVM type so generic params that only appear in the
	// returned closure (e.g. `fn take[t](n i64) fn(*Seq[t]) *Seq[t]`)
	// can be inferred without the user typing `take[i64](n)`.
	if cg.pipeCurriedRetHint != nil && tmpl.RetType != nil {
		if ft, ok := tmpl.RetType.(*ast.FuncType); ok && len(ft.Params) > 0 {
			cg.inferTypeArgsFromParamPrio(ft.Params[0], cg.pipeCurriedRetHint, tmpl.TypeParams, subst, fromConst, false)
		}
	}

	return subst
}

// inferTypeArgsFromParamPrio is like inferTypeArgsFromParam but respects a priority rule:
// a binding derived from a runtime (non-constant) argument always wins over one derived
// from a literal constant.
func (cg *CodeGen) inferTypeArgsFromParamPrio(paramType ast.TypeExpr, argType irtypes.Type, typeParams []string, subst map[string]TypeName, fromConst map[string]bool, isConst bool) {
	switch pt := paramType.(type) {
	case *ast.SimpleType:
		for _, tp := range typeParams {
			if pt.Name == tp {
				// Capture the Canon form of argType so the substitution
				// can never carry a leaked `_iface` suffix from a trait
				// used as a value type.  Pre-TypeName this read
				// typeNameOf(argType) directly, which returned the raw
				// struct.Name() including `_iface`; downstream
				// SimpleType{Name: subst[tp]} then propagated that into
				// monomorph keys and silently diverged from the source-
				// form path.  See docs/plans/typename-refactor.md.
				name := cg.typeNameFromLLVM(argType).Canon
				if name == "" {
					if ptr, ok2 := argType.(*irtypes.PointerType); ok2 {
						if st2, ok3 := ptr.ElemType.(*irtypes.StructType); ok3 {
							// `*StructName` -- preserve the pointer
							// indirection in the substitution.
							// Pre-fix this returned just `StructName`
							// (value type) so a generic over
							// `[*Foo]` instantiated as `T = Foo`,
							// reading values out at the wrong type;
							// for trait fat-pointer types
							// (`*Show_iface`) it also produced an
							// un-resolvable raw `Show_iface` SimpleType
							// in the monomorphized signature.
							inner := st2.Name()

							if isTraitFatPtrShape(ptr.ElemType) && strings.HasSuffix(inner, "_iface") {
								inner = strings.TrimSuffix(inner, "_iface")
							}

							name = "*" + inner
						}
					}
				}

				if name == "" {
					name = llvmTypeName(argType)
				}

				if name != "" {
					// Non-const always wins; const only fills a gap or replaces another const.
					// Additionally, string wins over __atom even if atom came from a non-const
					// argument: atoms are coercible to string, so mixed (atom, string) calls
					// should resolve t = string rather than t = __atom.
					existingIsAtom := subst[tp].Canon == "__atom"

					currentIsString := name == "string"
					if existing, exists := subst[tp]; !exists || (fromConst[tp] && !isConst) || (existingIsAtom && currentIsString) {
						_ = existing
						subst[tp] = cg.typeNameFromCanon(name)
						fromConst[tp] = isConst
					}
				}
			}
		}
	case *ast.PointerType:
		if ptr, ok := argType.(*irtypes.PointerType); ok {
			cg.inferTypeArgsFromParamPrio(pt.Elem, ptr.ElemType, typeParams, subst, fromConst, isConst)
		}
	case *ast.GenericType:
		structName := ""

		if st, ok2 := argType.(*irtypes.StructType); ok2 {
			structName = st.Name()
		}

		if structName == "" {
			break
		}

		prefix := pt.Name + "__"
		if !strings.HasPrefix(structName, prefix) {
			break
		}

		innerName := strings.TrimPrefix(structName, prefix)

		// Prefer the arg list recorded at monomorphization time, because type
		// parts themselves may contain `__` (e.g. package-qualified names like
		// json__Value). If no record exists, fall back to a `__`-split, which
		// is correct when every arg is a bare name.
		var parts []string
		if recorded := cg.instPartsFor(CanonKey(structName)); recorded != nil && len(recorded) == len(pt.TypeParams) {
			parts = recorded
		} else if len(pt.TypeParams) == 1 {
			// Single type arg: the whole remainder is the arg (preserves
			// embedded `__` from package-qualified names).
			parts = []string{innerName}
		} else {
			parts = strings.Split(innerName, "__")
		}

		if len(parts) == 1 && len(pt.TypeParams) == 1 {
			innerParam := pt.TypeParams[0]

			if simpleInner, ok := innerParam.(*ast.SimpleType); ok {
				for _, tp := range typeParams {
					if simpleInner.Name == tp {
						if _, exists := subst[tp]; !exists || (fromConst[tp] && !isConst) {
							subst[tp] = cg.typeNameFromCanon(parts[0])
							fromConst[tp] = isConst
						}

						break
					}
				}
			} else {
				if innerST := cg.structTypeFor(CanonKey(parts[0])); innerST != nil {
					cg.inferTypeArgsFromParamPrio(innerParam, innerST, typeParams, subst, fromConst, isConst)
				}
			}

			break
		}

		if len(parts) != len(pt.TypeParams) {
			break
		}

		for i, innerParam := range pt.TypeParams {
			part := parts[i]
			simpleInner, ok := innerParam.(*ast.SimpleType)

			if !ok {
				continue
			}

			for _, tp := range typeParams {
				if simpleInner.Name == tp {
					if _, exists := subst[tp]; !exists || (fromConst[tp] && !isConst) {
						subst[tp] = cg.typeNameFromCanon(part)
						fromConst[tp] = isConst
					}

					break
				}
			}
		}
	case *ast.ArrayType:
		if st, ok := argType.(*irtypes.StructType); ok && len(st.Fields) >= 2 {
			if ptrField, ok2 := st.Fields[0].(*irtypes.PointerType); ok2 {
				cg.inferTypeArgsFromParamPrio(pt.Elem, ptrField.ElemType, typeParams, subst, fromConst, isConst)
			}
		}
	case *ast.FuncType:
		// Two argType shapes are accepted:
		//   1. Fat-fn-ptr {fn(i8*, ...)*, i8*}  - a wrapped closure
		//   2. Raw func pointer fn(...)*       - a bare named function
		// reference (e.g. `is_pos` passed directly to `filter(is_pos)`)
		// before any closure shim is built. Falling through on shape (2)
		// would skip inference and leave subst[t] unset, causing the
		// caller to monomorphize with the literal type-param name (e.g.
		// `@filter__t`) - all callers would then share one IR instance
		// and read its slice with the wrong stride for any element type
		// that doesn't happen to be 8 bytes (atom struct{i32}, i32 array,
		// etc).
		var (
			innerFnType *irtypes.FuncType
			envOffset   int
		)

		if isFatFnPtr(argType) {
			st := argType.(*irtypes.StructType)

			fn, ok := st.Fields[0].(*irtypes.PointerType).ElemType.(*irtypes.FuncType)
			if !ok {
				break
			}

			innerFnType = fn
			envOffset = 1 // skip the i8* env in fat-fn-ptr inner sig
		} else if rawPtr, ok := argType.(*irtypes.PointerType); ok {
			fn, ok2 := rawPtr.ElemType.(*irtypes.FuncType)
			if !ok2 {
				break
			}

			innerFnType = fn
			envOffset = 0 // raw fn pointer carries no env slot
		} else {
			break
		}

		if pt.RetType != nil && innerFnType.RetType != nil {
			cg.inferTypeArgsFromParamPrio(pt.RetType, innerFnType.RetType, typeParams, subst, fromConst, isConst)
		}

		for i, astParam := range pt.Params {
			llIdx := i + envOffset

			if llIdx < len(innerFnType.Params) {
				cg.inferTypeArgsFromParamPrio(astParam, innerFnType.Params[llIdx], typeParams, subst, fromConst, isConst)
			}
		}
	}
}

// inferTypeArgsFromParam recursively matches an AST parameter type against an
// LLVM argument type to infer type-parameter bindings.  Handles:
//   - Direct type-param: fn foo[t](x t)   arg: i64      -> t=i64
//   - Pointer-to-param:  fn foo[t](x *t)  arg: *struct  -> t=struct
//   - Generic struct:    fn foo[t](x S[t]) arg: S__i64   -> t=i64
//   - Pointer-to-generic fn foo[t](x *S[t]) arg: *S__i64 -> t=i64

// evalConstExpr attempts to evaluate a Tin AST expression as a compile-time
// LLVM constant integer or float. Handles literals, type casts (as), bitwise
// NOT (~), unary negation (-), and integer arithmetic / shifts (+, -, <<).
// Returns nil for any expression that cannot be fully reduced to a constant.
//
// Integer values are computed using math/big so that operations like
//
//	const I128_MIN i128 = 1 as i128 << 127
//
// produce real constant.Int values rather than LLVM constant-expression nodes
// (which newer LLVM backends reject for shift/bitwise operators).
//
// This is used by Pass 4 of loadPackageFromSource so that complex package
// constants such as limits::I128_MIN are propagated to callers.
// evalConstExprTyped is like evalConstExpr but uses the declared Tin type as an
// integer-type hint so that typed constants (e.g. const T u32 = 0xd76aa478)
// are created with the correct LLVM bit-width rather than defaulting to i64.
// Also handles struct-literal constants (e.g. const C Color = Color{r:200,...}).

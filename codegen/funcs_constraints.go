package codegen

import (
	"fmt"
	"strings"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) typeBoundSatisfied(concreteName string, bound ast.TypeBound) (bool, *ast.TBAtom) {
	switch b := bound.(type) {
	case *ast.TBAtom:
		got := cg.structSatisfiesConstraint(concreteName, b.Trait)
		if b.Neg {
			got = !got
		}

		if got {
			return true, nil
		}

		return false, b

	case *ast.TBAnd:
		if ok, w := cg.typeBoundSatisfied(concreteName, b.Left); !ok {
			return false, w
		}

		if ok, w := cg.typeBoundSatisfied(concreteName, b.Right); !ok {
			return false, w
		}

		return true, nil

	case *ast.TBOr:
		if ok, _ := cg.typeBoundSatisfied(concreteName, b.Left); ok {
			return true, nil
		}

		if ok, _ := cg.typeBoundSatisfied(concreteName, b.Right); ok {
			return true, nil
		}
		// Both sides failed; report the right-side failure (the last one
		// tried) so the error message lists a concrete missing trait.
		_, w := cg.typeBoundSatisfied(concreteName, b.Right)

		return false, w
	}

	return true, nil
}

// formatStripWitnesses renders a list of dead-strip witnesses (one per
// stripped overload) for inline use in a single-line error message.
// Single witness: "doesn't match <bound>". Multi: "doesn't match any
// of: <bound>, <bound>, ...".
func formatStripWitnesses(witnesses []string) string {
	if len(witnesses) == 0 {
		return ""
	}

	if len(witnesses) == 1 {
		return "doesn't match " + witnesses[0]
	}

	return "doesn't match any of: " + strings.Join(witnesses, ", ")
}

// methodConstraintWitness reports whether every where-clause on a generic
// struct method holds under the given type-parameter substitution. Returns
// an empty string when all constraints hold (the method survives), or a
// human-readable description of the FIRST failing constraint when it
// doesn't (the method is dead-stripped from the concrete struct).
//
// The returned witness format depends on the bound's shape so the
// diagnostic stays informative:
//   - pure leaf:      `where t is X` (t = "Y")
//   - AND with miss:  `where t is X && Z` failed at `Z` (t = "Y")
//   - OR all fail:    `where t is X || Z` matches neither (t = "Y")
//
// A method with no Constraints always survives.
func (cg *CodeGen) methodConstraintWitness(m *ast.FuncDecl, typeSubst map[string]string) string {
	if len(m.Constraints) == 0 {
		return ""
	}

	for _, c := range m.Constraints {
		concreteName, ok := typeSubst[c.TypeParam]
		if !ok {
			// The constraint references a type-param that isn't part
			// of the struct's substitution (e.g. a method-level
			// generic that hasn't been instantiated yet). Defer to
			// the call-site path, which already validates these.
			continue
		}

		ok, witness := cg.typeBoundSatisfied(concreteName, c.Bound)
		if ok {
			continue
		}

		full := typeBoundString(c.Bound)

		// Mixed AND/OR bound -- pointing at the failing AND-conjunct
		// is genuinely informative: that's the specific missing
		// requirement.
		if !isPureOrBound(c.Bound) {
			return fmt.Sprintf("where %s is %s (missing %s)",
				c.TypeParam, full, typeBoundString(witness))
		}

		// Single-leaf or pure-OR bound: the bound itself describes
		// the requirement; the concrete type didn't satisfy it.
		_ = concreteName

		return fmt.Sprintf("where %s is %s", c.TypeParam, full)
	}

	return ""
}

// isPureOrBound reports whether b is built only from atoms and OR
// nodes (no AND). Used by the witness formatter to choose between
// "failed at sub-check" and "matches none of" wording.
func isPureOrBound(b ast.TypeBound) bool {
	switch v := b.(type) {
	case *ast.TBAtom:
		return true
	case *ast.TBOr:
		return isPureOrBound(v.Left) && isPureOrBound(v.Right)
	case *ast.TBAnd:
		return false
	}

	return true
}

// typeArgsContainAnyOf reports whether any type-argument expression in args
// references a name in the `paramNames` list as a top-level SimpleType.
// Used to decide whether a template's type alias still has symbolic type
// parameters (we skip constraint checks in that case).
func typeArgsContainAnyOf(args []ast.TypeExpr, paramNames []string) bool {
	nameSet := make(map[string]bool, len(paramNames))
	for _, n := range paramNames {
		nameSet[n] = true
	}

	for _, a := range args {
		if st, ok := a.(*ast.SimpleType); ok && nameSet[st.Name] {
			return true
		}
	}

	return false
}

// flattenPositiveTraits collects every non-negated leaf trait of a bound.
// Used by the monomorphizer to inject default trait methods the concrete
// type inherits. Negated atoms (`not X`) and OR branches are skipped since
// they describe what the type might lack, not what it must have.
func flattenPositiveTraits(bound ast.TypeBound) []ast.TypeExpr {
	var out []ast.TypeExpr

	var walk func(ast.TypeBound)

	walk = func(b ast.TypeBound) {
		switch v := b.(type) {
		case *ast.TBAtom:
			if !v.Neg {
				out = append(out, v.Trait)
			}
		case *ast.TBAnd:
			walk(v.Left)
			walk(v.Right)
		case *ast.TBOr:
			// OR can't guarantee either leaf holds, so we don't inject
			// default methods from either side.
		}
	}

	walk(bound)

	return out
}

// typeBoundString renders a TypeBound back to its source-level form so
// constraint-violation errors echo the user's own syntax.
func typeBoundString(bound ast.TypeBound) string {
	switch b := bound.(type) {
	case *ast.TBAtom:
		s := typeExprToString(b.Trait)
		if b.Neg {
			return "not " + s
		}

		return s
	case *ast.TBAnd:
		return typeBoundStringParen(b.Left) + " && " + typeBoundStringParen(b.Right)
	case *ast.TBOr:
		return typeBoundStringParen(b.Left) + " || " + typeBoundStringParen(b.Right)
	}

	return "<bound>"
}

// typeBoundStringParen wraps nested And/Or bounds in parens for unambiguous
// rendering. Atoms are rendered bare.
func typeBoundStringParen(bound ast.TypeBound) string {
	if _, ok := bound.(*ast.TBAtom); ok {
		return typeBoundString(bound)
	}

	return "(" + typeBoundString(bound) + ")"
}

// structSatisfiesConstraint checks that structName satisfies a trait expression.
// traitExpr may be a SimpleType ("labeled"), GenericType ("iter[i64]"), or a
// type alias that expands to a union ("addable" = i8|i16|i32|...).
// typeExprStructuralMatch reports whether the concrete TypeExpr
// matches the bound TypeExpr structurally, treating WildcardType
// in `bound` as "matches anything".  Examples (concrete vs bound):
//
//	*i64           vs *_             --> true
//	*i64           vs *i64           --> true
//	*i64           vs *Foo           --> false
//	i64            vs *_             --> false (not a pointer)
//	Point[A, B]    vs Point[_, _]    --> true
//	Point[A, B]    vs Point[A, _]    --> true
//	Point[A, B]    vs Pair[_, _]     --> false (different generic head)
//	[i64]          vs [_]            --> true
//
// Used by structSatisfiesConstraint for wildcard-shape bounds
// (`where t is *_`, `where t is Point[_, _]`, etc.).
func typeExprStructuralMatch(concrete, bound ast.TypeExpr) bool {
	if bound == nil {
		return concrete == nil
	}
	// Wildcard matches any type.
	if _, ok := bound.(*ast.WildcardType); ok {
		return true
	}

	if concrete == nil {
		return false
	}

	switch b := bound.(type) {
	case *ast.SimpleType:
		c, ok := concrete.(*ast.SimpleType)

		return ok && c.Name == b.Name
	case *ast.PointerType:
		c, ok := concrete.(*ast.PointerType)
		if !ok {
			return false
		}

		return typeExprStructuralMatch(c.Elem, b.Elem)
	case *ast.ArrayType:
		c, ok := concrete.(*ast.ArrayType)
		if !ok {
			return false
		}
		// Size must match exactly: `[_; 3]` is "fixed-size 3 of any
		// element", not "any array of any length".  Fat-array bound
		// `[_]` (Size=-1) accepts only fat-array concretes; a fixed
		// `[T; N]` concrete is a distinct shape.
		if c.Size != b.Size {
			return false
		}

		return typeExprStructuralMatch(c.Elem, b.Elem)
	case *ast.GenericType:
		c, ok := concrete.(*ast.GenericType)
		if !ok {
			return false
		}

		if c.Name != b.Name || len(c.TypeParams) != len(b.TypeParams) {
			return false
		}

		for i := range b.TypeParams {
			if !typeExprStructuralMatch(c.TypeParams[i], b.TypeParams[i]) {
				return false
			}
		}

		return true
	}

	return false
}

func (cg *CodeGen) structSatisfiesConstraint(structName string, traitExpr ast.TypeExpr) bool {
	// Pointer / wildcard bound: match by structural shape.  Parses
	// the concrete name back into a TypeExpr and recursively
	// matches against the bound, treating WildcardType as "any
	// type" and PointerType / ArrayType / GenericType wrappers as
	// structural constraints.
	//
	//   bound = *_                   --> any pointer type
	//   bound = *Foo                 --> exactly *Foo
	//   bound = Point[_, _]          --> Point with 2 type args
	//   bound = HashMap[string, _]   --> string-keyed HashMap of anything
	//
	// SimpleType bounds fall through to the trait / type-equality
	// path below (where `t is i64` or `t is comp` is checked).
	//
	// The concrete name may arrive in either form -- bracketed
	// ("Point[i64, string]") from the type-alias expansion path,
	// or mangled ("Point__i64__string") from the fn monomorph
	// path's typeSubst.  prettyStructName converts the mangled
	// form to bracketed; bracketed input passes through unchanged.
	if _, isSimple := traitExpr.(*ast.SimpleType); !isSimple {
		// Prefer the *structured* TypeExpr recorded at struct
		// monomorphization (cg.dataInstShape).  prettyStructName +
		// parseTypeParamStr can't recover nested generics from the
		// mangled name because `__` separator is ambiguous between
		// arity-N and one-nested-N-arity-1 forms:
		// "Box__Pair__i64__string" could be Box[Pair, i64, string]
		// (3 args) or Box[Pair[i64, string]] (1 nested arg).
		// dataInstShape carries the original TypeExpr list, so we
		// rebuild the GenericType exactly.
		var concreteExpr ast.TypeExpr
		if shape, ok := cg.instShapeFor(CanonKey(structName)); ok {
			concreteExpr = &ast.GenericType{Name: shape.Tmpl, TypeParams: shape.Args}
		} else {
			// Fallback when no structured shape was recorded: build the
			// TypeExpr directly via the same `__`-split heuristic
			// previously routed through parseTypeParamStr(prettyStructName(...)).
			concreteExpr = canonNameToTypeExpr(structName)
		}

		if typeExprStructuralMatch(concreteExpr, traitExpr) {
			return true
		}
		// PointerType / GenericType bounds that didn't match
		// structurally fall through: maybe the trait name path
		// below resolves it (e.g. user-declared
		// `trait Box[T]` invoked as `t is Box[T]`).
	}

	var traitName string

	switch te := traitExpr.(type) {
	case *ast.SimpleType:
		traitName = te.Name
	case *ast.GenericType:
		traitName = te.Name
	default:
		return false
	}

	// Built-in type-set shortcut for ord/comp on primitive types. Falls through
	// to the trait-impl path for non-primitives so user-defined ord/comp impls
	// can also satisfy these constraints.
	if traitName == "ord" && isOrdType(structName) {
		return true
	}

	if traitName == "comp" && isCompType(structName) {
		return true
	}

	// If the name is a tagged union type, the literal tagged-union type
	// itself satisfies the bound (`where t is num` matches t = num, the
	// whole union value), and so does any of its structural variants
	// (`where t is num` matches t = i64 when num = i64 | f64).
	if members, ok := cg.unionTypeMembers[traitName]; ok {
		if structName == traitName {
			return true
		}

		for _, member := range members {
			if cg.typeExprContains(member, structName) {
				return true
			}
		}

		return false
	}

	td := cg.traitFor(CanonKey(traitName))
	if td == nil {
		// Not a declared trait or union alias: type-equality constraint.
		// "where t is i64" is satisfied iff concreteName == "i64".
		return traitName == structName
	}

	// Where-shorthand: a bare reference to a single-type-param trait defaults
	// its parameter to the constrained type variable. So `where t is ord`
	// means `where t is ord[t]`. Multi-param traits (e.g. add[rhs, ret])
	// require explicit args.
	if _, isSimple := traitExpr.(*ast.SimpleType); isSimple && len(td.TypeParams) == 1 {
		traitExpr = &ast.GenericType{
			Name:       traitName,
			TypeParams: []ast.TypeExpr{&ast.SimpleType{Name: structName}},
		}
	}

	bareKey := traitQualifierKey(bareTraitImplKey(traitExpr))

	if td.IsAlias {
		// Alias-form trait: the single method name equals the trait name.
		// Accept any of: explicit-args qualified form, base trait-name form,
		// or the plain alias registered by registerPlainMethodAliases.
		candidates := []string{
			structName + "_" + bareKey + "_" + traitName,
			structName + "_" + traitName + "_" + traitName,
			structName + "_" + traitName,
		}

		for _, c := range candidates {
			if _, found := cg.curScope.lookup(c); found {
				return true
			}
		}

		return false
	}

	for _, m := range td.Methods {
		if !m.IsVirtual {
			continue
		}

		qualName := structName + "_" + bareKey + "_" + m.Name
		plainName := structName + "_" + m.Name
		_, hasQual := cg.curScope.lookup(qualName)

		_, hasPlain := cg.curScope.lookup(plainName)
		if !hasQual && !hasPlain {
			return false
		}
	}

	return true
}

// typeExprContains reports whether the type named target is a member of te,
// recursively expanding tagged union types.
func (cg *CodeGen) typeExprContains(te ast.TypeExpr, target string) bool {
	switch t := te.(type) {
	case *ast.SimpleType:
		if t.Name == target {
			return true
		}

		// Recurse into tagged union members.
		if members, ok := cg.unionTypeMembers[t.Name]; ok {
			for _, member := range members {
				if cg.typeExprContains(member, target) {
					return true
				}
			}
		}

		return false
	case *ast.UnionTypeExpr:
		for _, member := range t.Types {
			if cg.typeExprContains(member, target) {
				return true
			}
		}

		return false
	default:
		return false
	}
}

// isOrdType reports whether typeName is an ordered type that supports <, <=, >, >=.
// Covers all integer and float primitives.
func isOrdType(typeName string) bool {
	switch typeName {
	case "i8", "i16", "i32", "i64", "i128",
		"u8", "u16", "u32", "u64", "u128",
		"f32", "f64", "f128",
		"byte", "char":
		return true
	}

	return false
}

// isCompType reports whether typeName is a comparable type that supports ==, !=.
// Covers all ordered types plus string, bool, and atoms.
func isCompType(typeName string) bool {
	return isOrdType(typeName) || typeName == "string" || typeName == "bool" || typeName == "__atom"
}

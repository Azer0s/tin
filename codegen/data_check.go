package codegen

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Azer0s/tin/ast"
)

func sortedVariants(variants map[string]*dataVariantInfo) []struct {
	Name string
	Info *dataVariantInfo
} {
	entries := make([]struct {
		Name string
		Info *dataVariantInfo
	}, 0, len(variants))

	for name, vi := range variants {
		entries = append(entries, struct {
			Name string
			Info *dataVariantInfo
		}{Name: name, Info: vi})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Info.Tag < entries[j].Info.Tag
	})

	return entries
}

// checkNoWildcardInVariantFields enforces that ADT variant fields do
// not reference a call-site-supplied wildcard slot. A field's type is
// fixed at declaration; a wildcard's concrete type isn't known until
// the call site fills it. Method bodies (which are monomorphized per
// call) may freely use the wildcard - only field types are forbidden.
func checkNoWildcardInVariantFields(n *ast.DataDecl) error {
	for _, v := range n.Variants {
		for _, f := range v.Fields {
			if typeExprContainsWildcard(f.Type) {
				return fmt.Errorf("data %s: variant %s field %q has type %s which references a call-site-forwarded slot (`_`); field layout is fixed at declaration but the slot's concrete type is only known per call. Wildcards may appear in trait bounds and method signatures (which monomorphize per call), not in field types",
					n.Name, v.Name, f.Name, fmtTypeExpr(f.Type))
			}
		}
	}

	return nil
}

// unusedWildcardSlots returns warning messages for wildcard slots
// declared in the data's trait bounds that are never referenced by
// any variant or method, OR are referenced exactly once in a single
// method (in which case a method-level generic type parameter would
// express the same intent without obscuring the type at call sites).
// Each violation gets one warning string in the returned slice.
func unusedWildcardSlots(n *ast.DataDecl) []string {
	declaredNames := map[string]bool{}

	declaredAnon := false

	for _, impl := range n.Implements {
		walkWildcardNames(impl, declaredNames, &declaredAnon)
	}

	if !declaredAnon && len(declaredNames) == 0 {
		return nil
	}

	used := map[string]bool{}
	usedAnon := false

	// usageCount counts how many distinct method signatures (params
	// + return type) reference each named wildcard. A count of 1
	// means the slot is forwarded through the data's bound but is
	// only ever consumed by one method - a method-level type
	// parameter would localize the choice to that method instead of
	// every call to the data type.
	usageCount := map[string]int{}

	for _, m := range n.Methods {
		seenInMethod := map[string]bool{}
		seenAnonInMethod := false

		for _, p := range m.Params {
			walkWildcardNames(p.Type, used, &usedAnon)
			walkWildcardNames(p.Type, seenInMethod, &seenAnonInMethod)
		}

		walkWildcardNames(m.RetType, used, &usedAnon)
		walkWildcardNames(m.RetType, seenInMethod, &seenAnonInMethod)

		for name := range seenInMethod {
			usageCount[name]++
		}
	}

	var out []string

	if declaredAnon && !usedAnon {
		out = append(out, fmt.Sprintf(
			"data %s declares an anonymous `_` wildcard in its trait bound but never references it; the call-site forwarding is unnecessary",
			n.Name))
	}

	for name := range declaredNames {
		switch {
		case !used[name]:
			out = append(out, fmt.Sprintf(
				"data %s declares wildcard slot `_: %s` in its trait bound but never references %s; the call-site forwarding is unnecessary",
				n.Name, name, name))
		case usageCount[name] == 1:
			out = append(out, fmt.Sprintf(
				"data %s declares wildcard slot `_: %s` but only one method references it; consider making %s a method-level generic instead so the type choice is local to that method",
				n.Name, name, name))
		}
	}

	return out
}

// walkWildcardNames collects every wildcard name from a type
// expression into the names set; flips anon when an anonymous `_`
// is seen.
func walkWildcardNames(te ast.TypeExpr, names map[string]bool, anon *bool) {
	if te == nil {
		return
	}

	switch t := te.(type) {
	case *ast.WildcardType:
		if t.Name == "" {
			*anon = true
		} else {
			names[t.Name] = true
		}
	case *ast.GenericType:
		for _, p := range t.TypeParams {
			walkWildcardNames(p, names, anon)
		}
	case *ast.PointerType:
		walkWildcardNames(t.Elem, names, anon)
	case *ast.ArrayType:
		walkWildcardNames(t.Elem, names, anon)
	case *ast.UnionTypeExpr:
		for _, u := range t.Types {
			walkWildcardNames(u, names, anon)
		}
	}
}

// fmtTypeExpr renders a TypeExpr in user-facing Tin syntax. Falls
// back to the AST's String() method when the type isn't one of the
// common shapes.
func fmtTypeExpr(te ast.TypeExpr) string {
	if te == nil {
		return "void"
	}

	return te.String()
}

// checkConstraintsReferenceDeclared rejects where-guards that
// reference a name that is not declared in the type's TypeParams or
// Wildcards. Catches typos like `data Foo[T] where Q is comp` where
// Q is undeclared. The error names the available declared params /
// wildcards so the user can spot the typo.  Also runs an occurs
// check: `where t is X[t]` (or any bound expression that names the
// type-param itself as a non-trivial subterm) is unsatisfiable
// because no concrete type T can equal *T or List[T] for some
// outer wrapper that adds structure -- the constraint would
// require an infinitely recursive type.
func checkConstraintsReferenceDeclared(declName string, typeParams, wildcards []string, constraints []ast.TypeConstraint) error {
	if len(constraints) == 0 {
		return nil
	}

	declared := map[string]bool{}
	for _, p := range typeParams {
		declared[p] = true
	}

	for _, w := range wildcards {
		declared[w] = true
	}

	for _, c := range constraints {
		if !declared[c.TypeParam] {
			return fmt.Errorf("%s: where-guard `where %s is %s` references %s, which is not in the type-param list (have: %s); add %s as a type parameter or as a wildcard slot `_: %s`",
				declName, c.TypeParam, typeBoundString(c.Bound), c.TypeParam,
				declaredNamesString(typeParams, wildcards), c.TypeParam, c.TypeParam)
		}

		if msg, bad := boundOccursCheck(c.TypeParam, c.Bound); bad {
			return fmt.Errorf("%s: where-guard `where %s is %s` is never satisfiable: %s",
				declName, c.TypeParam, typeBoundString(c.Bound), msg)
		}
	}

	return nil
}

// boundOccursCheck reports whether a TypeBound's structure makes the
// constraint impossible to satisfy because the bound-side
// expression references the type-param itself as a strict subterm
// (i.e. wrapped in a pointer / array / generic-args layer).  For
// example:
//
//	where t is *t          --> t = *t impossible (recursive)
//	where t is Box[t]      --> t = Box[t] impossible (recursive)
//	where t is pointer[t]  --> after expanding pointer[t] = *t,
//	                           same as the first case
//
// Returns (description, true) when unsatisfiable, ("", false) when
// the bound is plausibly satisfiable.  The check is intentionally
// CONSERVATIVE: it walks the bound's TypeExpr looking for a
// SimpleType whose name equals TypeParam, but only fires when that
// SimpleType is NESTED inside another wrapper (a top-level `t is t`
// would be tautological -- legal, weird, but not unsatisfiable).
func boundOccursCheck(typeParam string, bound ast.TypeBound) (string, bool) {
	var walk func(b ast.TypeBound) (string, bool)

	walk = func(b ast.TypeBound) (string, bool) {
		switch v := b.(type) {
		case *ast.TBAtom:
			if v.Trait == nil {
				return "", false
			}
			// Tautological `t is t` is legal (always true); only
			// flag when the type-param appears as a STRICT subterm.
			if id, ok := v.Trait.(*ast.SimpleType); ok && id.Name == typeParam {
				return "", false
			}

			if typeExprContains(v.Trait, typeParam) {
				return fmt.Sprintf(
					"the bound %q contains %q as a nested subterm, so the constraint reduces to %q = %q (no concrete type satisfies %q = wrapper(%q))",
					typeExprString(v.Trait), typeParam, typeParam, typeExprString(v.Trait), typeParam, typeParam,
				), true
			}

			return "", false
		case *ast.TBAnd:
			if msg, bad := walk(v.Left); bad {
				return msg, true
			}

			return walk(v.Right)
		case *ast.TBOr:
			// Disjunction: only flag if BOTH sides are unsatisfiable.
			lm, lb := walk(v.Left)
			rm, rb := walk(v.Right)

			if lb && rb {
				return lm + "; " + rm, true
			}

			return "", false
		}

		return "", false
	}

	return walk(bound)
}

// typeExprString renders a TypeExpr in a short user-facing form
// for diagnostic messages.  Falls back to the AST's own String
// method when available.
func typeExprString(t ast.TypeExpr) string {
	if t == nil {
		return "<nil>"
	}

	type stringer interface{ String() string }

	if s, ok := t.(stringer); ok {
		return s.String()
	}

	return fmt.Sprintf("%v", t)
}

// declaredNamesString joins type-params and wildcards into a single
// human-readable list, prefixing wildcard slots with "_: " so the
// kind is obvious.
func declaredNamesString(typeParams, wildcards []string) string {
	var parts []string

	parts = append(parts, typeParams...)

	for _, w := range wildcards {
		parts = append(parts, "_: "+w)
	}

	if len(parts) == 0 {
		return "(none)"
	}

	return strings.Join(parts, ", ")
}

// checkMethodsAgainstImpls rejects trait-qualified method definitions
// (`fn TraitName::method`) when the data/struct does not list
// TraitName in its implements list (the parens after the type-param
// list, e.g. `data Foo(Reader, Writer) =`). Without this check a typo
// in the qualifier or a forgotten Implements entry compiles silently
// and the method is dead code.
//
// Both static (`fn ::method`) and unqualified methods are skipped:
// they don't claim a trait impl. Only qualified methods get checked.
func checkMethodsAgainstImpls(declName, declKind string, impls []ast.TypeExpr, methods []*ast.FuncDecl) error {
	if len(methods) == 0 {
		return nil
	}

	implTraits := map[string]bool{}

	for _, impl := range impls {
		for _, name := range collectTraitBaseNames(impl) {
			implTraits[name] = true
		}
	}

	for _, m := range methods {
		if m.TraitQualifier == "" {
			continue
		}
		// Static methods on traits write `::method`; the qualifier
		// is empty in that case, so we never reach here for them.
		// Trait-qualified methods produce a non-empty qualifier
		// like `Reader` / `myt[T, Pair[_: W, T]]` / `pkg::Trait`.
		bare := traitBaseFromQualifier(m.TraitQualifier)
		if bare == "" {
			continue
		}

		if !implTraits[bare] {
			return fmt.Errorf("%s %s: method `fn %s::%s` references trait %q, but %s does not declare trait %q in its implements list. Add %s to %s's parens (e.g. `%s %s(...,%s) =`)",
				declKind, declName, m.TraitQualifier, m.Name, bare, declName, bare, bare, declName, declKind, declName, bare)
		}
	}

	return nil
}

// traitBaseFromQualifier extracts the bare trait name from a
// qualifier string. Strips package prefixes (`pkg::Trait` -> `Trait`)
// and type-args (`Trait[T, U]` -> `Trait`).
func traitBaseFromQualifier(q string) string {
	// Strip type-args first: anything from the first '[' onward.
	if idx := strings.Index(q, "["); idx >= 0 {
		q = q[:idx]
	}
	// Strip package: keep the last segment after `::`.
	if idx := strings.LastIndex(q, "::"); idx >= 0 {
		q = q[idx+2:]
	}

	return q
}

// collectTraitBaseNames extracts the bare trait name(s) from a
// trait-impl TypeExpr. For SimpleType / GenericType the base name is
// the type's name; for a UnionTypeExpr (rare) collect from each
// alternative. Package qualifiers strip down to the last segment so
// `pkg::Trait` matches a `Trait` qualifier on a method.
func collectTraitBaseNames(t ast.TypeExpr) []string {
	switch v := t.(type) {
	case *ast.SimpleType:
		name := v.Name

		if idx := strings.LastIndex(name, "::"); idx >= 0 {
			name = name[idx+2:]
		}

		return []string{name}
	case *ast.GenericType:
		name := v.Name

		if idx := strings.LastIndex(name, "::"); idx >= 0 {
			name = name[idx+2:]
		}

		return []string{name}
	case *ast.UnionTypeExpr:
		var out []string
		for _, u := range v.Types {
			out = append(out, collectTraitBaseNames(u)...)
		}

		return out
	}

	return nil
}

// dataVariantInfo holds the per-variant layout of an ADT variant.
// Tag is the ordinal index (declaration order). PayloadType is the LLVM struct
// packed from the variant's fields (empty struct for nullary variants).
// Tag is int64 to mirror the i64 in-memory layout chosen for alignment; an

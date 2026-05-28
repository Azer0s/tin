package codegen

import (
	"fmt"
	"strings"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) ensureDefaultTraitMethods(concreteName string, traitExpr ast.TypeExpr) error {
	var traitName string

	switch te := traitExpr.(type) {
	case *ast.SimpleType:
		traitName = te.Name
	case *ast.GenericType:
		traitName = te.Name
	default:
		return nil
	}

	td := cg.traitFor(CanonKey(traitName))
	if td == nil {
		return nil
	}

	for _, m := range td.Methods {
		if m.IsVirtual || m.Body == nil {
			continue // virtual methods must be explicitly implemented
		}

		scopeKey := concreteName + "_" + m.Name
		if _, exists := cg.curScope.lookup(scopeKey); exists {
			continue // already generated
		}
		// Create a concrete copy of the method with this param bound to *concreteName.
		injected := *m

		ptrType := &ast.PointerType{Elem: &ast.SimpleType{Name: concreteName}}
		if len(injected.Params) == 0 || injected.Params[0].Name != "this" {
			injected.Params = append([]ast.Param{{Name: "this", Type: ptrType}}, injected.Params...)
		} else {
			newParams := make([]ast.Param, len(injected.Params))
			copy(newParams, injected.Params)
			newParams[0] = ast.Param{Name: "this", Type: ptrType}
			injected.Params = newParams
		}
		// Pre-declare the stub in the current scope so genFuncDeclAs (line 677)
		// finds it via cg.curScope.vars[scopeKey] (direct map lookup, not parent-walk).
		// This also ensures that after generation the function lives in the global
		// scope (not just a temporary inner scope) by registering at every level.
		if err := cg.predeclareFuncAs(&injected, scopeKey); err != nil {
			return fmt.Errorf("ensureDefaultTraitMethods predeclare: %w", err)
		}
		// Walk to global scope and ensure the entry is also there (predeclareFuncAs
		// writes to cg.curScope which may be an inner function scope at this point).
		if entry, ok := cg.curScope.vars[scopeKey]; ok {
			global := cg.curScope
			for global.parent != nil {
				global = global.parent
			}

			global.set(scopeKey, entry)
		}

		if err := cg.genStructMethod(concreteName, &injected); err != nil {
			return fmt.Errorf("ensureDefaultTraitMethods: %w", err)
		}
	}

	return nil
}

// Constrained generic function monomorphization

// monomorphizeFunc compiles a concrete instance of a constrained generic
// function by substituting type-parameter names with concrete struct names.
//
// instKey is the unique suffix, e.g. "animal" for fn foo[t] with t->animal.
// typeSubst maps type-param names to concrete struct names: {"t": "animal"}.
// parseConcreteSubstName turns the string form of an inferred type
// (e.g. "i64", "Show", "*Foo", "**Bar", "[string]") into a structural
// ast.TypeExpr. Leading `*` runs become nested *ast.PointerType wrappers
// around an inner *ast.SimpleType. The bracketed `[T]` form (what
// llvmTypeName / prettyForExpr emit for fat arrays) lifts to a proper
// *ast.ArrayType so downstream typeExprCanonicalKey produces the same
// `[]T` canonical key as the AST-driven path. Without this lift the inner
// name `[string]` would survive as a SimpleType and the canonical key
// (which echoes SimpleType names verbatim) would diverge into
// `Result__[string]__...` while the matching return-type annotation
// renders `Result__[]string__...` -- two distinct named structs with the
// same shape that no longer compare Equal in LLVM.
func parseConcreteSubstName(name string) ast.TypeExpr {
	stars := 0

	for stars < len(name) && name[stars] == '*' {
		stars++
	}

	rest := name[stars:]

	var t ast.TypeExpr

	switch {
	case strings.HasPrefix(rest, "[]"):
		// Canonical-key form for fat arrays from typeExprCanonicalKey
		// (`[]string`, `[]Tuple__a__b`).  Lift back to an ArrayType so
		// downstream re-canonicalization produces the same form rather
		// than a leaf SimpleType named literally `[]string`.
		t = &ast.ArrayType{Elem: parseConcreteSubstName(rest[2:]), Size: -1}
	case len(rest) >= 2 && rest[0] == '[' && rest[len(rest)-1] == ']':
		// Bracketed source-form (`[string]`) -- e.g. from llvmTypeName /
		// prettyForExpr.  Same lift to ArrayType so both encodings
		// collapse to one canonical shape in the monomorphization key.
		t = &ast.ArrayType{Elem: parseConcreteSubstName(rest[1 : len(rest)-1]), Size: -1}
	default:
		t = &ast.SimpleType{Name: rest}
	}

	for i := 0; i < stars; i++ {
		t = &ast.PointerType{Elem: t}
	}

	return t
}

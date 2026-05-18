package codegen

import (
	"fmt"

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
// (e.g. "i64", "Show", "*Foo", "**Bar") into a structural ast.TypeExpr.
// Leading `*` runs become nested *ast.PointerType wrappers around an
// inner *ast.SimpleType; anything else lands as a bare SimpleType.
// Used by monomorphizeFunc to expand `typeSubst[T] = "*Show"` into a
// well-formed `*Show` (PointerType -> SimpleType) so the type
// resolver sees structural pointer-ness rather than a SimpleType
// whose name happens to start with `*`.
func parseConcreteSubstName(name string) ast.TypeExpr {
	stars := 0

	for stars < len(name) && name[stars] == '*' {
		stars++
	}

	var t ast.TypeExpr = &ast.SimpleType{Name: name[stars:]}
	for i := 0; i < stars; i++ {
		t = &ast.PointerType{Elem: t}
	}

	return t
}

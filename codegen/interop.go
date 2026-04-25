package codegen

// `#interop` control tag - validation pass.
//
// `fn{#interop} name(...)` requests a C-callable wrapper alongside the
// Tin-internal entry point. v1 only validates that the function is in
// a shape we will actually be able to wrap; codegen for the wrapper
// itself comes in a later phase.
//
// Restrictions enforced here (Phase A):
//   - Cannot also be `#async` (an async fn is a coroutine; C cannot drive one)
//   - Return type must not contain `Future[T]` (no way for C to await)
//   - No parameter type may contain `any` (no stable C representation)
//   - Cannot be generic (no concrete name for the wrapper symbol)
//   - Cannot be a struct method (v1: top-level functions only)
//   - Cannot be `extern` (already C, has its own symbol)
//   - Cannot be named `main` (would clobber the binary's entry point)
//   - Two `#interop` functions sharing a name are rejected here rather
//     than letting the linker speak.
//
// Type whitelisting for the boundary (Phase B) is a separate pass.

import (
	"github.com/Azer0s/tin/ast"
)

// reservedInteropNames are symbols whose external collision would break
// linking or surprise the user. `main` is the obvious one; expand as
// concrete cases come up.
var reservedInteropNames = map[string]bool{
	"main": true,
}

// checkAllInteropFuncs walks the program AST and validates every
// `#interop`-tagged function. Methods on structs are walked separately
// from top-level functions so the diagnostic can say "method" rather
// than "function" for a method-level violation.
func (cg *CodeGen) checkAllInteropFuncs(stmts []ast.Node) error {
	seen := make(map[string]bool)

	for _, node := range stmts {
		switch n := node.(type) {
		case *ast.FuncDecl:
			if !hasTag(n.Tags, "interop") {
				continue
			}

			if err := cg.validateInteropFunc(n); err != nil {
				return err
			}

			if seen[n.Name] {
				return cg.nodeErr(n, "fn %s: duplicate #interop function name", n.Name)
			}

			seen[n.Name] = true

		case *ast.StructDecl:
			for _, m := range n.Methods {
				if hasTag(m.Tags, "interop") {
					return cg.nodeErr(m, "fn %s.%s: #interop is not allowed on methods (top-level functions only in v1)",
						n.Name, m.Name)
				}
			}
		}
	}

	return nil
}

// validateInteropFunc runs all per-declaration checks. The order is
// chosen so the most surprising rejection comes first (e.g. `#async`
// conflicts before `Future[T]` mention).
func (cg *CodeGen) validateInteropFunc(fn *ast.FuncDecl) error {
	if hasTag(fn.Tags, "async") {
		return cg.nodeErr(fn, "fn %s: #interop and #async cannot be combined; an async fn is a coroutine and cannot be invoked from C",
			fn.Name)
	}

	if fn.IsExtern != "" {
		return cg.nodeErr(fn, "fn %s: #interop on an extern declaration is meaningless (the symbol is already C)",
			fn.Name)
	}

	if len(fn.TypeParams) > 0 {
		return cg.nodeErr(fn, "fn %s: #interop cannot be applied to a generic function (no single C symbol exists for an un-instantiated template)",
			fn.Name)
	}

	if reservedInteropNames[fn.Name] {
		return cg.nodeErr(fn, "fn %s: #interop functions cannot use the reserved name %q",
			fn.Name, fn.Name)
	}

	if typeExprContains(fn.RetType, "Future") {
		return cg.nodeErr(fn, "fn %s: #interop return type must not contain Future[T]; C has no way to await",
			fn.Name)
	}

	for _, p := range fn.Params {
		if typeExprContains(p.Type, "any") {
			return cg.nodeErr(fn, "fn %s: #interop parameter %q has type %s which contains `any`; no stable C representation exists for boxed values",
				fn.Name, p.Name, p.Type)
		}
	}

	return nil
}

// typeExprContains returns true when the type tree rooted at t names
// a type whose root identifier matches `name`. Walks SimpleType,
// GenericType, ArrayType, PointerType, FuncType, TupleArrayType.
// Used by the interop validator to spot `Future` and `any` anywhere
// in a parameter or return position.
func typeExprContains(t ast.TypeExpr, name string) bool {
	if t == nil {
		return false
	}

	switch v := t.(type) {
	case *ast.SimpleType:
		return v.Name == name
	case *ast.GenericType:
		if v.Name == name {
			return true
		}

		for _, p := range v.TypeParams {
			if typeExprContains(p, name) {
				return true
			}
		}
	case *ast.ArrayType:
		return typeExprContains(v.Elem, name)
	case *ast.PointerType:
		return typeExprContains(v.Elem, name)
	case *ast.FuncType:
		for _, p := range v.Params {
			if typeExprContains(p, name) {
				return true
			}
		}

		return typeExprContains(v.RetType, name)
	case *ast.TupleArrayType:
		for _, p := range v.ElemTypes {
			if typeExprContains(p, name) {
				return true
			}
		}
	}

	return false
}


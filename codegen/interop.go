package codegen

// `#interop` control tag - validation pass.
//
// `fn{#interop} name(...)` requests a C-callable wrapper alongside the
// Tin-internal entry point. v1 only validates that the function is in
// a shape we will actually be able to wrap; codegen for the wrapper
// itself comes in a later phase.
//
// Phase A (declaration-level):
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
// Phase B (type whitelist):
//   - Each parameter must be a primitive, pointer, `string`, or fat
//     array `[T]`.
//   - Return type must be a primitive, pointer, `string`, fat array,
//     or `void`.
//   - Anything else (struct, trait object, ADT, union, fn, tuple)
//     rejected with a per-position diagnostic.

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

		if reason := interopTypeReason(p.Type, false); reason != "" {
			return cg.nodeErr(fn, "fn %s: #interop parameter %q has type %s; %s",
				fn.Name, p.Name, p.Type, reason)
		}
	}

	if fn.RetType != nil {
		if reason := interopTypeReason(fn.RetType, true); reason != "" {
			return cg.nodeErr(fn, "fn %s: #interop return type %s; %s",
				fn.Name, fn.RetType, reason)
		}
	}

	return nil
}

// interopAllowedPrimitives lists the SimpleType names that pass through
// the FFI boundary unchanged. Includes the explicit numeric/bool/char
// set plus a small set of canonical aliases (`size_t`, `uint32`) the
// language doc treats as bare types in interop contexts.
var interopAllowedPrimitives = map[string]bool{
	"i8": true, "i16": true, "i32": true, "i64": true,
	"u8": true, "u16": true, "u32": true, "u64": true,
	"f32": true, "f64": true,
	"bool":  true,
	"char":  true,
	"byte":  true,
	"size_t": true,
	"uint32": true,
}

// interopTypeReason returns "" when t is allowed at an interop
// boundary, or a short reason string otherwise. isReturn loosens the
// rule for `void`. The reason is plugged into the diagnostic message
// at the call site so the user sees both the offending type and a
// pointer at why.
func interopTypeReason(t ast.TypeExpr, isReturn bool) string {
	if t == nil {
		if isReturn {
			return "" // void
		}

		return "void parameters are not representable"
	}

	switch v := t.(type) {
	case *ast.SimpleType:
		switch {
		case interopAllowedPrimitives[v.Name]:
			return ""
		case v.Name == "string":
			return ""
		case v.Name == "void":
			if isReturn {
				return ""
			}

			return "void parameters are not representable"
		case v.Name == "any":
			// Already caught by the typeExprContains pass; keep the
			// message consistent here too.
			return "`any` is not C-representable"
		case v.Name == "atom":
			return "`atom` has no stable C representation in v1"
		}
		// Unknown SimpleType - treat as a user struct/trait/ADT name.
		// v1 disallows all named types at the boundary; users wanting
		// struct interop should pass *Struct explicitly.
		return "v1 does not allow named user types at the interop boundary; pass a pointer (*" + v.Name + ") instead"

	case *ast.PointerType:
		return "" // any *T is fine - opaque to Tin's marshalling

	case *ast.ArrayType:
		// Fat arrays [T]: v1 allows; size != -1 (fixed-size [T;N]) is
		// rejected because there is no clean C ABI for a Tin
		// fixed-size value array distinct from `T*`.
		if v.Size != -1 {
			return "fixed-size arrays are not representable; use a fat array [T] or a pointer *T"
		}

		if reason := interopTypeReason(v.Elem, false); reason != "" {
			return "array element type rejected: " + reason
		}

		return ""

	case *ast.GenericType:
		return "generic types like " + v.Name + "[T] are not representable"

	case *ast.FuncType:
		return "fn-typed values (closures, function pointers) are not representable; use a *void with a hand-written shim if needed"

	case *ast.TupleArrayType:
		return "tuple-array destructuring types (@[...]) are not representable"

	case *ast.UnionTypeExpr:
		return "union types are not representable"
	}

	return "type is not allowed at the interop boundary in v1"
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


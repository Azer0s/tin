package codegen

import (
	"strings"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) checkAllInteropFuncs(stmts []ast.Node) error {
	// Pre-scan for #packed struct declarations so the validator can
	// allow them at the boundary (the wrapper rebuilds a Tin-layout
	// instance from the C-layout the caller provided).
	cg.interopPackedStructs = collectPackedStructNames(stmts)

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

// collectPackedStructNames returns the set of struct names declared
// with the `#packed` tag. Used by the interop validator to allow
// pass-by-value packed structs at the boundary (the wrapper rebuilds
// the Tin-layout from the C-layout the caller provides).
func collectPackedStructNames(stmts []ast.Node) map[string]bool {
	out := map[string]bool{}

	for _, node := range stmts {
		sd, ok := node.(*ast.StructDecl)
		if !ok {
			continue
		}

		if hasTag(sd.Tags, "packed") {
			out[sd.Name] = true
		}
	}

	return out
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
		return cg.nodeErr(fn, "fn %s: #interop functions cannot use the reserved name %q (would clash with a runtime symbol)",
			fn.Name, fn.Name)
	}

	if strings.HasPrefix(fn.Name, "__tin_interop_") {
		return cg.nodeErr(fn, "fn %s: #interop function names cannot start with __tin_interop_ (reserved internal-symbol prefix)",
			fn.Name)
	}

	if typeExprContains(fn.RetType, "Future") {
		return cg.nodeErr(fn, "fn %s: #interop return type contains Future[T]; C has no way to await",
			fn.Name)
	}

	for _, p := range fn.Params {
		if typeExprContains(p.Type, "any") {
			return cg.nodeErr(fn, "fn %s: #interop parameter %q has type %s; the any type is not C-representable - no stable layout exists for boxed values",
				fn.Name, p.Name, p.Type)
		}

		if reason := cg.interopTypeReason(p.Type, false); reason != "" {
			return cg.nodeErr(fn, "fn %s: #interop parameter %q has type %s; %s",
				fn.Name, p.Name, p.Type, reason)
		}
	}

	if fn.RetType != nil {
		if reason := cg.interopTypeReason(fn.RetType, true); reason != "" {
			return cg.nodeErr(fn, "fn %s: #interop return type %s; %s",
				fn.Name, fn.RetType, reason)
		}
	}

	return nil
}

// callbackInnerReason restricts the param/return types allowed inside
// a callback signature crossing the interop boundary. Primitives,
// pointers to primitives, `string`, and `[T]` (param-only) are
// supported by per-signature thunk marshaling. Other aggregates are
// rejected. The returned reason is plugged in after `callback
// parameter ` / `callback return ` at the call site.
func callbackInnerReason(t ast.TypeExpr) string {
	return callbackInnerReasonFor(t, false)
}

func callbackInnerReasonReturn(t ast.TypeExpr) string {
	return callbackInnerReasonFor(t, true)
}

func callbackInnerReasonFor(t ast.TypeExpr, isReturn bool) string {
	if t == nil {
		return ""
	}

	if st, ok := t.(*ast.SimpleType); ok {
		switch {
		case interopAllowedPrimitives[st.Name]:
			return ""
		case st.Name == "void":
			return ""
		case st.Name == "string":
			return ""
		}

		return "type " + st.Name + " is not a primitive C type"
	}

	if pt, ok := t.(*ast.PointerType); ok {
		if elem, ok := pt.Elem.(*ast.SimpleType); ok {
			if elem.Name == "void" || interopAllowedPrimitives[elem.Name] || elem.Name == "char" || elem.Name == "byte" {
				return ""
			}
		}

		return "pointer must be *void or a pointer to a primitive"
	}

	if at, ok := t.(*ast.ArrayType); ok && at.Size < 0 {
		if isReturn {
			return "fat array [T] is not supported as a callback return (would need out-params; not representable in a single fn-pointer signature)"
		}
		// Restrict element type to primitives / pointers - same as
		// outer #interop slice rules.
		if r := interopElemTypeReason(at.Elem); r != "" {
			return "fat array element type " + at.Elem.String() + ": " + r
		}

		return ""
	}

	return "type is not allowed inside a callback signature"
}

// interopElemTypeReason restricts the element type allowed inside a
// fat array `[T]` at the boundary. Only primitives and pointers can
// safely cross via memcpy. Reject everything else with a friendly
// message.
func interopElemTypeReason(t ast.TypeExpr) string {
	if st, ok := t.(*ast.SimpleType); ok {
		switch {
		case interopAllowedPrimitives[st.Name]:
			return ""
		case st.Name == "string":
			return "Tin strings are ARC-managed fat pointers; a raw byte copy would produce dangling references in the callee - pass strings individually, or model the array C-side as const char* const*"
		case st.Name == "atom":
			return "atom has no stable C representation"
		case st.Name == "any":
			return "any is not C-representable"
		case st.Name == "void":
			return "[void] is not representable"
		}
		// Named user type (struct / trait / ADT) - no stable C layout
		// guarantee, often ARC-managed inside.
		return "named user types are not representable as fat-array elements; use [u8] as a payload or pass items individually"
	}

	if _, ok := t.(*ast.PointerType); ok {
		return "" // [*T] is OK - pointers are 8 bytes, no ARC.
	}

	if _, ok := t.(*ast.ArrayType); ok {
		return "nested fat arrays are ARC-managed; a raw byte copy would produce dangling references in the callee"
	}

	return "type is not allowed as a fat-array element"
}

// interopAllowedPrimitives lists the SimpleType names that pass through
// the FFI boundary unchanged. Includes the explicit numeric/bool/char
// set plus a small set of canonical aliases (`size_t`, `uint32`) the
// language doc treats as bare types in interop contexts.
var interopAllowedPrimitives = map[string]bool{
	"i8": true, "i16": true, "i32": true, "i64": true,
	"u8": true, "u16": true, "u32": true, "u64": true,
	"f32": true, "f64": true,
	"bool":   true,
	"char":   true,
	"byte":   true,
	"size_t": true,
	"uint32": true,
}

// interopTypeReason returns "" when t is allowed at an interop
// boundary, or a short reason string otherwise. isReturn loosens the
// rule for `void`. The reason is plugged into the diagnostic message
// at the call site so the user sees both the offending type and a
// pointer at why.
func (cg *CodeGen) interopTypeReason(t ast.TypeExpr, isReturn bool) string {
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
			return "any is not C-representable"
		case v.Name == "atom":
			return "atom has no stable C representation"
		}
		// Allow #packed structs by value: the wrapper applies SysV's
		// struct-coercion rules so the C-side struct ABI agrees with
		// what LLVM emits for our wrapper signature. Non-packed user
		// structs would carry vtable pointers and field padding that
		// C cannot anticipate, so they remain rejected.
		if cg.interopPackedStructs[v.Name] {
			return ""
		}

		return "named user types must be either *void (opaque handle) or struct{#packed} for pass-by-value at the interop boundary"

	case *ast.PointerType:
		// Allow *void, *<primitive>, or *<another-pointer>. Reject
		// pointer-to-named-struct: Tin's struct layout has a hidden
		// type_id prefix (and possibly vtable pointers) that C cannot
		// know about, so a C-allocated struct passed by pointer would
		// silently read wrong fields. Force users into *void for
		// struct handles.
		switch elem := v.Elem.(type) {
		case *ast.SimpleType:
			if elem.Name == "void" || interopAllowedPrimitives[elem.Name] || elem.Name == "char" || elem.Name == "byte" {
				return ""
			}
			// Allow *NamedUserType. The header generator renders it
			// as void* so C cannot accidentally dereference, and the
			// pointer round-trips correctly as long as it was
			// originally produced by Tin (e.g. returned from an
			// #interop function that constructed it). Documented in
			// docs/08-interop.md.
			return ""
		case *ast.PointerType:
			return cg.interopTypeReason(v.Elem, false)
		}

		return "this pointer type is not safe at the interop boundary; use *void as an opaque handle"

	case *ast.ArrayType:
		// Fat arrays [T]: v1 allows; size != -1 (fixed-size [T;N]) is
		// rejected because there is no clean C ABI for a Tin
		// fixed-size value array distinct from `T*`.
		if v.Size != -1 {
			return "fixed-size arrays are not representable; use a fat array [T] or a pointer *T"
		}
		// The marshaler does a raw memcpy of `len * sizeof(T)` bytes
		// across the C/Tin boundary. That is only safe when T has no
		// ARC headers - i.e. T is a primitive or pointer. Strings,
		// nested slices, structs, ADTs, etc. would be copied as
		// shallow byte blobs without retain/release, producing
		// dangling-pointer crashes the moment Tin touches them.
		if reason := interopElemTypeReason(v.Elem); reason != "" {
			return "array element type " + v.Elem.String() + " is not safe to memcpy across the boundary: " + reason
		}

		return ""

	case *ast.GenericType:
		return "generic types like " + v.Name + "[T] are not representable"

	case *ast.FuncType:
		// Callbacks at the boundary:
		//   * As a parameter: wrapper boxes the raw C fn pointer into a
		//     Tin fat fn-ptr backed by a per-signature thunk.
		//   * As a return: wrapper extracts the Tin closure's {fn, env}
		//     and hands C a per-instance trampoline that captures env
		//     and tail-jumps to a per-signature dispatcher.
		// Both directions share the same sub-signature restrictions:
		// only primitive/pointer-shaped param/return types.
		for _, pt := range v.Params {
			if r := callbackInnerReason(pt); r != "" {
				return "callback parameter " + r
			}
		}

		if v.RetType != nil {
			if r := callbackInnerReasonReturn(v.RetType); r != "" {
				return "callback return " + r
			}
		}

		return ""

	case *ast.TupleArrayType:
		return "tuple-array destructuring types (@[...]) are not representable"

	case *ast.UnionTypeExpr:
		return "union types are not representable"
	}

	return "type is not allowed at the interop boundary in v1"
}

// programHasInteropFunc reports whether any top-level function in the

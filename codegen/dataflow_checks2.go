package codegen

import (
	"fmt"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) dfCheckUncheckedDiv(e *ast.BinExpr, st *dfState) {
	if e.Op != "/" && e.Op != "%" {
		return
	}

	// Constant RHS: zero already errored, non-zero is provably safe.
	if v := cg.tryFoldExpr(e.Right); v.kind == foldInt {
		return
	}

	// Identifier RHS with an interval that excludes zero, OR with a
	// flow-sensitive notZero proof, is provably safe.
	if id, ok := e.Right.(*ast.Identifier); ok {
		if iv, has := st.intv[id.Name]; has && iv.set && (iv.lo > 0 || iv.hi < 0) {
			return
		}

		if st.notZero[id.Name] {
			return
		}
	}

	// Float division by zero is well-defined IEEE (Inf/NaN); only warn
	// when the operands look integral.  We don't have type info here
	// cheap, so guard on op kind: `%` is integer-only in Tin, and `/`
	// on identifiers we conservatively warn about.
	rhsDesc := "divisor"
	if id, ok := e.Right.(*ast.Identifier); ok {
		rhsDesc = fmt.Sprintf("divisor %q", id.Name)
	}

	cg.warn(DiagUncheckedDiv, e.Pos(),
		"%q with unproven non-zero %s; guard with `if <divisor> != 0:` or narrow the value before this point",
		e.Op, rhsDesc)
}

// dfCheckUncheckedIndex warns on `arr[i]` when the dataflow pass
// cannot prove the access is safe on the current path.  Two
// regimes:
//
//   - Built-in arrays / slices / strings / pointers: index must be
//     bounds-checked.  Proof comes from a constant index that
//     fits, a flow-sensitive `boundsChecked` fact set by
//     narrowOnCond on `if i < expr:` / `if i <= expr:` guards.
//   - User structs with a `::index` overload: the impl returns
//     `(V, bool)` and the canonical safe access is the destructure
//     `let (v, ok) = t[k]` (the caller then checks `ok`).  A bare
//     `t[k]` at any other context relies on the codegen auto-
//     unwrap, which panics on miss -- that's the "unchecked" path.
//     The TupleDestructDecl pre-walk handler sets a transient
//     skip-flag so the access doesn't double-fire on the
//     destructured form.
func (cg *CodeGen) dfCheckUncheckedIndex(e *ast.IndexExpr, st *dfState) {
	// Constant index: handled by the default-on -Warray-bounds path.
	if v := cg.tryFoldExpr(e.Index); v.kind == foldInt {
		return
	}

	// Skip when this IndexExpr is the RHS of a tuple-destructure
	// (`let (v, ok) = t[k]`).  dfWalkStmt sets this transient flag
	// before walking the destructure's value so the IndexExpr's
	// visit knows it was unwrapped.  Applies to both array-like
	// receivers (rare; usually a fat-ptr-of-tuples access) and
	// custom `::index` receivers (the canonical safe form).
	if cg.dfSkipIndexCheck[e] {
		return
	}

	receiverIsArray := cg.indexReceiverTypeIsArrayLike(e.Expr, st)

	if receiverIsArray {
		// Identifier index with a flow-sensitive bounds-check proof
		// is provably safe.
		idIndex, isIdent := e.Index.(*ast.Identifier)
		if isIdent && st.boundsChecked[idIndex.Name] {
			return
		}

		idxDesc := "index"
		if isIdent {
			idxDesc = fmt.Sprintf("index %q", idIndex.Name)
		}

		cg.warn(DiagUncheckedIndex, e.Pos(),
			"array %s accessed without proven bounds check; guard with `if <index> < len(arr):` or narrow the value before this point",
			idxDesc)

		return
	}

	// Custom `::index` overload receiver: access without ok-
	// destructuring relies on the auto-unwrap panic to catch a
	// missing key at runtime.  Pedantic: warn so the user either
	// destructures `(v, ok) = t[k]` and checks `ok`, or accepts
	// the runtime panic by silencing the diagnostic explicitly.
	t := cg.dfResolveType(e.Expr, st)
	if t != nil && !isArrayLikeType(t) {
		recvDesc := dfExprDescription(e.Expr)
		cg.warn(DiagUncheckedIndex, e.Pos(),
			"indexed access on %s (custom `::index` impl) without `(v, ok)` destructure; pattern: `let (v, ok) = %s[...]; if ok: ...` or silence with `-Wno-unchecked-index`",
			recvDesc, recvDesc)
	}
}

// dfExprDescription returns a short rendering of an expression for
// use in diagnostic messages.  Identifier names render as `name`;
// FieldAccess as `recv.field`; everything else as `<expr>`.
func dfExprDescription(expr ast.Node) string {
	switch e := expr.(type) {
	case *ast.Identifier:
		return fmt.Sprintf("%q", e.Name)
	case *ast.FieldAccess:
		return fmt.Sprintf("%s.%s", dfExprDescription(e.Expr), e.Field)
	}

	return "<expr>"
}

// indexReceiverTypeIsArrayLike reports whether the IndexExpr
// receiver `expr` resolves to a built-in indexable type (array
// literal, slice / fat-ptr `[T]`, string, or pointer) on the
// current dataflow state.  Handles identifiers, field access on
// struct-typed receivers, and chained index expressions
// (`rows[i][j]`).  For unresolvable shapes (CallExpr, etc.) it
// returns false so we don't false-warn on map accesses we can't
// see through.
func (cg *CodeGen) indexReceiverTypeIsArrayLike(expr ast.Node, st *dfState) bool {
	t := cg.dfResolveType(expr, st)
	if t == nil {
		return false
	}

	return isArrayLikeType(t)
}

// dfResolveType walks an expression and returns its declared AST
// type when we can resolve it statically, or nil otherwise.
// Supported shapes:
//
//   - Identifier  -> st.types[name] (param, local, captured)
//   - FieldAccess -> recursively resolve receiver to a struct, then
//     look up the named field in structDeclsByName
//   - IndexExpr   -> recursively resolve receiver, peel one `[T]`
//     or pointer layer (chained indexing)
//
// CallExpr / overloads / generic instantiation are NOT resolved
// (would require a full type checker rerun); callers get nil and
// fall back to "don't warn" so map-like / opaque receivers stay
// silent.
func (cg *CodeGen) dfResolveType(expr ast.Node, st *dfState) ast.TypeExpr {
	switch e := expr.(type) {
	case *ast.Identifier:
		if t, ok := st.types[e.Name]; ok {
			return t
		}

		return nil

	case *ast.FieldAccess:
		recvT := cg.dfResolveType(e.Expr, st)
		if recvT == nil {
			return nil
		}

		structName := dfStructNameOf(recvT)
		if structName == "" {
			return nil
		}

		decl, ok := cg.structDeclsByName[structName]
		if !ok || decl == nil {
			return nil
		}

		for _, f := range decl.Fields {
			if f.Name == e.Field {
				return f.Type
			}
		}

		return nil

	case *ast.IndexExpr:
		recvT := cg.dfResolveType(e.Expr, st)
		if recvT == nil {
			return nil
		}
		// Peel one layer of array / pointer / string.
		switch t := recvT.(type) {
		case *ast.ArrayType:
			return t.Elem
		case *ast.PointerType:
			return t.Elem
		}

		return nil
	}

	return nil
}

// dfStructNameOf returns the struct name a TypeExpr refers to, or
// "" if it does not name a struct.  Resolves through pointer
// wrappers (`*Foo` -> "Foo") and bare names.
func dfStructNameOf(t ast.TypeExpr) string {
	switch n := t.(type) {
	case *ast.SimpleType:
		return n.Name
	case *ast.PointerType:
		return dfStructNameOf(n.Elem)
	case *ast.GenericType:
		return n.Name
	}

	return ""
}

// dfIsPointerLike reports whether t is a pointer or pointer-like
// type (raw `*T`, `*Trait`).  Used by dfCheckExpr's FieldAccess
// case to decide whether `s.field` warrants the
// -Wunchecked-nil-deref pedantic check.  Slices and arrays return
// false: their bounds story is the -Wunchecked-index regime, not
// the nil-deref regime.
func dfIsPointerLike(t ast.TypeExpr) bool {
	_, ok := t.(*ast.PointerType)

	return ok
}

// isArrayLikeType reports whether t is a built-in indexable type
// (array literal `[T; N]`, slice / fat-pointer `[T]`, string, raw
// pointer `*T`).  Excludes user struct types -- those route through
// `::index` overloads with `(V, bool)` semantics.
func isArrayLikeType(t ast.TypeExpr) bool {
	if t == nil {
		return false
	}

	switch n := t.(type) {
	case *ast.ArrayType:
		return true
	case *ast.PointerType:
		return true
	case *ast.SimpleType:
		return n.Name == "string"
	}

	return false
}

// dfInferTypeFromRHS attempts to recover the type of a let-binding
// from its initializer when the user did not write an explicit
// annotation.  Supported shapes:
//   - `[a, b, c]` literal -> *ast.ArrayType
//   - `[T; N]` typed array literal -> *ast.ArrayType
//   - `fn(...)` direct call -> looked up in cg.funcDecls for RetType
//   - `&x` address-of -> *ast.PointerType (element type left blank)
//
// Returns nil when the type cannot be inferred locally; the binding
// is then left untyped in dfState.types and the
// -Wunchecked-{index,nil-deref} checks skip it (conservative).
func (cg *CodeGen) dfInferTypeFromRHS(rhs ast.Node) ast.TypeExpr {
	switch n := rhs.(type) {
	case *ast.ArrayLit:
		// Bare literal -- element type unknown without full inference,
		// but the SHAPE is an array, which is all we need to tell
		// array-like from custom-index.  Synthesize a placeholder
		// element type; isArrayLikeType only checks the outer node
		// kind.
		_ = n

		return &ast.ArrayType{Elem: &ast.SimpleType{Name: ""}}

	case *ast.AddressOfExpr:
		// `&x` is always a pointer to whatever x is.  Element type
		// is left blank -- the dataflow only checks the OUTER kind
		// of the inferred type, so the placeholder suffices.
		return &ast.PointerType{Elem: &ast.SimpleType{Name: ""}}

	case *ast.CallExpr:
		// Direct call by simple-name: look up the callee's
		// declared return type.  Falls back to nil for method
		// calls, scope-qualified calls, generic templates, etc.
		// -- those require more machinery the dataflow pass
		// doesn't carry.
		id, ok := n.Func.(*ast.Identifier)
		if !ok {
			return nil
		}

		decl, ok := cg.funcDecls[id.Name]
		if !ok || decl == nil {
			return nil
		}

		return decl.RetType
	}

	return nil
}

// dfCheckFloatPrecision flags `==` / `!=` whose two sides are float
// expressions that disagree under IEEE 754 but agree under exact
// arithmetic (the 0.1 + 0.2 == 0.3 trap), threading let-bindings through
// the dataflow state so it works on variables, not just literals.
func (cg *CodeGen) dfCheckFloatPrecision(e *ast.BinExpr, st *dfState) {
	if e.Op != "==" && e.Op != "!=" {
		return
	}

	lhs := dfFoldFloat(e.Left, st)
	if lhs == nil {
		return
	}

	rhs := dfFoldFloat(e.Right, st)
	if rhs == nil {
		return
	}

	floatEq := lhs.ieee == rhs.ieee
	exactEq := lhs.exact.Cmp(rhs.exact) == 0

	if floatEq == exactEq {
		return
	}

	ieeeResult := floatEq
	exactResult := exactEq

	if e.Op == "!=" {
		ieeeResult = !ieeeResult
		exactResult = !exactResult
	}

	cg.warn(DiagFloatPrecision, e.Pos(),
		"%q evaluates to %v under IEEE 754 but %v under exact arithmetic; "+
			"use `abs(a - b) < eps` instead",
		e.Op, ieeeResult, exactResult)
}

// dfFoldFloat folds an expression to a (IEEE, exact) float pair under the
// given state. Handles FloatLit, IntLit, the four arithmetic ops, unary
// minus, and Identifier (via the state's tracked floats). Returns nil for
// anything we can't statically resolve.

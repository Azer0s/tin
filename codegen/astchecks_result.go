package codegen

import (
	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) checkResultMatchAntipattern(s *ast.MatchStmt) {
	if s == nil || len(s.Cases) != 2 || s.Default != nil {
		return
	}

	var okCase, errCase *ast.MatchCase

	for i := range s.Cases {
		c := &s.Cases[i]
		if c.Guard != nil {
			return
		}

		name := patternVariantName(c.Pattern)
		switch name {
		case "Ok":
			if okCase != nil {
				return
			}

			okCase = c
		case "Err":
			if errCase != nil {
				return
			}

			errCase = c
		default:
			return
		}
	}

	if okCase == nil || errCase == nil {
		return
	}

	okBinder := singleBinderOf(okCase.Pattern)
	errBinder := singleBinderOf(errCase.Pattern)

	// Order matters: more-specific lints win over the generic try
	// propagation. Each call emits at most one diagnostic.
	if cg.matchSuggestsUnwrap(s, okCase, errCase, okBinder) {
		return
	}

	if cg.matchSuggestsExpect(s, okCase, errCase, okBinder) {
		return
	}

	if cg.matchSuggestsUnwrapOr(s, okCase, errCase, okBinder) {
		return
	}

	if cg.matchSuggestsMap(s, okCase, errCase, okBinder, errBinder) {
		return
	}

	if cg.matchSuggestsMapErr(s, okCase, errCase, okBinder, errBinder) {
		return
	}

	cg.matchSuggestsTry(s, okCase, errCase)
}

// singleBinderOf returns the single binder name in an Ok(x) / Err(e)
// pattern, or "" if the pattern doesn't bind exactly one name (e.g.
// `Ok(_)`, `Ok()`, `Err`, `Err(_, _)`).
func singleBinderOf(pat ast.Node) string {
	call, ok := pat.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return ""
	}

	id, ok := call.Args[0].(*ast.Identifier)
	if !ok {
		return ""
	}

	if id.Name == "_" {
		return ""
	}

	return id.Name
}

// matchSuggestsUnwrap fires for `case Ok(v): v / case Err(_): panic(...)`,
// or the Ok-arm assignment variant that reduces to the same shape.
// .unwrap() panics with a fixed "result::unwrap on Err" message; if the
// user wants their own text, .expect("msg") covers it.
func (cg *CodeGen) matchSuggestsUnwrap(s *ast.MatchStmt, okCase, errCase *ast.MatchCase, okBinder string) bool {
	if okBinder == "" {
		return false
	}

	if !blockReturnsOnlyTheBinder(okCase.Body, okBinder) {
		return false
	}

	if !blockPanicsConstant(errCase.Body) {
		return false
	}

	cg.warn(DiagMatchResultTry, s.Pos(),
		"this `match` panics on Err and yields the Ok value - prefer `expr.unwrap()` (or `expr.expect(\"msg\")` for a custom panic message)")

	return true
}

// matchSuggestsExpect fires for the unwrap shape above but where the
// panic message is a non-trivial expression -- still .expect-shaped.
func (cg *CodeGen) matchSuggestsExpect(s *ast.MatchStmt, okCase, errCase *ast.MatchCase, okBinder string) bool {
	if okBinder == "" {
		return false
	}

	if !blockReturnsOnlyTheBinder(okCase.Body, okBinder) {
		return false
	}

	if !blockPanicsArbitrary(errCase.Body) {
		return false
	}

	cg.warn(DiagMatchResultTry, s.Pos(),
		"this `match` panics on Err and yields the Ok value - prefer `expr.expect(\"<message>\")`")

	return true
}

// matchSuggestsUnwrapOr fires when both arms produce a final value of
// the same Ok type: the Err arm yields a default, the Ok arm yields
// the bound value untouched.
func (cg *CodeGen) matchSuggestsUnwrapOr(s *ast.MatchStmt, okCase, errCase *ast.MatchCase, okBinder string) bool {
	if okBinder == "" {
		return false
	}

	if !blockReturnsOnlyTheBinder(okCase.Body, okBinder) {
		return false
	}

	if !blockReturnsSomeNonErrValue(errCase.Body) {
		return false
	}

	cg.warn(DiagMatchResultTry, s.Pos(),
		"this `match` returns a default on Err and the bound value on Ok - prefer `expr.unwrap_or(default)`")

	return true
}

// matchSuggestsMap fires for `Ok(v): Ok(f(v))` / `Err(m): Err(m)` --
// pure transformation of the Ok payload.
func (cg *CodeGen) matchSuggestsMap(s *ast.MatchStmt, okCase, errCase *ast.MatchCase, okBinder, errBinder string) bool {
	if okBinder == "" || errBinder == "" {
		return false
	}

	if !blockReturnsCtor(errCase.Body, "Err", errBinder) {
		return false
	}

	if !blockReturnsCtorOfTransform(okCase.Body, "Ok", okBinder) {
		return false
	}

	cg.warn(DiagMatchResultTry, s.Pos(),
		"this `match` transforms only the Ok payload - prefer `expr.map(fn(v) ...)`")

	return true
}

// matchSuggestsMapErr fires for `Ok(v): Ok(v)` / `Err(m): Err(g(m))`.
func (cg *CodeGen) matchSuggestsMapErr(s *ast.MatchStmt, okCase, errCase *ast.MatchCase, okBinder, errBinder string) bool {
	if okBinder == "" || errBinder == "" {
		return false
	}

	if !blockReturnsCtor(okCase.Body, "Ok", okBinder) {
		return false
	}

	if !blockReturnsCtorOfTransform(errCase.Body, "Err", errBinder) {
		return false
	}

	cg.warn(DiagMatchResultTry, s.Pos(),
		"this `match` transforms only the Err payload - prefer `expr.map_err(fn(e) ...)`")

	return true
}

// matchSuggestsTry is the fallback "looks like err-propagation"
// catch.  Only fires inside a function whose declared return type is
// `Result[_, _]` and whose Err arm propagates the error verbatim
// (e.g. `return Err(e)`).  Without the return-type guard the lint
// was firing on any match-with-early-return-on-Err, including ones
// where the enclosing function isn't even a Result-returner -- there
// `try` wouldn't compile.
func (cg *CodeGen) matchSuggestsTry(s *ast.MatchStmt, okCase, errCase *ast.MatchCase) {
	if cg.curFnReturnsResult == 0 {
		return
	}

	if !blockReturnsErrPassthrough(errCase.Body) {
		return
	}

	if !okArmIsRewriteableToTry(okCase.Body) {
		return
	}

	cg.warn(DiagMatchResultTry, s.Pos(),
		"this `match` on a Result propagates the error verbatim - prefer `let x = try expr` (or `try expr.map_err(...)` when the error type needs to change)")
}

// blockReturnsOnlyTheBinder returns true when b is a single
// `return BINDER` (or just `BINDER` as the arm-result expression).
func blockReturnsOnlyTheBinder(b *ast.Block, binder string) bool {
	if b == nil || len(b.Stmts) != 1 {
		return false
	}

	switch v := b.Stmts[0].(type) {
	case *ast.ReturnStmt:
		if id, ok := v.Value.(*ast.Identifier); ok {
			return id.Name == binder
		}
	case *ast.ExprStmt:
		if id, ok := v.Expr.(*ast.Identifier); ok {
			return id.Name == binder
		}
	}

	return false
}

// blockPanicsConstant returns true when b's only statement is a
// `panic(<string literal or interpolated string>)` call.
func blockPanicsConstant(b *ast.Block) bool {
	if b == nil || len(b.Stmts) != 1 {
		return false
	}

	es, ok := b.Stmts[0].(*ast.ExprStmt)
	if !ok {
		return false
	}

	call, ok := es.Expr.(*ast.CallExpr)
	if !ok {
		return false
	}

	id, ok := call.Func.(*ast.Identifier)
	if !ok || id.Name != "panic" {
		return false
	}

	if len(call.Args) != 1 {
		return false
	}

	switch call.Args[0].(type) {
	case *ast.StringLit, *ast.InterpolatedString:
		return true
	}

	return false
}

// blockPanicsArbitrary returns true when b's only statement is a
// `panic(<anything>)` call.  Used after blockPanicsConstant has
// already taken the simpler shape.
func blockPanicsArbitrary(b *ast.Block) bool {
	if b == nil || len(b.Stmts) != 1 {
		return false
	}

	es, ok := b.Stmts[0].(*ast.ExprStmt)
	if !ok {
		return false
	}

	call, ok := es.Expr.(*ast.CallExpr)
	if !ok {
		return false
	}

	id, ok := call.Func.(*ast.Identifier)
	if !ok || id.Name != "panic" {
		return false
	}

	return true
}

// blockReturnsSomeNonErrValue returns true when b is a single
// `return <expr>` where the expression is NOT `Err(...)` -- the
// shape of a default-value fallback.
func blockReturnsSomeNonErrValue(b *ast.Block) bool {
	if b == nil || len(b.Stmts) != 1 {
		return false
	}

	ret, ok := b.Stmts[0].(*ast.ReturnStmt)
	if !ok || ret.Value == nil {
		return false
	}

	if call, isCall := ret.Value.(*ast.CallExpr); isCall {
		if id, ok2 := call.Func.(*ast.Identifier); ok2 && id.Name == "Err" {
			return false
		}
	}

	return true
}

// blockReturnsCtor returns true when b is exactly `return CTOR(BINDER)` /
// `CTOR(BINDER)` (arm-result form).  Used to identify pass-through arms.
func blockReturnsCtor(b *ast.Block, ctor, binder string) bool {
	if b == nil || len(b.Stmts) != 1 {
		return false
	}

	var expr ast.Node

	switch v := b.Stmts[0].(type) {
	case *ast.ReturnStmt:
		expr = v.Value
	case *ast.ExprStmt:
		expr = v.Expr
	default:
		return false
	}

	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}

	id, ok := call.Func.(*ast.Identifier)
	if !ok || id.Name != ctor {
		return false
	}

	if len(call.Args) != 1 {
		return false
	}

	argID, ok := call.Args[0].(*ast.Identifier)
	if !ok {
		return false
	}

	return argID.Name == binder
}

// blockReturnsCtorOfTransform returns true when b is `return CTOR(<expr
// not equal to bare BINDER>)` -- the binder is referenced but
// transformed inside the constructor.
func blockReturnsCtorOfTransform(b *ast.Block, ctor, binder string) bool {
	if b == nil || len(b.Stmts) != 1 {
		return false
	}

	var expr ast.Node

	switch v := b.Stmts[0].(type) {
	case *ast.ReturnStmt:
		expr = v.Value
	case *ast.ExprStmt:
		expr = v.Expr
	default:
		return false
	}

	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}

	id, ok := call.Func.(*ast.Identifier)
	if !ok || id.Name != ctor {
		return false
	}

	if len(call.Args) != 1 {
		return false
	}
	// Bail if the arg is just the bare binder (that's the pass-through
	// case handled by blockReturnsCtor).
	if argID, ok2 := call.Args[0].(*ast.Identifier); ok2 && argID.Name == binder {
		return false
	}
	// Must reference the binder somewhere; a constant-replacement
	// arm isn't really a `.map(...)` -- it's a different shape.
	return astReferencesIdentifier(call.Args[0], binder)
}

// blockReturnsErrPassthrough returns true when b is `return Err(<binder>)`
// where the Err's argument is some identifier -- the shape of a
// straight error propagation that `try` replaces.
func blockReturnsErrPassthrough(b *ast.Block) bool {
	if b == nil || len(b.Stmts) != 1 {
		return false
	}

	ret, ok := b.Stmts[0].(*ast.ReturnStmt)
	if !ok {
		return false
	}

	call, ok := ret.Value.(*ast.CallExpr)
	if !ok {
		return false
	}

	id, ok := call.Func.(*ast.Identifier)
	if !ok || id.Name != "Err" {
		return false
	}

	if len(call.Args) != 1 {
		return false
	}

	_, isID := call.Args[0].(*ast.Identifier)

	return isID
}

// astReturnTypeIsResult reports whether a FuncDecl.RetType is a
// `Result[_, _]` shape. Recognized: bare `Result` (no params) and
// `Result[T, E]`, possibly qualified as `result::Result`. Used by
// the match-as-try lint to gate its noisiness.
func astReturnTypeIsResult(t ast.TypeExpr) bool {
	if t == nil {
		return false
	}

	switch tt := t.(type) {
	case *ast.SimpleType:
		return tt.Name == "Result" || tt.Name == "result::Result"
	case *ast.GenericType:
		return tt.Name == "Result" || tt.Name == "result::Result"
	}

	return false
}

// astReferencesIdentifier walks expr and reports whether any
// sub-expression is an Identifier whose name matches `name`.
func astReferencesIdentifier(expr ast.Node, name string) bool {
	found := false

	walkAST(expr, func(n ast.Node) {
		if found {
			return
		}

		if id, ok := n.(*ast.Identifier); ok && id.Name == name {
			found = true
		}
	})

	return found
}

// patternVariantName extracts the variant constructor name from a
// match pattern, e.g. "Ok" from `Ok(v)`, "Err" from `Err(_)`. Returns
// "" for patterns that aren't variant ctors.
func patternVariantName(p ast.Node) string {
	switch pat := p.(type) {
	case *ast.CallExpr:
		if id, ok := pat.Func.(*ast.Identifier); ok {
			return id.Name
		}
	case *ast.Identifier:
		// Niladic variant like `case None:` -- still has a name.
		return pat.Name
	}

	return ""
}

// okArmIsRewriteableToTry returns true when the Ok arm's body is the
// trivial "use the bound value" shape that `try` would replace. Keeps
// the lint conservative: complex bodies stay un-flagged.
func okArmIsRewriteableToTry(b *ast.Block) bool {
	if b == nil || len(b.Stmts) == 0 || len(b.Stmts) > 3 {
		return false
	}

	for _, s := range b.Stmts {
		switch s.(type) {
		case *ast.AssignStmt, *ast.VarDecl, *ast.ExprStmt, *ast.ReturnStmt:
			continue
		default:
			return false
		}
	}

	return true
}

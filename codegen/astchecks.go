package codegen

// Cheap, syntax-directed checks. Each one walks the AST once and emits a
// diagnostic on a recognizable shape - no dataflow, no type inference
// beyond what the existing scope already knows.

import (
	"reflect"

	irtypes "github.com/llir/llvm/ir/types"

	"github.com/Azer0s/tin/ast"
)

// runAstChecks runs every syntax-level check over the program.
func (cg *CodeGen) runAstChecks(prog *ast.Program) {
	if cg.replMode {
		return
	}

	for _, n := range prog.Stmts {
		if fd, ok := n.(*ast.FuncDecl); ok {
			cg.checkInfiniteRecursion(fd)
			cg.checkFiberMisuse(fd)
		}

		walkAST(n, func(node ast.Node) {
			switch e := node.(type) {
			case *ast.BinExpr:
				cg.checkIdenticalOperands(e)
				cg.checkArithIdentity(e)
				cg.checkFloatEqual(e)
			case *ast.AsExpr:
				cg.checkUselessCast(e)
			case *ast.IfStmt:
				cg.checkEmptyIfBody(e)
			case *ast.ForStmt:
				cg.checkLoopInvariant(e)
			}
		})
	}

	cg.checkMagicNumbers(prog)
	cg.checkStyle(prog)
}

// checkInfiniteRecursion flags a `f(x, y) = ... f(x, y) ...` where the
// recursive call passes the same arguments as the parameters and there's
// no observable change to those arguments before the call. Catches the
// classic typo where the user forgot to decrement a counter.
//
// The check is conservative: it only fires when EVERY function call to
// itself in the body uses identical-shaped args, which keeps it from
// flagging legitimate recursion that wraps a base-case branch.
func (cg *CodeGen) checkInfiniteRecursion(fn *ast.FuncDecl) {
	if fn.Body == nil || fn.IsExtern != "" || fn.IsVirtual {
		return
	}

	if len(fn.Params) == 0 {
		return
	}

	var (
		anySelfCall  bool
		allIdentical = true
		firstCallPos ast.Pos
		firstSeen    bool
	)

	walkAST(fn.Body, func(n ast.Node) {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return
		}

		id, ok := c.Func.(*ast.Identifier)
		if !ok || id.Name != fn.Name {
			return
		}

		anySelfCall = true

		if !firstSeen {
			firstCallPos = c.Pos()
			firstSeen = true
		}

		if len(c.Args) != len(fn.Params) {
			allIdentical = false

			return
		}

		for i, arg := range c.Args {
			pid, ok := arg.(*ast.Identifier)
			if !ok || pid.Name != fn.Params[i].Name {
				allIdentical = false

				return
			}
		}
	})

	if !anySelfCall || !allIdentical {
		return
	}

	cg.warn(DiagInfiniteRecursion, firstCallPos,
		"recursive call to %q passes the same arguments as its parameters; "+
			"the recursion never makes progress", fn.Name)
}

// checkIdenticalOperands flags `x == x`, `x != x`, `x - x`, etc., where
// both sides are the same syntactic expression. For floats the rewrite
// would be unsound (NaN), so the check is skipped on float operands AND
// on operands whose type can't yet be statically determined -- runAstChecks
// runs before scope is fully built, and exprIsFloat conservatively returns
// false for unknown identifiers; without the explicit unknown-skip we'd
// fire on `let v = 0.0/0.0; if v != v: ...` and miss the NaN check the
// programmer wrote.
func (cg *CodeGen) checkIdenticalOperands(e *ast.BinExpr) {
	// Logical ops where the duplicated operand is redundant -- `x && x`
	// is just `x`, `x || x` is just `x`. Side-effect-free identifiers
	// only; a duplicated CallExpr could be intentional (idempotency
	// check) and we have no purity info this early in the pipeline.
	if e.Op == "&&" || e.Op == "||" {
		if astEqual(e.Left, e.Right) && isPureForDuplicateCheck(e.Left) {
			cg.warn(DiagIdenticalOperands, e.Pos(),
				"both sides of %q are identical; the duplicate is redundant", e.Op)
		}
		// `x && !x` / `x || !x`: contradictions/tautologies.
		if neg, ok := e.Right.(*ast.UnaryExpr); ok && (neg.Op == "!" || neg.Op == "not") {
			if astEqual(e.Left, neg.Expr) && isPureForDuplicateCheck(e.Left) {
				cg.warnTautologyAndOr(e)
			}
		}

		if neg, ok := e.Left.(*ast.UnaryExpr); ok && (neg.Op == "!" || neg.Op == "not") {
			if astEqual(neg.Expr, e.Right) && isPureForDuplicateCheck(e.Right) {
				cg.warnTautologyAndOr(e)
			}
		}

		return
	}

	switch e.Op {
	case "==", "!=", "<", "<=", ">", ">=", "-", "&", "|", "^", "/", "%":
	default:
		return
	}

	if !astEqual(e.Left, e.Right) {
		return
	}
	// Float operand -> keep silent; NaN makes != / == meaningful.
	if cg.exprIsFloat(e.Left) {
		return
	}
	// Unknown type (typically: identifier whose let-binding hasn't been
	// resolved yet) -> skip rather than risk a false positive on a float.
	if cg.staticTypeOf(e.Left) == nil {
		return
	}

	cg.warn(DiagIdenticalOperands, e.Pos(),
		"both sides of %q are identical; result is constant", e.Op)
}

// warnTautologyAndOr fires the boolean-fold warning for `x && !x`
// (always false) or `x || !x` (always true).
func (cg *CodeGen) warnTautologyAndOr(e *ast.BinExpr) {
	val := e.Op == "||"
	cg.warn(DiagBoolAnalysis, e.Pos(),
		"%q with operand and its negation is always %v", e.Op, val)
}

// isPureForDuplicateCheck reports whether an expression is safe to
// flag as a duplicate operand without risking a false positive on a
// side-effecting call. Conservative: only allows identifiers, literals,
// field accesses, and unary/binary trees over those.
func isPureForDuplicateCheck(n ast.Node) bool {
	switch e := n.(type) {
	case *ast.Identifier, *ast.IntLit, *ast.FloatLit, *ast.StringLit, *ast.BoolLit, *ast.NilLit:
		return true
	case *ast.FieldAccess:
		return isPureForDuplicateCheck(e.Expr)
	case *ast.UnaryExpr:
		return isPureForDuplicateCheck(e.Expr)
	case *ast.BinExpr:
		return isPureForDuplicateCheck(e.Left) && isPureForDuplicateCheck(e.Right)
	}

	return false
}

// checkArithIdentity flags arithmetic / bitwise ops that fold to a known
// constant (or are no-ops) because one operand is a saturating identity:
// x & 0, x * 0, x + 0, x | -1, etc.
func (cg *CodeGen) checkArithIdentity(e *ast.BinExpr) {
	lConst := cg.tryFoldExpr(e.Left)
	rConst := cg.tryFoldExpr(e.Right)

	chk := func(c foldedValue, side string) {
		if c.kind != foldInt {
			return
		}

		switch e.Op {
		case "&":
			if c.intVal == 0 {
				cg.warn(DiagUselessIdentity, e.Pos(),
					"%s & 0 is always 0", side)
			}
		case "|":
			if c.intVal == -1 {
				cg.warn(DiagUselessIdentity, e.Pos(),
					"%s | -1 is always -1", side)
			}
		case "*":
			switch c.intVal {
			case 0:
				cg.warn(DiagUselessIdentity, e.Pos(),
					"%s * 0 is always 0", side)
			case 1:
				cg.warn(DiagUselessIdentity, e.Pos(),
					"%s * 1 is a no-op", side)
			}
		case "+":
			if c.intVal == 0 {
				cg.warn(DiagUselessIdentity, e.Pos(),
					"%s + 0 is a no-op", side)
			}
		case "-":
			if c.intVal == 0 && side == "x" {
				cg.warn(DiagUselessIdentity, e.Pos(),
					"x - 0 is a no-op")
			}
		case "/":
			if c.intVal == 1 && side == "x" {
				cg.warn(DiagUselessIdentity, e.Pos(),
					"x / 1 is a no-op")
			}
		case "<<", ">>":
			if c.intVal == 0 && side == "x" {
				cg.warn(DiagUselessIdentity, e.Pos(),
					"shift by 0 is a no-op")
			}
		}
	}

	if lConst.kind == foldInt && rConst.kind != foldInt {
		chk(lConst, "0")
	}

	if rConst.kind == foldInt && lConst.kind != foldInt {
		chk(rConst, "x")
	}
}

// checkFloatEqual fires on `==` / `!=` between float operands. The
// warning is default-off because direct float equality is sometimes
// intentional (NaN canary, exact zero check).
func (cg *CodeGen) checkFloatEqual(e *ast.BinExpr) {
	if e.Op != "==" && e.Op != "!=" {
		return
	}

	if !cg.exprIsFloat(e.Left) && !cg.exprIsFloat(e.Right) {
		return
	}

	cg.warn(DiagFloatEqual, e.Pos(),
		"direct equality on floats is fragile; consider `abs(a - b) < eps`")
}

// checkUselessCast flags `x as T` when x is statically already T.
func (cg *CodeGen) checkUselessCast(e *ast.AsExpr) {
	src := cg.staticTypeOf(e.Expr)
	if src == nil {
		return
	}

	dst, err := cg.tinTypeToLLVM(e.Type)
	if err != nil || dst == nil {
		return
	}

	if !src.Equal(dst) {
		return
	}

	cg.warn(DiagUselessCast, e.Pos(),
		"cast to %s has no effect; the expression is already %s", dst, src)
}

// checkEmptyIfBody flags `if x: { }` or `else: { }` where the block is
// empty. Almost always an unfinished edit. An explicit `pass` keyword is
// the user telling us the empty body is intentional, so we suppress.
func (cg *CodeGen) checkEmptyIfBody(s *ast.IfStmt) {
	if s.Then != nil && len(s.Then.Stmts) == 0 && !s.Then.IsExplicitPass {
		cg.warn(DiagEmptyBody, s.Pos(), "empty if body")
	}

	if s.Else != nil && len(s.Else.Stmts) == 0 && !s.Else.IsExplicitPass {
		cg.warn(DiagEmptyBody, s.Pos(), "empty else body")
	}
}

// exprIsFloat reports whether expr's static type is f32 or f64. Conservative
// (returns false on unknown), so the check never fires when in doubt.
func (cg *CodeGen) exprIsFloat(expr ast.Node) bool {
	t := cg.staticTypeOf(expr)
	if t == nil {
		return false
	}

	return irtypes.IsFloat(t)
}

// checkLoopInvariant flags pure expressions inside a loop body whose
// operands are never written (or address-taken) within the body or post
// statements. The hint suggests hoisting the expression before the loop.
//
// Only arithmetic/bitwise/comparison/boolean/unary/field/cast trees over
// identifiers and literals are considered: anything reached through a
// call, pointer deref, or indexed read may observe state we don't
// statically prove invariant, so we leave those alone.
//
// Emits at the maximal invariant subtree -- if `(a + b) * c` is fully
// invariant, fires once on the whole expression rather than separately on
// each subterm.
func (cg *CodeGen) checkLoopInvariant(loop *ast.ForStmt) {
	if loop.Body == nil || len(loop.Body.Stmts) == 0 {
		return
	}

	mutated := map[string]bool{}

	collectLoopMutations(loop.Body, mutated)

	if loop.Post != nil {
		collectLoopMutations(loop.Post, mutated)
	}

	if loop.VarName != "" {
		mutated[loop.VarName] = true
	}

	cg.walkLicm(loop.Body, false, mutated)
}

// walkLicm descends through n's children. When it finds a BinExpr or
// UnaryExpr that is fully loop-invariant and references at least one
// identifier (so the optimizer can't trivially fold it), it warns -- but
// only at the outermost such subtree, by passing parentInv=true to
// children of an emitted node so they suppress their own emit.
//
// Nested ForStmt and LambdaExpr bodies are skipped: nested loops get
// their own checkLoopInvariant pass, and lambdas may be invoked outside
// the loop where "loop-invariant" no longer applies.
func (cg *CodeGen) walkLicm(n ast.Node, parentInv bool, mutated map[string]bool) {
	if n == nil {
		return
	}
	// Same typed-nil guard as walkAST: an interface value can wrap a nil
	// concrete pointer (e.g. *ast.Block) which `n == nil` misses, and
	// dereferencing it (n.Pos(), or any field access) would segfault.
	if rv := reflect.ValueOf(n); rv.Kind() == reflect.Ptr && rv.IsNil() {
		return
	}

	switch n.(type) {
	case *ast.ForStmt, *ast.LambdaExpr:
		return
	}

	isInv := false

	switch n.(type) {
	case *ast.BinExpr, *ast.UnaryExpr:
		if isLoopInvariantExpr(n, mutated) && containsIdentifier(n) {
			isInv = true

			if !parentInv {
				cg.warn(DiagLoopInvariant, n.Pos(),
					"expression does not depend on loop state; consider hoisting it before the loop")
			}
		}
	}

	switch e := n.(type) {
	case *ast.Block:
		for _, s := range e.Stmts {
			cg.walkLicm(s, false, mutated)
		}
	case *ast.IfStmt:
		cg.walkLicm(e.Cond, false, mutated)
		cg.walkLicm(e.Then, false, mutated)

		for _, ei := range e.ElseIfs {
			cg.walkLicm(ei.Cond, false, mutated)
			cg.walkLicm(ei.Body, false, mutated)
		}

		cg.walkLicm(e.Else, false, mutated)
	case *ast.MatchStmt:
		cg.walkLicm(e.Expr, false, mutated)

		for _, c := range e.Cases {
			cg.walkLicm(c.Pattern, false, mutated)
			cg.walkLicm(c.Guard, false, mutated)
			cg.walkLicm(c.Body, false, mutated)
		}

		cg.walkLicm(e.Default, false, mutated)
	case *ast.AssignStmt:
		cg.walkLicm(e.Target, false, mutated)
		cg.walkLicm(e.Value, false, mutated)
	case *ast.AugAssignStmt:
		cg.walkLicm(e.Target, false, mutated)
		cg.walkLicm(e.Value, false, mutated)
	case *ast.PostfixStmt:
		cg.walkLicm(e.Expr, false, mutated)
	case *ast.VarDecl:
		cg.walkLicm(e.Value, false, mutated)
	case *ast.ReturnStmt:
		cg.walkLicm(e.Value, false, mutated)
	case *ast.EchoStmt:
		cg.walkLicm(e.Value, false, mutated)
	case *ast.ExprStmt:
		cg.walkLicm(e.Expr, false, mutated)
	case *ast.DeferStmt:
		cg.walkLicm(e.Call, false, mutated)
	case *ast.BinExpr:
		cg.walkLicm(e.Left, isInv, mutated)
		cg.walkLicm(e.Right, isInv, mutated)
	case *ast.UnaryExpr:
		cg.walkLicm(e.Expr, isInv, mutated)
	case *ast.CallExpr:
		cg.walkLicm(e.Func, false, mutated)

		for _, a := range e.Args {
			cg.walkLicm(a, false, mutated)
		}
	case *ast.IndexExpr:
		cg.walkLicm(e.Expr, false, mutated)
		cg.walkLicm(e.Index, false, mutated)
	case *ast.FieldAccess:
		cg.walkLicm(e.Expr, false, mutated)
	case *ast.AsExpr:
		cg.walkLicm(e.Expr, false, mutated)
	case *ast.AwaitExpr:
		cg.walkLicm(e.Future, false, mutated)
	case *ast.SpawnExpr:
		cg.walkLicm(e.Call, false, mutated)
		cg.walkLicm(e.DoBlock, false, mutated)
	case *ast.TernaryExpr:
		cg.walkLicm(e.Cond, false, mutated)
		cg.walkLicm(e.Then, false, mutated)
		cg.walkLicm(e.Else, false, mutated)
	case *ast.TaggedBlock:
		cg.walkLicm(e.Body, false, mutated)
	}
}

// isLoopInvariantExpr reports whether n is a pure expression all of
// whose identifier operands are not in the mutated set. Conservative:
// returns false for any node kind that could reach a side-effecting
// operation (calls, derefs, indexing, address-of, await, spawn).
func isLoopInvariantExpr(n ast.Node, mutated map[string]bool) bool {
	if n == nil {
		return true
	}

	switch e := n.(type) {
	case *ast.IntLit, *ast.FloatLit, *ast.StringLit, *ast.BoolLit, *ast.NilLit:
		return true
	case *ast.Identifier:
		return !mutated[e.Name]
	case *ast.FieldAccess:
		return isLoopInvariantExpr(e.Expr, mutated)
	case *ast.UnaryExpr:
		return isPureUnaryOp(e.Op) && isLoopInvariantExpr(e.Expr, mutated)
	case *ast.BinExpr:
		return isPureBinOp(e.Op) &&
			isLoopInvariantExpr(e.Left, mutated) &&
			isLoopInvariantExpr(e.Right, mutated)
	case *ast.AsExpr:
		return isLoopInvariantExpr(e.Expr, mutated)
	}

	return false
}

func isPureBinOp(op string) bool {
	switch op {
	case "+", "-", "*", "/", "%",
		"&", "|", "^", "<<", ">>",
		"==", "!=", "<", "<=", ">", ">=",
		"&&", "||":
		return true
	}

	return false
}

func isPureUnaryOp(op string) bool {
	switch op {
	case "-", "+", "!", "~", "not":
		return true
	}

	return false
}

// containsIdentifier reports whether n's subtree contains at least one
// *ast.Identifier. Used by the LICM check so we don't emit on pure
// constant expressions like `1 + 2` -- the optimizer folds those without
// a programmer-visible improvement.
func containsIdentifier(n ast.Node) bool {
	found := false

	walkAST(n, func(c ast.Node) {
		if _, ok := c.(*ast.Identifier); ok {
			found = true
		}
	})

	return found
}

// collectLoopMutations records every name that is written, address-
// taken, or rebound inside the loop body. Conservative on every front:
// any `&x`, any assignment-target chain rooted at an Identifier, and any
// VarDecl introduces the bound name into the mutated set. CallExpr args
// of the form `&x` also mark x, since the callee can mutate through the
// address.
func collectLoopMutations(body ast.Node, mutated map[string]bool) {
	walkAST(body, func(n ast.Node) {
		switch e := n.(type) {
		case *ast.AssignStmt:
			markBaseIdent(e.Target, mutated)
		case *ast.AugAssignStmt:
			markBaseIdent(e.Target, mutated)
		case *ast.PostfixStmt:
			markBaseIdent(e.Expr, mutated)
		case *ast.VarDecl:
			mutated[e.Name] = true
		case *ast.AddrExpr:
			markBaseIdent(e.Val, mutated)
		case *ast.AddressOfExpr:
			markBaseIdent(e.Expr, mutated)
		}
	})
}

// markBaseIdent walks an l-value chain (FieldAccess / IndexExpr /
// DerefExpr) down to its root identifier and records the name. A write
// through `obj.field`, `arr[i]`, or `*p` means we can no longer prove
// `obj`, `arr`, or `p` is loop-invariant, so the entire base name is
// marked.
func markBaseIdent(n ast.Node, m map[string]bool) {
	for n != nil {
		switch e := n.(type) {
		case *ast.Identifier:
			m[e.Name] = true

			return
		case *ast.FieldAccess:
			n = e.Expr
		case *ast.IndexExpr:
			n = e.Expr
		case *ast.DerefExpr:
			n = e.Expr
		default:
			return
		}
	}
}

// checkMagicNumbers flags int and float literals that aren't in the
// universal exempt set ({-1, 0, 1, 2}) and aren't in a context where
// embedding the literal directly is conventional.
//
// Two-pass design: the first walkAST classifies each node into context
// buckets (const-init descendants, array-index descendants, direct
// comparison/bitwise operands -- including those wrapped in unary +/-
// or `as` casts). The second walk inspects each literal against its
// bucket flags and emits when none of them apply.
//
// Default-off; gated through warn() but we also short-circuit the
// classification pass when the diagnostic is disabled to avoid building
// per-node maps for nothing.
func (cg *CodeGen) checkMagicNumbers(prog *ast.Program) {
	if !cg.diagEnabled(DiagMagicNumber) {
		return
	}

	inConst := map[ast.Node]bool{}
	inIndex := map[ast.Node]bool{}
	cmpOperand := map[ast.Node]bool{}
	bitOperand := map[ast.Node]bool{}

	classify := func(n ast.Node) {
		switch e := n.(type) {
		case *ast.VarDecl:
			if e.IsConst && e.Value != nil {
				walkAST(e.Value, func(c ast.Node) { inConst[c] = true })
			}
		case *ast.TopLevelVar:
			if e.IsConst && e.Value != nil {
				walkAST(e.Value, func(c ast.Node) { inConst[c] = true })
			}
		case *ast.IndexExpr:
			if e.Index != nil {
				walkAST(e.Index, func(c ast.Node) { inIndex[c] = true })
			}
		case *ast.BinExpr:
			switch e.Op {
			case "==", "!=", "<", "<=", ">", ">=":
				peelLitContext(e.Left, cmpOperand)
				peelLitContext(e.Right, cmpOperand)
			case "&", "|", "^", "<<", ">>":
				peelLitContext(e.Left, bitOperand)
				peelLitContext(e.Right, bitOperand)
			}
		}
	}

	flag := func(n ast.Node) {
		switch e := n.(type) {
		case *ast.IntLit:
			if shouldFlagIntMagic(e, inConst[e], inIndex[e], cmpOperand[e], bitOperand[e]) {
				cg.warn(DiagMagicNumber, e.Pos(),
					"magic number %d; consider naming it as a const", e.Value)
			}
		case *ast.FloatLit:
			if shouldFlagFloatMagic(e, inConst[e], inIndex[e], cmpOperand[e]) {
				cg.warn(DiagMagicNumber, e.Pos(),
					"magic number %g; consider naming it as a const", e.Value)
			}
		}
	}

	for _, s := range prog.Stmts {
		walkAST(s, classify)
	}

	for _, s := range prog.Stmts {
		walkAST(s, flag)
	}
}

// peelLitContext records `n` and any literal-shaped descendant reached
// through unary +/- or an `as` cast. Captures `5`, `-5`, and `5 as i64`
// uniformly so the magic-number check exempts a literal regardless of
// the syntactic decoration around it.
func peelLitContext(n ast.Node, m map[ast.Node]bool) {
	for n != nil {
		m[n] = true

		switch e := n.(type) {
		case *ast.UnaryExpr:
			if e.Op == "-" || e.Op == "+" {
				n = e.Expr

				continue
			}
		case *ast.AsExpr:
			n = e.Expr

			continue
		}

		return
	}
}

// shouldFlagIntMagic decides whether an integer literal warrants a
// magic-number warning given its context flags.
func shouldFlagIntMagic(e *ast.IntLit, inConst, inIdx, cmpOp, bitOp bool) bool {
	if e.Big != nil {
		return false
	}

	if inConst || inIdx || cmpOp {
		return false
	}

	v := e.Value
	if v >= -1 && v <= 2 {
		return false
	}

	if bitOp && isBitOpExempt(v) {
		return false
	}

	return true
}

// shouldFlagFloatMagic decides whether a float literal warrants a
// magic-number warning. Floats don't get a bit-op exemption since
// bitwise ops aren't defined for them.
func shouldFlagFloatMagic(e *ast.FloatLit, inConst, inIdx, cmpOp bool) bool {
	if inConst || inIdx || cmpOp {
		return false
	}

	switch e.Value {
	case -1, 0, 0.5, 1, 2:
		return false
	}

	return true
}

// isBitOpExempt reports whether v is a recognizable bit pattern that
// commonly appears as a bitwise operand: power of two (single-bit set
// or shift count) or 2^N - 1 (an all-ones mask).
func isBitOpExempt(v int64) bool {
	if v <= 0 {
		return v == 0 || v == -1
	}

	if v&(v-1) == 0 {
		return true
	}

	if (v+1)&v == 0 {
		return true
	}

	return false
}

// diagEnabled reports whether a default-off diagnostic has been opted
// into via -W<name>, -Wpedantic, etc. Default-on diagnostics always
// return true.
func (cg *CodeGen) diagEnabled(name string) bool {
	if !defaultOffWarnings[name] {
		return true
	}

	if s := cg.diags[name]; s != nil {
		return s.enabled
	}

	return false
}

// astEqual reports whether two AST nodes are syntactically identical for
// the purposes of identical-operand detection. Conservative: only
// recognizes shapes that have no chance of side effects (identifiers,
// field access on the same target, integer literals).
func astEqual(a, b ast.Node) bool {
	switch x := a.(type) {
	case *ast.Identifier:
		y, ok := b.(*ast.Identifier)

		return ok && x.Name == y.Name
	case *ast.IntLit:
		y, ok := b.(*ast.IntLit)
		if !ok {
			return false
		}

		if (x.Big == nil) != (y.Big == nil) {
			return false
		}

		if x.Big != nil {
			return x.Big.Cmp(y.Big) == 0
		}

		return x.Value == y.Value
	case *ast.BoolLit:
		y, ok := b.(*ast.BoolLit)

		return ok && x.Value == y.Value
	case *ast.FieldAccess:
		y, ok := b.(*ast.FieldAccess)

		return ok && x.Field == y.Field && astEqual(x.Expr, y.Expr)
	}

	return false
}

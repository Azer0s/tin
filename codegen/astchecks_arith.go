package codegen

import (
	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) checkInfiniteRecursion(fn *ast.FuncDecl) {
	if fn.Body == nil || fn.IsExtern != "" || fn.IsVirtual {
		return
	}

	// Zero-arg self-recursion (`fn f() i64 = return f()`) also never
	// makes progress: there are no values to vary across the call.
	// Don't early-return here; the args/params loop below handles
	// the zero-param case trivially via allIdentical staying true.
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

	// constOnRight distinguishes `x OP K` from `K OP x` so the non-commutative
	// gates (sub, div, shift) only fire on the canonical shape.  The format
	// strings always label the variable side `x`; previously the call site
	// reused the same string as both the gate and the `%s` substitution,
	// which produced misfires like "0 * 1 is a no-op" on `1 * (per * ...)`.
	chk := func(c foldedValue, constOnRight bool) {
		if c.kind != foldInt {
			return
		}

		switch e.Op {
		case "&":
			if c.intVal == 0 {
				cg.warn(DiagUselessIdentity, e.Pos(),
					"x & 0 is always 0")
			}
		case "|":
			if c.intVal == -1 {
				cg.warn(DiagUselessIdentity, e.Pos(),
					"x | -1 is always -1")
			}
		case "*":
			switch c.intVal {
			case 0:
				cg.warn(DiagUselessIdentity, e.Pos(),
					"x * 0 is always 0")
			case 1:
				cg.warn(DiagUselessIdentity, e.Pos(),
					"x * 1 is a no-op")
			}
		case "+":
			if c.intVal == 0 {
				cg.warn(DiagUselessIdentity, e.Pos(),
					"x + 0 is a no-op")
			}
		case "-":
			if c.intVal == 0 && constOnRight {
				cg.warn(DiagUselessIdentity, e.Pos(),
					"x - 0 is a no-op")
			}
		case "/":
			if c.intVal == 1 && constOnRight {
				cg.warn(DiagUselessIdentity, e.Pos(),
					"x / 1 is a no-op")
			}
		case "<<", ">>":
			if c.intVal == 0 && constOnRight {
				cg.warn(DiagUselessIdentity, e.Pos(),
					"shift by 0 is a no-op")
			}
		}
	}

	if lConst.kind == foldInt && rConst.kind != foldInt {
		chk(lConst, false)
	}

	if rConst.kind == foldInt && lConst.kind != foldInt {
		chk(rConst, true)
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
// checkResultMatchAntipattern walks a two-arm `match ...: case Ok / case Err`
// and suggests the shorter Result-method form when one fits:
//
//   - `return Err(...)` propagation         -> `let x = try expr`
//   - `panic(...)` on Err                   -> `expr.unwrap()` or .expect
//   - return-a-default on Err               -> `expr.unwrap_or(default)`
//   - `Ok(f(v))` / `Err(passthrough)`        -> `expr.map(f)`
//   - `Err(g(e))` / `Ok(passthrough)`        -> `expr.map_err(g)`
//
// Stays conservative: skips guards, default arms, anything that's not
// the two-arm Ok/Err shape, and Ok bodies with control flow.

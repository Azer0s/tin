package codegen

// Cheap, syntax-directed checks. Each one walks the AST once and emits a
// diagnostic on a recognizable shape - no dataflow, no type inference
// beyond what the existing scope already knows.

import (
	"math/big"
	"strconv"

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
		}

		walkAST(n, func(node ast.Node) {
			switch e := node.(type) {
			case *ast.BinExpr:
				cg.checkIdenticalOperands(e)
				cg.checkArithIdentity(e)
				cg.checkFloatEqual(e)
				cg.checkFloatPrecision(e)
			case *ast.AsExpr:
				cg.checkUselessCast(e)
			case *ast.IfStmt:
				cg.checkEmptyIfBody(e)
			}
		})
	}
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
// would be unsound (NaN), so the check is skipped on float operands.
func (cg *CodeGen) checkIdenticalOperands(e *ast.BinExpr) {
	switch e.Op {
	case "==", "!=", "<", "<=", ">", ">=", "-", "&", "|", "^", "/", "%":
	default:
		return
	}

	if !astEqual(e.Left, e.Right) {
		return
	}

	if cg.exprIsFloat(e.Left) {
		return
	}

	cg.warn(DiagIdenticalOperands, e.Pos(),
		"both sides of %q are identical; result is constant", e.Op)
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
// empty. Almost always an unfinished edit.
func (cg *CodeGen) checkEmptyIfBody(s *ast.IfStmt) {
	if s.Then != nil && len(s.Then.Stmts) == 0 {
		cg.warn(DiagEmptyBody, s.Pos(), "empty `if` body")
	}

	if s.Else != nil && len(s.Else.Stmts) == 0 {
		cg.warn(DiagEmptyBody, s.Pos(), "empty `else` body")
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

// checkFloatPrecision flags `==` / `!=` whose two sides are constant float
// expressions that disagree under IEEE arithmetic but would compare equal
// under exact rational arithmetic. The classic 0.1 + 0.2 == 0.3 trap.
//
// We fold both sides twice: once with float64 (the runtime semantics) and
// once with big.Rat parsed from each literal's shortest-decimal
// representation (the user's mental model). A disagreement between the
// two outcomes is the signal to warn.
func (cg *CodeGen) checkFloatPrecision(e *ast.BinExpr) {
	if e.Op != "==" && e.Op != "!=" {
		return
	}

	lf, lr, lok := tryFoldFloat(e.Left)
	if !lok {
		return
	}

	rf, rr, rok := tryFoldFloat(e.Right)
	if !rok {
		return
	}

	floatEq := lf == rf
	ratEq := lr.Cmp(rr) == 0

	if floatEq == ratEq {
		return
	}

	ieeeResult := floatEq
	exactResult := ratEq

	if e.Op == "!=" {
		ieeeResult = !ieeeResult
		exactResult = !exactResult
	}

	cg.warn(DiagFloatPrecision, e.Pos(),
		"%q evaluates to %v under IEEE 754 but %v under exact arithmetic; "+
			"use `abs(a - b) < eps` instead",
		e.Op, ieeeResult, exactResult)
}

// tryFoldFloat folds a float-valued expression to (float64, *big.Rat, ok).
// Handles FloatLit and the four arithmetic ops on folded operands. Returns
// !ok for anything we can't statically resolve (identifiers, calls, etc.).
func tryFoldFloat(n ast.Node) (float64, *big.Rat, bool) {
	switch e := n.(type) {
	case *ast.FloatLit:
		f := e.Value
		// Use the shortest-decimal text so 0.1's rational really is 1/10
		// rather than the bit-exact float-64 value.
		r := new(big.Rat)

		s := strconv.FormatFloat(f, 'g', -1, 64)
		if _, ok := r.SetString(s); !ok {
			return 0, nil, false
		}

		return f, r, true

	case *ast.UnaryExpr:
		if e.Op != "-" {
			return 0, nil, false
		}

		f, r, ok := tryFoldFloat(e.Expr)
		if !ok {
			return 0, nil, false
		}

		return -f, new(big.Rat).Neg(r), true

	case *ast.BinExpr:
		lf, lr, lok := tryFoldFloat(e.Left)
		if !lok {
			return 0, nil, false
		}

		rf, rr, rok := tryFoldFloat(e.Right)
		if !rok {
			return 0, nil, false
		}

		out := new(big.Rat)

		switch e.Op {
		case "+":
			return lf + rf, out.Add(lr, rr), true
		case "-":
			return lf - rf, out.Sub(lr, rr), true
		case "*":
			return lf * rf, out.Mul(lr, rr), true
		case "/":
			if rf == 0 || rr.Sign() == 0 {
				return 0, nil, false
			}

			return lf / rf, out.Quo(lr, rr), true
		}
	}

	return 0, nil, false
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

		return ok && x.Value == y.Value
	case *ast.BoolLit:
		y, ok := b.(*ast.BoolLit)

		return ok && x.Value == y.Value
	case *ast.FieldAccess:
		y, ok := b.(*ast.FieldAccess)

		return ok && x.Field == y.Field && astEqual(x.Expr, y.Expr)
	}

	return false
}

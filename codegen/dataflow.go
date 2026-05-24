package codegen

// Intraprocedural dataflow analysis. The pass is syntax-directed: it walks
// the AST recursively, threading an abstract state through each statement
// and merging at join points. Loops iterate to a small fixed bound.
//
// The lattice combines three pieces:
//
//   - SCCP-style constant propagation for integer and boolean locals.
//     Lattice elements are TOP (uninitialized), CONST(c), or BOTTOM (varying).
//
//   - Nil tracking for pointer / RC-typed locals, an Andersen's-style
//     "may-point-to" set restricted to two abstract addresses: NIL and
//     NON_NIL. Per-variable lattice: TOP, NIL, NON_NIL, BOTTOM (could be
//     either).
//
//   - Branch narrowing: an `if p == nil { ... } else { ... }` propagates
//     `p == NIL` into the then-branch and `p == NON_NIL` into the else;
//     `if p != nil` is the inverse.
//
// Findings drive two warning sources:
//
//   - flow-sensitive `deref-nil`: dereferencing or field-accessing a name
//     that's provably nil at that program point.
//   - flow-sensitive `bool-analysis`: an `if`/`elif`/`while` whose
//     condition folds to a constant after substituting locals.

import (
	"math/big"
	"strconv"

	"github.com/Azer0s/tin/ast"
)

// floatPair holds a float-valued constant in two parallel forms: the IEEE
// 754 result (what runtime arithmetic produces) and the exact rational
// (what arbitrary-precision arithmetic produces). Used to detect comparisons
// where the two disagree, like 0.1 + 0.2 == 0.3.

func dfFoldFloat(n ast.Node, st *dfState) *floatPair {
	switch e := n.(type) {
	case *ast.FloatLit:
		return floatPairFromLit(e.Value)
	case *ast.IntLit:
		// Integer literals participate in mixed expressions like `x + 1`.
		return &floatPair{
			ieee:  float64(e.Value),
			exact: new(big.Rat).SetInt64(e.Value),
		}
	case *ast.Identifier:
		if fp, ok := st.floats[e.Name]; ok {
			return fp
		}
	case *ast.UnaryExpr:
		if e.Op != "-" {
			return nil
		}

		inner := dfFoldFloat(e.Expr, st)
		if inner == nil {
			return nil
		}

		return &floatPair{
			ieee:  -inner.ieee,
			exact: new(big.Rat).Neg(inner.exact),
		}
	case *ast.BinExpr:
		l := dfFoldFloat(e.Left, st)
		if l == nil {
			return nil
		}

		r := dfFoldFloat(e.Right, st)
		if r == nil {
			return nil
		}

		out := new(big.Rat)

		switch e.Op {
		case "+":
			return &floatPair{ieee: l.ieee + r.ieee, exact: out.Add(l.exact, r.exact)}
		case "-":
			return &floatPair{ieee: l.ieee - r.ieee, exact: out.Sub(l.exact, r.exact)}
		case "*":
			return &floatPair{ieee: l.ieee * r.ieee, exact: out.Mul(l.exact, r.exact)}
		case "/":
			if r.ieee == 0 || r.exact.Sign() == 0 {
				return nil
			}

			return &floatPair{ieee: l.ieee / r.ieee, exact: out.Quo(l.exact, r.exact)}
		}
	}

	return nil
}

// floatPairFromLit builds a (IEEE, exact) pair from a float literal value.
// Routes through shortest-decimal so 0.1 maps to the exact 1/10 rather
// than the IEEE-rounded approximation.
func floatPairFromLit(f float64) *floatPair {
	r := new(big.Rat)

	s := strconv.FormatFloat(f, 'g', -1, 64)
	if _, ok := r.SetString(s); !ok {
		return nil
	}

	return &floatPair{ieee: f, exact: r}
}

// isDeinitCall returns the variable name being deinitialized when expr
// matches `x.deinit()` or `(*x).deinit()`. The second form is what shows
// up after parsing the typical `(*ptr).deinit()` syntax.
func isDeinitCall(expr ast.Node) (string, bool) {
	c, ok := expr.(*ast.CallExpr)
	if !ok {
		return "", false
	}

	fa, ok := c.Func.(*ast.FieldAccess)
	if !ok || fa.Field != "deinit" {
		return "", false
	}

	switch r := fa.Expr.(type) {
	case *ast.Identifier:
		return r.Name, true
	case *ast.DerefExpr:
		if id, ok := r.Expr.(*ast.Identifier); ok {
			return id.Name, true
		}
	}

	return "", false
}

// dfNilOf returns the nil-state of an expression evaluated under st.
func (cg *CodeGen) dfNilOf(expr ast.Node, st *dfState) nilFact {
	if expr == nil {
		return nilTop
	}

	switch e := expr.(type) {
	case *ast.NilLit:
		return nilIsNil
	case *ast.AddressOfExpr:
		return nilNonNil
	case *ast.Identifier:
		if v, ok := st.nil[e.Name]; ok {
			return v
		}

		return nilBottom
	}

	return nilBottom
}

// dfConstInt extracts an int64 from a constFact when its kind is cInt.
func dfConstInt(f constFact) (int64, bool) {
	if f.kind != cInt {
		return 0, false
	}

	return f.intVal, true
}

// dfEval evaluates expr against the abstract state, returning a constFact.
// Recursively folds BinExpr/UnaryExpr using flow-sensitive identifier
// lookups, falling through to tryFoldExpr for everything else.
func (cg *CodeGen) dfEval(expr ast.Node, st *dfState) constFact {
	if expr == nil {
		return cTopFact()
	}

	switch e := expr.(type) {
	case *ast.IntLit:
		return cIntFact(e.Value)
	case *ast.BoolLit:
		return cBoolFact(e.Value)
	case *ast.Identifier:
		if v, ok := st.cnst[e.Name]; ok {
			return v
		}
	case *ast.BinExpr:
		// Strided modulo fold: when the left side has a tracked strided
		// interval all of whose members are exact multiples of the right
		// side's constant divisor, the result is provably 0. This is
		// what makes `if epoch % 500 == 0` fire bool-analysis inside
		// `for epoch = 0; ...; epoch += 500`.
		if e.Op == "%" {
			rv := cg.dfEval(e.Right, st)
			if n, ok := dfConstInt(rv); ok && n > 0 {
				if iv := cg.intervalOf(e.Left, st); iv.set && allMembersDivisibleBy(iv, n) {
					return cIntFact(0)
				}
			}
		}

		l := cg.dfEval(e.Left, st)
		r := cg.dfEval(e.Right, st)

		return foldBinConst(e.Op, l, r)
	case *ast.UnaryExpr:
		v := cg.dfEval(e.Expr, st)

		return foldUnaryConst(e.Op, v)
	}

	v := cg.tryFoldExpr(expr)
	switch v.kind {
	case foldInt:
		return cIntFact(v.intVal)
	case foldBool:
		return cBoolFact(v.boolVal)
	case foldUnknown, foldAtom:
		return cBotFact()
	}

	return cBotFact()
}

// foldBinConst evaluates `a op b` when both operands are concrete
// constants, returning BOTTOM otherwise.
func foldBinConst(op string, a, b constFact) constFact {
	if a.kind == cBottom || b.kind == cBottom || a.kind == cTop || b.kind == cTop {
		return cBotFact()
	}

	if a.kind == cInt && b.kind == cInt {
		switch op {
		case "+":
			return cIntFact(a.intVal + b.intVal)
		case "-":
			return cIntFact(a.intVal - b.intVal)
		case "*":
			return cIntFact(a.intVal * b.intVal)
		case "/":
			if b.intVal == 0 {
				return cBotFact()
			}

			return cIntFact(a.intVal / b.intVal)
		case "%":
			if b.intVal == 0 {
				return cBotFact()
			}

			return cIntFact(a.intVal % b.intVal)
		case "==":
			return cBoolFact(a.intVal == b.intVal)
		case "!=":
			return cBoolFact(a.intVal != b.intVal)
		case "<":
			return cBoolFact(a.intVal < b.intVal)
		case "<=":
			return cBoolFact(a.intVal <= b.intVal)
		case ">":
			return cBoolFact(a.intVal > b.intVal)
		case ">=":
			return cBoolFact(a.intVal >= b.intVal)
		}
	}

	if a.kind == cBool && b.kind == cBool {
		switch op {
		case "==":
			return cBoolFact(a.boolVal == b.boolVal)
		case "!=":
			return cBoolFact(a.boolVal != b.boolVal)
		case "&&":
			return cBoolFact(a.boolVal && b.boolVal)
		case "||":
			return cBoolFact(a.boolVal || b.boolVal)
		}
	}

	return cBotFact()
}

func foldUnaryConst(op string, v constFact) constFact {
	if v.kind == cInt && op == "-" {
		return cIntFact(-v.intVal)
	}

	if v.kind == cBool && (op == "!" || op == "not") {
		return cBoolFact(!v.boolVal)
	}

	return cBotFact()
}

// narrowOnCond examines the test in `if cond { then } else { else }` and
// returns the (then, else) abstract states refined with whatever facts the
// branch establishes. The supported shapes are:
//
//	p == nil  -> then: p is NIL, else: p is NON_NIL
//	p != nil  -> then: p is NON_NIL, else: p is NIL
//
// Anything else falls back to the unrefined incoming state.
func narrowOnCond(cond ast.Node, st *dfState) (thenSt, elseSt *dfState) {
	thenSt = st.clone()
	elseSt = st.clone()

	bin, ok := cond.(*ast.BinExpr)
	if !ok {
		return
	}

	// Nil narrowing for `p == nil` / `p != nil`.
	if bin.Op == "==" || bin.Op == "!=" {
		var name string

		if id, ok := bin.Left.(*ast.Identifier); ok {
			if _, isNil := bin.Right.(*ast.NilLit); isNil {
				name = id.Name
			}
		} else if id, ok := bin.Right.(*ast.Identifier); ok {
			if _, isNil := bin.Left.(*ast.NilLit); isNil {
				name = id.Name
			}
		}

		if name != "" {
			if bin.Op == "==" {
				thenSt.nil[name] = nilIsNil
				elseSt.nil[name] = nilNonNil
			} else {
				thenSt.nil[name] = nilNonNil
				elseSt.nil[name] = nilIsNil
			}

			return
		}
	}

	// Integer-interval narrowing: `x op c` (or `c op x`) where one side is
	// an identifier we track and the other a constant.
	switch bin.Op {
	case "<", "<=", ">", ">=", "==", "!=":
	default:
		return
	}

	var (
		name string
		c    int64
		op   string
	)

	if id, ok := bin.Left.(*ast.Identifier); ok {
		iv := constIntOf(bin.Right)
		if !iv.set {
			// Non-constant RHS: still record a flow-sensitive
			// upper-bound proof for `i < expr` / `i <= expr`,
			// even though we can't narrow the interval.  Matches
			// the user's intent ("I wrote an upper-bound guard")
			// for the -Wunchecked-index pedantic check.  The
			// classic pattern is `if i < len(arr): arr[i]`,
			// where `len(arr)` is a call and not constIntOf-able.
			if bin.Op == "<" || bin.Op == "<=" {
				thenSt.boundsChecked[id.Name] = true
			}

			return
		}

		name = id.Name
		c = iv.lo
		op = bin.Op
	} else if id, ok := bin.Right.(*ast.Identifier); ok {
		iv := constIntOf(bin.Left)
		if !iv.set {
			// Non-constant LHS: mirror image of the above.
			// `expr > i` / `expr >= i` imply an upper bound on i.
			if bin.Op == ">" || bin.Op == ">=" {
				thenSt.boundsChecked[id.Name] = true
			}

			return
		}

		name = id.Name
		c = iv.lo
		op = flipOp(bin.Op)
	}

	if name == "" {
		return
	}

	cur, ok := st.intv[name]
	if !ok || !cur.set {
		return
	}

	thenIv, elseIv := narrowIntervalCmp(cur, op, c)
	if thenIv.set {
		thenSt.intv[name] = thenIv
	}

	if elseIv.set {
		elseSt.intv[name] = elseIv
	}

	// notZero narrowing: x != 0 -> then proves non-zero; x == 0 -> else
	// proves non-zero.  Also covers strict-sign guards (x > 0, x < 0
	// imply non-zero on the then side) and !=0-style relations against
	// a non-zero constant (x != c with c != 0 doesn't imply x != 0, so
	// only handle c == 0 here).  Used by dfCheckUncheckedDiv to silence
	// guarded divisions where the residual interval can't represent the
	// punctured range.
	switch op {
	case "!=":
		if c == 0 {
			thenSt.notZero[name] = true
		}
	case "==":
		if c == 0 {
			elseSt.notZero[name] = true
		}
	case ">":
		if c >= 0 {
			thenSt.notZero[name] = true
		}
	case ">=":
		if c > 0 {
			thenSt.notZero[name] = true
		}
	case "<":
		if c <= 0 {
			thenSt.notZero[name] = true
		}
	case "<=":
		if c < 0 {
			thenSt.notZero[name] = true
		}
	}

	// boundsChecked narrowing: `x < c` or `x <= c` with c > 0 records
	// that x has been compared against an upper bound, so the
	// pedantic -Wunchecked-index check stays silent for `arr[x]`
	// inside the then branch.  We don't verify c <= len(arr) -- the
	// goal is "did the user write an upper-bound guard," not "is the
	// guard sound" (since c could be a const < len(arr) too).
	switch op {
	case "<", "<=":
		if c > 0 {
			thenSt.boundsChecked[name] = true
		}
	}

	return
}

// constIntOf returns the IntLit value of an expression as a single-point
// interval, or unset if it's not a literal int.
func constIntOf(n ast.Node) interval {
	if il, ok := n.(*ast.IntLit); ok {
		return singletonIv(il.Value)
	}

	return interval{}
}

// flipOp swaps a comparison operator so that `c op x` becomes `x op' c`.
func flipOp(op string) string {
	switch op {
	case "<":
		return ">"
	case "<=":
		return ">="
	case ">":
		return "<"
	case ">=":
		return "<="
	}

	return op
}

// narrowIntervalCmp refines the interval of `x` given the branch
// `x op c` is taken (then) or not taken (else). Returns the refined
// (then, else) intervals; an unset interval means "no information".
func narrowIntervalCmp(x interval, op string, c int64) (thenIv, elseIv interval) {
	if !x.set {
		return interval{}, interval{}
	}

	// c\pm1 underflow / overflow at i64::MIN / i64::MAX would feed bogus
	// bounds into clipInterval and produce spurious "impossible range"
	// warnings.  Clamp instead -- at the boundary the resulting clip is
	// trivially the original interval (or empty), which is semantically
	// the correct narrowing.
	cMinus1, okM := subOverflow(c, 1)
	if !okM {
		cMinus1 = c
	}

	cPlus1, okP := addOverflow(c, 1)
	if !okP {
		cPlus1 = c
	}

	switch op {
	case "<":
		thenIv = clipInterval(x, x.lo, cMinus1)
		elseIv = clipInterval(x, c, x.hi)
	case "<=":
		thenIv = clipInterval(x, x.lo, c)
		elseIv = clipInterval(x, cPlus1, x.hi)
	case ">":
		thenIv = clipInterval(x, cPlus1, x.hi)
		elseIv = clipInterval(x, x.lo, c)
	case ">=":
		thenIv = clipInterval(x, c, x.hi)
		elseIv = clipInterval(x, x.lo, cMinus1)
	case "==":
		thenIv = clipInterval(x, c, c)
		elseIv = x // can't refine the else side from a single point
	case "!=":
		thenIv = x
		elseIv = clipInterval(x, c, c)
	}

	return
}

// checkImpossibleCmp emits a warning when an integer comparison evaluates
// to a constant under the current interval state. Catches:
//
//	let x u8 = ... ; if x < 0:        // u8 is in [0,255], always false
//	if x >= 5 && x < 5:               // narrowed RHS is empty
//	if x == 100:  /* x \in [0, 50] */   // never holds
//
// For `A && B` the right conjunct is checked under the state where A is
// known to hold, so a contradiction with the narrowed state (the second
// example above) gets caught.

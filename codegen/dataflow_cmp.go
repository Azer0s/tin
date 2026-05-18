package codegen

import (
	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) checkImpossibleCmp(cond ast.Node, st *dfState) {
	cg.checkImpossibleCmpIn(cond, st)
}

func (cg *CodeGen) checkImpossibleCmpIn(cond ast.Node, st *dfState) *dfState {
	if bin, ok := cond.(*ast.BinExpr); ok {
		switch bin.Op {
		case "&&":
			leftThen := cg.checkImpossibleCmpIn(bin.Left, st)
			if leftThen == nil {
				leftThen = st
			}

			cg.checkImpossibleCmpIn(bin.Right, leftThen)

			return leftThen
		case "||":
			cg.checkImpossibleCmpIn(bin.Left, st)
			cg.checkImpossibleCmpIn(bin.Right, st)

			return st
		}
	}

	cg.checkSingleCmp(cond, st)

	thenSt, _ := narrowOnCond(cond, st)

	return thenSt
}

// checkSingleCmp handles a non-compound comparison. Emits the
// impossible-range warning if the result is determinate.
func (cg *CodeGen) checkSingleCmp(cond ast.Node, st *dfState) {
	bin, ok := cond.(*ast.BinExpr)
	if !ok {
		return
	}

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
			return
		}

		name = id.Name
		c = iv.lo
		op = bin.Op
	} else if id, ok := bin.Right.(*ast.Identifier); ok {
		iv := constIntOf(bin.Left)
		if !iv.set {
			return
		}

		name = id.Name
		c = iv.lo
		op = flipOp(bin.Op)
	} else {
		return
	}

	cur, ok := st.intv[name]
	if !ok || !cur.set {
		return
	}

	if always, val := cmpAlwaysHolds(cur, op, c); always {
		cg.warn(DiagImpossibleRange, bin.Pos(),
			"comparison %q is always %v: %s \\in [%d, %d]",
			op, val, name, cur.lo, cur.hi)
	}
}

// cmpAlwaysHolds reports whether `x op c` evaluates to a constant given
// x's interval. Returns (true, value) when fully determined.
func cmpAlwaysHolds(x interval, op string, c int64) (always, value bool) {
	switch op {
	case "<":
		if x.hi < c {
			return true, true
		}

		if x.lo >= c {
			return true, false
		}
	case "<=":
		if x.hi <= c {
			return true, true
		}

		if x.lo > c {
			return true, false
		}
	case ">":
		if x.lo > c {
			return true, true
		}

		if x.hi <= c {
			return true, false
		}
	case ">=":
		if x.lo >= c {
			return true, true
		}

		if x.hi < c {
			return true, false
		}
	case "==":
		if x.lo == c && x.hi == c {
			return true, true
		}

		if c < x.lo || c > x.hi {
			return true, false
		}
	case "!=":
		if c < x.lo || c > x.hi {
			return true, true
		}

		if x.lo == c && x.hi == c {
			return true, false
		}
	}

	return false, false
}

// clipInterval returns x \cap [lo, hi], or unset if the result is empty.
// When x has a non-trivial stride s > 1, the result is shrunk so that
// new lo and hi remain at exact multiples-of-s offsets from x.lo (Reps
// et al. 2006: clipping a strided interval to a bound). This keeps
// `if epoch < 5000` narrowing of `for epoch = 0; ...; epoch += 500`
// produce {0, 4500, 500} rather than {0, 4999} stride-1, so the
// downstream `epoch % 500 == 0` fold can prove always-true.
func clipInterval(x interval, lo, hi int64) interval {
	if !x.set {
		return interval{}
	}

	if lo < x.lo {
		lo = x.lo
	}

	if hi > x.hi {
		hi = x.hi
	}

	if lo > hi {
		return interval{}
	}

	s := x.stride
	if s <= 1 {
		if lo == hi {
			return singletonIv(lo)
		}

		stride := s
		if stride <= 0 {
			stride = 1
		}

		return interval{set: true, lo: lo, hi: hi, stride: stride}
	}

	if rem := (lo - x.lo) % s; rem != 0 {
		lo += s - rem
	}

	if rem := (hi - x.lo) % s; rem != 0 {
		hi -= rem
	}

	if lo > hi {
		return interval{}
	}

	if lo == hi {
		return singletonIv(lo)
	}

	return interval{set: true, lo: lo, hi: hi, stride: s}
}

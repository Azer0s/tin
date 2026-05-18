package codegen

import (
	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) runDataflow(prog *ast.Program) {
	cg.computeManualAllocSummaries(prog)

	for _, n := range prog.Stmts {
		switch v := n.(type) {
		case *ast.FuncDecl:
			cg.dfAnalyzeFunc(v)
		case *ast.StructDecl:
			for _, m := range v.Methods {
				cg.dfAnalyzeFunc(m)
			}
		}
	}
}

// computeManualAllocSummaries walks every top-level fn (and struct
// method) twice:
//
//  1. Direct pass: for each fn F:
//     - paramFrees[F] gets every param index i such that the body
//     contains `mem::free(p_i)` directly.
//     - returnsAlloc[F] is true when any return value is a direct
//     call to mem::malloc / calloc / realloc / alloc.
//
//  2. Fixpoint iteration: walk all calls.  If callee C has
//     paramFrees[C] containing index j, and the call's arg at
//     position j is the caller's own param p_k, propagate by
//     adding k to paramFrees[F].  Similarly, when `return f(...)`
//     and f is in returnsAlloc, set returnsAlloc[F] = true.
//     Iterate until no changes.
//
// Conservative: parameters that flow through complex expressions
// (struct field, address-of, etc.) are not tracked.  The
// dataflow's call-site handler treats such bindings as Escaped --
// which is the existing fallback -- so the summary just refines
// the common direct-pass shape.
func (cg *CodeGen) computeManualAllocSummaries(prog *ast.Program) {
	if cg.paramFrees == nil {
		cg.paramFrees = map[string]map[int]bool{}
	}

	if cg.returnsAlloc == nil {
		cg.returnsAlloc = map[string]bool{}
	}

	type fnEntry struct {
		decl       *ast.FuncDecl
		paramIndex map[string]int // param name -> position
	}

	fns := map[string]fnEntry{}

	addFn := func(fd *ast.FuncDecl) {
		if fd == nil || fd.Body == nil || fd.IsExtern != "" || fd.IsVirtual {
			return
		}

		idx := map[string]int{}
		pos := 0

		for _, p := range fd.Params {
			if p.IsVarArgs {
				continue
			}

			if p.Name != "" && p.Name != "_" {
				idx[p.Name] = pos
			}

			pos++
		}

		fns[fd.Name] = fnEntry{decl: fd, paramIndex: idx}
	}

	for _, n := range prog.Stmts {
		switch v := n.(type) {
		case *ast.FuncDecl:
			addFn(v)
		case *ast.StructDecl:
			for _, m := range v.Methods {
				addFn(m)
			}
		}
	}

	// Phase 1: direct summary.  Seed an empty entry for every
	// analyzed fn so the dataflow's call-site handler can tell
	// "analyzed callee, frees nothing" from "unanalyzed callee
	// (extern / indirect)".  Without the seed, both cases looked
	// identical (paramFrees lookup returned nil) and a Live alloc
	// passed to a read-only helper got marked Escaped, silently
	// suppressing the leak warning.
	for name := range fns {
		if cg.paramFrees[name] == nil {
			cg.paramFrees[name] = map[int]bool{}
		}
	}

	for name, entry := range fns {
		walkAST(entry.decl.Body, func(n ast.Node) {
			switch s := n.(type) {
			case *ast.ExprStmt:
				if argName, ok := isManualFreeCall(s.Expr); ok {
					if i, here := entry.paramIndex[argName]; here {
						cg.paramFrees[name][i] = true
					}
				}
			case *ast.ReturnStmt:
				if s.Value != nil && isManualAllocCall(s.Value) {
					cg.returnsAlloc[name] = true
				}
			}
		})
	}

	// Phase 2: fixpoint over call edges.
	for changed := true; changed; {
		changed = false

		for name, entry := range fns {
			walkAST(entry.decl.Body, func(n ast.Node) {
				switch s := n.(type) {
				case *ast.CallExpr:
					id, ok := s.Func.(*ast.Identifier)
					if !ok {
						return
					}

					calleeFrees, hasFrees := cg.paramFrees[id.Name]
					if !hasFrees {
						return
					}

					for i, arg := range s.Args {
						if !calleeFrees[i] {
							continue
						}

						argID, ok2 := arg.(*ast.Identifier)
						if !ok2 {
							continue
						}

						pidx, here := entry.paramIndex[argID.Name]
						if !here {
							continue
						}

						if cg.paramFrees[name] == nil {
							cg.paramFrees[name] = map[int]bool{}
						}

						if !cg.paramFrees[name][pidx] {
							cg.paramFrees[name][pidx] = true
							changed = true
						}
					}
				case *ast.ReturnStmt:
					if s.Value == nil {
						return
					}

					call, ok := s.Value.(*ast.CallExpr)
					if !ok {
						return
					}

					id, ok2 := call.Func.(*ast.Identifier)
					if !ok2 {
						return
					}

					if cg.returnsAlloc[id.Name] && !cg.returnsAlloc[name] {
						cg.returnsAlloc[name] = true
						changed = true
					}
				}
			})
		}
	}
}

func (cg *CodeGen) dfAnalyzeFunc(fn *ast.FuncDecl) {
	if fn.Body == nil || fn.IsExtern != "" || fn.IsVirtual {
		return
	}

	// Stash the current fn name for the Andersen lookup in
	// dfCheckExpr -- ptVarFor keys on the (fn, var) pair, and the
	// dataflow walker doesn't otherwise know which function it is
	// in.  Cleared at exit so a later subagent / monomorphization
	// step doesn't accidentally key into the wrong fn.
	prevDfFn := cg.dfCurFnName
	cg.dfCurFnName = fn.Name

	defer func() { cg.dfCurFnName = prevDfFn }()

	// Entry state: parameters are BOTTOM (could be anything from caller).
	// Their interval is seeded from the declared type, so a `u8` parameter
	// starts in [0, 255] - that's how `u8 < 0` falls out as always-false
	// without any further analysis.
	st := newDFState()

	for _, p := range fn.Params {
		if p.Name == "" || p.Name == "_" {
			continue
		}

		st.nil[p.Name] = nilBottom
		st.cnst[p.Name] = cBotFact()

		if iv := intervalForTinType(p.Type); iv.set {
			st.intv[p.Name] = iv
		}

		if p.Type != nil {
			st.types[p.Name] = p.Type
		}
	}

	cg.manualAllocSites = map[string]ast.Pos{}

	finalSt := cg.dfWalkAny(fn.Body, st)
	cg.dfCheckManualAllocLeaks(finalSt)
}

// intervalForTinType returns the integer range that a variable of type t
// is guaranteed to fall in. Returns the unset interval for non-integer or
// 64-bit-unsigned types (the latter doesn't fit in a Go int64).
func intervalForTinType(t ast.TypeExpr) interval {
	st, ok := t.(*ast.SimpleType)
	if !ok {
		return interval{}
	}

	switch st.Name {
	case "i8":
		return rangeIv(-128, 127)
	case "u8", "byte", "char":
		return rangeIv(0, 255)
	case "i16":
		return rangeIv(-32768, 32767)
	case "u16":
		return rangeIv(0, 65535)
	case "i32":
		return rangeIv(-2147483648, 2147483647)
	case "u32":
		return rangeIv(0, 4294967295)
	case "i64":
		return rangeIv(-9223372036854775808, 9223372036854775807)
	}

	return interval{}
}

// dfIntervalForBinding chooses the interval for `let <name> [type] = value`.
// Prefers the value's interval (it's a tighter bound), falling back to the
// declared type's range when the value's interval is unknown.
func (cg *CodeGen) dfIntervalForBinding(declType ast.TypeExpr, value ast.Node, st *dfState) interval {
	if iv := cg.intervalOf(value, st); iv.set {
		return iv
	}

	return intervalForTinType(declType)
}

// intervalOf returns the abstract interval of expr under st.
func (cg *CodeGen) intervalOf(expr ast.Node, st *dfState) interval {
	if expr == nil {
		return interval{}
	}

	switch e := expr.(type) {
	case *ast.IntLit:
		return singletonIv(e.Value)
	case *ast.Identifier:
		if v, ok := st.intv[e.Name]; ok {
			return v
		}
	case *ast.UnaryExpr:
		if e.Op == "-" {
			if iv := cg.intervalOf(e.Expr, st); iv.set {
				return interval{set: true, lo: -iv.hi, hi: -iv.lo, stride: iv.effectiveStride()}
			}
		}
	case *ast.BinExpr:
		l := cg.intervalOf(e.Left, st)
		r := cg.intervalOf(e.Right, st)

		return intervalArith(e.Op, l, r)
	}

	return interval{}
}

// intervalArith computes the strided interval of `a op b`. Handles `+`,
// `-`, `*` when at least one side narrows to useful bounds; returns the
// unset interval otherwise. Stride is preserved through shifts (single-
// constant `+`/`-`) and scaled through multiplications by a constant.
//
// Overflow at i64 boundaries collapses the result to the empty/unset
// interval rather than wrapping silently -- the wrapped bounds would
// drive narrowIntervalCmp to false-positive "impossible range" warnings.
func intervalArith(op string, a, b interval) interval {
	if !a.set || !b.set {
		return interval{}
	}

	aSingle := a.lo == a.hi
	bSingle := b.lo == b.hi

	switch op {
	case "+":
		if bSingle {
			return shiftIv(a, b.lo)
		}

		if aSingle {
			return shiftIv(b, a.lo)
		}

		lo, ok1 := addOverflow(a.lo, b.lo)
		hi, ok2 := addOverflow(a.hi, b.hi)

		if !ok1 || !ok2 {
			return interval{}
		}

		s := gcd64(a.effectiveStride(), b.effectiveStride())
		if s == 0 {
			s = 1
		}

		if (hi-lo)%s != 0 {
			s = 1
		}

		return interval{set: true, lo: lo, hi: hi, stride: s}
	case "-":
		if bSingle {
			neg, ok := subOverflow(0, b.lo)
			if !ok {
				return interval{}
			}

			return shiftIv(a, neg)
		}

		lo, ok1 := subOverflow(a.lo, b.hi)
		hi, ok2 := subOverflow(a.hi, b.lo)

		if !ok1 || !ok2 {
			return interval{}
		}

		s := gcd64(a.effectiveStride(), b.effectiveStride())
		if s == 0 {
			s = 1
		}

		if (hi-lo)%s != 0 {
			s = 1
		}

		return interval{set: true, lo: lo, hi: hi, stride: s}
	case "*":
		if bSingle {
			return scaleIv(a, b.lo)
		}

		if aSingle {
			return scaleIv(b, a.lo)
		}
	}

	return interval{}
}

// addOverflow returns a+b and a flag indicating whether the result fits
// in int64.  Used by intervalArith to bottom-out wraparound.
func addOverflow(a, b int64) (int64, bool) {
	r := a + b
	if (b > 0 && r < a) || (b < 0 && r > a) {
		return 0, false
	}

	return r, true
}

// subOverflow returns a-b with a fits-in-int64 flag.
func subOverflow(a, b int64) (int64, bool) {
	r := a - b
	if (b < 0 && r < a) || (b > 0 && r > a) {
		return 0, false
	}

	return r, true
}

// scaleIv multiplies a strided interval by a constant. Stride scales
// with the constant; bounds flip when the constant is negative.
func scaleIv(iv interval, k int64) interval {
	if !iv.set {
		return interval{}
	}

	if k == 0 {
		return singletonIv(0)
	}

	lo := iv.lo * k
	hi := iv.hi * k

	if k < 0 {
		lo, hi = hi, lo
	}

	if lo == hi {
		return singletonIv(lo)
	}

	s := iv.effectiveStride() * k
	if s < 0 {
		s = -s
	}

	if s == 0 {
		s = 1
	}

	return interval{set: true, lo: lo, hi: hi, stride: s}
}

// dfWalkAny dispatches a node to dfWalkBlock for *Block or dfWalkStmt for
// any other statement-shaped node. Used at function-body roots that are
// typed as ast.Node so they can carry expressions or where-clauses.

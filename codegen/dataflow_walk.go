package codegen

import (
	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) dfWalkAny(n ast.Node, st *dfState) *dfState {
	if n == nil {
		return st
	}

	if b, ok := n.(*ast.Block); ok {
		return cg.dfWalkBlock(b, st)
	}

	return cg.dfWalkStmt(n, st)
}

// dfWalkBlock threads state through a block's statements. Returns the
// outgoing state, or a dead state if every path terminated.
func (cg *CodeGen) dfWalkBlock(b *ast.Block, st *dfState) *dfState {
	if b == nil {
		return st
	}

	for _, s := range b.Stmts {
		if st == nil || st.dead {
			return st
		}

		st = cg.dfWalkStmt(s, st)
	}

	return st
}

func (cg *CodeGen) dfWalkStmt(stmt ast.Node, st *dfState) *dfState {
	switch v := stmt.(type) {
	case *ast.TupleDestructDecl:
		// Mark a `t[k]` RHS as ok-destructured so the
		// -Wunchecked-index pedantic check stays silent for the
		// canonical `let (v, ok) = t[k]` shape.  The IndexExpr's
		// visit consults cg.dfSkipIndexCheck.
		if ie, ok := v.Value.(*ast.IndexExpr); ok {
			if cg.dfSkipIndexCheck == nil {
				cg.dfSkipIndexCheck = map[*ast.IndexExpr]bool{}
			}

			cg.dfSkipIndexCheck[ie] = true
		}

		cg.dfCheckExpr(v.Value, st)

		// Seed types for the bound names.  Without type inference
		// from the destructured tuple we leave them untyped (no
		// downstream warning is silenced or fired incorrectly).
		st = cg.dfApplyEscapes(v.Value, st)
		st = st.clone()

		for _, name := range v.Names {
			if name == "" || name == "_" {
				continue
			}

			st.nil[name] = nilBottom
			st.cnst[name] = cBotFact()
			delete(st.uninit, name)
		}

		return st

	case *ast.VarDecl:
		cg.dfCheckExpr(v.Value, st)

		// Compute the binding's facts from the pre-escape state (the
		// RHS expression evaluates against the values held going in),
		// then bottom-ise any other names whose address escaped through
		// the RHS. Target gets overwritten next so escape on it is a
		// no-op.
		newNil := cg.dfNilOf(v.Value, st)
		newCnst := cg.dfEval(v.Value, st)
		newIv := cg.dfIntervalForBinding(v.Type, v.Value, st)
		newFp := dfFoldFloat(v.Value, st)

		st = cg.dfApplyEscapes(v.Value, st)
		st = st.clone()
		st.nil[v.Name] = newNil
		st.cnst[v.Name] = newCnst

		if newIv.set {
			st.intv[v.Name] = newIv
		}

		if newFp != nil {
			st.floats[v.Name] = newFp
		}
		// Track the declared type so dfCheckUncheckedIndex can tell
		// built-in arrays apart from user `::index` overloads.  When
		// the let binding is unannotated, infer from the RHS shape
		// where possible (array literals, len() calls produce known
		// types).
		if v.Name != "" && v.Name != "_" {
			if v.Type != nil {
				st.types[v.Name] = v.Type
			} else if rhsType := cg.dfInferTypeFromRHS(v.Value); rhsType != nil {
				st.types[v.Name] = rhsType
			}
			// Manual-alloc tracking: `let p = mem::malloc(...)`
			// (or calloc/realloc/alloc) starts a Live binding
			// that must be passed to `mem::free` before scope
			// exit.  ALSO: `let p = make()` where `make` was
			// proven by the interprocedural pre-pass to return
			// a freshly-allocated block starts a Live binding
			// too -- the caller now owns the allocation and
			// must free it.  Reassignment to anything else
			// clears the entry.
			if isManualAllocCall(v.Value) || cg.callReturnsAlloc(v.Value) {
				st.manualAlloc[v.Name] = manualAllocLive

				if cg.manualAllocSites == nil {
					cg.manualAllocSites = map[string]ast.Pos{}
				}

				cg.manualAllocSites[v.Name] = v.Pos()
			} else {
				delete(st.manualAlloc, v.Name)
			}
		}
		// Track the uninit-by-decl shape `let x T` (no initializer) so a
		// later read fires DiagUseBeforeAssign. `let x = expr` (or `let
		// x T = expr`) is initialized.
		if v.Value == nil && v.Name != "" && v.Name != "_" {
			st.uninit[v.Name] = true
		} else {
			delete(st.uninit, v.Name)
		}

		return st

	case *ast.AssignStmt:
		cg.dfCheckExpr(v.Value, st)

		newNil := cg.dfNilOf(v.Value, st)
		newCnst := cg.dfEval(v.Value, st)
		newIv := cg.intervalOf(v.Value, st)
		newFp := dfFoldFloat(v.Value, st)

		st = cg.dfApplyEscapes(v.Value, st)

		if id, ok := v.Target.(*ast.Identifier); ok {
			st = st.clone()
			st.nil[id.Name] = newNil
			st.cnst[id.Name] = newCnst
			delete(st.freed, id.Name) // reassign clears freed state
			delete(st.uninit, id.Name)
			delete(st.notZero, id.Name)
			delete(st.boundsChecked, id.Name) // reassign clears any prior non-zero proof
			// Manual-alloc reassign rules:
			//   - prior Live + RHS is mem::realloc(<same id>, ...):
			//       legal swap (realloc consumes the input
			//       pointer internally).  Stay Live, no warn.
			//   - prior Live + RHS is any OTHER alloc / call that
			//       returns alloc:  the prior block is dropped --
			//       warn leak, then transition to a fresh Live
			//       binding for the new allocation.
			//   - prior Live + RHS is a borrow (other identifier,
			//       field load, etc.):  same drop-warn, then
			//       clear the entry (no new alloc to track).
			//   - prior absent + RHS is an alloc: standard Live
			//       initialisation.
			//   - prior absent + RHS is anything else: no-op.
			rhsIsAlloc := isManualAllocCall(v.Value) || cg.callReturnsAlloc(v.Value)
			rhsIsReallocSelf := isReallocOfSelf(v.Value, id.Name)
			priorLive := st.manualAlloc[id.Name] == manualAllocLive

			switch {
			case priorLive && rhsIsReallocSelf:
				// Legal swap; nothing to do.
			case priorLive && rhsIsAlloc:
				cg.warn(DiagManualAllocLeak, v.Pos(),
					"reassigning %q drops the previous mem::malloc/calloc/realloc result without `mem::free`; add the free before the reassignment, or transfer ownership first",
					id.Name)

				st.manualAlloc[id.Name] = manualAllocLive
				cg.manualAllocSites[id.Name] = v.Pos()
			case priorLive:
				cg.warn(DiagManualAllocLeak, v.Pos(),
					"reassigning %q drops the previous mem::malloc/calloc/realloc result without `mem::free`; add the free before the reassignment, or transfer ownership first",
					id.Name)

				delete(st.manualAlloc, id.Name)
			case rhsIsAlloc:
				st.manualAlloc[id.Name] = manualAllocLive
				cg.manualAllocSites[id.Name] = v.Pos()
			default:
				delete(st.manualAlloc, id.Name)
			}

			if newIv.set {
				st.intv[id.Name] = newIv
			} else {
				delete(st.intv, id.Name)
			}

			if newFp != nil {
				st.floats[id.Name] = newFp
			} else {
				delete(st.floats, id.Name)
			}
		}

		return st

	case *ast.AugAssignStmt:
		cg.dfCheckExpr(v.Value, st)
		st = cg.dfApplyEscapes(v.Value, st)

		if id, ok := v.Target.(*ast.Identifier); ok {
			// `x += y` reads x before writing, so uninit fires here.
			if st.uninit[id.Name] {
				cg.warn(DiagUseBeforeAssign, v.Pos(),
					"%q is read by %q before being explicitly assigned",
					id.Name, v.Op)
			}

			st = st.clone()
			st.nil[id.Name] = nilBottom
			delete(st.floats, id.Name)
			delete(st.uninit, id.Name)
			delete(st.notZero, id.Name)
			delete(st.boundsChecked, id.Name)
			// `x += k` and `x -= k` for constant k shift the strided
			// interval. Preserving stride here is what lets the loop
			// fixpoint widen `for epoch = 0; ...; epoch += 500` to a
			// stride-500 interval rather than collapsing to BOTTOM.
			cur, hasIv := st.intv[id.Name]
			delta, isInt := dfConstInt(cg.dfEval(v.Value, st))

			switch {
			case isInt && (v.Op == "+=" || v.Op == "-=") && hasIv && cur.set:
				d := delta
				if v.Op == "-=" {
					d = -d
				}

				st.intv[id.Name] = shiftIv(cur, d)
			default:
				delete(st.intv, id.Name)
			}

			st.cnst[id.Name] = cBotFact()
		}

		return st

	case *ast.PostfixStmt:
		// `x++` / `x--` mutates the bound name. Treat as a strided shift
		// by \pm1 so a loop counter's interval widens through the fixpoint
		// instead of collapsing -- e.g. `for i = 0; i < 5; i++` ends up
		// with i \in [0, 5] stride 1 at the join.
		cg.dfCheckExpr(v.Expr, st)

		if id, ok := v.Expr.(*ast.Identifier); ok {
			if st.uninit[id.Name] {
				cg.warn(DiagUseBeforeAssign, v.Pos(),
					"%q is read by %q before being explicitly assigned",
					id.Name, v.Op)
			}

			st = st.clone()
			st.nil[id.Name] = nilBottom
			delete(st.floats, id.Name)
			delete(st.uninit, id.Name)
			delete(st.notZero, id.Name)
			delete(st.boundsChecked, id.Name)

			var d int64

			switch v.Op {
			case "++":
				d = 1
			case "--":
				d = -1
			}

			if cur, ok := st.intv[id.Name]; ok && cur.set && d != 0 {
				st.intv[id.Name] = shiftIv(cur, d)
			} else {
				delete(st.intv, id.Name)
			}

			st.cnst[id.Name] = cBotFact()
		}

		return st

	case *ast.ExprStmt:
		if name, ok := isDeinitCall(v.Expr); ok {
			st = st.clone()

			if st.freed[name] {
				cg.warn(DiagDoubleDeinit, v.Expr.Pos(),
					"deinit on %q which has already been deinitialized on this path", name)
			}

			st.freed[name] = true

			return st
		}
		// Manual-alloc tracking: detect `mem::free(p)` at the
		// statement level (the common shape).  Transitions p
		// from Live to Freed; warns on a re-free of an already
		// Freed binding.
		if name, ok := isManualFreeCall(v.Expr); ok {
			st = st.clone()

			if st.manualAlloc[name] == manualAllocFreed {
				cg.warn(DiagManualDoubleFree, v.Expr.Pos(),
					"mem::free(%q) but %q was already freed on this path", name, name)
			}

			st.manualAlloc[name] = manualAllocFreed

			return st
		}

		cg.dfCheckExpr(v.Expr, st)

		return cg.dfApplyEscapes(v.Expr, st)

	case *ast.EchoStmt:
		cg.dfCheckExpr(v.Value, st)

		return cg.dfApplyEscapes(v.Value, st)

	case *ast.ReturnStmt:
		if v.Value != nil {
			cg.dfCheckExpr(v.Value, st)
		}

		// Manual-alloc ownership transfer: returning a Live binding
		// transfers ownership to the caller; mark Escaped so the
		// scope-exit leak check stays silent.
		st = st.clone()
		dfEscapeManualAllocFromExpr(v.Value, st)
		// Early-return leak check: any binding still Live AND not
		// in the returned expression leaks via this exit point.
		// Without this the function-end leak check (which runs on
		// the joined non-dead state) misses leaks that escape via
		// early returns -- the dead branch's state is discarded.
		cg.dfCheckManualAllocLeaks(st)

		dead := st.clone()
		dead.dead = true

		return dead

	case *ast.IfStmt:
		return cg.dfWalkIf(v, st)

	case *ast.ForStmt:
		// For-in loops have nil Init/Cond/Post; piping them through
		// dfWalkLoop walks the body once with the entry state and the
		// induction variable is never registered, so any prior fact
		// about a name shadowed by VarName leaks into the body.  Drop
		// such facts before walking so the body sees a fresh slot for
		// the loop variable.
		if v.Kind == ast.ForIn {
			loopSt := st
			if v.VarName != "" {
				loopSt = st.clone()
				delete(loopSt.nil, v.VarName)
				delete(loopSt.cnst, v.VarName)
				delete(loopSt.intv, v.VarName)
				delete(loopSt.floats, v.VarName)
				delete(loopSt.uninit, v.VarName)
				delete(loopSt.freed, v.VarName)
				delete(loopSt.notZero, v.VarName)
				delete(loopSt.boundsChecked, v.VarName)
				// Range-form iteration (`for i in lo..hi`) makes the
				// loop variable an integer index bounded above by hi
				// (a typical bounds-check shape).  Mark it
				// boundsChecked so `arr[i]` inside the body skips the
				// -Wunchecked-index pedantic warning when the
				// surrounding code wrote the obvious `for i in
				// 0..len(arr): arr[i]` pattern.  We don't verify hi
				// matches the indexed array -- the warning is about
				// intent ("you wrote an upper bound"), not soundness.
				if bin, ok := v.Iter.(*ast.BinExpr); ok && bin.Op == ".." {
					loopSt.boundsChecked[v.VarName] = true
				}
			}
			// Evaluate the iterable for side-effect facts (e.g. nil
			// guard on the iter expression).
			if v.Iter != nil {
				cg.dfCheckExpr(v.Iter, loopSt)
			}

			return cg.dfWalkLoop(nil, nil, nil, v.Body, loopSt)
		}

		return cg.dfWalkLoop(v.Init, v.Cond, v.Post, v.Body, st)

	case *ast.MatchStmt:
		return cg.dfWalkMatch(v, st)

	case *ast.Block:
		return cg.dfWalkBlock(v, st)

	case *ast.DeferStmt:
		// Defers run on every exit; for the analysis treat them as a
		// side-effecting statement at the defer site.
		cg.dfCheckExpr(v.Call, st)

		return st

	case *ast.BreakStmt:
		dead := st.clone()
		dead.dead = true

		return dead

	default:
		return st
	}
}

// dfWalkIf processes an `if cond { then } elif (...) ... else { else }`.
// It folds the condition under the current state and emits a flow-sensitive
// bool-analysis warning when it's known. Branch-narrowing refines the
// per-arm state for the common `p == nil` / `p != nil` shape.
func (cg *CodeGen) dfWalkIf(s *ast.IfStmt, st *dfState) *dfState {
	cg.dfCheckExpr(s.Cond, st)

	cg.checkImpossibleCmp(s.Cond, st)

	// Flow-sensitive constant condition warning.
	if v := cg.dfEval(s.Cond, st); v.kind == cBool {
		// The non-flow-sensitive bool-analysis already fires on conditions
		// that fold without state context. Only emit here when the condition
		// folds *because* of flow facts that the AST-level fold misses.
		base := cg.tryFoldExpr(s.Cond)
		if base.kind != foldBool {
			cg.warn(DiagBoolAnalysis, s.Pos(),
				"if condition is always %v under current control flow", v.boolVal)
		}
	}

	thenSt, elseSt := narrowOnCond(s.Cond, st)

	thenSt = cg.dfWalkBlock(s.Then, thenSt)

	for _, ei := range s.ElseIfs {
		cg.dfCheckExpr(ei.Cond, elseSt)

		var inner *dfState

		inner, elseSt = narrowOnCond(ei.Cond, elseSt)
		inner = cg.dfWalkBlock(ei.Body, inner)
		thenSt = mergeStates(thenSt, inner)
	}

	if s.Else != nil {
		elseSt = cg.dfWalkBlock(s.Else, elseSt)
	}

	return mergeStates(thenSt, elseSt)
}

// dfWalkLoop runs the body to fixpoint (bounded). Init runs once; cond is
// re-checked each iteration; post runs at end of body. We cap iterations
// to avoid runaway loops on adversarial input.
func (cg *CodeGen) dfWalkLoop(init, cond, post ast.Node, body *ast.Block, st *dfState) *dfState {
	if init != nil {
		st = cg.dfWalkStmt(init, st)
	}

	const maxIter = 4

	prev := st

	// Phase 1: iterate to fixpoint silently. The first iterations see a
	// transient state where loop-modified locals still hold their init
	// values; warnings emitted then would be phantom (e.g. "epoch % 500 ==
	// 0 always true" on iter 0 of `for let epoch = 0; ...; epoch++`).
	cg.dfSuppressWarnings++

	converged := false

	for i := 0; i < maxIter; i++ {
		if cond != nil {
			cg.dfCheckExpr(cond, prev)
		}
		// Body sees the then-narrowed state from the loop condition
		// (`for i < len(arr): arr[i]` should treat `i` as
		// boundsChecked inside the body).  narrowOnCond returns
		// (thenSt, elseSt); we want thenSt for the body, the
		// elseSt is the post-loop continuation handled by the
		// outer caller via the un-narrowed `prev`.
		bodySt := prev.clone()
		if cond != nil {
			thenSt, _ := narrowOnCond(cond, prev)
			if thenSt != nil {
				bodySt = thenSt
			}
		}

		bodySt = cg.dfWalkBlock(body, bodySt)

		if post != nil && bodySt != nil && !bodySt.dead {
			bodySt = cg.dfWalkStmt(post, bodySt)
		}

		merged := mergeStates(prev, bodySt)
		if i >= 1 {
			// Apply widening from the second iteration onward so that
			// monotone-growing loop counters saturate to int64 bounds
			// (preserving stride). Without this, `for i = 0; i < N; i++`
			// adds 1 to the upper bound each iteration and never
			// converges within maxIter.
			merged = widenStates(prev, merged)
		}

		if statesEqual(prev, merged) {
			converged = true
			prev = merged

			break
		}

		prev = merged
	}

	cg.dfSuppressWarnings--

	if !converged {
		// Did not converge in maxIter; return BOTTOM-ised state for
		// everything the loop body might touch. Skip the emit pass --
		// flow-sensitive warnings on a state that didn't stabilize are
		// unreliable.
		//
		// `freed` and `uninit` are MAY-lattices: an entry means "the
		// variable might be freed / uninitialised on some path through
		// the loop."  Dropping them when the loop fails to converge
		// silently masks use-after-deinit and use-of-uninit on the
		// post-loop tail.  Carry them forward verbatim instead.
		out := newDFState()
		for k := range prev.nil {
			out.nil[k] = nilBottom
		}

		for k := range prev.cnst {
			out.cnst[k] = cBotFact()
		}

		for k, v := range prev.freed {
			out.freed[k] = v
		}

		for k, v := range prev.uninit {
			out.uninit[k] = v
		}

		return out
	}

	// Phase 2: walk the body once more with the converged input state so
	// flow-sensitive warnings fire against values that actually reflect
	// the loop's steady state. Any variable widened to BOTTOM during
	// fixpoint stays BOTTOM here, so phantom-constant conditions don't
	// fold; conditions that genuinely fold under the converged state
	// (e.g. a local re-assigned the same constant on every iteration)
	// still produce warnings. The walk's output state is discarded --
	// prev is already a fixpoint, so re-walking can't refine it.
	if cond != nil {
		cg.dfCheckExpr(cond, prev)
	}

	bodySt := prev.clone()
	if cond != nil {
		thenSt, _ := narrowOnCond(cond, prev)
		if thenSt != nil {
			bodySt = thenSt
		}
	}

	cg.dfWalkBlock(body, bodySt)

	return prev
}

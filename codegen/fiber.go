package codegen

// Fiber/channel/mutex misuse checks. Intra-function, syntax-directed; no
// type inference beyond pattern-matching the canonical sync stdlib spelling
// (`m.lock()`, `ch.close()`, `m = sync::Mutex.new()`, etc.).
//
// Path handling: control flow is descended branch-by-branch with a fresh
// snapshot of the state. Branch-local state never leaks into the
// post-branch flow, which keeps the check from flagging "if x: ch.close()
// else: ch.close()" as a double close. The trade-off is that bugs that
// only manifest across a branch boundary aren't caught here.

import (
	"sort"

	"github.com/Azer0s/tin/ast"
)

// fiberState is the per-path state tracked while walking a function body.
// `closed` maps a channel variable name to the position of the close that
// produced this state. `locked` maps a mutex variable name to the position
// of the most recent .lock() / .read_lock(). A name being absent from
// either map means "not closed" / "not locked" on this path.
type fiberState struct {
	closed map[string]ast.Pos
	locked map[string]ast.Pos
}

func newFiberState() fiberState {
	return fiberState{
		closed: map[string]ast.Pos{},
		locked: map[string]ast.Pos{},
	}
}

func (s fiberState) clone() fiberState {
	out := newFiberState()

	for k, v := range s.closed {
		out.closed[k] = v
	}

	for k, v := range s.locked {
		out.locked[k] = v
	}

	return out
}

// checkFiberMisuse runs the fiber/channel/mutex misuse pass over a
// function body. Bails out for extern, virtual, or empty bodies -- nothing
// to walk in those cases.
func (cg *CodeGen) checkFiberMisuse(fn *ast.FuncDecl) {
	if !cg.diagEnabled(DiagFiber) {
		return
	}

	if fn.Body == nil || fn.IsExtern != "" || fn.IsVirtual {
		return
	}

	body, ok := fn.Body.(*ast.Block)
	if !ok {
		return
	}

	cg.checkMutexUnused(fn, body)

	state := newFiberState()
	cg.walkFiberStmts(body.Stmts, &state)

	// Iterate in sorted order: Go's randomized map iteration would
	// otherwise leak into the warning order, which is observable to
	// snapshot tests and -Werror first-error-wins.
	names := make([]string, 0, len(state.locked))
	for name := range state.locked {
		names = append(names, name)
	}

	sort.Strings(names)

	for _, name := range names {
		cg.warn(DiagFiber, state.locked[name],
			"mutex %q locked but never unlocked before function returns", name)
	}
}

// checkMutexUnused flags a mutex local that's `let`-bound from
// `sync::Mutex.new()` / `sync::RWMutex.new()` but never locked anywhere
// in the function. We deliberately ignore assignments into outer-scope
// names: the common pattern is to assign a global mutex from a setup
// function (`g_mu = sync::Mutex.new()`) and lock it from a different
// fiber, which would always trip a false positive under intra-function
// analysis.
func (cg *CodeGen) checkMutexUnused(_ *ast.FuncDecl, body *ast.Block) {
	declared := map[string]ast.Pos{}
	locked := map[string]bool{}

	walkAST(body, func(n ast.Node) {
		switch e := n.(type) {
		case *ast.VarDecl:
			if isMutexConstruction(e.Value) {
				declared[e.Name] = e.Pos()
			}
		case *ast.CallExpr:
			if name, method, ok := extractMethodCall(e); ok {
				if method == "lock" || method == "read_lock" {
					locked[name] = true
				}
			}
		}
	})

	// Sorted iteration: deterministic warning order across builds.
	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}

	sort.Strings(names)

	for _, name := range names {
		if !locked[name] {
			cg.warn(DiagFiber, declared[name],
				"mutex %q declared but never locked", name)
		}
	}
}

// walkFiberStmts walks a sequence of statements in source order, threading
// the fiber state through linear stretches and forking it across branches.
// The state is mutated in place for sequential statements; for control
// flow, each branch gets a clone that's discarded at the join point.
func (cg *CodeGen) walkFiberStmts(stmts []ast.Node, state *fiberState) {
	for _, s := range stmts {
		cg.walkFiberStmt(s, state)
	}
}

func (cg *CodeGen) walkFiberStmt(s ast.Node, state *fiberState) {
	switch n := s.(type) {
	case *ast.Block:
		cg.walkFiberStmts(n.Stmts, state)
	case *ast.IfStmt:
		// A close/lock that fires on at least one arm propagates back
		// out as a "may have closed/locked" fact -- otherwise a
		// pattern like `if c: ch.close(); ch.close()` would not flag
		// the trailing close as a possible double-close.  Walk each
		// arm with its own clone, then merge the unions back in.
		cg.walkFiberBranchMerging(n.Then, state)

		for _, ei := range n.ElseIfs {
			cg.walkFiberBranchMerging(ei.Body, state)
		}

		if n.Else != nil {
			cg.walkFiberBranchMerging(n.Else, state)
		}
	case *ast.MatchStmt:
		for _, c := range n.Cases {
			cg.walkFiberBranchMerging(c.Body, state)
		}

		if n.Default != nil {
			cg.walkFiberBranchMerging(n.Default, state)
		}
	case *ast.ForStmt:
		cg.walkFiberBranch(n.Body, state)
	case *ast.ReturnStmt:
		// Linear path ends; nothing to do.
		_ = n
	case *ast.ExprStmt:
		cg.walkFiberExpr(n.Expr, state)
	case *ast.AssignStmt:
		cg.walkFiberExpr(n.Value, state)
	case *ast.VarDecl:
		cg.walkFiberExpr(n.Value, state)
	default:
		// Walk any expression children for nested method calls.
		walkAST(s, func(c ast.Node) {
			if call, ok := c.(*ast.CallExpr); ok {
				cg.checkFiberCall(call, state, false)
			}
		})
	}
}

// walkFiberBranch descends into a sub-block with a fresh clone of the
// state. The clone is discarded so the caller's state doesn't pick up
// closes/locks that only happen on one path.
func (cg *CodeGen) walkFiberBranch(blk *ast.Block, state *fiberState) {
	if blk == nil {
		return
	}

	branch := state.clone()
	cg.walkFiberStmts(blk.Stmts, &branch)
}

// walkFiberBranchMerging descends into a sub-block with a fresh clone,
// then union-merges any new close/lock facts the branch added back
// into the caller's state.  Used for if/match arms so that a close
// inside one arm carries forward as a "may have closed" fact -- the
// next sibling close at the same scope flags as a possible double.
func (cg *CodeGen) walkFiberBranchMerging(blk *ast.Block, state *fiberState) {
	if blk == nil {
		return
	}

	branch := state.clone()
	cg.walkFiberStmts(blk.Stmts, &branch)

	for name, pos := range branch.closed {
		if _, ok := state.closed[name]; !ok {
			state.closed[name] = pos
		}
	}

	for name, pos := range branch.locked {
		if _, ok := state.locked[name]; !ok {
			state.locked[name] = pos
		}
	}
}

// walkFiberExpr inspects an expression for fiber-relevant operations.
// Recognizes both `m.lock()` (sync) and `await m.lock()` (the usual
// spelling) -- the await wrapper is unwrapped here so the same handler
// fires for both.
func (cg *CodeGen) walkFiberExpr(e ast.Node, state *fiberState) {
	if e == nil {
		return
	}

	if aw, ok := e.(*ast.AwaitExpr); ok {
		cg.checkAwaitInCriticalSection(aw, state)

		if call, ok := aw.Future.(*ast.CallExpr); ok {
			cg.checkFiberCall(call, state, true)

			return
		}
	}

	if call, ok := e.(*ast.CallExpr); ok {
		cg.checkFiberCall(call, state, false)

		return
	}

	walkAST(e, func(n ast.Node) {
		if call, ok := n.(*ast.CallExpr); ok {
			cg.checkFiberCall(call, state, false)
		}
	})
}

// checkFiberCall reacts to a single CallExpr: close, send, lock, unlock,
// read_lock, read_unlock. underAwait reports whether this call sits
// inside an `await` (matters for distinguishing user error from spec --
// `lock` is meant to be awaited; `unlock` and `close` are not).
func (cg *CodeGen) checkFiberCall(c *ast.CallExpr, state *fiberState, _ bool) {
	name, method, ok := extractMethodCall(c)
	if !ok {
		return
	}

	switch method {
	case "close":
		if pos, already := state.closed[name]; already {
			cg.warn(DiagFiber, c.Pos(),
				"channel %q already closed at %d:%d", name, pos.Line, pos.Col)

			return
		}

		state.closed[name] = c.Pos()
	case "send":
		if pos, ok := state.closed[name]; ok {
			cg.warn(DiagFiber, c.Pos(),
				"send on channel %q after close at %d:%d", name, pos.Line, pos.Col)
		}
	case "lock", "read_lock":
		state.locked[name] = c.Pos()
	case "unlock", "read_unlock":
		delete(state.locked, name)
	}
}

// checkAwaitInCriticalSection flags an await that sits inside a held lock
// when the awaited expression is not the lock itself or a Cond.wait()
// (which is the documented pattern for hand-off and is exempt by design).
// Holding a mutex across an unrelated await serializes fibers that should
// be able to run in parallel.
func (cg *CodeGen) checkAwaitInCriticalSection(aw *ast.AwaitExpr, state *fiberState) {
	if len(state.locked) == 0 {
		return
	}

	// Recognized exempt method calls (lock/unlock/wait); awaiting any of
	// these on the held mutex itself is the documented hand-off pattern.
	if call, ok := aw.Future.(*ast.CallExpr); ok {
		if _, method, ok := extractMethodCall(call); ok {
			switch method {
			case "lock", "read_lock", "unlock", "read_unlock", "wait":
				return
			}
		}
	}

	for name, pos := range state.locked {
		cg.warn(DiagFiber, aw.Pos(),
			"await while mutex %q is held (locked at %d:%d) serializes fibers; release before awaiting",
			name, pos.Line, pos.Col)

		break
	}
}

// extractMethodCall recognizes `name.method(...)` where the receiver is a
// bare identifier. Returns the receiver name and the method name. Bails
// for chained method calls or expression receivers we can't name.
func extractMethodCall(c *ast.CallExpr) (string, string, bool) {
	fa, ok := c.Func.(*ast.FieldAccess)
	if !ok {
		return "", "", false
	}

	id, ok := fa.Expr.(*ast.Identifier)
	if !ok {
		return "", "", false
	}

	return id.Name, fa.Field, true
}

// isMutexConstruction reports whether expr is `sync::Mutex.new()` or
// `sync::RWMutex.new()`. Other shapes (e.g. assignment from a function
// return) fall through and don't seed the "declared but never locked"
// scan -- we'd rather miss those than warn on false positives where the
// mutex flows in from a parameter or a getter.
func isMutexConstruction(expr ast.Node) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}

	fa, ok := call.Func.(*ast.FieldAccess)
	if !ok || fa.Field != "new" {
		return false
	}

	sa, ok := fa.Expr.(*ast.ScopeAccess)
	if !ok || len(sa.Path) < 2 {
		return false
	}

	tail := sa.Path[len(sa.Path)-1]

	return tail == "Mutex" || tail == "RWMutex"
}

package codegen

import (
	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) dfWalkMatch(s *ast.MatchStmt, st *dfState) *dfState {
	cg.dfCheckExpr(s.Expr, st)

	var merged *dfState

	for _, c := range s.Cases {
		armSt := st.clone()
		armSt = cg.dfWalkStmt(c.Body, armSt)
		merged = mergeStates(merged, armSt)
	}

	if s.Default != nil {
		armSt := st.clone()
		armSt = cg.dfWalkStmt(s.Default, armSt)
		merged = mergeStates(merged, armSt)
	}

	if merged == nil {
		return st
	}

	return merged
}

// dfCheckExpr looks for nil-deref, nil-field-access, and use-after-deinit
// patterns inside expr using the current state. The walk is non-state-
// modifying.
func (cg *CodeGen) dfCheckExpr(expr ast.Node, st *dfState) {
	if expr == nil || st == nil {
		return
	}

	walkAST(expr, func(n ast.Node) {
		switch e := n.(type) {
		case *ast.Identifier:
			// Manual-alloc use-after-free: any read of a name
			// whose manualAlloc state is Freed fires the
			// diagnostic.  Skipped when the read is `mem::free`
			// dispatching (we want one warning per use site, not
			// recursively on the free arg itself -- isManualFreeCall
			// already handles the double-free message).
			if st.manualAlloc[e.Name] == manualAllocFreed {
				cg.warn(DiagManualUseAfterFree, e.Pos(),
					"use of %q after mem::free on this path", e.Name)
			}
		case *ast.DerefExpr:
			if id, ok := e.Expr.(*ast.Identifier); ok {
				if st.nil[id.Name] == nilIsNil {
					cg.warn(DiagDerefNil, e.Pos(),
						"dereferencing %q which is statically nil at this point", id.Name)
				} else if st.nil[id.Name] != nilNonNil {
					// Prefer the interprocedural diagnostic when
					// Andersen has more-specific info: "this came
					// from a fn that returns nil sometimes".  Falls
					// back to the generic intraprocedural pedantic
					// warning when no interprocedural source is
					// known (e.g. the value is a fresh param).
					if cg.andersenMayBeNil(cg.dfCurFnName, id.Name) {
						cg.warn(DiagUncheckedReturnedNil, e.Pos(),
							"dereferencing %q whose source function may return nil; guard with `if %s != nil:` or unwrap before this point",
							id.Name, id.Name)
					} else {
						cg.warn(DiagUncheckedNilDeref, e.Pos(),
							"dereference of %q without proving non-nil; guard with `if %s != nil:` or unwrap before this point",
							id.Name, id.Name)
					}
				}

				if st.freed[id.Name] {
					cg.warn(DiagUseAfterDeinit, e.Pos(),
						"dereference of %q after deinit on this path", id.Name)
				}
			}
		case *ast.FieldAccess:
			if id, ok := e.Expr.(*ast.Identifier); ok {
				if st.nil[id.Name] == nilIsNil {
					cg.warn(DiagDerefNil, e.Pos(),
						"field access on %q which is statically nil at this point", id.Name)
				} else if dfIsPointerLike(st.types[id.Name]) && st.nil[id.Name] != nilNonNil {
					// Same prefer-interprocedural logic as the
					// DerefExpr branch above: the Andersen warning
					// names the source function, which is more
					// actionable than the generic dataflow message.
					if cg.andersenMayBeNil(cg.dfCurFnName, id.Name) {
						cg.warn(DiagUncheckedReturnedNil, e.Pos(),
							"field access on %q whose source function may return nil; guard with `if %s != nil:` or unwrap before this point",
							id.Name, id.Name)
					} else {
						cg.warn(DiagUncheckedNilDeref, e.Pos(),
							"field access on pointer %q without proving non-nil; guard with `if %s != nil:` or unwrap before this point",
							id.Name, id.Name)
					}
				}

				if st.freed[id.Name] {
					cg.warn(DiagUseAfterDeinit, e.Pos(),
						"field access on %q after deinit on this path", id.Name)
				}
			}
		case *ast.IndexExpr:
			cg.dfCheckUncheckedIndex(e, st)
		case *ast.BinExpr:
			cg.dfCheckFloatPrecision(e, st)
			cg.dfCheckUncheckedDiv(e, st)
		}
	})

	cg.dfCheckUseBeforeAssign(expr, st)
}

// dfCheckUseBeforeAssign warns on every read of a name that's still in
// st.uninit. Excludes idents that appear directly under `&` (taking an
// address doesn't read the value) and the `name` half of FieldAccess
// (`s.field` -- field is a label, not a read of a binding called
// `field`). The expression is walked with custom recursion (rather than
// walkAST) so the skip rules can be applied site-locally.
func (cg *CodeGen) dfCheckUseBeforeAssign(expr ast.Node, st *dfState) {
	if expr == nil || st == nil || len(st.uninit) == 0 {
		return
	}

	cg.dfCheckUseBeforeAssignNode(expr, st)
}

func (cg *CodeGen) dfCheckUseBeforeAssignNode(n ast.Node, st *dfState) {
	if n == nil {
		return
	}

	switch e := n.(type) {
	case *ast.Identifier:
		if st.uninit[e.Name] {
			cg.warn(DiagUseBeforeAssign, e.Pos(),
				"%q is read before being explicitly assigned (zero-initialized at runtime)",
				e.Name)
		}
	case *ast.AddressOfExpr:
		// `&x` itself is a read of the storage location, not the value;
		// skip the inner identifier. But anything deeper (e.g. `&s.f`)
		// still walks normally so reads under field accesses are caught.
		if _, isIdent := e.Expr.(*ast.Identifier); isIdent {
			return
		}

		cg.dfCheckUseBeforeAssignNode(e.Expr, st)
	case *ast.FieldAccess:
		// Recurse only into the receiver: `s.field` reads s, not a
		// binding called field.
		cg.dfCheckUseBeforeAssignNode(e.Expr, st)
	case *ast.BinExpr:
		cg.dfCheckUseBeforeAssignNode(e.Left, st)
		cg.dfCheckUseBeforeAssignNode(e.Right, st)
	case *ast.UnaryExpr:
		cg.dfCheckUseBeforeAssignNode(e.Expr, st)
	case *ast.CallExpr:
		cg.dfCheckUseBeforeAssignNode(e.Func, st)

		for _, a := range e.Args {
			cg.dfCheckUseBeforeAssignNode(a, st)
		}
	case *ast.DerefExpr:
		cg.dfCheckUseBeforeAssignNode(e.Expr, st)
	case *ast.IndexExpr:
		cg.dfCheckUseBeforeAssignNode(e.Expr, st)
		cg.dfCheckUseBeforeAssignNode(e.Index, st)
	case *ast.TernaryExpr:
		cg.dfCheckUseBeforeAssignNode(e.Cond, st)
		cg.dfCheckUseBeforeAssignNode(e.Then, st)
		cg.dfCheckUseBeforeAssignNode(e.Else, st)
	}
}

// dfApplyEscapes returns a copy of `st` with any name whose address is
// taken in `expr` invalidated to BOTTOM across every lattice piece. The
// callee receiving `&x` may store, mutate, or deinit through the
// pointer; we have no way to know without interprocedural info, so the
// safe assumption after the expression is that x's previous facts no
// longer hold.
func (cg *CodeGen) dfApplyEscapes(expr ast.Node, st *dfState) *dfState {
	if expr == nil || st == nil {
		return st
	}

	var (
		escaped      []string
		callArgNames []string
		callArgFreed []string
	)

	walkAST(expr, func(n ast.Node) {
		switch e := n.(type) {
		case *ast.AddressOfExpr:
			if id, ok := e.Expr.(*ast.Identifier); ok && id.Name != "" && id.Name != "_" {
				escaped = append(escaped, id.Name)
			}
		case *ast.StructLit:
			// Storing a Live alloc into a struct field transfers
			// ownership to the struct: `Owner{buf: p}` makes the
			// Owner instance responsible for p, not the caller.
			// Mark p Escaped so the scope-exit leak check skips it.
			for _, f := range e.Fields {
				if id, ok := f.Value.(*ast.Identifier); ok && id.Name != "" && id.Name != "_" {
					callArgNames = append(callArgNames, id.Name)
				}
			}
		case *ast.ArrayLit:
			// Same logic for array element absorption:
			// `let xs = [p, q]` transfers ownership of p, q.
			for _, el := range e.Elems {
				if id, ok := el.(*ast.Identifier); ok && id.Name != "" && id.Name != "_" {
					callArgNames = append(callArgNames, id.Name)
				}
			}
		case *ast.TupleLit:
			for _, el := range e.Elems {
				if id, ok := el.(*ast.Identifier); ok && id.Name != "" && id.Name != "_" {
					callArgNames = append(callArgNames, id.Name)
				}
			}
		case *ast.CallExpr:
			// Skip mem::free -- handled separately in ExprStmt
			// (transitions Live -> Freed, not an escape).
			if _, isFree := isManualFreeCall(e); isFree {
				return
			}
			// Use the interprocedural paramFrees summary when
			// available.  Two cases:
			//   - hasSummary && calleeFrees[i] -> Live -> Freed
			//     (callee provably frees its own param at index i)
			//   - hasSummary && !calleeFrees[i] -> stays Live
			//     (callee provably does NOT free; the alloc is
			//     just borrowed for read/write through the pointer)
			//   - no summary (extern, indirect, unanalyzed) -> Escaped
			//     (conservative: callee MIGHT take ownership)
			var (
				calleeFrees map[int]bool
				hasSummary  bool
			)

			if id, ok := e.Func.(*ast.Identifier); ok {
				calleeFrees, hasSummary = cg.paramFrees[id.Name]
			}

			for i, a := range e.Args {
				argID, ok := a.(*ast.Identifier)
				if !ok || argID.Name == "" || argID.Name == "_" {
					continue
				}

				if calleeFrees != nil && calleeFrees[i] {
					// Caller's `helper(p)`-and-helper-frees-p
					// shape: transition Live -> Freed.
					callArgFreed = append(callArgFreed, argID.Name)

					continue
				}

				if hasSummary {
					// Analyzed callee provably does NOT free
					// this index -- the alloc remains the
					// caller's responsibility.
					continue
				}

				callArgNames = append(callArgNames, argID.Name)
			}
		}
	})

	if len(escaped) == 0 && len(callArgNames) == 0 && len(callArgFreed) == 0 {
		return st
	}

	out := st.clone()

	for _, name := range escaped {
		out.cnst[name] = cBotFact()
		out.nil[name] = nilBottom
		delete(out.intv, name)
		delete(out.floats, name)
		delete(out.freed, name)
		// An address taken doesn't tell us whether the callee assigned;
		// be lenient and clear uninit too so we don't flood the user
		// with warnings on idiomatic out-parameters (`fn fill(p *T)`).
		delete(out.uninit, name)
	}

	for _, name := range callArgNames {
		if out.manualAlloc[name] == manualAllocLive {
			out.manualAlloc[name] = manualAllocEscaped
		}
	}

	for _, name := range callArgFreed {
		// Interprocedural Live -> Freed (the callee's summary
		// proves the param is passed to mem::free).
		if out.manualAlloc[name] == manualAllocLive {
			out.manualAlloc[name] = manualAllocFreed
		}
	}

	return out
}

// dfEscapeManualAllocFromExpr walks `expr` and marks every manually
// allocated identifier that appears as Escaped in `st`.  Used by
// ReturnStmt to transfer ownership to the caller and by assignment
// to non-local destinations (struct field, array element, etc.) to
// signal that the binding now flows out of the current scope's
// responsibility.
func dfEscapeManualAllocFromExpr(expr ast.Node, st *dfState) {
	if expr == nil || st == nil {
		return
	}

	walkAST(expr, func(n ast.Node) {
		if id, ok := n.(*ast.Identifier); ok {
			if st.manualAlloc[id.Name] == manualAllocLive {
				st.manualAlloc[id.Name] = manualAllocEscaped
			}
		}
	})
}

// dfCheckManualAllocLeaks emits -Wmanual-alloc-leak for every name
// still in Live state at function exit.  Called once at the end of
// dfAnalyzeFunc with the joined post-body state -- intermediate
// scope exits don't fire (you're allowed to free in a later
// statement) but a path that reaches the function return without
// any `mem::free(p)` DOES fire.  Position is recovered from the
// scope entry where the binding was first seen; we keep that in a
// per-fn map populated during VarDecl walks.
func (cg *CodeGen) dfCheckManualAllocLeaks(st *dfState) {
	if st == nil {
		return
	}

	// Don't re-fire on a dead state: every path that ended in a
	// ReturnStmt already ran the leak check at that exit, so the
	// joined dead state would emit a second warning for the same
	// binding.  The fn-end caller and the ReturnStmt caller share
	// this helper; this guard keeps the second one silent.
	if st.dead {
		return
	}

	for name, state := range st.manualAlloc {
		if state != manualAllocLive {
			continue
		}

		pos := cg.manualAllocSites[name]
		cg.warn(DiagManualAllocLeak, pos,
			"%q holds an mem::malloc/calloc/realloc result that is not freed on every path; "+
				"add `mem::free(%s)` before scope exit, or transfer ownership by returning "+
				"the pointer / storing it in a field", name, name)
	}
}

// isReallocOfSelf reports whether `expr` is `mem::realloc(<name>, ...)`
// with the first argument being the identifier `name`.  The
// realloc convention is that the input pointer is consumed
// (potentially freed by the allocator), so reassigning the same
// binding to its realloc result is a LEGAL ownership transfer --
// not a dropped allocation.  Anything else (different name, no
// arg, different fn) returns false.
func isReallocOfSelf(expr ast.Node, name string) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}

	callee := call.Func
	if ie, ok2 := callee.(*ast.IndexExpr); ok2 {
		callee = ie.Expr
	}

	sa, ok := callee.(*ast.ScopeAccess)
	if !ok {
		return false
	}

	if len(sa.Path) != 2 || sa.Path[0] != "mem" || sa.Path[1] != "realloc" {
		return false
	}

	if len(call.Args) < 1 {
		return false
	}

	id, ok := call.Args[0].(*ast.Identifier)

	return ok && id.Name == name
}

// callReturnsAlloc reports whether `expr` is a direct call to a fn
// that the pre-pass summary proves returns a freshly-allocated
// block.  Counterpart to isManualAllocCall but interprocedural --
// recognizes `let p = make()` shapes where `make` ultimately
// returns mem::malloc.  Conservative: only direct identifier-call
// shapes are tracked; method calls / scope-access calls / generic
// instantiations are out of scope.
func (cg *CodeGen) callReturnsAlloc(expr ast.Node) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}

	id, ok := call.Func.(*ast.Identifier)
	if !ok {
		return false
	}

	return cg.returnsAlloc[id.Name]
}

// isManualAllocCall reports whether `expr` is a call to one of the
// manual-allocation primitives in `mem::`:
//
//	mem::malloc(size)         // raw libc malloc
//	mem::calloc(n, size)      // raw libc calloc
//	mem::realloc(p, new_size) // raw libc realloc (already-owned block
//	                          // becomes Live again under the new var)
//	mem::alloc[T]()           // typed wrapper that allocates + zeros
//	mem::alloc[T](n)          // typed wrapper allocating n elements
//
// Each of these returns a freshly-owned block that must reach
// `mem::free` (or escape the fn) before scope exit.  Used by
// VarDecl/AssignStmt to seed the manualAlloc tracker.
func isManualAllocCall(expr ast.Node) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}

	// Type args wrap the callee in an IndexExpr:
	// `mem::alloc[*Foo]()` -> CallExpr{Func: IndexExpr{Expr:
	// ScopeAccess[mem, alloc], Index: <typeArg>}}.  No-type-args
	// callers have ScopeAccess directly.  Unwrap one IndexExpr
	// layer if present.
	callee := call.Func
	if ie, ok2 := callee.(*ast.IndexExpr); ok2 {
		callee = ie.Expr
	}

	sa, ok := callee.(*ast.ScopeAccess)
	if !ok {
		return false
	}

	if len(sa.Path) != 2 || sa.Path[0] != "mem" {
		return false
	}

	switch sa.Path[1] {
	case "malloc", "calloc", "realloc", "alloc":
		return true
	}

	return false
}

// isManualFreeCall reports whether `expr` is `mem::free(p)` with `p`
// a simple identifier; returns the identifier's name.  Anything more
// elaborate (`mem::free(struct.field)`, `mem::free(call())`) is left
// to the user to verify -- the lattice keyed on name can't track
// non-binding free targets.
func isManualFreeCall(expr ast.Node) (string, bool) {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return "", false
	}

	sa, ok := call.Func.(*ast.ScopeAccess)
	if !ok {
		return "", false
	}

	if len(sa.Path) != 2 || sa.Path[0] != "mem" || sa.Path[1] != "free" {
		return "", false
	}

	if len(call.Args) != 1 {
		return "", false
	}

	id, ok := call.Args[0].(*ast.Identifier)
	if !ok {
		return "", false
	}

	return id.Name, true
}

// dfCheckUncheckedDiv warns on `a / b` and `a % b` when the dataflow
// pass cannot prove `b != 0` at the use site.  The default-on hard
// error for "division by zero" already rejects b == constant 0; this
// pedantic check covers the unproven case: b is a variable with an
// interval that may contain 0, or has no interval at all.  When b
// folds to a non-zero compile-time constant the check stays silent
// (the const case is proven safe).

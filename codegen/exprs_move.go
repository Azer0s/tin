package codegen

// exprs_move.go - codegen for the explicit `move x` expression.
//
// At runtime, `move x` evaluates to x's current value -- no special
// instructions emitted beyond a normal identifier read. The work is
// done at compile time:
//
//  1. The source must be a local binding the current scope owns.
//     Function parameters, for-loop iterator bindings, and other
//     non-owning bindings raise move-non-owning-binding errors with
//     fix-it suggestions to drop `move` or extract first.
//  2. The binding's scope entry gets marked ownershipMoved, so the
//     scope-exit release path skips this entry (the consumer that
//     received the value now owns the rc).
//  3. The binding's name gets added to cg.movedBindings, a per-scope
//     set the identifier reader consults to raise use-after-move on
//     any subsequent read.

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// genMoveExpr emits `move x`: validates the source, marks the entry
// as ownershipMoved, and returns the binding's current value just as
// a plain identifier read would.
func (cg *CodeGen) genMoveExpr(block *ir.Block, e *ast.MoveExpr) (value.Value, error) {
	if e == nil || e.Name == "" {
		return nil, fmt.Errorf("%s: malformed move expression", cg.posStr(e))
	}
	// Use-after-move on the same source binding within this fn body.
	if cg.movedBindings[e.Name] {
		return nil, fmt.Errorf("%s: use of moved value `%s`; `%s` was moved earlier in this scope",
			cg.posStr(e), e.Name, e.Name)
	}

	entry, ok := cg.curScope.lookup(e.Name)
	if !ok {
		return nil, fmt.Errorf("%s: cannot move `%s`: no such binding in scope",
			cg.posStr(e), e.Name)
	}
	// Self-move check (let x = move x): the binding is being moved
	// to itself.  Tin's parser does not emit this directly but a
	// pathological macro expansion could; reject for clarity.
	if cg.pendingMoveSelfName == e.Name {
		return nil, fmt.Errorf("%s: self-move: `%s = move %s` has no effect",
			cg.posStr(e), e.Name, e.Name)
	}
	// Non-owning binding checks.  All carry the same error code
	// (move-non-owning-binding) with the same two-path fix-it: drop
	// the move keyword, or extract into an owned copy first.
	if reason := cg.classifyNonOwningBinding(entry, e.Name); reason != "" {
		return nil, fmt.Errorf(
			"%s: cannot move `%s` (%s)\n"+
				"  help: drop the `move` keyword -- `%s` works as-is; the compiler\n"+
				"        will pick the cheapest semantics automatically\n"+
				"  help: or, if you need owned semantics, copy first:\n"+
				"          let owned = %s\n"+
				"          move owned",
			cg.posStr(e), e.Name, reason, e.Name, e.Name)
	}
	// Read the binding's value (same path as a plain identifier
	// read so type semantics carry through).
	idRef := &ast.Identifier{Name: e.Name}
	idRef.SetPos(e.Pos())

	val, err := cg.genIdentifier(block, idRef)
	if err != nil {
		return nil, err
	}
	// Partial-move compensation: when an enclosing if / match has
	// branches that do NOT move this binding, the outer scope will
	// release `x` on those non-moving paths.  To keep the rc balanced
	// on the moving path (where the consumer also takes a ref),
	// bump the rc by one before the consumer reads -- so the outer
	// release on this path drops what the retain put in.  See
	// docs/15-ownership.md "Per-branch move tracking".
	if cg.isBindingPartialMoved(e.Name) {
		cg.emitRetain(block, val)
	}
	// Mark the binding as moved.  emitScopeRelease will skip the
	// scope-exit release for entries in ownershipMoved state.
	entry.ownership = ownershipMoved

	if cg.movedBindings == nil {
		cg.movedBindings = map[string]bool{}
	}

	cg.movedBindings[e.Name] = true
	// Record the move for --explain-ownership.
	cg.recordOwnership(e.Name, ownershipMoved, "moved via explicit `move`")

	return val, nil
}

// classifyNonOwningBinding returns a short reason string when the
// binding cannot be moved (function parameter, iterator binding,
// etc.), or the empty string when the binding is move-eligible.
// Used by genMoveExpr to surface clear error messages with fix-its.
func (cg *CodeGen) classifyNonOwningBinding(entry *scopeEntry, name string) string {
	if entry == nil {
		return ""
	}
	// Function parameters: the caller owns the value; the callee
	// only borrows it for the duration of the call.  noDeinit is
	// the marker the rest of codegen uses to identify parameter
	// allocas.
	if entry.noDeinit {
		return "parameter, not owned by this scope"
	}
	// Iterator bindings: for-loop variables.  Even for the
	// pass-by-value form (`for item in xs`) the slot is filled
	// fresh per iteration with the corresponding retain emitted by
	// the loop ramp -- a `move item` would skip the body's
	// scope-exit release and leak one rc per iteration.  The ref
	// form (`for ref item in xs`) aliases the source array and
	// is even more obviously not movable.
	if entry.isForIterator {
		return "iterator binding, view into the container"
	}
	// noRelease catches synthetic borrowed bindings the runtime
	// uses (e.g. union `is` peeks).  Same reasoning as iterators:
	// the rc accounting lives elsewhere.
	if entry.noRelease {
		return "borrowed binding, owned elsewhere"
	}
	// Globals: the program owns these, not the function scope.
	if entry.isGlobal {
		return "module-level global"
	}

	return ""
}

// markUseAfterMoveCheck wires identifier reads through a check that
// errors when the binding has already been moved in the current
// function body.  Called from genIdentifier so every read
// participates.
func (cg *CodeGen) checkUseAfterMove(name string, node ast.Node) error {
	if !cg.movedBindings[name] {
		return nil
	}

	return fmt.Errorf("%s: use of moved value `%s`; `%s` was moved earlier in this scope",
		cg.posStr(node), name, name)
}

// collectMovedNames walks `node` and returns the set of binding names
// targeted by `move x` expressions inside it.  Stops descent at
// LambdaExpr / SpawnExpr / DeferStmt because their bodies execute in
// a captured scope independent of the enclosing branch's flow.
//
// Used by the per-branch move analyzer (genIf / genMatch) to identify
// candidates for partial-move classification before the branches are
// codegenned.
func collectMovedNames(node ast.Node) map[string]bool {
	out := map[string]bool{}

	if node == nil {
		return out
	}

	walkAST(node, func(n ast.Node) {
		// Bodies of lambdas / spawns / defers run in captured scope
		// and their `move x` targets the captured copy, not the
		// outer binding -- but walkAST cannot stop descent, so we
		// simply do not record names inside them here.  Conservative
		// over-counting would be fine for the analyzer's correctness
		// (it would emit extra retains that get balanced) -- but the
		// captured copy IS a different binding with its own name in
		// the inner scope, so the outer-scope name probably never
		// collides anyway.
		if me, ok := n.(*ast.MoveExpr); ok && me != nil && me.Name != "" {
			out[me.Name] = true
		}
	})

	return out
}

// isBindingPartialMoved reports whether `name` appears in any active
// partial-move frame on the stack.  A frame is pushed by genIf / genMatch
// for the duration of its branch codegen when the pre-analysis classified
// the binding as moved on some but not all branches.
func (cg *CodeGen) isBindingPartialMoved(name string) bool {
	if name == "" {
		return false
	}

	for _, frame := range cg.partialMovedStack {
		if frame[name] {
			return true
		}
	}

	return false
}

// analyzeIfPartialMoves returns the set of binding names that are moved
// in some but not all of the if/elif/else branches.  An if without an
// `else` is treated as having an implicit empty-move branch (the no-else
// fall-through path), so any move in a then/elif arm of such an if is
// automatically partial.
func (cg *CodeGen) analyzeIfPartialMoves(s *ast.IfStmt) map[string]bool {
	branches := []ast.Node{s.Then}
	for _, ei := range s.ElseIfs {
		branches = append(branches, ei.Body)
	}

	branches = append(branches, s.Else)
	// Per-branch move sets.  A nil branch (missing else) contributes
	// an empty set, which forces any move in a sibling to be partial.
	perBranch := make([]map[string]bool, len(branches))
	union := map[string]bool{}

	for i, b := range branches {
		perBranch[i] = collectMovedNames(b)

		for name := range perBranch[i] {
			union[name] = true
		}
	}

	out := map[string]bool{}

	for name := range union {
		all := true

		for _, set := range perBranch {
			if !set[name] {
				all = false

				break
			}
		}

		if !all {
			out[name] = true
		}
	}

	return out
}

// analyzeMatchPartialMoves is the match-statement analog of
// analyzeIfPartialMoves.  A match without a default arm is treated as
// having an implicit empty-move branch (the no-match fall-through).
func (cg *CodeGen) analyzeMatchPartialMoves(s *ast.MatchStmt) map[string]bool {
	branches := make([]ast.Node, 0, len(s.Cases)+1)
	for _, c := range s.Cases {
		branches = append(branches, c.Body)
	}

	branches = append(branches, s.Default)

	perBranch := make([]map[string]bool, len(branches))
	union := map[string]bool{}

	for i, b := range branches {
		perBranch[i] = collectMovedNames(b)

		for name := range perBranch[i] {
			union[name] = true
		}
	}

	out := map[string]bool{}

	for name := range union {
		all := true

		for _, set := range perBranch {
			if !set[name] {
				all = false

				break
			}
		}

		if !all {
			out[name] = true
		}
	}

	return out
}

// moveStateSnapshot captures cg.movedBindings and the ownership of each
// candidate name's outer-scope entry just before a branch runs.  The
// branch's codegen mutates these in-place (genMoveExpr writes
// entry.ownership and adds to movedBindings); restoreMoveState rolls them
// back so the next sibling branch sees the pre-if state.
type moveStateSnapshot struct {
	moved          map[string]bool
	priorOwnership map[string]ownership
}

func (cg *CodeGen) snapshotMoveState(candidates map[string]bool) moveStateSnapshot {
	snap := moveStateSnapshot{
		moved:          map[string]bool{},
		priorOwnership: map[string]ownership{},
	}
	for k, v := range cg.movedBindings {
		snap.moved[k] = v
	}

	for name := range candidates {
		if entry, ok := cg.curScope.lookup(name); ok && entry != nil {
			snap.priorOwnership[name] = entry.ownership
		}
	}

	return snap
}

func (cg *CodeGen) restoreMoveState(snap moveStateSnapshot) {
	cg.movedBindings = map[string]bool{}
	for k, v := range snap.moved {
		cg.movedBindings[k] = v
	}

	for name, prev := range snap.priorOwnership {
		if entry, ok := cg.curScope.lookup(name); ok && entry != nil {
			entry.ownership = prev
		}
	}
}

// captureBranchMoves returns the names freshly added to movedBindings
// in `branchMoved` that were not present in `priorMoved`.
func diffBranchMoves(priorMoved, branchMoved map[string]bool) map[string]bool {
	out := map[string]bool{}

	for name, v := range branchMoved {
		if !v {
			continue
		}

		if !priorMoved[name] {
			out[name] = true
		}
	}

	return out
}

// commitMergedMoves takes the per-branch move sets, computes the
// intersection (names moved on every reachable path), and applies that
// merged state at the outer scope: entry.ownership = Moved, and the
// merged movedBindings map gets the name as well so subsequent reads
// in the function fail with use-after-move.  Names in (union \
// intersection) are partial moves -- the moving paths emitted a
// balancing retain via genMoveExpr, so the outer scope's release is
// already rc-correct.  Returns the merged movedBindings.
func (cg *CodeGen) commitMergedMoves(branchSets []map[string]bool, priorMoved map[string]bool) map[string]bool {
	merged := map[string]bool{}
	for k, v := range priorMoved {
		merged[k] = v
	}

	if len(branchSets) == 0 {
		return merged
	}
	// Intersection across every branch.  Skip names already in
	// priorMoved (they're already merged-moved).
	union := map[string]bool{}

	for _, set := range branchSets {
		for name := range set {
			if !priorMoved[name] {
				union[name] = true
			}
		}
	}

	for name := range union {
		all := true

		for _, set := range branchSets {
			if !set[name] {
				all = false

				break
			}
		}

		if all {
			merged[name] = true

			if entry, ok := cg.curScope.lookup(name); ok && entry != nil {
				entry.ownership = ownershipMoved
			}
		}
	}

	return merged
}

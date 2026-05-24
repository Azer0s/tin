package codegen

// liveness.go - per-function liveness analysis driving implicit move
// on last-use call sites.  Conservative single-pass implementation:
// for each let-binding in a function body, count read sites and the
// AST positions where those reads occur.  When a binding has exactly
// one read in the body AND that read is the only call-arg Identifier
// reference AND no loop / defer / spawn / lambda body wraps it, the
// call site is marked as the binding's last use and codegen elides
// the caller's scope-exit release in favor of a post-call release
// (same lowering as an explicit `move x`).
//
// Conservative on every dimension that matters:
//   - parameters never qualify (the caller owns them, not us).
//   - bindings reassigned in the body never qualify (the "last
//     value" question becomes loop-dependent).
//   - bindings captured by a closure / spawn / defer body never
//     qualify (the capture extends lifetime past the visible body).
//   - bindings whose only read is INSIDE a loop body never qualify
//     (iteration 2 would observe a moved value).
//
// Branches (if / match) are handled by counting reads across all
// arms: if every arm contains a read of the binding, the analyzer
// stays silent (counting two reads, not one).  This avoids the
// partial-move accounting genMoveExpr already does for explicit
// moves; a future tightening could lift this restriction by
// reusing the same per-arm bookkeeping.

import (
	"github.com/Azer0s/tin/ast"
)

// markImplicitMoved tags a binding as moved at the codegen scope
// layer so emitScopeRelease elides its scope-exit release.  Pairs
// with the post-call release emitted in emitCallArgReleaseForRet.
// Also adds the name to movedBindings so any subsequent identifier
// read raises use-after-move (the liveness pass already proved the
// binding has no later read, but the check protects against macro
// expansions or codegen paths that re-evaluate the binding).
func (cg *CodeGen) markImplicitMoved(name string) {
	if name == "" {
		return
	}

	entry, ok := cg.curScope.lookup(name)
	if ok && entry != nil {
		entry.ownership = ownershipMoved
	}

	if cg.movedBindings == nil {
		cg.movedBindings = map[string]bool{}
	}

	cg.movedBindings[name] = true
}

// computeImplicitMoveSites returns the set of *ast.Identifier nodes
// that are the single read of their binding and therefore safe to
// lower as implicit moves at the call site.  Identity comparison
// against this set is the codegen's query mechanism.
//
// The set is empty when the function body has no qualifying
// bindings; callers should treat nil as "no implicit moves here."
func computeImplicitMoveSites(body ast.Node) map[*ast.Identifier]bool {
	if body == nil {
		return nil
	}

	state := implicitMoveState{
		bindingDecls: map[string]ast.Pos{},
		reads:        map[string][]*ast.Identifier{},
		captureNames: map[string]bool{},
		mutated:      map[string]bool{},
	}

	state.walk(body, 0, false)

	out := map[*ast.Identifier]bool{}

	for name, ids := range state.reads {
		if _, ok := state.bindingDecls[name]; !ok {
			continue
		}

		if state.captureNames[name] || state.mutated[name] {
			continue
		}

		if len(ids) != 1 {
			continue
		}

		out[ids[0]] = true
	}

	return out
}

// implicitMoveState tracks the walk's progress while computing
// implicit move sites.
type implicitMoveState struct {
	bindingDecls map[string]ast.Pos           // name -> position of `let name = ...`
	reads        map[string][]*ast.Identifier // name -> reads (in document order)
	captureNames map[string]bool              // names captured by closure / spawn / defer
	mutated      map[string]bool              // names reassigned via `=` / `op=`
}

// walk recursively visits the AST.  `loopDepth` increments inside
// for-bodies so loop-internal reads can be filtered out (they could
// fire multiple times).  `inCapture` is true inside closure /
// spawn / defer bodies so captured names get disqualified entirely.
func (s *implicitMoveState) walk(n ast.Node, loopDepth int, inCapture bool) {
	if n == nil {
		return
	}

	switch v := n.(type) {
	case *ast.VarDecl:
		if v != nil && v.Name != "" && !inCapture {
			s.bindingDecls[v.Name] = v.Pos()
		}

		if v != nil && v.Value != nil {
			s.walk(v.Value, loopDepth, inCapture)
		}
	case *ast.AssignStmt:
		if v != nil {
			if id, ok := v.Target.(*ast.Identifier); ok && id != nil {
				s.mutated[id.Name] = true
			}

			s.walk(v.Target, loopDepth, inCapture)
			s.walk(v.Value, loopDepth, inCapture)
		}
	case *ast.AugAssignStmt:
		if v != nil {
			if id, ok := v.Target.(*ast.Identifier); ok && id != nil {
				s.mutated[id.Name] = true
			}

			s.walk(v.Target, loopDepth, inCapture)
			s.walk(v.Value, loopDepth, inCapture)
		}
	case *ast.PostfixStmt:
		if v != nil {
			if id, ok := v.Expr.(*ast.Identifier); ok && id != nil {
				s.mutated[id.Name] = true
			}
		}
	case *ast.Identifier:
		if v != nil && v.Name != "" {
			if inCapture {
				s.captureNames[v.Name] = true
			} else if loopDepth == 0 {
				s.reads[v.Name] = append(s.reads[v.Name], v)
			} else {
				// In-loop reads disqualify the binding entirely
				// (iteration 2 would observe a moved value).
				// Track by adding two read entries -- the count
				// check at the end filters bindings with !=1
				// reads, so two virtual entries suffice.
				s.reads[v.Name] = append(s.reads[v.Name], v, v)
			}
		}
	case *ast.LambdaExpr:
		if v != nil {
			s.walk(v.Body, loopDepth, true)
		}
	case *ast.SpawnExpr:
		if v != nil {
			s.walk(v.Call, loopDepth, true)

			if v.DoBlock != nil {
				s.walk(v.DoBlock, loopDepth, true)
			}
		}
	case *ast.DeferStmt:
		if v != nil {
			s.walk(v.Call, loopDepth, true)
		}
	case *ast.ForStmt:
		if v != nil {
			s.walk(v.Cond, loopDepth+1, inCapture)

			if v.Body != nil {
				s.walk(v.Body, loopDepth+1, inCapture)
			}
		}
	case *ast.Block:
		if v != nil {
			for _, st := range v.Stmts {
				s.walk(st, loopDepth, inCapture)
			}
		}
	case *ast.IfStmt:
		if v != nil {
			s.walk(v.Cond, loopDepth, inCapture)

			if v.Then != nil {
				s.walk(v.Then, loopDepth, inCapture)
			}

			for _, elif := range v.ElseIfs {
				s.walk(elif.Cond, loopDepth, inCapture)
				s.walk(elif.Body, loopDepth, inCapture)
			}

			if v.Else != nil {
				s.walk(v.Else, loopDepth, inCapture)
			}
		}
	case *ast.MatchStmt:
		if v != nil {
			s.walk(v.Expr, loopDepth, inCapture)

			for _, c := range v.Cases {
				s.walk(c.Body, loopDepth, inCapture)
			}

			if v.Default != nil {
				s.walk(v.Default, loopDepth, inCapture)
			}
		}
	default:
		// Generic descent for any node not specifically handled.
		walkAST(n, func(child ast.Node) {
			if child == n {
				return
			}

			s.walk(child, loopDepth, inCapture)
		})
	}
}

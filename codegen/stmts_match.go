package codegen

import (
	"fmt"
	"math/big"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// isExhaustiveEnumMatch returns true when every case pattern is a member of
// the same enum and all members of that enum are covered - making a default
// clause unnecessary for exhaustiveness.
// isExhaustiveStructMatch returns true when the struct match has at least one
// total pattern arm: a StructPattern with no literal constraints and no guard,
// or a default arm. Such an arm covers all remaining values.

func (cg *CodeGen) genMatch(block *ir.Block, s *ast.MatchStmt) (*ir.Block, error) {
	return cg.genMatchWithResult(block, s, nil)
}

func (cg *CodeGen) genMatchWithResult(block *ir.Block, s *ast.MatchStmt, resAlloca value.Value) (*ir.Block, error) {
	cg.prepareMatchGuards(s.Cases)
	// Pre-analyze move usage across all arms so the case bodies see the
	// partial-move set on the stack and emit balancing retains.  The
	// per-arm snapshot/restore happens inside each match variant's
	// case-body loop (see genMatch's int-switch path and
	// genDataMatch).
	partial := cg.analyzeMatchPartialMoves(s)
	if len(partial) > 0 {
		cg.partialMovedStack = append(cg.partialMovedStack, partial)

		defer func() {
			cg.partialMovedStack = cg.partialMovedStack[:len(cg.partialMovedStack)-1]
		}()
	}

	if s.IsType {
		return cg.genMatchType(block, s)
	}

	cg.scanMatchForUnreachable(s)

	// ADT match: `case Ctor(bindings):` or bare nullary-variant identifier.
	for _, c := range s.Cases {
		if cg.isDataMatchPattern(c.Pattern) {
			return cg.genDataMatch(block, s, resAlloca)
		}
	}

	// Struct-pattern match: use if-else chain dispatch.
	for _, c := range s.Cases {
		if _, ok := c.Pattern.(*ast.StructPattern); ok {
			return cg.genStructMatch(block, s, resAlloca)
		}
	}

	// Array-pattern match: use if-else chain dispatch on array length.
	for _, c := range s.Cases {
		if _, ok := c.Pattern.(*ast.ArrayPattern); ok {
			return cg.genArrayMatch(block, s, resAlloca)
		}
	}

	// Tuple-pattern match: `case (p1, p2, ...):`.  Each slot pattern
	// may be a wildcard, a binding identifier, a literal, or an ADT
	// ctor (Ok(x), Err(_), None, Some(v)) - which is what motivates
	// the dedicated path: the existing integer-switch fallback
	// genExprs the pattern as an expression and trips on `Ok(x)`
	// trying to evaluate x in the scrutinee scope.
	for _, c := range s.Cases {
		if _, ok := c.Pattern.(*ast.TupleLit); ok {
			return cg.genTupleMatch(block, s, resAlloca)
		}
	}

	expr, err := cg.genExpr(block, s.Expr)
	if err != nil {
		return nil, err
	}
	// genExpr may have advanced curBlock (e.g. `match await f:` spawns
	// new blocks inside the await lowering). The switch must terminate
	// the block where execution actually lands, not the pre-await one
	// that's already closed by the await's own terminator.
	if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
	}

	afterBlock := cg.newBlock("match.after")

	defaultBlock := afterBlock
	if s.Default != nil {
		defaultBlock = cg.newBlock("match.default")
	}

	// Build cases.
	var (
		cases      []*ir.Case
		caseBlocks []*ir.Block
	)

	for i, c := range s.Cases {
		caseBlock := cg.newBlock(fmt.Sprintf("match.case.%d", i))
		caseBlocks = append(caseBlocks, caseBlock)

		pat, err := cg.genExpr(block, c.Pattern)
		if err != nil {
			return nil, err
		}

		if constPat, ok := pat.(constant.Constant); ok {
			intPat := cg.toConstInt(constPat, expr.Type())
			cases = append(cases, ir.NewCase(intPat, caseBlock))
		}
	}

	// Build switch. Use the natural type of the expression so case constants
	// always match the switch condition type (avoids i32 case vs i64 switch mismatch).
	switchExpr := expr
	switchType := expr.Type()
	// Re-build cases using the actual switch expression type so they match.
	for i, origCase := range cases {
		if constX, ok := origCase.X.(constant.Constant); ok {
			if target, ok2 := origCase.Target.(*ir.Block); ok2 {
				cases[i] = ir.NewCase(cg.toConstInt(constX, switchType), target)
			}
		}
	}

	block.NewSwitch(switchExpr, defaultBlock, cases...)

	// Generate case bodies. Track whether any arm fell through to afterBlock.
	// A match without a default is still exhaustive when every member of an
	// enum atom type is covered by an explicit case.
	anyFallthrough := s.Default == nil && !cg.isExhaustiveEnumMatch(s)
	// Per-arm move tracking: candidates is the union of moves across
	// all arms; the snapshot/restore lives in the genCaseBody closure
	// so each arm walks a fresh per-branch view of movedBindings.
	moveCandidates := map[string]bool{}

	for _, c := range s.Cases {
		for n := range collectMovedNames(c.Body) {
			moveCandidates[n] = true
		}
	}

	for n := range collectMovedNames(s.Default) {
		moveCandidates[n] = true
	}

	preMoveSnap := cg.snapshotMoveState(moveCandidates)
	branchSets := make([]map[string]bool, 0, len(s.Cases)+1)

	genCaseBody := func(caseBlock *ir.Block, body *ast.Block) (*ir.Block, error) {
		if resAlloca != nil {
			// Divergent arm (return/break/panic): emit via genStmt; the terminator
			// closes the block, and we skip the result store + br to afterBlock.
			if body != nil && len(body.Stmts) == 1 && isExplicitTerminator(body.Stmts[0]) {
				cg.curBlock = caseBlock

				var err2 error

				caseBlock, _, err2 = cg.genStmt(caseBlock, body)
				if err2 != nil {
					return nil, err2
				}

				return caseBlock, nil
			}

			// Expression mode: single-expression body (ExprStmt or nested MatchStmt).
			if body != nil && len(body.Stmts) == 1 {
				if expr := armExprNode(body.Stmts[0]); expr != nil {
					// Reset curBlock so a previous arm's inner-match advancement
					// doesn't pollute emission into caseBlock.
					cg.curBlock = caseBlock

					exprVal, err2 := cg.genExpr(caseBlock, expr)
					if err2 != nil {
						return nil, err2
					}

					if cg.curBlock != caseBlock {
						caseBlock = cg.curBlock
					}

					if exprVal != nil {
						resType := resAlloca.Type().(*irtypes.PointerType).ElemType
						caseBlock.NewStore(cg.coerce(caseBlock, exprVal, resType), resAlloca)
					}
				}
			}

			caseBlock.NewBr(afterBlock)

			return nil, nil
		}

		cg.curScope = newScope(cg.curScope)

		var err2 error

		caseBlock, _, err2 = cg.genStmt(caseBlock, body)

		// ARC: release case-body scope vars before falling through to afterBlock.
		cg.emitScopeRelease(caseBlock, cg.curScope)
		cg.curScope = cg.curScope.parent

		return caseBlock, err2
	}

	for i, c := range s.Cases {
		var caseBlock *ir.Block
		if i < len(caseBlocks) {
			caseBlock = caseBlocks[i]
		} else {
			caseBlock = cg.newBlock(fmt.Sprintf("match.case.%d", i))
		}

		caseBlock, err = genCaseBody(caseBlock, c.Body)
		if err != nil {
			return nil, err
		}

		branchSets = append(branchSets, diffBranchMoves(preMoveSnap.moved, cg.movedBindings))
		cg.restoreMoveState(preMoveSnap)

		if resAlloca != nil {
			// Expression mode: genCaseBody returns nil for non-divergent arms
			// (which already branch to afterBlock). Divergent arms return a
			// terminated block; nothing reaches afterBlock from them.
			if caseBlock == nil {
				anyFallthrough = true
			}
		} else if caseBlock != nil && caseBlock.Term == nil {
			caseBlock.NewBr(afterBlock)

			anyFallthrough = true
		}
	}

	// Default.
	if s.Default != nil {
		defaultBlock, err = genCaseBody(defaultBlock, s.Default)
		if err != nil {
			return nil, err
		}

		branchSets = append(branchSets, diffBranchMoves(preMoveSnap.moved, cg.movedBindings))
		cg.restoreMoveState(preMoveSnap)

		if resAlloca != nil {
			if defaultBlock == nil {
				anyFallthrough = true
			}
		} else if defaultBlock != nil && defaultBlock.Term == nil {
			defaultBlock.NewBr(afterBlock)

			anyFallthrough = true
		}
	} else {
		// No-default fall-through is an empty-move branch -- same
		// rationale as the no-else path in genIf.  Add an empty set
		// so the intersection logic sees the no-move path.
		branchSets = append(branchSets, map[string]bool{})
	}

	cg.movedBindings = cg.commitMergedMoves(branchSets, preMoveSnap.moved)

	// All arms terminated - afterBlock is unreachable; signal exhaustive termination.
	if !anyFallthrough {
		afterBlock.NewUnreachable()

		return nil, nil
	}

	return afterBlock, nil
}

func (cg *CodeGen) toConstInt(c constant.Constant, targetType irtypes.Type) *constant.Int {
	if ci, ok := c.(*constant.Int); ok {
		if it, ok2 := targetType.(*irtypes.IntType); ok2 {
			// Preserve the full big.Int magnitude when the target is at least
			// as wide as the source (e.g. i128 case against an i128 switch
			// expression). Calling X.Int64() here would silently truncate
			// 99999999999999999999 to its bottom 64 bits.
			if uint(it.BitSize) >= uint(ci.Typ.BitSize) {
				return &constant.Int{Typ: it, X: new(big.Int).Set(ci.X)}
			}

			return constant.NewInt(it, ci.X.Int64())
		}

		return ci
	}

	return constant.NewInt(irtypes.I64, 0)
}

// genAwaitMatch implements:
//
//	await match [a, b, c]:
//	  case [x, _, _] if guard: body
//	  case [_, y, _]: body
//	  default: body
//
// Without default: blocks until a future fires and a guard passes; re-blocks if
// a future fires but its guard fails; panics if all futures are exhausted with
// no guard passing.
//
// With default (Go select semantics): one non-blocking check; if nothing is
// actionable (no future done with a passing guard), runs the default body.

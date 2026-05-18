package codegen

import (
	"github.com/llir/llvm/ir"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) genFoldedIf(block *ir.Block, s *ast.IfStmt, takeThen bool) (*ir.Block, bool, error) {
	if takeThen {
		return cg.genFoldedBranch(block, s.Then)
	}
	// Initial cond is false; consider elifs.
	for i, elif := range s.ElseIfs {
		v, ok := cg.foldedBoolCondition(elif.Cond)
		if !ok {
			// Re-fall to the runtime path with the remaining (possibly
			// shorter) chain so the parts we did fold are still elided.
			rest := &ast.IfStmt{
				Cond:    elif.Cond,
				Then:    elif.Body,
				ElseIfs: s.ElseIfs[i+1:],
				Else:    s.Else,
			}

			return cg.genIfRuntime(block, rest)
		}

		if v {
			return cg.genFoldedBranch(block, elif.Body)
		}
	}
	// All conds folded false; emit else if present.
	if s.Else != nil {
		return cg.genFoldedBranch(block, s.Else)
	}
	// No live branch and no else: control falls through.
	return block, false, nil
}

// genFoldedBranch emits a body block that takes over from `block`, opens
// a fresh scope, releases it on exit (when not already terminated), and
// returns the post-body state in the same shape as genIf would.
func (cg *CodeGen) genFoldedBranch(block *ir.Block, body *ast.Block) (*ir.Block, bool, error) {
	cg.curScope = newScope(cg.curScope)

	cur, term, err := cg.genBlock(block, body)
	if cur == nil {
		cur = block
	}

	cg.emitScopeRelease(cur, cg.curScope)
	cg.curScope = cg.curScope.parent

	if err != nil {
		return nil, false, err
	}

	return cur, term || cur.Term != nil, nil
}

// genIfRuntime is the original genIf, used as a fallback by genFoldedIf
// when only a prefix of the chain folded.
func (cg *CodeGen) genIfRuntime(block *ir.Block, s *ast.IfStmt) (*ir.Block, bool, error) {
	return cg.genIf(block, s)
}

func (cg *CodeGen) genIf(block *ir.Block, s *ast.IfStmt) (*ir.Block, bool, error) {
	// Simplify the condition (De Morgan, double-negation, comparison
	// inversion, bool-literal absorption) and emit an "always true/false"
	// warning when the simplified form folds to a constant.
	s.Cond = cg.prepareBoolCond(s.Cond, "if", false)
	for i := range s.ElseIfs {
		s.ElseIfs[i].Cond = cg.prepareBoolCond(s.ElseIfs[i].Cond, "elif", false)
	}
	// Try to constant-fold the condition. When it folds we elide the
	// dead branch entirely so the strict per-arg type check at call sites
	// doesn't trip on type-incorrect code that would never execute (the
	// canonical case is `if typeof(v) == 'string: ... v as string` inside
	// a monomorphized generic where T isn't string). See codegen/fold.go.
	if v, ok := cg.foldedBoolCondition(s.Cond); ok {
		return cg.genFoldedIf(block, s, v)
	}

	mergeBlock := cg.newBlock("if.merge")

	// Reset cg.curBlock to the entry block before evaluating the condition.
	// Stale curBlock values left by prior statements (e.g. genBinExpr sets it
	// to the condition-check block of the PREVIOUS if) cause the arg-loop in
	// genCallExpr to emit the call into a non-dominating block while the arg
	// loads already went into the correct block, violating SSA dominance.
	// This mirrors the identical reset done for elif chains (see below).
	cg.curBlock = block

	cond, err := cg.genExpr(block, s.Cond)
	if err != nil {
		return nil, false, err
	}

	// genExpr may have advanced cg.curBlock through short-circuit && / ||
	// (genLogicalAnd/Or update curBlock to their merge block). Continue
	// emitting the cond-branch in that block, not the original.
	condEnd := block
	if cg.curBlock != nil {
		condEnd = cg.curBlock
	}

	cond = cg.toBoolImplicit(condEnd, cond)

	thenBlock := cg.newBlock("if.then")

	var elseStart *ir.Block
	if s.Else != nil || len(s.ElseIfs) > 0 {
		elseStart = cg.newBlock("if.else")
	} else {
		elseStart = mergeBlock
	}

	condEnd.NewCondBr(cond, thenBlock, elseStart)

	// Then branch.
	cg.curScope = newScope(cg.curScope)

	thenCurBlock, thenTerm, err := cg.genBlock(thenBlock, s.Then)
	if thenCurBlock == nil {
		thenCurBlock = thenBlock
	}
	// ARC: release scope before popping (only when not already terminated).
	cg.emitScopeRelease(thenCurBlock, cg.curScope)
	cg.curScope = cg.curScope.parent

	if err != nil {
		return nil, false, err
	}

	thenTerminated := thenTerm || thenCurBlock.Term != nil
	if !thenTerminated {
		thenCurBlock.NewBr(mergeBlock)
	}

	// ElseIf chains.
	allElifTerminated := true
	currentElse := elseStart

	for _, elif := range s.ElseIfs {
		nextBlock := cg.newBlock("elif.next")

		// Reset curBlock to the condition-check block before generating the
		// condition expression.  genBlock for the previous elif body may have
		// left curBlock pointing to a block inside that body, which would cause
		// genExpr to emit loads in the wrong (non-dominating) block.
		cg.curBlock = currentElse

		elifCond, err := cg.genExpr(currentElse, elif.Cond)
		if err != nil {
			return nil, false, err
		}

		// Pick up curBlock in case the elif's cond was short-circuited.
		elifCondEnd := currentElse
		if cg.curBlock != nil {
			elifCondEnd = cg.curBlock
		}

		elifCond = cg.toBoolImplicit(elifCondEnd, elifCond)
		elifThen := cg.newBlock("elif.then")
		elifCondEnd.NewCondBr(elifCond, elifThen, nextBlock)

		cg.curScope = newScope(cg.curScope)

		elifCurBlock, elifTerm, err := cg.genBlock(elifThen, elif.Body)
		if elifCurBlock == nil {
			elifCurBlock = elifThen
		}

		cg.emitScopeRelease(elifCurBlock, cg.curScope)
		cg.curScope = cg.curScope.parent

		if err != nil {
			return nil, false, err
		}

		elifTerminated := elifTerm || elifCurBlock.Term != nil
		if !elifTerminated {
			elifCurBlock.NewBr(mergeBlock)

			allElifTerminated = false
		}

		currentElse = nextBlock
	}

	// Else branch.
	elseTerminated := false

	if s.Else != nil {
		cg.curScope = newScope(cg.curScope)

		elseCurBlock, elseTerm, err := cg.genBlock(currentElse, s.Else)
		if elseCurBlock == nil {
			elseCurBlock = currentElse
		}

		cg.emitScopeRelease(elseCurBlock, cg.curScope)
		cg.curScope = cg.curScope.parent

		if err != nil {
			return nil, false, err
		}

		elseTerminated = elseTerm || elseCurBlock.Term != nil
		if !elseTerminated {
			elseCurBlock.NewBr(mergeBlock)
		}
	} else if currentElse != mergeBlock && currentElse.Term == nil {
		currentElse.NewBr(mergeBlock)
	}

	// Only add unreachable to mergeBlock if ALL branches terminated (returned/
	// branched elsewhere). When there is no else clause, the false path always
	// reaches mergeBlock, so it can never be unreachable.
	allTerminated := thenTerminated && allElifTerminated && (s.Else != nil && elseTerminated)
	if mergeBlock.Term == nil && allTerminated {
		mergeBlock.NewUnreachable()
	}

	return mergeBlock, allTerminated, nil
}

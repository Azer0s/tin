package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) genStructMatch(block *ir.Block, s *ast.MatchStmt, resAlloca value.Value) (*ir.Block, error) {
	scrutinee, err := cg.genExpr(block, s.Expr)
	if err != nil {
		return nil, err
	}

	if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
	}

	scrutType := scrutinee.Type()
	// Auto-deref pointer to named struct.
	if pt, ok := scrutType.(*irtypes.PointerType); ok {
		if cg.typeNameOf(pt.ElemType) != "" {
			scrutinee = block.NewLoad(pt.ElemType, scrutinee)
			scrutType = pt.ElemType
		}
	}

	scrutAlloca := block.NewAlloca(scrutType)
	block.NewStore(scrutinee, scrutAlloca)

	structName := cg.typeNameOf(scrutType)

	afterBlock := cg.newBlock("match.after")
	anyFallthrough := false

	curCheckBlock := block

	for i, c := range s.Cases {
		sp, ok := c.Pattern.(*ast.StructPattern)
		if !ok {
			return nil, fmt.Errorf("genStructMatch: non-struct pattern in struct match (case %d)", i)
		}

		nextCaseBlock := cg.newBlock(fmt.Sprintf("match.next.%d", i))
		bodyBlock := cg.newBlock(fmt.Sprintf("match.case.%d", i))
		checkBlock := curCheckBlock

		// Emit constraint checks for this pattern (recurses into nested StructPatterns).
		passSeq := 0

		var err2 error

		checkBlock, err2 = cg.applyPatternChecks(checkBlock, nextCaseBlock, scrutType, scrutAlloca, structName, sp, i, &passSeq)
		if err2 != nil {
			return nil, err2
		}

		// Bind free fields (including nested) before the guard so guard expressions
		// can reference them. Allocas in checkBlock dominate bodyBlock and nextCaseBlock.
		cg.curScope = newScope(cg.curScope)

		if err2 = cg.bindPatternFree(checkBlock, scrutType, scrutAlloca, structName, sp); err2 != nil {
			cg.curScope = cg.curScope.parent

			return nil, err2
		}

		// After field constraints (and bindings): apply guard if present.
		if c.Guard != nil {
			guardVal, err2 := cg.genExpr(checkBlock, c.Guard)
			if err2 != nil {
				cg.curScope = cg.curScope.parent

				return nil, err2
			}

			checkBlock.NewCondBr(cg.toBoolImplicit(checkBlock, guardVal), bodyBlock, nextCaseBlock)
		} else {
			checkBlock.NewBr(bodyBlock)
		}

		var bodyErr error

		_, bodyErr = cg.emitMatchArmBody(c, bodyBlock, afterBlock, resAlloca, &anyFallthrough)
		cg.curScope = cg.curScope.parent

		if bodyErr != nil {
			return nil, bodyErr
		}

		curCheckBlock = nextCaseBlock
	}

	// Default or exhaustiveness fallthrough.
	var defaultErr error

	_, defaultErr = cg.emitMatchDefaultArm(s, curCheckBlock, afterBlock, resAlloca, &anyFallthrough, cg.isExhaustiveStructMatch(s))
	if defaultErr != nil {
		return nil, defaultErr
	}

	if !anyFallthrough && resAlloca == nil {
		afterBlock.NewUnreachable()

		return nil, nil
	}

	return afterBlock, nil
}

// isExhaustiveArrayMatch returns true when the match is guaranteed to cover
// every possible array length.  Exhaustive when:
//   - default: arm is present, or
//   - a guard-free [...xs] arm catches everything, or
//   - the union of exact-length arms (guard-free) and the minimum-length
//     intervals of rest arms (guard-free) covers all non-negative integers.
//
// Example: []  +  [x, ...xs]  ->  {0} union [1,inf) = [0,inf)  -> exhaustive.
func (cg *CodeGen) isExhaustiveArrayMatch(s *ast.MatchStmt) bool {
	if s.Default != nil {
		return true
	}

	exactLengths := make(map[int]bool)
	minRestCover := -1 // smallest min-length seen across rest arms; -1 = none

	for _, c := range s.Cases {
		ap, ok := c.Pattern.(*ast.ArrayPattern)
		if !ok || c.Guard != nil {
			continue
		}

		hasRest := false
		fixed := 0

		for _, e := range ap.Elems {
			if e.IsRest {
				hasRest = true
			} else {
				fixed++
			}
		}

		if hasRest {
			// This arm covers [fixed, inf).
			if minRestCover < 0 || fixed < minRestCover {
				minRestCover = fixed
			}
		} else {
			exactLengths[fixed] = true
		}
	}

	if minRestCover < 0 {
		return false // no rest arm
	}

	// Check that every integer 0 .. minRestCover-1 is covered by exact arms.
	for i := 0; i < minRestCover; i++ {
		if !exactLengths[i] {
			return false
		}
	}

	return true
}

// emitMatchDefaultArm emits the default arm (or exhaustiveness terminator) that
// is shared by genStructMatch and genArrayMatch.  It mutates anyFallthrough
// through the supplied pointer and returns the (possibly updated) curCheckBlock.
func (cg *CodeGen) emitMatchDefaultArm(
	s *ast.MatchStmt,
	curCheckBlock *ir.Block,
	afterBlock *ir.Block,
	resAlloca value.Value,
	anyFallthrough *bool,
	isExhaustive bool,
) (*ir.Block, error) {
	if s.Default != nil {
		if resAlloca != nil {
			if len(s.Default.Stmts) == 1 && isExplicitTerminator(s.Default.Stmts[0]) {
				// Divergent default arm (return/break/panic): emit via genStmt and
				// let the terminator close the block. No store, no branch to after.
				cg.curBlock = curCheckBlock

				var err2 error

				curCheckBlock, _, err2 = cg.genStmt(curCheckBlock, s.Default)
				if err2 != nil {
					return nil, err2
				}

				return curCheckBlock, nil
			}

			if len(s.Default.Stmts) == 1 {
				if expr := armExprNode(s.Default.Stmts[0]); expr != nil {
					// Reset curBlock so a previous arm's inner-match advancement
					// doesn't pollute emission into curCheckBlock.
					cg.curBlock = curCheckBlock

					exprVal, err2 := cg.genExpr(curCheckBlock, expr)
					if err2 != nil {
						return nil, err2
					}

					if cg.curBlock != curCheckBlock {
						curCheckBlock = cg.curBlock
					}

					if exprVal != nil {
						resType := resAlloca.Type().(*irtypes.PointerType).ElemType
						curCheckBlock.NewStore(cg.coerce(curCheckBlock, exprVal, resType), resAlloca)
					}
				}
			}

			curCheckBlock.NewBr(afterBlock)

			*anyFallthrough = true
		} else {
			cg.curScope = newScope(cg.curScope)

			var err2 error

			curCheckBlock, _, err2 = cg.genStmt(curCheckBlock, s.Default)
			cg.curScope = cg.curScope.parent

			if err2 != nil {
				return nil, err2
			}

			if curCheckBlock != nil && curCheckBlock.Term == nil {
				curCheckBlock.NewBr(afterBlock)

				*anyFallthrough = true
			}
		}
	} else if isExhaustive {
		curCheckBlock.NewUnreachable()
	} else {
		curCheckBlock.NewBr(afterBlock)

		*anyFallthrough = true
	}

	return curCheckBlock, nil
}

// emitMatchArmBody emits the body of a single match arm in expression or
// statement mode.  Scope management (push/pop) is the caller's responsibility.
// On error the caller must still pop the scope before propagating.
func (cg *CodeGen) emitMatchArmBody(
	c ast.MatchCase,
	bodyBlock *ir.Block,
	afterBlock *ir.Block,
	resAlloca value.Value,
	anyFallthrough *bool,
) (*ir.Block, error) {
	if resAlloca != nil {
		// Divergent arm (return/break/panic): emit via genStmt; the terminator
		// closes the block, and we skip the result store + br to afterBlock.
		// genStmt for ReturnStmt already calls emitAllScopeReleases.
		if c.Body != nil && len(c.Body.Stmts) == 1 && isExplicitTerminator(c.Body.Stmts[0]) {
			cg.curBlock = bodyBlock

			var err2 error

			bodyBlock, _, err2 = cg.genStmt(bodyBlock, c.Body)
			if err2 != nil {
				return nil, err2
			}

			return bodyBlock, nil
		}

		// Expression mode: single-expression body (ExprStmt or nested MatchStmt).
		if c.Body != nil && len(c.Body.Stmts) == 1 {
			if expr := armExprNode(c.Body.Stmts[0]); expr != nil {
				// Reset curBlock so a previous arm's inner-match advancement
				// doesn't pollute emission into bodyBlock.
				cg.curBlock = bodyBlock

				exprVal, err2 := cg.genExpr(bodyBlock, expr)
				if err2 != nil {
					return nil, err2
				}

				// genExpr may have advanced cg.curBlock (e.g. inner match expression).
				if cg.curBlock != bodyBlock {
					bodyBlock = cg.curBlock
				}

				if exprVal != nil {
					resType := resAlloca.Type().(*irtypes.PointerType).ElemType
					coerced := cg.coerce(bodyBlock, exprVal, resType)
					bodyBlock.NewStore(coerced, resAlloca)
					// The match-scrut release that runs at afterBlock
					// walks the active variant's payload and decrements
					// any pointer fields the arm just transferred out
					// via this store.  Without an extra retain here, the
					// caller's load from resAlloca would see freed
					// pointers (e.g. a *rc::Cell payload that the
					// scrutinee release dropped to rc=0).  Walk the
					// stored value's fields and retain inner Tin-managed
					// pointers; primitive values and borrowed pointers
					// are no-ops.
					cg.emitStructFieldRetain(bodyBlock, coerced)
				}
			}
		}

		// Release match-arm-bound variables (e.g. ...rest subslice) before
		// branching out of the arm.  This mirrors the scope-release emitted at
		// normal function exit but is scoped to only the arm's own bindings.
		cg.emitScopeRelease(bodyBlock, cg.curScope)
		bodyBlock.NewBr(afterBlock)

		*anyFallthrough = true
	} else {
		var err2 error

		bodyBlock, _, err2 = cg.genStmt(bodyBlock, c.Body)
		if err2 != nil {
			return nil, err2
		}

		if bodyBlock != nil && bodyBlock.Term == nil {
			// Release match-arm-bound variables (e.g. ...rest subslice) before
			// branching out of the arm.  Explicit returns inside the arm already
			// call emitAllScopeReleases, so this only runs when control falls
			// through to afterBlock.
			cg.emitScopeRelease(bodyBlock, cg.curScope)
			bodyBlock.NewBr(afterBlock)

			*anyFallthrough = true
		}
	}

	return bodyBlock, nil
}

// genArrayMatch generates an if-else chain for match statements whose cases
// use array destructuring patterns.  resAlloca is non-nil in expression mode.
//
// Each case checks:
//   - No rest: exact length match (len == n)
//   - Rest at end: minimum length match (len >= n-1)
//
// Variables are bound to individual elements (or a sub-slice for rest).

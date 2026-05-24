package codegen

// builtin_nrect.go - nrect(arr) reports whether a (possibly nested)
// array is rectangular: every sub-array at the same depth has the
// same length.  Depth-1 inputs (flat arrays, strings) are vacuously
// rectangular and short-circuit to a constant `true`.
//
//   nrect([1, 2, 3])                    -> true
//   nrect([[1, 2], [3, 4]])             -> true
//   nrect([[1, 2, 3], [4]])             -> false
//   nrect([])                           -> true  (no rows to compare)
//   nrect([[]] : [[i64]])               -> true  (one empty row, vacuously rect)
//
// Cost is O(total elements) -- the verifier walks every element at
// every interior depth.  Reach for nrect when shape integrity matters
// (BLAS kernels, image processing, anything that flattens to a
// contiguous buffer); nlen alone only samples arr[0] at each level.

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// genBuiltinNrect implements the nrect(expr) built-in.
func (cg *CodeGen) genBuiltinNrect(block *ir.Block, arg ast.Node) (value.Value, error) {
	val, err := cg.genExpr(block, arg)
	if err != nil {
		return nil, err
	}

	if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
	}

	t := val.Type()
	if !isFatArrayPtr(t) && !isStringType(t) {
		return nil, fmt.Errorf("nrect() requires an array or string argument, got %s",
			cg.tinTypeDisplay(t))
	}

	depth := fatArrayDepth(t)

	// Depth 0 (shouldn't happen given the guard above) or depth 1
	// (flat arrays, strings) are trivially rectangular: there's no
	// interior level whose sub-arrays could mismatch.
	if depth <= 1 {
		if isRCTrackedType(t) && !isCopyExpr(arg) {
			cg.emitRelease(block, val)
		}

		return constant.NewBool(true), nil
	}

	// Result lives in an alloca so any failing path can stamp `false`
	// and bail to join without phi bookkeeping.
	resultAlloca := block.NewAlloca(irtypes.I1)
	block.NewStore(constant.NewBool(true), resultAlloca)

	joinBlk := cg.newBlock("nrect.join")

	// Step 1: descend via [0] at each level to compute the canonical
	// length expected at every interior depth.  If descent ever lands
	// on an empty layer there's no interior to verify, so we bail to
	// join with result still true.
	canonical := make([]value.Value, depth)
	descBlk := block
	descVal := val
	descType := t

	for i := 0; i < depth; i++ {
		canonical[i] = descBlk.NewExtractValue(descVal, 1)

		if i+1 < depth {
			cond := descBlk.NewICmp(enum.IPredSGT, canonical[i],
				constant.NewInt(irtypes.I64, 0))
			descendBlk := cg.newBlock(fmt.Sprintf("nrect.canon.%d", i+1))
			descBlk.NewCondBr(cond, descendBlk, joinBlk)

			innerStruct := descType.(*irtypes.StructType)
			innerElemType := innerStruct.Fields[0].(*irtypes.PointerType).ElemType

			arrDataPtr := descendBlk.NewExtractValue(descVal, 0)
			innerVal := descendBlk.NewLoad(innerElemType, arrDataPtr)

			descBlk = descendBlk
			descVal = innerVal
			descType = innerElemType
		}
	}

	// Step 2: emit nested loops, one per interior level (depth-1
	// loops total).  At each level we iterate every sub-array and
	// verify its length equals canonical[level+1].  A mismatch stores
	// `false` and jumps to join.
	afterBlk := cg.emitNrectCheck(descBlk, val, t, 0, depth, canonical, resultAlloca, joinBlk)
	afterBlk.NewBr(joinBlk)

	result := joinBlk.NewLoad(irtypes.I1, resultAlloca)

	// Release the outer argument if it's a temporary RC allocation.
	if isRCTrackedType(t) && !isCopyExpr(arg) {
		cg.emitRelease(joinBlk, val)
	}

	cg.curBlock = joinBlk

	return result, nil
}

// emitNrectCheck emits a counted loop over `arr`'s elements at the
// given `level`, verifying that each element's length matches the
// canonical dim for one level deeper.  Recurses into each element
// when there are more interior levels to check.  Returns the block
// where the caller should continue (the "after this loop" block).
//
//	level                0 (outer) .. depth-2 (innermost interior)
//	arrType              type of arr at this level (fat-pointer shape)
//	canonical[level+1]   the length every sub-array at this level
//	                     must have
func (cg *CodeGen) emitNrectCheck(
	curBlk *ir.Block,
	arr value.Value,
	arrType irtypes.Type,
	level, depth int,
	canonical []value.Value,
	resultAlloca value.Value,
	joinBlk *ir.Block,
) *ir.Block {
	// At the innermost scalar level there's nothing left to verify.
	if level >= depth-1 {
		return curBlk
	}

	arrLen := curBlk.NewExtractValue(arr, 1)
	arrData := curBlk.NewExtractValue(arr, 0)

	innerStruct := arrType.(*irtypes.StructType)
	innerElemType := innerStruct.Fields[0].(*irtypes.PointerType).ElemType

	iAlloca := curBlk.NewAlloca(irtypes.I64)
	curBlk.NewStore(constant.NewInt(irtypes.I64, 0), iAlloca)

	headerBlk := cg.newBlock(fmt.Sprintf("nrect.loop.header.%d", level))
	bodyBlk := cg.newBlock(fmt.Sprintf("nrect.loop.body.%d", level))
	failBlk := cg.newBlock(fmt.Sprintf("nrect.fail.%d", level))
	afterBlk := cg.newBlock(fmt.Sprintf("nrect.after.%d", level))

	curBlk.NewBr(headerBlk)

	// header: i < arrLen ?
	iCur := headerBlk.NewLoad(irtypes.I64, iAlloca)
	iLtLen := headerBlk.NewICmp(enum.IPredSLT, iCur, arrLen)
	headerBlk.NewCondBr(iLtLen, bodyBlk, afterBlk)

	// body: load arr[i], compare its length, possibly recurse.
	iBody := bodyBlk.NewLoad(irtypes.I64, iAlloca)
	elemPtr := bodyBlk.NewGetElementPtr(innerElemType, arrData, iBody)
	elem := bodyBlk.NewLoad(innerElemType, elemPtr)
	elemLen := bodyBlk.NewExtractValue(elem, 1)

	lenOk := bodyBlk.NewICmp(enum.IPredEQ, elemLen, canonical[level+1])
	continueBlk := cg.newBlock(fmt.Sprintf("nrect.continue.%d", level))
	bodyBlk.NewCondBr(lenOk, continueBlk, failBlk)

	failBlk.NewStore(constant.NewBool(false), resultAlloca)
	failBlk.NewBr(joinBlk)

	// Recurse into elem for the next interior level.  The recursion
	// returns the block where post-recursion code (i++, loop back)
	// should run.
	postRecurseBlk := cg.emitNrectCheck(continueBlk, elem, innerElemType,
		level+1, depth, canonical, resultAlloca, joinBlk)

	iAfter := postRecurseBlk.NewAdd(iBody, constant.NewInt(irtypes.I64, 1))
	postRecurseBlk.NewStore(iAfter, iAlloca)
	postRecurseBlk.NewBr(headerBlk)

	return afterBlk
}

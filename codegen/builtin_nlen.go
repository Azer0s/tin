package codegen

// builtin_nlen.go - nlen(arr) returns the dimensions of a (possibly
// nested) array as an [i64], one element per static layer.  The
// elements are read at runtime by descending through arr[0] at each
// level; when descent hits an empty layer the remaining inner dims
// are reported as 0 (so the runtime never dereferences a null data
// pointer).
//
//   nlen([1, 2, 3])                              -> [3]
//   nlen([[1, 2], [3, 4]])                       -> [2, 2]
//   nlen([[[1,2],[3,4]],[[5,6],[7,8]]])          -> [2, 2, 2]
//   nlen([])                                     -> [0]
//
// Strings count as a single layer (their element type is byte, not
// itself a fat-array), so nlen("abc") returns [3] like len("abc") = 3.

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// genBuiltinNlen implements the nlen(expr) built-in.
func (cg *CodeGen) genBuiltinNlen(block *ir.Block, arg ast.Node) (value.Value, error) {
	val, err := cg.genExpr(block, arg)
	if err != nil {
		return nil, err
	}

	if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
	}

	t := val.Type()
	if !isFatArrayPtr(t) && !isStringType(t) {
		return nil, fmt.Errorf("nlen() requires an array or string argument, got %s",
			cg.tinTypeDisplay(t))
	}

	depth := fatArrayDepth(t)

	// Allocate `depth * 8` bytes of i64 storage through the ARC
	// allocator so the returned fat-array participates in normal RC
	// lifetime.  depth is always >= 1 here because we've already
	// proven t is a fat-pointer shape.
	totalSize := constant.NewInt(irtypes.I64, int64(depth)*8)
	rawBuf := block.NewCall(cg.ensureRCAlloc(), totalSize)
	dataPtr := block.NewBitCast(rawBuf, irtypes.NewPointer(irtypes.I64))

	// joinBlk: every descent and zero-fill path converges here for
	// the final fat-pointer construction.
	joinBlk := cg.newBlock("nlen.join")

	curBlk := block
	curVal := val
	curType := t

	for i := 0; i < depth; i++ {
		curLen := curBlk.NewExtractValue(curVal, 1)
		slotPtr := curBlk.NewGetElementPtr(irtypes.I64, dataPtr,
			constant.NewInt(irtypes.I64, int64(i)))
		curBlk.NewStore(curLen, slotPtr)

		if i+1 < depth {
			// Branch: if the current layer is non-empty, descend into
			// arr[0]; otherwise zero-fill the remaining slots and
			// jump straight to join.  Skipping the load on empty
			// layers is mandatory: their data pointer is null.
			cond := curBlk.NewICmp(enum.IPredSGT, curLen,
				constant.NewInt(irtypes.I64, 0))
			descendBlk := cg.newBlock(fmt.Sprintf("nlen.descend.%d", i+1))
			zeroBlk := cg.newBlock(fmt.Sprintf("nlen.zero.%d", i+1))
			curBlk.NewCondBr(cond, descendBlk, zeroBlk)

			for j := i + 1; j < depth; j++ {
				slot := zeroBlk.NewGetElementPtr(irtypes.I64, dataPtr,
					constant.NewInt(irtypes.I64, int64(j)))
				zeroBlk.NewStore(constant.NewInt(irtypes.I64, 0), slot)
			}

			zeroBlk.NewBr(joinBlk)

			// Borrow curVal[0] - the outer array still owns the
			// inner storage, so no retain is needed.
			innerStruct := curType.(*irtypes.StructType)
			innerPtrType := innerStruct.Fields[0].(*irtypes.PointerType)
			innerElemType := innerPtrType.ElemType

			arrDataPtr := descendBlk.NewExtractValue(curVal, 0)
			innerVal := descendBlk.NewLoad(innerElemType, arrDataPtr)

			curBlk = descendBlk
			curVal = innerVal
			curType = innerElemType
		} else {
			curBlk.NewBr(joinBlk)
		}
	}

	fatType := irtypes.NewStruct(irtypes.NewPointer(irtypes.I64), irtypes.I64)
	fatAlloca := joinBlk.NewAlloca(fatType)
	ptrGep := joinBlk.NewGetElementPtr(fatType, fatAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	joinBlk.NewStore(dataPtr, ptrGep)
	lenGep := joinBlk.NewGetElementPtr(fatType, fatAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	joinBlk.NewStore(constant.NewInt(irtypes.I64, int64(depth)), lenGep)

	result := joinBlk.NewLoad(fatType, fatAlloca)

	// Release the outer argument if it's a temporary RC allocation
	// (e.g. nlen(make_matrix()) - the matrix is fresh and unowned).
	if isRCTrackedType(t) && !isCopyExpr(arg) {
		cg.emitRelease(joinBlk, val)
	}

	cg.curBlock = joinBlk

	return result, nil
}

// fatArrayDepth counts the number of nested fat-pointer layers in t.
// Strings ({i8*, i64}) count as depth 1: their element type is i8,
// which isn't itself a fat-pointer, so the walk stops after one step.
// Returns 0 if t isn't a fat-pointer shape at all.
func fatArrayDepth(t irtypes.Type) int {
	depth := 0
	for isFatArrayPtr(t) || isStringType(t) {
		depth++
		st := t.(*irtypes.StructType)
		ptr := st.Fields[0].(*irtypes.PointerType)
		t = ptr.ElemType
	}

	return depth
}

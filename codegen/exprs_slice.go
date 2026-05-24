package codegen

import (
	"fmt"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) genPtrRangeSlice(block *ir.Block, ptrExpr ast.Node, loExpr ast.Node, hiExpr ast.Node) (value.Value, error) {
	loVal, err := cg.genExpr(block, loExpr)
	if err != nil {
		return nil, err
	}

	hiVal, err := cg.genExpr(block, hiExpr)
	if err != nil {
		return nil, err
	}

	loVal = cg.coerce(block, loVal, irtypes.I64)
	hiVal = cg.coerce(block, hiVal, irtypes.I64)

	// Try the lvalue path first: a fixed-size array decays to a pointer
	// to its element type. Falls back to the rvalue path for raw *T or
	// non-addressable expressions.
	var ptrVal value.Value

	if arrPtr, lvErr := cg.genLValue(block, ptrExpr); lvErr == nil && arrPtr != nil {
		if pt2, ok := arrPtr.Type().(*irtypes.PointerType); ok {
			if at, ok2 := pt2.ElemType.(*irtypes.ArrayType); ok2 {
				ptrVal = block.NewGetElementPtr(at, arrPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
			}
		}
	}

	if ptrVal == nil {
		v, err2 := cg.genExpr(block, ptrExpr)
		if err2 != nil {
			return nil, err2
		}

		ptrVal = v
	}

	// Fat-array source `arr[lo..hi]`: route through the same
	// copy-into-fresh-buffer path as `genSliceExpr` so the result is
	// an owned, freely-mutable `[T]`.
	if isFatArrayPtr(ptrVal.Type()) {
		fatType := ptrVal.Type().(*irtypes.StructType)
		dataPtrType := fatType.Fields[0].(*irtypes.PointerType)
		elemT := dataPtrType.ElemType

		srcDataPtr := block.NewExtractValue(ptrVal, 0)
		length := block.NewSub(hiVal, loVal)
		srcRange := block.NewGetElementPtr(elemT, srcDataPtr, loVal)

		nullElemPtr := constant.NewNull(irtypes.NewPointer(elemT))
		sizeGep := block.NewGetElementPtr(elemT, nullElemPtr,
			constant.NewInt(irtypes.I64, 1))
		elemSize := block.NewPtrToInt(sizeGep, irtypes.I64)
		totalBytes := block.NewMul(length, elemSize)

		newI8 := block.NewCall(cg.ensureRCAlloc(), totalBytes)
		newDataPtr := block.NewBitCast(newI8, irtypes.NewPointer(elemT))
		srcI8 := block.NewBitCast(srcRange, irtypes.I8Ptr)
		block.NewCall(cg.ensureMemcpy(), newI8, srcI8, totalBytes,
			constant.NewInt(irtypes.I1, 0))

		if isRCTrackedType(elemT) {
			cg.emitRetainElemSlice(block, newDataPtr, length, elemT)
		}

		return cg.buildFatArrayValue(block, elemT, newDataPtr, length, length), nil
	}

	pt, ok := ptrVal.Type().(*irtypes.PointerType)
	if !ok {
		return nil, fmt.Errorf("range slice requires a pointer or fat-array source, got %s", cg.fmtArgType(ptrVal.Type()))
	}

	length := block.NewSub(hiVal, loVal)
	startPtr := block.NewGetElementPtr(pt.ElemType, ptrVal, loVal)

	// *byte -> call _tin_bytes_from_buf for an ARC-managed [byte].
	if pt.ElemType.Equal(irtypes.I8) {
		return cg.callExtern(block, cg.ensureBytesFromBuf(), startPtr, length), nil
	}

	// Other pointer types: copy the [lo..hi) range into a fresh
	// RC-allocated buffer so the returned slice is owned and freely
	// mutable (`++=` works without surprising aliasing).  Cap == len:
	// the first append triggers a grow.
	nullElemPtr := constant.NewNull(irtypes.NewPointer(pt.ElemType))
	sizeGep := block.NewGetElementPtr(pt.ElemType, nullElemPtr,
		constant.NewInt(irtypes.I64, 1))
	elemSize := block.NewPtrToInt(sizeGep, irtypes.I64)
	totalBytes := block.NewMul(length, elemSize)
	mallocI8 := block.NewCall(cg.ensureRCAlloc(), totalBytes)
	newDataPtr := block.NewBitCast(mallocI8, irtypes.NewPointer(pt.ElemType))
	srcI8 := block.NewBitCast(startPtr, irtypes.I8Ptr)
	block.NewCall(cg.ensureMemcpy(), mallocI8, srcI8, totalBytes,
		constant.NewInt(irtypes.I1, 0))

	return cg.buildFatArrayValue(block, pt.ElemType, newDataPtr, length, length), nil
}

// genSliceExpr is the codegen entry point for the `arr[start:end]`
// form, which Tin used to accept as an alias for `arr[start..end]`.
// The `..` form is now canonical (consistent with `for i in 0..n` and
// visually distinct from `[T; N]`); this entry point exists solely to
// emit a clear migration hint when the user reaches for `:`.
//
// Open-ended ranges (`[:end]` and `[start:]`) are rejected the same
// way -- callers should write `[0..end]` / `[start..len(arr)]`.
func (cg *CodeGen) genSliceExpr(block *ir.Block, e *ast.SliceExpr) (value.Value, error) {
	lo := "0"
	if e.Start != nil {
		lo = strings.TrimSpace(ast.PrintExpr(e.Start))
	}

	hi := "n"
	if e.End != nil {
		hi = strings.TrimSpace(ast.PrintExpr(e.End))
	}

	return nil, cg.nodeErr(e, "range slice uses `..` (e.g. `arr[%s..%s]`), not `:`", lo, hi)
}

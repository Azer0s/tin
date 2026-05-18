package codegen

import (
	"fmt"

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

	pt, ok := ptrVal.Type().(*irtypes.PointerType)
	if !ok {
		return nil, fmt.Errorf("range slice requires a pointer, got %s", cg.fmtArgType(ptrVal.Type()))
	}

	length := block.NewSub(hiVal, loVal)
	startPtr := block.NewGetElementPtr(pt.ElemType, ptrVal, loVal)

	// *byte -> call _tin_bytes_from_buf for an ARC-managed [byte].
	if pt.ElemType.Equal(irtypes.I8) {
		return block.NewCall(cg.ensureBytesFromBuf(), startPtr, length), nil
	}

	// Other pointer types: build a non-owning fat pointer {T*, i64}.
	fatType := irtypes.NewStruct(irtypes.NewPointer(pt.ElemType), irtypes.I64)
	alloca := block.NewAlloca(fatType)
	ptrGep := block.NewGetElementPtr(fatType, alloca, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(startPtr, ptrGep)
	lenGep := block.NewGetElementPtr(fatType, alloca, constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(length, lenGep)

	return block.NewLoad(fatType, alloca), nil
}

// genSliceExpr generates code for a slice expression arr[start:end].
func (cg *CodeGen) genSliceExpr(block *ir.Block, e *ast.SliceExpr) (value.Value, error) {
	// Fixed-size byte arrays [byte; N]: heap-copy the slice to produce a [byte].
	// Use genLValue to get the alloca pointer directly (no spurious full-array load).
	if arrPtr, err2 := cg.genLValue(block, e.Expr); err2 == nil {
		if pt, ok := arrPtr.Type().(*irtypes.PointerType); ok {
			if at, ok2 := pt.ElemType.(*irtypes.ArrayType); ok2 && at.ElemType.Equal(irtypes.I8) {
				var startVal, endVal value.Value

				if e.Start != nil {
					sv, err := cg.genExpr(block, e.Start)
					if err != nil {
						return nil, err
					}

					startVal = cg.coerce(block, sv, irtypes.I64)
				} else {
					startVal = constant.NewInt(irtypes.I64, 0)
				}

				if e.End != nil {
					ev, err := cg.genExpr(block, e.End)
					if err != nil {
						return nil, err
					}

					endVal = cg.coerce(block, ev, irtypes.I64)
				} else {
					endVal = constant.NewInt(irtypes.I64, int64(at.Len))
				}

				length := block.NewSub(endVal, startVal)
				elemPtr := block.NewGetElementPtr(at, arrPtr,
					constant.NewInt(irtypes.I32, 0), startVal)
				srcPtr := block.NewBitCast(elemPtr, irtypes.I8Ptr)

				return block.NewCall(cg.ensureBytesFromBuf(), srcPtr, length), nil
			}
		}
	}

	arrVal, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}

	// Only fat-pointer arrays {T*, i64} are supported for slicing.
	arrType, ok := arrVal.Type().(*irtypes.StructType)
	if !ok || len(arrType.Fields) < 2 {
		return nil, fmt.Errorf("slice expression requires a fat-array type, got %s", cg.fmtArgType(arrVal.Type()))
	}

	ptrField := arrType.Fields[0]

	ptrType, isPtrType := ptrField.(*irtypes.PointerType)
	if !isPtrType {
		return nil, fmt.Errorf("slice expression: first field must be a pointer, got %s", ptrField)
	}

	elemType := ptrType.ElemType

	alloca := block.NewAlloca(arrType)
	block.NewStore(arrVal, alloca)

	// Extract data pointer and length from fat-array.
	dataGep := block.NewGetElementPtr(arrType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	lenGep := block.NewGetElementPtr(arrType, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	dataPtr := block.NewLoad(ptrType, dataGep)
	arrLen := block.NewLoad(irtypes.I64, lenGep)

	var startVal, endVal value.Value

	if e.Start != nil {
		sv, err := cg.genExpr(block, e.Start)
		if err != nil {
			return nil, err
		}

		startVal = cg.coerce(block, sv, irtypes.I64)
	} else {
		startVal = constant.NewInt(irtypes.I64, 0)
	}

	if e.End != nil {
		ev, err := cg.genExpr(block, e.End)
		if err != nil {
			return nil, err
		}

		endVal = cg.coerce(block, ev, irtypes.I64)
	} else {
		endVal = arrLen
	}

	// newDataPtr = GEP(elemType, dataPtr, startVal)
	newDataPtr := block.NewGetElementPtr(elemType, dataPtr, startVal)
	// newLen = endVal - startVal
	newLen := block.NewSub(endVal, startVal)

	// Build new fat-array {T*, i64}.
	resultAlloca := block.NewAlloca(arrType)
	newDataGep := block.NewGetElementPtr(arrType, resultAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	newLenGep := block.NewGetElementPtr(arrType, resultAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(newDataPtr, newDataGep)
	block.NewStore(newLen, newLenGep)

	// The slice escapes whatever scope-exit release the source binding
	// owns: when the source's RC hits 0, the underlying ARC block is
	// freed and the slice goes dangling.  Bump the base block's RC here
	// so the slice carries its own +1 reference; the matching release
	// fires either via genVarDecl's basePtr (when the slice is the
	// direct init of a let-binding) or via the fat-array's normal
	// scope-exit release (when the slice escapes through a call, e.g.
	// `return xs[0:m]` consumed by the caller).
	baseI8 := block.NewBitCast(dataPtr, irtypes.I8Ptr)
	block.NewCall(cg.ensureRetain(), baseI8)
	// Expose the BASE allocation pointer (before the GEP offset) so that genVarDecl
	// can retain/release the actual ARC block rather than a possibly-interior pointer.
	// For start==0 newDataPtr==dataPtr; for start>0 newDataPtr is interior.
	cg.lastSliceBase = baseI8

	return block.NewLoad(arrType, resultAlloca), nil
}

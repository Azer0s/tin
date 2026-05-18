package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) genBinExpr(block *ir.Block, e *ast.BinExpr) (value.Value, error) {
	// Short-circuit for && and ||.
	switch e.Op {
	case "&&":
		return cg.genLogicalAnd(block, e)
	case "||":
		return cg.genLogicalOr(block, e)
	}

	// String concat fusion: detect `++` chains on strings of length >= 3
	// before evaluating operands.  Pairwise emission would alloc a temp
	// per intermediate node; the fused path does one alloc + N memcpys.
	// Length 2 falls through to the existing 2-way path (identical IR).
	if e.Op == "++" && cg.isStringConcatNode(e) {
		if chain := cg.flattenStringConcat(e); len(chain) >= 3 {
			return cg.genFusedStringConcat(block, chain)
		}
	}

	cg.curBlock = block

	left, err := cg.genExpr(block, e.Left)
	if err != nil {
		return nil, err
	}

	if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
	}

	cg.curBlock = block

	right, err := cg.genExpr(block, e.Right)
	if err != nil {
		return nil, err
	}

	if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
	}

	if left == nil || right == nil {
		return constant.NewInt(irtypes.I64, 0), nil
	}

	// Unify types.
	lt := left.Type()
	rt := right.Type()

	// Type promotion.
	if irtypes.IsInt(lt) && irtypes.IsInt(rt) {
		lBits := lt.(*irtypes.IntType).BitSize

		rBits := rt.(*irtypes.IntType).BitSize
		if lBits < rBits {
			if cg.exprElemIsUnsigned(e.Left) {
				left = block.NewZExt(left, rt)
			} else {
				left = block.NewSExt(left, rt)
			}

			lt = rt
		} else if rBits < lBits {
			if cg.exprElemIsUnsigned(e.Right) {
				right = block.NewZExt(right, lt)
			} else {
				right = block.NewSExt(right, lt)
			}
		}
	} else if irtypes.IsFloat(lt) && irtypes.IsInt(rt) {
		right = block.NewSIToFP(right, lt)
	} else if irtypes.IsInt(lt) && irtypes.IsFloat(rt) {
		left = block.NewSIToFP(left, rt)
		lt = rt
	} else if irtypes.IsFloat(lt) && irtypes.IsFloat(rt) {
		lBits := floatBits(lt.(*irtypes.FloatType))
		rBits := floatBits(rt.(*irtypes.FloatType))

		if lBits != rBits {
			if lfc, ok := left.(*constant.Float); ok {
				// Left is a float literal: reinterpret it as the right side's type.
				v, _ := lfc.X.Float64()
				left = constant.NewFloat(rt.(*irtypes.FloatType), v)
				lt = rt
			} else if rfc, ok := right.(*constant.Float); ok {
				// Right is a float literal: reinterpret it as the left side's type.
				v, _ := rfc.X.Float64()
				right = constant.NewFloat(lt.(*irtypes.FloatType), v)
			} else {
				// Two non-literal floats of different sizes: promote smaller to larger.
				if lBits < rBits {
					left = block.NewFPExt(left, rt)
					lt = rt
				} else {
					right = block.NewFPExt(right, lt)
				}
			}
		}
	}

	isFloat := irtypes.IsFloat(lt)
	// Also treat vectors of floats as float for operator selection.
	if !isFloat {
		if vt, ok := lt.(*irtypes.VectorType); ok {
			isFloat = irtypes.IsFloat(vt.ElemType)
		}
	}

	// Pointer arithmetic: ptr + int -> getelementptr; ptr - int -> getelementptr with negation.
	if ptrType, isPtr := lt.(*irtypes.PointerType); isPtr && irtypes.IsInt(rt) {
		switch e.Op {
		case "+", "-":
			if cg.unsafeDepth == 0 {
				return nil, cg.nodeErr(e,
					"pointer arithmetic requires an `{#unsafe}` block")
			}
		}
		// Ensure the index is i64.
		if rt.(*irtypes.IntType).BitSize < 64 {
			right = block.NewSExt(right, irtypes.I64)
		}

		switch e.Op {
		case "+":
			return block.NewGetElementPtr(ptrType.ElemType, left, right), nil
		case "-":
			negIdx := block.NewSub(constant.NewInt(irtypes.I64, 0), right)

			return block.NewGetElementPtr(ptrType.ElemType, left, negIdx), nil
		}
	}

	// Operator overloading dispatch (Phase 3): if either operand is a user
	// struct that implements the corresponding built-in operator trait, lower
	// to a method call. Falls through to the primitive path when neither
	// operand is a struct, and to the Phase 0 error gate when a struct
	// operand has no matching impl.
	if isStructType(lt) || isStructType(rt) {
		if res, dispatched, derr := cg.dispatchBinOp(block, e, left, right, lt, rt); dispatched {
			return res, derr
		}

		return nil, cg.nodeErr(e, "binary operator %q is not defined for operands of type %s and %s",
			e.Op, cg.tinTypeDisplay(lt), cg.tinTypeDisplay(rt))
	}

	// Reject arithmetic on string / fat-ptr operands before falling into the
	// integer add/sub paths below -- without this, `s1 + s2` would emit
	// `add { i8*, i64 }` which clang rejects with a confusing low-level
	// error instead of a Tin-level diagnostic. The right concat operator
	// for strings is `++`; surface that in the message.
	if cg.isBadFatPtrArithmetic(e.Op, lt, rt) {
		hint := ""
		if e.Op == "+" && isStringType(lt) && isStringType(rt) {
			hint = " (use %q to concatenate strings)"

			return nil, cg.nodeErr(e,
				"binary operator %q is not defined for operands of type %s and %s"+hint,
				e.Op, cg.tinTypeDisplay(lt), cg.tinTypeDisplay(rt), "++")
		}

		return nil, cg.nodeErr(e,
			"binary operator %q is not defined for operands of type %s and %s",
			e.Op, cg.tinTypeDisplay(lt), cg.tinTypeDisplay(rt))
	}

	switch e.Op {
	case "+":
		if isFloat {
			return block.NewFAdd(left, right), nil
		}

		return block.NewAdd(left, right), nil
	case "-":
		if isFloat {
			return block.NewFSub(left, right), nil
		}

		return block.NewSub(left, right), nil
	case "*":
		if isFloat {
			return block.NewFMul(left, right), nil
		}

		return block.NewMul(left, right), nil
	case "/":
		if v := cg.tryFoldExpr(e.Right); v.kind == foldInt && v.intVal == 0 {
			return nil, cg.nodeErr(e, "division by zero")
		}

		if isFloat {
			return block.NewFDiv(left, right), nil
		}

		if cg.exprElemIsUnsigned(e.Left) {
			return block.NewUDiv(left, right), nil
		}

		return block.NewSDiv(left, right), nil
	case "%":
		if v := cg.tryFoldExpr(e.Right); v.kind == foldInt && v.intVal == 0 {
			return nil, cg.nodeErr(e, "modulo by zero")
		}

		if cg.exprElemIsUnsigned(e.Left) {
			return block.NewURem(left, right), nil
		}

		return block.NewSRem(left, right), nil
	case "==":
		cg.checkTautologicalNilCmp(e, false)

		result := cg.genEqNeqExpr(block, left, right, lt, rt, isFloat, false)
		// Release temporary string operands after comparison (e.g., fn() == fn()).
		if isFatPtrType(lt) {
			if isTemporaryProducer(e.Left) {
				cg.emitRelease(block, left)
			}

			if isTemporaryProducer(e.Right) {
				cg.emitRelease(block, right)
			}
		}

		return result, nil
	case "!=":
		cg.checkTautologicalNilCmp(e, true)

		result := cg.genEqNeqExpr(block, left, right, lt, rt, isFloat, true)
		// Release temporary string operands after comparison (e.g., fn() != fn()).
		if isFatPtrType(lt) {
			if isTemporaryProducer(e.Left) {
				cg.emitRelease(block, left)
			}

			if isTemporaryProducer(e.Right) {
				cg.emitRelease(block, right)
			}
		}

		return result, nil
	case "<":
		if isFloat {
			return block.NewFCmp(enum.FPredOLT, left, right), nil
		}

		if cg.exprElemIsUnsigned(e.Left) {
			return block.NewICmp(enum.IPredULT, left, right), nil
		}

		return block.NewICmp(enum.IPredSLT, left, right), nil
	case "<=":
		if isFloat {
			return block.NewFCmp(enum.FPredOLE, left, right), nil
		}

		if cg.exprElemIsUnsigned(e.Left) {
			return block.NewICmp(enum.IPredULE, left, right), nil
		}

		return block.NewICmp(enum.IPredSLE, left, right), nil
	case ">":
		if isFloat {
			return block.NewFCmp(enum.FPredOGT, left, right), nil
		}

		if cg.exprElemIsUnsigned(e.Left) {
			return block.NewICmp(enum.IPredUGT, left, right), nil
		}

		return block.NewICmp(enum.IPredSGT, left, right), nil
	case ">=":
		if isFloat {
			return block.NewFCmp(enum.FPredOGE, left, right), nil
		}

		if cg.exprElemIsUnsigned(e.Left) {
			return block.NewICmp(enum.IPredUGE, left, right), nil
		}

		return block.NewICmp(enum.IPredSGE, left, right), nil
	case "&":
		return block.NewAnd(left, right), nil
	case "|":
		return block.NewOr(left, right), nil
	case "^":
		return block.NewXor(left, right), nil
	case "<<":
		if err := cg.checkShiftAmount(e, left); err != nil {
			return nil, err
		}

		return block.NewShl(left, right), nil
	case ">>":
		if err := cg.checkShiftAmount(e, left); err != nil {
			return nil, err
		}
		// Use logical (zero-fill) right shift for unsigned types.
		if cg.exprElemIsUnsigned(e.Left) {
			return block.NewLShr(left, right), nil
		}

		return block.NewAShr(left, right), nil
	case "++":
		// string ++ byte  /  byte ++ string: coerce the i8 operand to a 1-char string fat-ptr.
		// The byte is stored in a stack alloca; the memcpy inside the concat path happens in the
		// same basic block so the alloca lifetime is valid.
		// Track coercion so we skip ARC release on the coerced side (stack, not RC-managed).
		leftCoerced, rightCoerced := false, false

		if isStringType(left.Type()) && irtypes.IsInt(right.Type()) && right.Type().(*irtypes.IntType).BitSize == 8 {
			right = byteToStringFatPtr(block, right)
			rightCoerced = true
		} else if isStringType(right.Type()) && irtypes.IsInt(left.Type()) && left.Type().(*irtypes.IntType).BitSize == 8 {
			left = byteToStringFatPtr(block, left)
			leftCoerced = true
		}
		// `++` is slice-slice concat (mirrors `++=`).  String ++ byte is
		// handled above; everything else requires both sides to be the
		// same fat-array (or string) type.  Without this check,
		// `[1, 2] ++ 3` silently fell into the array path and produced
		// garbage IR (insertvalue/extractvalue with a non-fat-ptr RHS).
		leftIsArr := isFatArrayPtr(left.Type()) && !isStringType(left.Type())
		rightIsArr := isFatArrayPtr(right.Type()) && !isStringType(right.Type())

		if leftIsArr != rightIsArr {
			return nil, cg.nodeErr(e,
				"`++` is slice concat: both sides must be the same slice "+
					"type, got %s ++ %s. To prepend or append a single value, "+
					"wrap it as a one-element slice: `[v] ++ xs` or `xs ++ [v]`",
				cg.fmtArgType(left.Type()), cg.fmtArgType(right.Type()))
		}

		if leftIsArr && rightIsArr && !left.Type().Equal(right.Type()) {
			return nil, cg.nodeErr(e,
				"`++` requires matching slice element types, got %s ++ %s",
				cg.fmtArgType(left.Type()), cg.fmtArgType(right.Type()))
		}
		// Typed array concatenation: {T*, i64} ++ {T*, i64} -> {T*, i64}
		// (strings {i8*, i64} are handled by the string path below)
		if isFatArrayPtr(left.Type()) && !isStringType(left.Type()) {
			fatType := left.Type().(*irtypes.StructType)
			dataPtrType := fatType.Fields[0].(*irtypes.PointerType)
			elemT := dataPtrType.ElemType

			leftDataPtr := block.NewExtractValue(left, 0)
			leftLen := block.NewExtractValue(left, 1)
			rightDataPtr := block.NewExtractValue(right, 0)
			rightLen := block.NewExtractValue(right, 1)
			totalLen := block.NewAdd(leftLen, rightLen)

			// sizeof(elemT) via GEP trick.
			nullElemPtr := constant.NewNull(irtypes.NewPointer(elemT))
			sizeGep := block.NewGetElementPtr(elemT, nullElemPtr, constant.NewInt(irtypes.I64, 1))
			elemSize := block.NewPtrToInt(sizeGep, irtypes.I64)

			// new_ptr = _tin_rc_alloc(totalLen * elemSize)
			totalBytes := block.NewMul(totalLen, elemSize)
			newI8Ptr := block.NewCall(cg.ensureRCAlloc(), totalBytes)
			newPtr := block.NewBitCast(newI8Ptr, irtypes.NewPointer(elemT))

			// memcpy left data
			leftBytes := block.NewMul(leftLen, elemSize)
			leftI8Ptr := block.NewBitCast(leftDataPtr, irtypes.I8Ptr)
			block.NewCall(cg.ensureMemcpy(), newI8Ptr, leftI8Ptr, leftBytes, constant.NewInt(irtypes.I1, 0))

			// memcpy right data at offset leftLen*elemSize
			rightOffset := block.NewMul(leftLen, elemSize)
			rightDst := block.NewGetElementPtr(irtypes.I8, newI8Ptr, rightOffset)
			rightI8Ptr := block.NewBitCast(rightDataPtr, irtypes.I8Ptr)
			rightBytes := block.NewMul(rightLen, elemSize)
			block.NewCall(cg.ensureMemcpy(), rightDst, rightI8Ptr, rightBytes, constant.NewInt(irtypes.I1, 0))

			// Build new fat ptr {T*, i64}
			v0 := block.NewInsertValue(constant.NewUndef(fatType), newPtr, 0)
			result := block.NewInsertValue(v0, totalLen, 1)
			// For non-temporary sources, the new buffer shares element pointers
			// with the source array.  Retain each shared element so that releasing
			// the source and the new buffer are independent: each holds its own RC
			// claim and can be released in any order without use-after-free.
			//
			// For temporary sources, the temp buffer is released below (buffer-only,
			// no element release), so elements are effectively transferred to the new
			// buffer without needing a retain.
			//
			// Note: elemNeedsRelease returns false for *irtypes.PointerType (pointer
			// variables don't need scope release), but pointer elements inside [*T]
			// arrays DO need retain/release so we check that case explicitly.
			_, elemIsPtr := elemT.(*irtypes.PointerType)
			needsElemRetain := cg.elemNeedsRelease(elemT) || isRCTrackedType(elemT) || elemIsPtr

			if !isTemporaryProducer(e.Left) && needsElemRetain {
				cg.emitRetainElemSlice(block, newI8Ptr, leftLen, elemT)
			}

			if !isTemporaryProducer(e.Right) && needsElemRetain {
				cg.emitRetainElemSlice(block, rightDst, rightLen, elemT)
			}

			// Release sub-expression temporaries: buffer-only release transfers
			// ownership of elements to the new buffer without a retain.
			if isTemporaryProducer(e.Left) {
				if rcPtr := cg.extractRCDataPtr(block, left, left.Type()); rcPtr != nil {
					block.NewCall(cg.ensureRelease(), rcPtr)
				}
			}

			if isTemporaryProducer(e.Right) {
				if rcPtr := cg.extractRCDataPtr(block, right, right.Type()); rcPtr != nil {
					block.NewCall(cg.ensureRelease(), rcPtr)
				}
			}

			return result, nil
		}

		// String concatenation: both operands are {i8*, i64} fat-ptrs.
		leftPtr := cg.extractStringPtr(block, left)
		leftLen := cg.extractStringLen(block, left)
		rightPtr := cg.extractStringPtr(block, right)
		rightLen := cg.extractStringLen(block, right)
		totalLen := block.NewAdd(leftLen, rightLen)
		// rc_alloc(totalLen + 1) for null terminator; ARC manages the result.
		allocSize := block.NewAdd(totalLen, constant.NewInt(irtypes.I64, 1))
		buf := block.NewCall(cg.ensureRCAlloc(), allocSize)
		// memcpy(buf, leftPtr, leftLen)
		block.NewCall(cg.ensureMemcpy(), buf, leftPtr, leftLen, constant.NewInt(irtypes.I1, 0))
		// memcpy(buf + leftLen, rightPtr, rightLen)
		rightDst := block.NewGetElementPtr(irtypes.I8, buf, leftLen)
		block.NewCall(cg.ensureMemcpy(), rightDst, rightPtr, rightLen, constant.NewInt(irtypes.I1, 0))
		// null-terminate
		nullByte := block.NewGetElementPtr(irtypes.I8, buf, totalLen)
		block.NewStore(constant.NewInt(irtypes.I8, 0), nullByte)
		// build {i8*, i64} fat-ptr result
		fatPtrType := stringFatPtrType()
		v0 := block.NewInsertValue(constant.NewUndef(fatPtrType), buf, 0)
		result := block.NewInsertValue(v0, totalLen, 1)
		// Release sub-expression temporaries now that the result is built.
		// Skip byte-to-string coerced operands: their ptr is a stack alloca, not ARC-managed.
		if isTemporaryProducer(e.Left) && !leftCoerced {
			cg.emitRelease(block, left)
		}

		if isTemporaryProducer(e.Right) && !rightCoerced {
			cg.emitRelease(block, right)
		}

		return result, nil
	}

	// No primitive / built-in lowering matched. Until operator overloading
	// lands (docs/plans/operator-overloading.md), there is no user hook
	// either; reject loudly instead of silently producing 0. Phase 0 of
	// that plan exists because the previous silent-zero fall-through hid
	// real bugs at every callsite.
	return nil, cg.nodeErr(e, "binary operator %q is not defined for operands of type %s and %s",
		e.Op, cg.tinTypeDisplay(left.Type()), cg.tinTypeDisplay(right.Type()))
}

// genEqNeqExpr implements shared handling for == and != operators.
func (cg *CodeGen) genEqNeqExpr(block *ir.Block, left, right value.Value, lt, rt irtypes.Type, isFloat bool, notEqual bool) value.Value {
	if isFloat {
		// IEEE 754 NaN: x == x is false, x != x is true. OEQ matches the
		// first (false on NaN); UNE the second (true on NaN). Using ONE
		// for != would silently fold `x != x` to false, breaking the
		// canonical NaN test pattern.
		if notEqual {
			return block.NewFCmp(enum.FPredUNE, left, right)
		}

		return block.NewFCmp(enum.FPredOEQ, left, right)
	}

	pred := enum.IPredEQ
	if notEqual {
		pred = enum.IPredNE
	}

	// any equality/inequality: dynamically dispatched by runtime.
	if isAnyType(lt) || isAnyType(rt) {
		var tempLeft, tempRight value.Value

		if !isAnyType(lt) {
			left = cg.boxToAny(block, left)
			tempLeft = left
		}

		if !isAnyType(rt) {
			right = cg.boxToAny(block, right)
			tempRight = right
		}

		cmp := block.NewCall(cg.ensureAnyEq(), left, right)

		// Release temporary boxes created by boxToAny - they are fresh RC=1
		// allocations that exist only for this comparison.
		if tempLeft != nil {
			cg.emitRelease(block, tempLeft)
		}

		if tempRight != nil {
			cg.emitRelease(block, tempRight)
		}

		result := cmp
		if notEqual {
			return block.NewICmp(enum.IPredEQ, result, constant.NewInt(irtypes.I64, 0))
		}

		return block.NewICmp(enum.IPredNE, result, constant.NewInt(irtypes.I64, 0))
	}

	// atom ==/!= atom: compare CRC32 codes directly.
	if isAtomType(lt) && isAtomType(rt) {
		lcode := cg.extractAtomCode(block, left)
		rcode := cg.extractAtomCode(block, right)

		return block.NewICmp(pred, lcode, rcode)
	}

	// atom <-> string: convert atom to string, then strcmp.
	if isAtomType(lt) && isFatPtrType(rt) {
		strVal := block.NewCall(cg.ensureAtomToString(), cg.extractAtomCode(block, left))
		lptr := cg.extractStringPtr(block, strVal)
		rptr := cg.extractStringPtr(block, right)
		cmp := block.NewCall(cg.ensureStrcmp(), lptr, rptr)

		return block.NewICmp(pred, cmp, constant.NewInt(irtypes.I32, 0))
	}

	if isFatPtrType(lt) && isAtomType(rt) {
		strVal := block.NewCall(cg.ensureAtomToString(), cg.extractAtomCode(block, right))
		lptr := cg.extractStringPtr(block, left)
		rptr := cg.extractStringPtr(block, strVal)
		cmp := block.NewCall(cg.ensureStrcmp(), lptr, rptr)

		return block.NewICmp(pred, cmp, constant.NewInt(irtypes.I32, 0))
	}

	// String equality/inequality: compare via strcmp.
	if isFatPtrType(lt) {
		lptr := cg.extractStringPtr(block, left)
		rptr := cg.extractStringPtr(block, right)
		cmp := block.NewCall(cg.ensureStrcmp(), lptr, rptr)

		return block.NewICmp(pred, cmp, constant.NewInt(irtypes.I32, 0))
	}

	// Pointer vs integer-zero (None): coerce i64(0) to typed null pointer.
	if irtypes.IsPointer(lt) && !irtypes.IsPointer(rt) {
		right = constant.NewNull(lt.(*irtypes.PointerType))
	} else if irtypes.IsPointer(rt) && !irtypes.IsPointer(lt) {
		left = constant.NewNull(rt.(*irtypes.PointerType))
	}

	return block.NewICmp(pred, left, right)
}

// genLogicalAnd emits short-circuit `A && B` as `if A { B } else { false }`.
// The RHS evaluates only when LHS is true. cg.curBlock is updated to the
// merge block on return so the caller continues emitting there. Callers that
// reference `block` (the input) post-call would target a terminated block;
// they must use cg.curBlock instead.
func (cg *CodeGen) genLogicalAnd(block *ir.Block, e *ast.BinExpr) (value.Value, error) {
	return cg.genShortCircuit(block, e, false)
}

// genLogicalOr emits short-circuit `A || B` as `if A { true } else { B }`.
// Symmetric to genLogicalAnd; see that function's note about cg.curBlock.
func (cg *CodeGen) genLogicalOr(block *ir.Block, e *ast.BinExpr) (value.Value, error) {
	return cg.genShortCircuit(block, e, true)
}

// genShortCircuit lowers a logical && or || with proper short-circuit
// semantics. shortVal is the value the operator returns when the LHS
// already determines the result: false for &&, true for ||. The RHS
// is evaluated only when the LHS does NOT short-circuit.
func (cg *CodeGen) genShortCircuit(block *ir.Block, e *ast.BinExpr, shortVal bool) (value.Value, error) {
	cg.curBlock = block

	left, err := cg.genExpr(block, e.Left)
	if err != nil {
		return nil, err
	}

	if err := cg.rejectStructAsBoolOperand(e, left.Type()); err != nil {
		return nil, err
	}

	leftEnd := cg.curBlock
	leftBool := cg.toBool(leftEnd, left)

	var label string
	if shortVal {
		label = "or"
	} else {
		label = "and"
	}

	rhsBlock := cg.newBlock(label + ".rhs")
	mergeBlock := cg.newBlock(label + ".merge")

	if shortVal {
		// `A || B`: short-circuit to merge when A is true.
		leftEnd.NewCondBr(leftBool, mergeBlock, rhsBlock)
	} else {
		// `A && B`: short-circuit to merge when A is false.
		leftEnd.NewCondBr(leftBool, rhsBlock, mergeBlock)
	}

	cg.curBlock = rhsBlock

	right, err := cg.genExpr(rhsBlock, e.Right)
	if err != nil {
		return nil, err
	}

	if err := cg.rejectStructAsBoolOperand(e, right.Type()); err != nil {
		return nil, err
	}

	rightEnd := cg.curBlock
	rightBool := cg.toBool(rightEnd, right)
	rightEnd.NewBr(mergeBlock)

	var shortConst constant.Constant
	if shortVal {
		shortConst = constant.NewInt(irtypes.I1, 1)
	} else {
		shortConst = constant.NewInt(irtypes.I1, 0)
	}

	phi := mergeBlock.NewPhi(
		ir.NewIncoming(shortConst, leftEnd),
		ir.NewIncoming(rightBool, rightEnd),
	)
	cg.curBlock = mergeBlock

	return phi, nil
}

package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) genUnaryExpr(block *ir.Block, e *ast.UnaryExpr) (value.Value, error) {
	val, err := cg.genExpr(block, e.Expr)
	if err != nil {
		return nil, err
	}

	if val == nil {
		return nil, nil
	}

	// genExpr may have advanced cg.curBlock through short-circuit && / ||.
	// The unary op (xor, fneg, sub, load) consumes `val` (often a phi
	// rooted in a merge block) and must be emitted there, not in the
	// stale input block. Without this `!(a || b)` lowers to an `xor`
	// in `entry` that uses a phi defined later in the merge -- invalid
	// SSA: "Instruction does not dominate all uses".
	if cg.curBlock != nil {
		block = cg.curBlock
	}

	// Operator overloading dispatch (Phase 3): if the operand is a user
	// struct that implements the corresponding built-in unary operator trait,
	// lower to a method call. Falls through to the primitive switch
	// otherwise; primitive structs (any, string, fat array) are excluded by
	// isStructType.
	if isStructType(val.Type()) {
		if traitName, isOp := unaryOpTraitName(e.Op); isOp {
			structName := cg.typeNameOf(val.Type())
			if fn := cg.lookupOpMethod(structName, traitName, nil); fn != nil {
				return cg.emitOpDispatch(block, fn, val, nil)
			}

			return nil, cg.nodeErr(e, "unary operator %q is not defined for operand of type %s", e.Op, cg.tinTypeDisplay(val.Type()))
		}
	}

	switch e.Op {
	case "-":
		if irtypes.IsFloat(val.Type()) {
			return block.NewFNeg(val), nil
		}

		zero := cg.coerce(block, constant.NewInt(irtypes.I64, 0), val.Type())

		return block.NewSub(zero, val), nil
	case "!":
		b := cg.toBool(block, val)

		return block.NewXor(b, constant.NewInt(irtypes.I1, 1)), nil
	case "~":
		minusOne := cg.coerce(block, constant.NewInt(irtypes.I64, -1), val.Type())

		return block.NewXor(val, minusOne), nil
	case "*":
		// Dereference
		if pt, ok := val.Type().(*irtypes.PointerType); ok {
			return block.NewLoad(pt.ElemType, val), nil
		}

		return val, nil
	}

	return val, nil
}

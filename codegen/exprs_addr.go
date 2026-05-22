package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) genAddrExpr(block *ir.Block, e *ast.AddrExpr) (value.Value, error) {
	// `addr(...)` is the raw-address builtin -- it materializes a
	// pointer at a specific bare-metal address.  The only legitimate
	// argument is an integer literal under `{#unsafe}`; anything else
	// (taking the address of an lvalue) belongs to the `&` operator.
	// Without this rejection users learn the wrong mental model and
	// stdlib code starts mixing the two forms.
	il, ok := e.Val.(*ast.IntLit)
	if !ok {
		return nil, cg.nodeErr(e,
			"addr(...) takes a raw integer address (e.g. addr(0xDEADBEEF)). "+
				"To take the address of an lvalue, use `&expr` instead.")
	}

	if cg.unsafeDepth == 0 {
		return nil, cg.nodeErr(e,
			"addr(int_literal) creates a raw pointer and requires an `{#unsafe}` block")
	}

	if il.Big != nil {
		return nil, cg.nodeErr(e,
			"addr(int_literal) target must fit in 64 bits (got %s)", il.Big.String())
	}

	v := constant.NewInt(irtypes.I64, il.Value)

	return block.NewIntToPtr(v, irtypes.I8Ptr), nil
}

func (cg *CodeGen) genAddrOfExpr(block *ir.Block, e *ast.AddressOfExpr) (value.Value, error) {
	return cg.genLValue(block, e.Expr)
}

func (cg *CodeGen) genDerefExpr(block *ir.Block, e *ast.DerefExpr) (value.Value, error) {
	if _, isNil := e.Expr.(*ast.NilLit); isNil {
		return nil, cg.nodeErr(e, "dereferencing nil literal")
	}

	// `*(p + n)` is a permitted transient-pointer consumption site:
	// the address from the arithmetic is consumed immediately by the
	// deref load and never reaches a binding / arg / return.
	prevTransient := cg.transientPtrAllowed
	cg.transientPtrAllowed = true
	val, err := cg.genExpr(block, e.Expr)
	cg.transientPtrAllowed = prevTransient
	if err != nil {
		return nil, err
	}

	if val == nil {
		return nil, nil
	}

	if pt, ok := val.Type().(*irtypes.PointerType); ok {
		loaded := block.NewLoad(pt.ElemType, val)

		// ARC move semantics: if the pointer is a temporary (e.g. *parse_value(&p)),
		// the caller owns the RC block but no variable will release it. Free the
		// outer allocation now. Do NOT call emitRelease (which walks struct fields) -
		// the fields are transferred to the loaded copy and will be released there.
		if isTemporaryProducer(e.Expr) {
			rcPtr := block.NewBitCast(val, irtypes.I8Ptr)
			block.NewCall(cg.ensureRelease(), rcPtr)
		}

		return loaded, nil
	}

	return val, nil
}

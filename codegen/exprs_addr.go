package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) genAddrExpr(block *ir.Block, e *ast.AddrExpr) (value.Value, error) {
	// `addr(...)` is the raw-address builtin.  Two accepted forms,
	// both requiring `{#unsafe}` and both producing `volatile *T`:
	//
	//   1. addr(int_literal)  -> volatile *char  (bare-metal poke)
	//   2. addr(arr[i])       -> volatile *T     (escape an array
	//                                              slot into raw-ptr
	//                                              world; emits a
	//                                              warning)
	//
	// The `volatile` qualifier (addrspace 1 at LLVM level) tells the
	// rc machinery to skip retain/release for the resulting pointer
	// -- raw addresses don't live in tin's heap and reading their
	// rc header would fault.  Crossing back to `*T` requires an
	// explicit cast (also in `{#unsafe}`), which is the user's
	// promise that the address is in fact rc-managed.
	if cg.unsafeDepth == 0 {
		return nil, cg.nodeErr(e,
			"addr(...) creates a raw `volatile` pointer and requires an `{#unsafe}` block")
	}

	switch arg := e.Val.(type) {
	case *ast.IntLit:
		if arg.Big != nil {
			return nil, cg.nodeErr(e,
				"addr(int_literal) target must fit in 64 bits (got %s)", arg.Big.String())
		}

		// Result type: volatile *char  ->  addrspace(1) i8*
		vptr := &irtypes.PointerType{ElemType: irtypes.I8, AddrSpace: volatileAddrSpace}

		return block.NewIntToPtr(constant.NewInt(irtypes.I64, arg.Value), vptr), nil
	case *ast.IndexExpr:
		// Take the slot address via the usual lvalue path, then
		// shed rc tracking by promoting to addrspace(1).  The
		// resulting pointer is volatile *T (T = element type),
		// and the user owns the lifetime hazards.
		cg.warn("volatile-from-lvalue", e.Pos(),
			"taking addr() of an array slot escapes the rc lifetime "+
				"checker; prefer `&arr[i]` unless you specifically need a "+
				"`volatile` raw pointer.")

		lval, err := cg.genLValue(block, arg)
		if err != nil {
			return nil, err
		}

		pt, ok := lval.Type().(*irtypes.PointerType)
		if !ok {
			return nil, cg.nodeErr(e, "addr(arr[i]): lvalue did not produce a pointer (got %s)", lval.Type())
		}

		vptr := &irtypes.PointerType{ElemType: pt.ElemType, AddrSpace: volatileAddrSpace}

		return block.NewAddrSpaceCast(lval, vptr), nil
	default:
		return nil, cg.nodeErr(e,
			"addr(...) takes a raw integer address (e.g. addr(0xDEADBEEF)) or "+
				"an indexed lvalue (addr(arr[i])).  To take the address of a "+
				"regular lvalue, use `&expr` instead.")
	}
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

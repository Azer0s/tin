package codegen

import (
	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

func (cg *CodeGen) argTypeImplicitlyOK(src, pt irtypes.Type) bool {
	if isAnyType(pt) {
		return true
	}

	if _, ok := cg.isTraitFatPtr(pt); ok {
		return true
	}

	if isFatFnPtr(pt) {
		return true
	}
	// Implicit-conversion functions registered for the target type.
	if name := cg.typeNameOf(pt); name != "" {
		for _, e := range cg.implicitConvFns[name] {
			if e.srcLLVM.Equal(src) {
				return true
			}
		}
	}
	// Raw C-pointer / fat-ptr extraction shims.  This was previously
	// "any *X -> any *Y is OK", which silently accepted unrelated
	// pointer types of matching shape (e.g. `*B` passed where `*A`
	// was expected -- both end up i64-sized and the runtime memcpy
	// would be UB if the field layouts diverge).  Tighten to: only
	// allow when one side is the opaque `*i8` (void* / raw handle)
	// or when the pointees match.  Cross-type pointer casts must be
	// spelled explicitly with `as *T` so the author opts in.
	if srcPt, srcIsPtr := src.(*irtypes.PointerType); srcIsPtr {
		if tgtPt, tgtIsPtr := pt.(*irtypes.PointerType); tgtIsPtr {
			// Equal pointees: trivially compatible.
			if srcPt.ElemType.Equal(tgtPt.ElemType) {
				return true
			}
			// Either side opaque `i8*` / `*void`: ABI-compatible by
			// design (used for C handles, generic raw buffers).
			if srcPt.ElemType.Equal(irtypes.I8) || tgtPt.ElemType.Equal(irtypes.I8) {
				return true
			}
		}
	}

	if isFatPtrType(src) {
		if _, tgtIsPtr := pt.(*irtypes.PointerType); tgtIsPtr {
			return true
		}
	}

	if isFatPtrType(pt) {
		if _, srcIsPtr := src.(*irtypes.PointerType); srcIsPtr {
			return true
		}
	}
	// Same-size integer types (e.g. i32 vs u32 / char vs i8): coerce
	// returned the value unchanged because the bit width matches and the
	// runtime ABI passes them identically.
	if srcInt, ok := src.(*irtypes.IntType); ok {
		if tgtInt, ok2 := pt.(*irtypes.IntType); ok2 && srcInt.BitSize == tgtInt.BitSize {
			return true
		}
	}

	return false
}

func (cg *CodeGen) adaptArgs(block *ir.Block, args []value.Value, sig *irtypes.FuncType) []value.Value {
	if sig == nil {
		return args
	}

	result := make([]value.Value, len(args))
	for i, arg := range args {
		if i < len(sig.Params) {
			result[i] = cg.coerce(block, arg, sig.Params[i])
		} else if sig.Variadic && arg != nil && isAtomType(arg.Type()) {
			// Variadic position: atoms must become i8* (the atom string rep).
			code := cg.extractAtomCode(block, arg)
			strFatPtr := block.NewCall(cg.ensureAtomToString(), code)
			result[i] = cg.extractFatPtrData(block, strFatPtr, stringFatPtrType())
		} else if sig.Variadic && arg != nil && isFatPtrType(arg.Type()) {
			// Variadic position: fat-ptrs are not valid C varargs - unwrap to
			// the underlying raw pointer so printf-style calls work correctly.
			result[i] = cg.extractFatPtrData(block, arg, arg.Type().(*irtypes.StructType))
		} else {
			result[i] = arg
		}
	}

	return result
}

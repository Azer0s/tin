package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

func (cg *CodeGen) emitMemcpy(block *ir.Block, dst, src value.Value, n int64) {
	dstI8 := dst
	srcI8 := src

	if _, ok := dst.Type().(*irtypes.PointerType); ok && !dst.Type().Equal(irtypes.I8Ptr) {
		dstI8 = block.NewBitCast(dst, irtypes.I8Ptr)
	}

	if _, ok := src.Type().(*irtypes.PointerType); ok && !src.Type().Equal(irtypes.I8Ptr) {
		srcI8 = block.NewBitCast(src, irtypes.I8Ptr)
	}

	block.NewCall(cg.ensureMemcpy(),
		dstI8, srcI8,
		constant.NewInt(irtypes.I64, n),
		constant.NewInt(irtypes.I1, 0))
}

// ensureInteropStrIn declares `TinString tin_interop_str_in(i8*)` in the
// active LLVM module (cg.mod by default; cg.shimMod during CTFE shim emit).
// Routed through ensureExternDecl so ARM64 / SysV ABI lowering (sret for
// the widened 24-byte TinString return) matches what clang emits in runtime.c.
func (cg *CodeGen) ensureInteropStrIn() *ir.Func {
	return cg.ensureExternDecl("tin_interop_str_in",
		stringFatPtrType(),
		[]*ir.Param{ir.NewParam("cstr", irtypes.I8Ptr)}, false)
}

// ensureInteropStrOut declares `i8* tin_interop_str_out(TinString)` in the
// active LLVM module.
func (cg *CodeGen) ensureInteropStrOut() *ir.Func {
	return cg.ensureExternDecl("tin_interop_str_out",
		irtypes.I8Ptr,
		[]*ir.Param{ir.NewParam("s", stringFatPtrType())}, false)
}

// ensureInteropSliceIn declares
// `TinSlice tin_interop_slice_in(i8* data, i64 len, i64 elem_size)` in the
// active LLVM module.
func (cg *CodeGen) ensureInteropSliceIn() *ir.Func {
	sliceTy := fatArrayPtrType(irtypes.I8)

	return cg.ensureExternDecl("tin_interop_slice_in",
		sliceTy,
		[]*ir.Param{
			ir.NewParam("data", irtypes.I8Ptr),
			ir.NewParam("len", irtypes.I64),
			ir.NewParam("elem_size", irtypes.I64),
		}, false)
}

// ensureInteropSliceOut declares
// `i32 tin_interop_slice_out(TinSlice, i64 elem_size, i8** out_data,
// i64* out_len)` in the active LLVM module.
func (cg *CodeGen) ensureInteropSliceOut() *ir.Func {
	sliceTy := fatArrayPtrType(irtypes.I8)

	return cg.ensureExternDecl("tin_interop_slice_out",
		irtypes.I32,
		[]*ir.Param{
			ir.NewParam("s", sliceTy),
			ir.NewParam("elem_size", irtypes.I64),
			ir.NewParam("out_data", irtypes.NewPointer(irtypes.I8Ptr)),
			ir.NewParam("out_len", irtypes.NewPointer(irtypes.I64)),
		}, false)
}

// getOrCreateCallbackThunk returns a Tin-calling-convention thunk for
// the given callback signature. The thunk receives `i8* env` first
// (Tin's fat fn-ptr ABI), reads the raw C function pointer from env,
// marshals each Tin-shape argument into its C ABI shape (bool: i1->i8;
// string: TinString -> const char*; slice: TinSlice -> (T*, len) split;
// primitives passthrough), calls the C function, marshals the return
// back to Tin shape, and returns. One thunk is emitted per unique Tin
// signature and cached on the codegen.

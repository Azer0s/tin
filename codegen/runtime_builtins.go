package codegen

import (
	"crypto/sha1"
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) ensureMemcpy() *ir.Func {
	if cg.memcpyFn != nil {
		return cg.memcpyFn
	}
	// LLVM intrinsic: declare void @llvm.memcpy.p0i8.p0i8.i64(i8*, i8*, i64, i1)
	f := cg.mod.NewFunc("llvm.memcpy.p0i8.p0i8.i64", irtypes.Void,
		ir.NewParam("dst", irtypes.I8Ptr),
		ir.NewParam("src", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64),
		ir.NewParam("isvolatile", irtypes.I1),
	)
	f.Blocks = nil
	cg.memcpyFn = f

	return f
}

// ensureMemset lazily declares the llvm.memset.p0.i64 intrinsic.
// Used to zero-initialize large fixed-size arrays without generating huge
// aggregate-value stores that crash or hang LLVM's instruction selector.
func (cg *CodeGen) ensureMemset() *ir.Func {
	if cg.memsetFn != nil {
		return cg.memsetFn
	}

	f := cg.mod.NewFunc("llvm.memset.p0.i64", irtypes.Void,
		ir.NewParam("dst", irtypes.I8Ptr),
		ir.NewParam("val", irtypes.I8),
		ir.NewParam("len", irtypes.I64),
		ir.NewParam("isvolatile", irtypes.I1),
	)
	f.Blocks = nil
	cg.memsetFn = f

	return f
}

// ensureAnyEq declares _tin_any_eq if not already done.
// Signature: i64 _tin_any_eq({i32, i8*} a, {i32, i8*} b)
func (cg *CodeGen) ensureAnyEq() *ir.Func {
	if cg.anyEqFn != nil {
		return cg.anyEqFn
	}

	anyT := anyFatPtrType()
	cg.anyEqFn = cg.ensureExternDecl("_tin_any_eq", irtypes.I64,
		[]*ir.Param{ir.NewParam("a", anyT), ir.NewParam("b", anyT)}, false)

	return cg.anyEqFn
}

// ensureStrcmp declares strcmp if not already done.
func (cg *CodeGen) ensureStrcmp() *ir.Func {
	if cg.strcmpFn != nil {
		return cg.strcmpFn
	}

	cg.strcmpFn = cg.ensureExternDecl("strcmp", irtypes.I32,
		[]*ir.Param{ir.NewParam("s1", irtypes.I8Ptr), ir.NewParam("s2", irtypes.I8Ptr)}, false)

	return cg.strcmpFn
}

// newGlobalString creates a private unnamed_addr constant for a string,
// returning a pointer to its first byte.  The global is wrapped in a
// { i64, i64, [N x i8] } struct where the first i64 holds TIN_IMMORTAL_RC (-1)
// and the second i64 is padding to match the 16-byte TinRCHdr layout, so that
// _tin_retain / _tin_release treat it as an immortal, never-freed block and
// the data pointer is 16-byte aligned (needed for SIMD boxing).
//
//goland:noinspection GoSnakeCaseUsage
func (cg *CodeGen) newGlobalString(s string) value.Value {
	data := []byte(s)
	data = append(data, 0) // null terminator
	arrType := irtypes.NewArray(uint64(len(data)), irtypes.I8)
	ca := constant.NewCharArray(data)

	// Wrap in { i64, i64, [N x i8] } with immortal ARC header (rc = -1, pad = 0)
	immortalRC := constant.NewInt(irtypes.I64, -1)
	pad := constant.NewInt(irtypes.I64, 0)
	hdrStructType := irtypes.NewStruct(irtypes.I64, irtypes.I64, arrType)
	hdrConst := constant.NewStruct(hdrStructType, immortalRC, pad, ca)

	// Route through activeModule so the string lives in the same per-pkg
	// module that references it. linkonce_odr linkage with a content-
	// hashed symbol name means the LINKER deduplicates identical
	// strings across object boundaries. Without it, per-pkg compile
	// would either (a) link-fail when a fn moves modules between mono
	// instantiations and references a string defined elsewhere, or
	// (b) duplicate every string per pkg with no dedup.
	//
	// The hash is content-only: two `str.N` symbols with the same
	// payload across modules collide on the same symbol name and
	// linkonce_odr keeps one definition. unnamed_addr lets the
	// optimizer merge identical string globals within a module too.
	hash := sha1.Sum([]byte(s))
	symName := fmt.Sprintf("__tin_str_%x", hash[:8])

	if cg.stringPool == nil {
		cg.stringPool = map[*ir.Module]map[string]value.Value{}
	}

	mod := cg.activeModule()

	perMod, ok := cg.stringPool[mod]
	if !ok {
		perMod = map[string]value.Value{}
		cg.stringPool[mod] = perMod
	}

	if cached, ok := perMod[symName]; ok {
		return cached
	}

	g := mod.NewGlobalDef(symName, hdrConst)
	g.Immutable = true
	g.Linkage = enum.LinkageWeakODR
	g.UnnamedAddr = enum.UnnamedAddrUnnamedAddr
	cg.strCount++

	// GEP: { i64, i64, [N x i8] }* -> [N x i8]* -> i8* (skipping the 16-byte ARC header)
	i32_0 := constant.NewInt(irtypes.I32, 0)
	i32_2 := constant.NewInt(irtypes.I32, 2)
	gep := constant.NewGetElementPtr(hdrStructType, g, i32_0, i32_2, i32_0)
	gep.InBounds = true

	perMod[symName] = gep

	return gep
}

// buildStringFatPtr creates a tin string fat-pointer
// `{i8* data, i64 len, i64 cap}` from a literal string.  Literals live
// in immortal `@.str` globals, so cap = -1: the borrowed-view encoding
// signals "do not mutate, do not RC-release".
func (cg *CodeGen) buildStringFatPtr(block *ir.Block, s string) value.Value {
	ptr := cg.newGlobalString(s)
	length := constant.NewInt(irtypes.I64, int64(len(s)))
	borrowed := constant.NewInt(irtypes.I64, -1)
	fatPtrType := stringFatPtrType()
	v0 := block.NewInsertValue(constant.NewUndef(fatPtrType), ptr, 0)
	v1 := block.NewInsertValue(v0, length, 1)

	return block.NewInsertValue(v1, borrowed, 2)
}

// extractStringPtr extracts the i8* data pointer from a tin string fat-ptr.
func (cg *CodeGen) extractStringPtr(block *ir.Block, fatPtr value.Value) value.Value {
	raw := block.NewExtractValue(fatPtr, 0)
	// Null-safety: a zero-initialized string has data=null; treat as empty string "".
	nullPtr := constant.NewNull(irtypes.I8Ptr)
	emptyPtr := cg.newGlobalString("")
	isNull := block.NewICmp(enum.IPredEQ, raw, nullPtr)

	return block.NewSelect(isNull, emptyPtr, raw)
}

// extractStringLen extracts the i64 length from a tin string fat-ptr.
func (cg *CodeGen) extractStringLen(block *ir.Block, fatPtr value.Value) value.Value {
	return block.NewExtractValue(fatPtr, 1)
}

// panic builtin

// ensurePanicFn lazily declares the _tin_panic external function.
//
// Note on `cg.stacktraceUsed`: the flag is NOT flipped here.  Every Tin
// program is reachable to `_tin_panic` (cap-checks, array bounds, ADT
// mismatches all funnel through it); flipping in this helper would
// switch on `frame-pointer="all"` and pclntab emission for every
// binary, which adds 5-10x to LTO link time even when the user never
// wrote an explicit `panic(...)`.  Instead, `detectStacktraceUsage`
// scans the AST for explicit `panic(...)` / `stacktrace(...)` calls
// and that's what gates the resolver section.  Compiler-emitted
// panics still print their message and exit -- they just drop the
// trace block.
func (cg *CodeGen) ensurePanicFn() *ir.Func {
	if cg.tinPanicFn != nil {
		return cg.tinPanicFn
	}

	cg.tinPanicFn = cg.mod.NewFunc("_tin_panic", irtypes.Void,
		ir.NewParam("msg", irtypes.I8Ptr),
	)

	return cg.tinPanicFn
}

// genBuiltinPanic implements panic(msg): runs the runtime defer chain and
// terminates the program.  The call does not return; a NewUnreachable
// terminator is appended so the block is valid LLVM IR.
func (cg *CodeGen) genBuiltinPanic(block *ir.Block, msgNode ast.Node) (value.Value, error) {
	msg, err := cg.genExpr(block, msgNode)
	if err != nil {
		return nil, err
	}

	var msgPtr value.Value

	t := msg.Type()

	switch {
	case isStringType(t):
		msgPtr = cg.extractStringPtr(block, msg)
	default:
		if strVal, ok := cg.callPrintTrait(block, msg); ok {
			msgPtr = cg.extractStringPtr(block, strVal)
		} else if t == irtypes.I1 {
			msgPtr = block.NewSelect(msg, cg.newGlobalString("true"), cg.newGlobalString("false"))
		} else if irtypes.IsInt(t) {
			arrTy := irtypes.NewArray(64, irtypes.I8)
			buf := block.NewAlloca(arrTy)
			bufPtr := block.NewGetElementPtr(arrTy, buf,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))

			var wide value.Value

			if t == irtypes.I64 {
				wide = msg
			} else {
				wide = block.NewSExt(msg, irtypes.I64)
			}

			block.NewCall(cg.ensureSnprintf(), bufPtr, constant.NewInt(irtypes.I64, 64), cg.newGlobalString("%lld"), wide)
			msgPtr = bufPtr
		} else if irtypes.IsFloat(t) {
			arrTy := irtypes.NewArray(64, irtypes.I8)
			buf := block.NewAlloca(arrTy)
			bufPtr := block.NewGetElementPtr(arrTy, buf,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))

			var fval value.Value

			if t == irtypes.Double {
				fval = msg
			} else {
				fval = block.NewFPExt(msg, irtypes.Double)
			}

			block.NewCall(cg.ensureSnprintf(), bufPtr, constant.NewInt(irtypes.I64, 64), cg.newGlobalString("%g"), fval)
			msgPtr = bufPtr
		} else {
			msgPtr = block.NewBitCast(msg, irtypes.I8Ptr)
		}
	}

	block.NewCall(cg.ensurePanicFn(), msgPtr)
	// If _tin_panic returns (a deferred function called recover()), all of this
	// function's defer lambdas have already run inside _tin_panic.  Their heap
	// envs were not freed by _tin_panic, so free them here.  This is the only
	// cleanup path for the panic branch; the normal exit path (emitDefers) is
	// never reached when panic() is called directly in the function body.
	for _, env := range cg.pendingDeferEnvs {
		if _, isNull := env.(*constant.Null); !isNull {
			block.NewCall(cg.ensureFree(), env)
		}
	}
	// Also release ARC-tracked scope variables so that e.g. a [any] array
	// allocated before the panic call is freed even when recover() is in play.
	cg.emitAllScopeReleases(block, "")
	// _tin_panic normally calls exit(1) and never returns.  However, when a
	// deferred function calls recover(), _tin_panic returns instead of exiting.
	// We must emit a valid terminator so the IR block is well-formed.
	//
	// Inside a coroutine body we emit the proper coro completion path so that
	// the fiber is marked as done and the coro frame is cleaned up correctly.
	// (A bare `ret` in a presplit coroutine body bypasses llvm.coro.end and
	// leaves the frame in an undefined state when the destroy path is called.)
	// If a subsequent explicit `return` statement is present (e.g. `return 0`
	// after `panic(...)`), its genCoroReturn call will overwrite block.Term with
	// the correct br->coro.final, which is harmless.
	if cg.inCoroFn {
		cg.ensureFiberRuntime()
		// If _tin_panic returns (panic was caught by defer+recover in this coro),
		// complete with the defer-override value if a thunk set one, otherwise
		// the zero value of the declared return type.  Passing nil would leave
		// the fiber result as NULL, causing a null dereference in the outer awaiter.
		cg.emitCoroComplete(block, cg.recoverRetVal(block))
		cg.emitFinalSuspend(block, cg.curCoroFrame)
	} else {
		retType := cg.curFn.Sig.RetType
		if irtypes.IsVoid(retType) {
			block.NewRet(nil)
		} else {
			block.NewRet(cg.zeroValue(retType))
		}
	}

	return nil, nil
}

// ensureSliceSubslice lazily declares _tin_slice_subslice(TinSlice s, i64 start, i64 elem_size) -> TinSlice.
// TinSlice has the fat-array layout: `{i8*, i64 len, i64 cap}`.
// Routed through ensureExternDecl so the ABI shim wraps it.
func (cg *CodeGen) ensureSliceSubslice() *ir.Func {
	// No top-level cache: see ensureBytesFromBuf for the rationale.
	sliceType := fatArrayPtrType(irtypes.I8)

	return cg.ensureExternDecl("_tin_slice_subslice", sliceType,
		[]*ir.Param{
			ir.NewParam("s", sliceType),
			ir.NewParam("start", irtypes.I64),
			ir.NewParam("elem_size", irtypes.I64),
		}, false)
}

// ensureSliceConvertInt lazily declares _tin_slice_convert_int(TinSlice s,
// i64 src_sz, i64 tgt_sz, i32 src_signed) -> TinSlice.
// Used by fat-array cross-type coercion to reallocate the buffer and convert
// integer elements from one width to another.
func (cg *CodeGen) ensureSliceConvertInt() *ir.Func {
	// No top-level cache: see ensureBytesFromBuf for the rationale.
	sliceType := fatArrayPtrType(irtypes.I8)

	return cg.ensureExternDecl("_tin_slice_convert_int", sliceType,
		[]*ir.Param{
			ir.NewParam("s", sliceType),
			ir.NewParam("src_sz", irtypes.I64),
			ir.NewParam("tgt_sz", irtypes.I64),
			ir.NewParam("src_signed", irtypes.I32),
		}, false)
}

// ensureRecoverFn lazily declares _tin_recover as a void function
// writing the recovered TinString to its `out` param.  The runtime
// uses an out-param shape instead of returning the 24-byte struct
// because the SRet shim path in ensureExternDecl has an ABI mismatch
// with clang 18's lowering on Linux x86_64; out-params route through
// pointer-passing, which both compilers handle identically.
// No top-level cache: see ensureBytesFromBuf for the rationale.
func (cg *CodeGen) ensureRecoverFn() *ir.Func {
	return cg.ensureExternDecl("_tin_recover", irtypes.Void,
		[]*ir.Param{ir.NewParam("out", irtypes.NewPointer(stringFatPtrType()))},
		false)
}

// genBuiltinRecover implements recover(): returns the panic message from a
// deferred function, or an empty string if not currently panicking.
func (cg *CodeGen) genBuiltinRecover(block *ir.Block) (value.Value, error) {
	outSlot := cg.hoistAlloca(block, stringFatPtrType())
	block.NewCall(cg.ensureRecoverFn(), outSlot)

	return block.NewLoad(stringFatPtrType(), outSlot), nil
}

// ensureRecoverTraceAtomsFn declares _tin_recover_trace_atoms as a
// void function writing to an out param.  Same ABI-dodge rationale
// as ensureRecoverFn.
func (cg *CodeGen) ensureRecoverTraceAtomsFn() *ir.Func {
	atomArrType := fatArrayPtrType(cg.atomType)

	return cg.ensureExternDecl("_tin_recover_trace_atoms", irtypes.Void,
		[]*ir.Param{ir.NewParam("out", irtypes.NewPointer(atomArrType))},
		false)
}

// genBuiltinRecoverTrace implements `recover('trace)`: returns a
// `(string, [atom])` tuple where the first element is the panic
// message (same string `recover()` would have returned) and the
// second is the call-site backtrace captured when `_tin_panic`
// fired.  Order matters: capture the trace BEFORE recovering the
// message because `_tin_recover` clears `_tin_panic_msg` which is
// also the "still panicking" gate inside `_tin_recover_trace_atoms`;
// if recover ran first the trace call would observe a cleared
// state and hand back an empty array.
func (cg *CodeGen) genBuiltinRecoverTrace(block *ir.Block) (value.Value, error) {
	atomArrTy := fatArrayPtrType(cg.atomType)
	traceSlot := cg.hoistAlloca(block, atomArrTy)
	block.NewCall(cg.ensureRecoverTraceAtomsFn(), traceSlot)
	traceVal := block.NewLoad(atomArrTy, traceSlot)

	msgSlot := cg.hoistAlloca(block, stringFatPtrType())
	block.NewCall(cg.ensureRecoverFn(), msgSlot)
	msgVal := block.NewLoad(stringFatPtrType(), msgSlot)

	// Monomorphise `Tuple[string, [atom]]` on demand so the resulting
	// value carries the same struct type a hand-written
	// `let t = ("msg", ['frame)` would have produced -- this lets
	// downstream destructuring (`let (msg, trace) = recover('trace)`)
	// resolve through the normal Tuple struct path.
	atomTypeName := "atom"
	tupName := "Tuple__string__[" + atomTypeName + "]"

	if cg.structTypeFor(CanonKey(tupName)) == nil {
		synthDecl := &ast.TypeDecl{
			Name: tupName,
			Type: &ast.GenericType{Name: "Tuple", TypeParams: []ast.TypeExpr{
				&ast.SimpleType{Name: "string"},
				&ast.ArrayType{Elem: &ast.SimpleType{Name: atomTypeName}, Size: -1},
			}},
		}
		_ = cg.genTypeDecl(synthDecl)
	}

	for i := 0; i < 64; i++ {
		alias := cg.aliasTypeFor(CanonKey(tupName))
		if alias == nil {
			break
		}

		st, isSimple := alias.(*ast.SimpleType)
		if !isSimple {
			break
		}

		if st.Name == tupName {
			break
		}

		tupName = st.Name
	}

	st := cg.structTypeFor(CanonKey(tupName))
	if st == nil {
		return nil, fmt.Errorf("recover('trace): failed to monomorphise Tuple[string, [atom]]")
	}

	alloca := block.NewAlloca(st)
	block.NewStore(constant.NewZeroInitializer(st), alloca)

	if typeID, has := cg.structTypeIDs[tupName]; has {
		typeIDGep := block.NewGetElementPtr(st, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		block.NewStore(constant.NewInt(irtypes.I32, int64(typeID)), typeIDGep)
	}

	userOff := cg.userFieldOffset(tupName)

	msgGep := block.NewGetElementPtr(st, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(userOff)))
	block.NewStore(msgVal, msgGep)

	traceGep := block.NewGetElementPtr(st, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(userOff+1)))
	block.NewStore(traceVal, traceGep)

	return block.NewLoad(st, alloca), nil
}

// ensureBytesFromBuf lazily declares _tin_bytes_from_buf(ptr *i8, len i64) {i8*, i64}.
// Copies len bytes from ptr into a new RC-allocated heap buffer and returns
// a fat [byte] slice.  Used to convert fixed-size stack arrays to [byte].
func (cg *CodeGen) ensureBytesFromBuf() *ir.Func {
	// Route through ensureExternDecl so the ABI-lowering shim wraps
	// the 24-byte TinSlice return.  No top-level cache: each pkg
	// module that uses the helper has to emit its own shim copy
	// (linkonce_odr dedups at link time) -- a single shared
	// `*ir.Func` from the first module won't satisfy later modules'
	// references.
	sliceType := fatArrayPtrType(irtypes.I8)

	return cg.ensureExternDecl("_tin_bytes_from_buf", sliceType,
		[]*ir.Param{
			ir.NewParam("ptr", irtypes.I8Ptr),
			ir.NewParam("len", irtypes.I64),
		}, false)
}

// ensureSnprintf lazily declares the snprintf external function.
// int snprintf(char* buf, size_t n, const char* format, ...)
func (cg *CodeGen) ensureSnprintf() *ir.Func {
	if cg.sprintfFn != nil {
		return cg.sprintfFn
	}

	cg.sprintfFn = cg.ensureExternDecl("snprintf", irtypes.I32,
		[]*ir.Param{ir.NewParam("buf", irtypes.I8Ptr), ir.NewParam("n", irtypes.I64), ir.NewParam("format", irtypes.I8Ptr)}, true)

	return cg.sprintfFn
}

// 128-bit echo / format helpers

// ensureEchoI128 lazily declares _tin_echo_i128(i128) void.
func (cg *CodeGen) ensureEchoI128() *ir.Func {
	if cg.echoI128Fn != nil {
		return cg.echoI128Fn
	}

	cg.echoI128Fn = cg.ensureExternDecl("_tin_echo_i128", irtypes.Void,
		[]*ir.Param{ir.NewParam("v", irtypes.I128)}, false)

	return cg.echoI128Fn
}

// ensureEchoU128 lazily declares _tin_echo_u128(i128) void.
func (cg *CodeGen) ensureEchoU128() *ir.Func {
	if cg.echoU128Fn != nil {
		return cg.echoU128Fn
	}

	cg.echoU128Fn = cg.ensureExternDecl("_tin_echo_u128", irtypes.Void,
		[]*ir.Param{ir.NewParam("v", irtypes.I128)}, false)

	return cg.echoU128Fn
}

// ensureEchoF128 lazily declares _tin_echo_f128(fp128) void.
func (cg *CodeGen) ensureEchoF128() *ir.Func {
	if cg.echoF128Fn != nil {
		return cg.echoF128Fn
	}

	cg.echoF128Fn = cg.ensureExternDecl("_tin_echo_f128", irtypes.Void,
		[]*ir.Param{ir.NewParam("v", irtypes.FP128)}, false)

	return cg.echoF128Fn
}

// ensureEchoStringEscaped lazily declares _tin_echo_string_escaped(i8*, i64) void.
func (cg *CodeGen) ensureEchoStringEscaped() *ir.Func {
	if cg.echoStringEscapedFn != nil {
		return cg.echoStringEscapedFn
	}

	cg.echoStringEscapedFn = cg.ensureExternDecl("_tin_echo_string_escaped", irtypes.Void,
		[]*ir.Param{ir.NewParam("ptr", irtypes.I8Ptr), ir.NewParam("len", irtypes.I64)}, false)

	return cg.echoStringEscapedFn
}

// ensurePrintStringEscaped lazily declares _tin_print_string_escaped(i8*, i64) void.
func (cg *CodeGen) ensurePrintStringEscaped() *ir.Func {
	if cg.printStringEscapedFn != nil {
		return cg.printStringEscapedFn
	}

	cg.printStringEscapedFn = cg.ensureExternDecl("_tin_print_string_escaped", irtypes.Void,
		[]*ir.Param{ir.NewParam("ptr", irtypes.I8Ptr), ir.NewParam("len", irtypes.I64)}, false)

	return cg.printStringEscapedFn
}

// ensureI128ToCstr lazily declares _tin_i128_to_cstr(i128) i8*.
func (cg *CodeGen) ensureI128ToCstr() *ir.Func {
	if cg.i128ToCstrFn != nil {
		return cg.i128ToCstrFn
	}

	cg.i128ToCstrFn = cg.ensureExternDecl("_tin_i128_to_cstr", irtypes.I8Ptr,
		[]*ir.Param{ir.NewParam("v", irtypes.I128)}, false)

	return cg.i128ToCstrFn
}

// ensureU128ToCstr lazily declares _tin_u128_to_cstr(i128) i8*.
func (cg *CodeGen) ensureU128ToCstr() *ir.Func {
	if cg.u128ToCstrFn != nil {
		return cg.u128ToCstrFn
	}

	cg.u128ToCstrFn = cg.ensureExternDecl("_tin_u128_to_cstr", irtypes.I8Ptr,
		[]*ir.Param{ir.NewParam("v", irtypes.I128)}, false)

	return cg.u128ToCstrFn
}

// ensureF128ToCstr lazily declares _tin_f128_to_cstr(fp128) i8*.
func (cg *CodeGen) ensureF128ToCstr() *ir.Func {
	if cg.f128ToCstrFn != nil {
		return cg.f128ToCstrFn
	}

	cg.f128ToCstrFn = cg.ensureExternDecl("_tin_f128_to_cstr", irtypes.I8Ptr,
		[]*ir.Param{ir.NewParam("v", irtypes.FP128)}, false)

	return cg.f128ToCstrFn
}

// emitAnyDispatchRegistrations emits calls to _tin_register_any_release
// for every struct that has a release helper (deinit or RC-tracked
// fields). Without this, an any-boxed *Cell or other struct with a
// custom destructor would skip its deinit entirely on scope exit -- the
// generic _tin_release_any path only frees the heap block.
//
// Returns the (possibly new) block at which subsequent code should
// continue emitting.

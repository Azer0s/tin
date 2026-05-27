package codegen

import (
	"fmt"
	"os"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// chanPtrFieldLoad loads `Channel._ptr` and returns it as a plain
// `i8*` regardless of whether the field is declared `*void` (default
// addrspace) or `volatile *void` (addrspace 1).  Channel ships with
// the volatile form so codegen does not emit an ARC retain/release
// for the foreign posix_memalign block; the matching-type load + an
// addrspacecast keeps the inline send/recv fast paths compatible
// with that declaration without forcing the runtime extern signatures
// to also live in addrspace 1.
func chanPtrFieldLoad(block *ir.Block, chanStructTy irtypes.Type, ptrFieldIdx int64, gep value.Value) value.Value {
	st, ok := chanStructTy.(*irtypes.StructType)
	if !ok || ptrFieldIdx < 0 || int(ptrFieldIdx) >= len(st.Fields) {
		return block.NewLoad(irtypes.I8Ptr, gep)
	}

	fieldTy := st.Fields[ptrFieldIdx]
	loaded := block.NewLoad(fieldTy, gep)

	if pt, ok := fieldTy.(*irtypes.PointerType); ok && pt.AddrSpace != 0 {
		return block.NewAddrSpaceCast(loaded, irtypes.I8Ptr)
	}

	return loaded
}

func (cg *CodeGen) genInlineAsyncDrive(block *ir.Block, callNode *ast.CallExpr) (value.Value, error) {
	cg.ensureCoroIntrinsics()
	cg.ensureFiberRuntime()

	var (
		coroFn     *ir.Func
		coroArgs   []value.Value
		origFnName string
	)

	switch fn := callNode.Func.(type) {
	case *ast.FieldAccess:
		// Method call: obj.method(args...)
		// Peek at the struct type without evaluating to decide quickly.
		structName := ""

		if id, ok2 := fn.Expr.(*ast.Identifier); ok2 {
			if se, ok3 := cg.curScope.lookup(id.Name); ok3 {
				t := se.val.Type()
				if se.isAlloc {
					if pt, ok4 := t.(*irtypes.PointerType); ok4 {
						t = pt.ElemType
					}
				}

				structName = cg.typeNameOf(t)
				if structName == "" {
					if pt, ok4 := t.(*irtypes.PointerType); ok4 {
						structName = cg.typeNameOf(pt.ElemType)
					}
				}
			}
		}

		if structName == "" {
			return nil, nil // can't determine struct type without evaluation - fall through
		}

		coroName := structName + "_" + fn.Field + "$coro"

		se, ok2 := cg.curScope.lookup(coroName)
		if !ok2 {
			return nil, nil // not {#async} - fall through
		}

		var ok3 bool

		coroFn, ok3 = se.val.(*ir.Func)
		if !ok3 {
			return nil, nil
		}

		origFnName = structName + "_" + fn.Field

		// Evaluate the object expression.
		objVal, err := cg.genExpr(block, fn.Expr)
		if err != nil {
			return nil, err
		}

		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}

		// Coerce this arg to the $coro ramp's first param type.
		// If the method expects a pointer receiver (*T) but we have a value T,
		// use genLValue to get the address or fall back to a temp alloca.
		thisArg := objVal

		if len(coroFn.Params) > 0 {
			firstParamTy := coroFn.Params[0].Type()
			if pt, isPtr := firstParamTy.(*irtypes.PointerType); isPtr && pt.ElemType.Equal(objVal.Type()) {
				if lv, err2 := cg.genLValue(block, fn.Expr); err2 == nil {
					thisArg = lv
				} else {
					tmp := block.NewAlloca(objVal.Type())
					block.NewStore(objVal, tmp)
					thisArg = tmp
				}
			} else {
				thisArg = cg.coerce(block, objVal, firstParamTy)
			}
		}

		coroArgs = []value.Value{thisArg}

		for i, arg := range callNode.Args {
			av, err2 := cg.genExpr(block, arg)
			if err2 != nil {
				return nil, err2
			}

			if cg.curBlock != nil && cg.curBlock != block {
				block = cg.curBlock
			}

			if i+1 < len(coroFn.Params) {
				av = cg.coerce(block, av, coroFn.Params[i+1].Type())
			}

			coroArgs = append(coroArgs, av)
		}

		// Fast path: Channel[T].send and Channel[T].recv - inline the blocking
		// retry loop directly into the outer coro using the outer coro's own
		// coro.suspend.  This eliminates the inner $coro frame allocation
		// (2 malloc/free per operation, 4 per round trip) at the cost of a
		// slightly larger outer coro frame (pid + blocked_val spilled to frame).
		// Channel[T].send and Channel[T].recv fast path.
		// structName may be bare ("Channel__i64") or package-prefixed ("sync__Channel__i64").
		if strings.HasPrefix(structName, "Channel__") || strings.HasPrefix(structName, "sync__Channel__") {
			if fn.Field == "send" && len(coroArgs) == 2 {
				var sendAstArg ast.Node
				if len(callNode.Args) >= 1 {
					sendAstArg = callNode.Args[0]
				}

				return cg.genDirectChanSend(block, coroArgs[0], coroArgs[1], sendAstArg)
			}

			if fn.Field == "recv" && len(coroArgs) == 1 {
				// Prefer deriving the element type from the concrete struct name
				// (e.g. "sync__Channel__*counter_t" -> elemType = *counter_t).
				// This handles pointer and other complex type parameters correctly
				// because the type-param alias (T -> *counter_t) has already been
				// cleaned up by the time the caller runs genDirectChanRecv.
				if elemType := cg.chanElemTypeFromName(structName); elemType != nil && !irtypes.IsVoid(elemType) {
					return cg.genDirectChanRecv(block, coroArgs[0], elemType)
				}
				// Fallback: origDecl.RetType (works for simple non-aliased types).
				if origDecl, ok4 := cg.funcDecls[origFnName]; ok4 && origDecl.RetType != nil {
					if elemType, err4 := cg.tinTypeToLLVM(origDecl.RetType); err4 == nil && elemType != nil && !irtypes.IsVoid(elemType) {
						return cg.genDirectChanRecv(block, coroArgs[0], elemType)
					}
				}
			}
		}

	case *ast.Identifier:
		// Free function call: fn(args...)
		coroName := fn.Name + "$coro"

		se, ok2 := cg.curScope.lookup(coroName)
		if !ok2 {
			return nil, nil // not {#async} - fall through
		}

		var ok3 bool

		coroFn, ok3 = se.val.(*ir.Func)
		if !ok3 {
			return nil, nil
		}

		origFnName = fn.Name

		for i, arg := range callNode.Args {
			av, err2 := cg.genExpr(block, arg)
			if err2 != nil {
				return nil, err2
			}

			if cg.curBlock != nil && cg.curBlock != block {
				block = cg.curBlock
			}

			if i < len(coroFn.Params) {
				av = cg.coerce(block, av, coroFn.Params[i].Type())
			}

			coroArgs = append(coroArgs, av)
		}

	default:
		return nil, nil // unsupported callee shape - fall through
	}

	cg.usesAnyFiber = true

	// Call the $coro ramp: allocates (or stack-allocates if coro-elide fires)
	// the inner coroutine frame and returns i8* handle.
	// Does NOT run the body; body starts on the first coro.resume call.
	innerHdl := block.NewCall(coroFn, coroArgs...)

	// Drive loop:
	//   drive.loop:
	//     _tin_inline_result_mode_begin()   ; arm TLS result buffer for inner coro
	//     llvm.coro.resume(inner)           ; run body until yield or done
	//     done = llvm.coro.done(inner)
	//     br done ? drive.done : drive.yield
	//   drive.yield:
	//     sp = llvm.coro.suspend(outer) ; outer suspends
	//     switch sp: 0 -> drive.loop, 1 -> cleanup
	//   drive.done:
	//     result = _tin_coro_take_result()
	//     llvm.coro.destroy(inner)
	//     _tin_inline_result_mode_end()
	// mode_begin is placed at the TOP of driveLoopBlk so it fires before EVERY
	// coro.resume - including re-entries after the outer fiber was parked and
	// resumed (at which point the worker loop reset _inline_result_mode to 0).
	// This keeps the TLS fast path active across park/unpark cycles.
	inlineBeginFn := cg.ensureExternDecl("_tin_inline_result_mode_begin", irtypes.Void,
		[]*ir.Param{}, false)

	driveLoopBlk := cg.newBlock("coro.drive.loop")
	block.NewBr(driveLoopBlk)

	driveLoopBlk.NewCall(inlineBeginFn)
	driveLoopBlk.NewCall(cg.coroResumeFn, innerHdl)
	done := driveLoopBlk.NewCall(cg.coroDoneFn, innerHdl)
	driveDoneBlk := cg.newBlock("coro.drive.done")
	driveYieldBlk := cg.newBlock("coro.drive.yield")
	driveLoopBlk.NewCondBr(done, driveDoneBlk, driveYieldBlk)

	// Yield path: inner yielded -> outer suspends to let inner run.
	// No _tin_fiber_yield_coro call needed (it's a no-op; worker loop
	// handles re-enqueue when FIBER_RUNNING status after _coro_resume returns).
	sp := driveYieldBlk.NewCall(cg.coroSuspendFn, coroNone, constant.NewInt(irtypes.I1, 0))
	suspBlk := cg.newBlock("coro.drive.suspended")
	cleanupBrBlk := cg.newBlock("coro.drive.cleanup.br")
	driveYieldBlk.NewSwitch(sp, suspBlk,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), driveLoopBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), cleanupBrBlk),
	)
	suspBlk.NewRet(cg.curCoroHdl)
	cleanupBrBlk.NewBr(cg.curCoroFrame.cleanupEntry)

	// Done path: take result, destroy inner frame.
	// llvm.coro.destroy would run the cleanup path (coro.end + coro.free), but
	// LLVM's coro-split pass generates empty destroy functions for trivially-
	// destructible C/Tin coroutines (no C++ dtors) - the cleanup call is optimized
	// away.  Call _tin_coro_free explicitly to return the heap-allocated frame to
	// the per-thread pool.  _tin_coro_free(null) is a no-op when coro-elide
	// stack-allocated the frame, so this is safe in all cases.
	resultRaw := driveDoneBlk.NewCall(cg.coroTakeResultFn)
	coroFreeFn := cg.ensureExternDecl("_tin_coro_free", irtypes.Void,
		[]*ir.Param{ir.NewParam("ptr", irtypes.I8Ptr)}, false)
	driveDoneBlk.NewCall(coroFreeFn, innerHdl)

	// End inline-result mode (balanced with the begin call before the ramp).
	inlineEndFn := cg.ensureExternDecl("_tin_inline_result_mode_end", irtypes.Void,
		[]*ir.Param{}, false)
	driveDoneBlk.NewCall(inlineEndFn)

	cg.curBlock = driveDoneBlk

	// Mark driveDoneBlk as a yield-resume equivalent so genYieldAutoAt suppresses
	// the redundant autoyield at the enclosing for-loop backedge.  The drive loop
	// already contains its own suspension points (coro.drive.yield) that fire when
	// the inner $coro blocks.  When the drive completes without blocking, the outer
	// fiber's natural park/unpark via the channel wakes the next fiber - no extra
	// autoyield is needed.
	if cg.yieldResumeBlocks != nil {
		cg.yieldResumeBlocks[driveDoneBlk] = true
	}

	// Determine result type (same lookup as wrapPidInFuture).
	var retTypeExpr ast.TypeExpr

	if origFnName != "" {
		if origDecl, ok2 := cg.funcDecls[origFnName]; ok2 && origDecl.RetType != nil {
			retTypeExpr = origDecl.RetType
		}
	}

	if retTypeExpr == nil {
		// void/Unit result - nothing to free.
		return constant.NewInt(irtypes.I1, 1), nil
	}

	retLLVM, err := cg.tinTypeToLLVM(retTypeExpr)
	if err != nil || retLLVM == nil || irtypes.IsVoid(retLLVM) {
		return constant.NewInt(irtypes.I1, 1), nil
	}

	typedPtr := driveDoneBlk.NewBitCast(resultRaw, irtypes.NewPointer(retLLVM))
	result := driveDoneBlk.NewLoad(retLLVM, typedPtr)
	// Free the result buffer (no-op if TLS, free() if heap-allocated for spawned fibers).
	inlineFreeFn := cg.ensureExternDecl("_tin_inline_result_free", irtypes.Void,
		[]*ir.Param{ir.NewParam("ptr", irtypes.I8Ptr)}, false)
	driveDoneBlk.NewCall(inlineFreeFn, resultRaw)

	return result, nil
}

// genDirectChanSend emits an inline channel-send retry loop that uses the outer
// coro's own llvm.coro.suspend instead of allocating an inner send$coro frame.
//
// Equivalent to the generated code for:
//
//	fn{#async #no_autoyield} send(this *Channel[T], val T) =
//	  let pid = _tin_current_pid()
//	  for true:
//	    let r = _tin_channel_send_blocking(this._ptr, &val, sizeof(T), isrc(T), pid)
//	    if r == -1: panic("send on closed channel")
//	    if r == 0: return
//	    yield   <- replaced by outer coro.suspend
//
// Eliminates 1 malloc + 1 free per send (2 per round trip).
func (cg *CodeGen) genDirectChanSend(block *ir.Block, thisPtr value.Value, valArg value.Value, astArg ast.Node) (value.Value, error) {
	cg.ensureCoroIntrinsics()
	cg.ensureFiberRuntime()
	cg.usesAnyFiber = true

	// Load ch._ptr from the Channel struct.
	// Layout: [i32 type_id, i8* _ptr, ...] so _ptr is at LLVM field index 1.
	// Use fieldIndex for correctness in case the layout changes.
	pt, isPtr := thisPtr.Type().(*irtypes.PointerType)
	if !isPtr {
		_, _ = fmt.Fprintf(os.Stderr, "tin: warning: genDirectChanSend: expected pointer type, got %T - falling back to slow send$coro path\n", thisPtr.Type())

		return nil, nil
	}

	chanStructTy := pt.ElemType

	ptrFieldIdx := int64(cg.fieldIndex(cg.typeNameOf(chanStructTy), "_ptr"))
	if ptrFieldIdx < 0 {
		ptrFieldIdx = 1 // fallback: type_id at 0, _ptr at 1
	}

	ptrFieldGEP := block.NewGetElementPtr(chanStructTy, thisPtr,
		constant.NewInt(irtypes.I32, 0),
		constant.NewInt(irtypes.I32, ptrFieldIdx))
	// _ptr is declared `volatile *void` so the field's LLVM type is
	// `i8 addrspace(1)*`.  Load with the matching type, then drop the
	// address space for the extern call (LLVM rejects bitcast across
	// address spaces, hence the explicit addrspacecast).  Loading
	// straight as `i8*` would trip the IR verifier on linux x86_64.
	chPtr := chanPtrFieldLoad(block, chanStructTy, ptrFieldIdx, ptrFieldGEP)

	// Alloca for val so send_blocking can take &val.  HOIST to function
	// entry: emitting in `block` would put it inside the caller's for-loop
	// body and grow the stack per iteration.  Each `await ch.send(v)` would
	// otherwise leak sizeof(T) bytes per loop iteration (~4 MB on the MPMC
	// bench, blowing macOS's 544 KB worker-thread stack).
	elemType := valArg.Type()
	valSlot := cg.hoistAlloca(block, elemType)
	block.NewStore(valArg, valSlot)
	valPtr := block.NewBitCast(valSlot, irtypes.I8Ptr)

	// sizeof(T) and is_rc - compile-time constants.
	elemSize := cg.llvmSizeOf(block, elemType)

	isRCVal := constant.NewInt(irtypes.I32, int64(channelRCKindOf(elemType)))

	// pid is constant for the lifetime of the fiber - hoist before the retry loop
	// so the TLS lookup is not repeated on every iteration.
	// Load _current_pid directly as a TLS variable (no function call overhead).
	pidVar := cg.ensureExternTLSVar("_current_pid", irtypes.I64)
	pid := block.NewLoad(irtypes.I64, pidVar)

	sendFn := cg.ensureExternDecl("_tin_channel_send_blocking", irtypes.I32,
		[]*ir.Param{
			ir.NewParam("ch", irtypes.I8Ptr),
			ir.NewParam("val", irtypes.I8Ptr),
			ir.NewParam("elem_size", irtypes.I64),
			ir.NewParam("is_rc", irtypes.I32),
			ir.NewParam("pid", irtypes.I64),
		}, false)

	retryBlk := cg.newBlock("chan.send.retry")
	block.NewBr(retryBlk)

	r := retryBlk.NewCall(sendFn, chPtr, valPtr, elemSize, isRCVal, pid)

	// r == -1 -> channel closed -> panic.
	isClosed := retryBlk.NewICmp(enum.IPredEQ, r, constant.NewInt(irtypes.I32, -1))
	checkDoneBlk := cg.newBlock("chan.send.check")
	panicBlk := cg.newBlock("chan.send.panic")
	retryBlk.NewCondBr(isClosed, panicBlk, checkDoneBlk)

	// Panic block - must follow the coro completion path (not a bare ret).
	panicMsg := cg.newGlobalString("send on closed channel")
	panicBlk.NewCall(cg.ensurePanicFn(), panicMsg)
	cg.emitCoroComplete(panicBlk, cg.recoverRetVal(panicBlk))
	cg.emitFinalSuspend(panicBlk, cg.curCoroFrame)

	// r == 0 -> success
	// r == 2 -> handoff: direct delivery to a waiting receiver; yield once then done
	// otherwise -> park and retry
	isDone := checkDoneBlk.NewICmp(enum.IPredEQ, r, constant.NewInt(irtypes.I32, 0))
	doneBlk := cg.newBlock("chan.send.done")
	checkHandoffBlk := cg.newBlock("chan.send.check.handoff")
	checkDoneBlk.NewCondBr(isDone, doneBlk, checkHandoffBlk)

	isHandoff := checkHandoffBlk.NewICmp(enum.IPredEQ, r, constant.NewInt(irtypes.I32, 2))
	handoffBlk := cg.newBlock("chan.send.handoff")
	yieldBlk := cg.newBlock("chan.send.yield")
	checkHandoffBlk.NewCondBr(isHandoff, handoffBlk, yieldBlk)

	// Handoff: the prepark optimization (advisory pre-registration of
	// the sender's next recv) was disabled in stdlib/sync/channel_arc.c.
	// Don't emit the call - it was returning 0 on entry, so every chan-send
	// handoff site paid for an extern call into a no-op.
	_ = pid
	// On resume the send is already complete: go straight to doneBlk.
	cg.emitInlineChanSuspend("chan.send.handoff", handoffBlk, doneBlk, doneBlk)

	// Park and retry: outer coro suspends until the channel has room.
	cg.emitInlineChanSuspend("chan.send", yieldBlk, retryBlk, doneBlk)
	// cg.curBlock == doneBlk after emitInlineChanSuspend.

	// Release temporary RC-tracked value after the send succeeds.
	// _tin_channel_send_blocking retains the element when is_rc==1, so the
	// sender's original reference must be dropped once the send completes.
	// Named variable arguments are owned by their enclosing scope and must NOT
	// be released here - the scope's exit will handle them.
	if astArg != nil && !isCopyExpr(astArg) && isRCTrackedType(valArg.Type()) {
		cg.emitRelease(doneBlk, valArg)
	}

	return constant.NewInt(irtypes.I1, 1), nil // void send - return sentinel i1 true
}

// genDirectChanRecv emits an inline channel-recv retry loop that uses the outer
// coro's own llvm.coro.suspend instead of allocating an inner recv$coro frame.
//
// Equivalent to the generated code for:
//
//	fn{#async #no_autoyield} recv(this *Channel[T]) T =
//	  let blocked = _tin_channel_recv_blocked_val()
//	  let pid = _tin_current_pid()
//	  for true:
//	    let r = _tin_channel_recv_blocking(this._ptr, pid)
//	    if r == null: panic("recv on closed channel")
//	    if (r as i64) != blocked: return *(r as *T)
//	    yield   <- replaced by outer coro.suspend
//
// Eliminates 1 malloc + 1 free per recv (2 per round trip).
func (cg *CodeGen) genDirectChanRecv(block *ir.Block, thisPtr value.Value, elemType irtypes.Type) (value.Value, error) {
	cg.ensureCoroIntrinsics()
	cg.ensureFiberRuntime()
	cg.usesAnyFiber = true

	// Load ch._ptr from the Channel struct.
	pt, isPtr := thisPtr.Type().(*irtypes.PointerType)
	if !isPtr {
		_, _ = fmt.Fprintf(os.Stderr, "tin: warning: genDirectChanRecv: expected pointer type, got %T - falling back to slow recv$coro path\n", thisPtr.Type())

		return nil, nil
	}

	chanStructTy := pt.ElemType

	ptrFieldIdx := int64(cg.fieldIndex(cg.typeNameOf(chanStructTy), "_ptr"))
	if ptrFieldIdx < 0 {
		ptrFieldIdx = 1 // fallback: type_id at 0, _ptr at 1
	}

	ptrFieldGEP := block.NewGetElementPtr(chanStructTy, thisPtr,
		constant.NewInt(irtypes.I32, 0),
		constant.NewInt(irtypes.I32, ptrFieldIdx))
	// `_ptr` is `volatile *void` (addrspace 1); see chanPtrFieldLoad
	// for the matching-addrspace load + addrspacecast rationale.
	chPtr := chanPtrFieldLoad(block, chanStructTy, ptrFieldIdx, ptrFieldGEP)

	// Alloca for result - written by _tin_channel_recv_direct, persists across
	// suspensions so the retry loop can safely re-use the slot on wakeup.
	// HOIST to function entry: emitting in `block` would put the alloca
	// inside the caller's for-loop body and grow the stack per iter (each
	// `await ch.recv()` in a `for` would leak elemType bytes).
	outSlot := cg.hoistAlloca(block, elemType)
	outPtr := block.NewBitCast(outSlot, irtypes.I8Ptr)

	// pid is constant for the lifetime of the fiber - hoist before the retry loop
	// so the TLS lookup is not repeated on every iteration.
	// Load _current_pid directly as a TLS variable (no function call overhead).
	pidVar := cg.ensureExternTLSVar("_current_pid", irtypes.I64)
	pid := block.NewLoad(irtypes.I64, pidVar)

	// _tin_channel_recv_direct writes directly into caller's alloca, eliminating
	// the per-thread TLS scratch buffer and pthread_getspecific overhead.
	// Returns: 0 = dequeued, 1 = blocked/contended (yield+retry), -1 = closed.
	recvFn := cg.ensureExternDecl("_tin_channel_recv_direct", irtypes.I32,
		[]*ir.Param{
			ir.NewParam("ch", irtypes.I8Ptr),
			ir.NewParam("pid", irtypes.I64),
			ir.NewParam("out", irtypes.I8Ptr),
		}, false)

	retryBlk := cg.newBlock("chan.recv.retry")
	block.NewBr(retryBlk)

	r := retryBlk.NewCall(recvFn, chPtr, pid, outPtr)

	// r == -1 -> channel closed and drained -> panic.
	isClosed := retryBlk.NewICmp(enum.IPredEQ, r, constant.NewInt(irtypes.I32, -1))
	checkBlk := cg.newBlock("chan.recv.check")
	panicBlk := cg.newBlock("chan.recv.panic")
	retryBlk.NewCondBr(isClosed, panicBlk, checkBlk)

	panicMsg := cg.newGlobalString("recv on closed channel")
	panicBlk.NewCall(cg.ensurePanicFn(), panicMsg)
	cg.emitCoroComplete(panicBlk, cg.recoverRetVal(panicBlk))
	cg.emitFinalSuspend(panicBlk, cg.curCoroFrame)

	// r == 1 -> yield and retry; r == 0 -> value written to outSlot.
	isBlocked := checkBlk.NewICmp(enum.IPredEQ, r, constant.NewInt(irtypes.I32, 1))
	doneBlk := cg.newBlock("chan.recv.done")
	yieldBlk := cg.newBlock("chan.recv.yield")
	checkBlk.NewCondBr(isBlocked, yieldBlk, doneBlk)

	// Yield: outer coro suspends until the channel has data.
	cg.emitInlineChanSuspend("chan.recv", yieldBlk, retryBlk, doneBlk)

	// Done: load T from the alloca that recv_direct wrote into.
	result := doneBlk.NewLoad(elemType, outSlot)

	return result, nil
}

// tryChannelWrapperFastPath detects `ch.recv()` / `ch.send(val)` call sites
// that target the Channel.{recv,send} sync wrapper (which returns
// `Future[T]` constructed via `spawn this.{recv,send}_impl(...)`) and emits
// the inline channel direct op directly, returning T (or no value for send)
// to the caller's coro frame.  The caller is expected to be `genAwaitExpr`
// in a coro context -- it bypasses both the wrapper call and the
// subsequent await on its returned Future.  Returns (val, true, err) when
// the fast path was applied; (nil, false, nil) when the call shape doesn't
// match and the caller should fall through to the standard await lowering.
//
// Mirrors the in-coro inline-drive path that genInlineAsyncDrive applies
// to a direct `fn{#async}` call.  The wrapper rework moved the actual
// `#async` body to `recv_impl` / `send_impl`, so we recognize the wrapper
// shape by callee name + receiver-struct prefix and re-anchor on the
// `_impl` method when emitting the inline retry loop.
func (cg *CodeGen) tryChannelWrapperFastPath(block *ir.Block, callNode *ast.CallExpr) (value.Value, bool, error) {
	fa, ok := callNode.Func.(*ast.FieldAccess)
	if !ok {
		return nil, false, nil
	}

	if fa.Field != "recv" && fa.Field != "send" {
		return nil, false, nil
	}

	// Try to get a direct lvalue pointer first.  When fa.Expr is an
	// identifier resolving to a let-binding (alloca) or a global
	// `Channel[T]`, genLValue returns a `*Channel[T]` directly -- saving
	// the per-call full-struct load + store into a scratch alloca that
	// the genExpr+spill path would emit.  In the workload bench's hot
	// loop (`for true: await g_in.recv(); ...; await g_out.send(...)`)
	// this removes two %Channel__T struct copies per iteration.
	var (
		thisPtr value.Value
		err     error
	)

	if lv, lvErr := cg.genLValue(block, fa.Expr); lvErr == nil && lv != nil {
		if _, isPtr := lv.Type().(*irtypes.PointerType); isPtr {
			thisPtr = lv
		}
	}

	if thisPtr == nil {
		thisVal, err2 := cg.genExpr(block, fa.Expr)
		if err2 != nil {
			return nil, false, err2
		}

		if thisVal == nil {
			return nil, false, nil
		}

		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}

		thisPtr = thisVal
		if _, isPtr := thisVal.Type().(*irtypes.PointerType); !isPtr {
			alloca := cg.hoistAlloca(block, thisVal.Type())
			block.NewStore(thisVal, alloca)
			thisPtr = alloca
		}
	} else if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
	}

	_ = err

	pt, isPtr := thisPtr.Type().(*irtypes.PointerType)
	if !isPtr {
		return nil, false, nil
	}

	structName := cg.typeNameOf(pt.ElemType)
	if !strings.HasPrefix(structName, "Channel__") && !strings.HasPrefix(structName, "sync__Channel__") {
		return nil, false, nil
	}

	// Receiver and shape match.  Emit the inline op against the channel
	// element type.  Element type is recovered from the struct's name
	// suffix (chanElemTypeFromName); falls through on inability to
	// resolve, letting the slow path take over.
	elemType := cg.chanElemTypeFromName(structName)
	if elemType == nil || irtypes.IsVoid(elemType) {
		return nil, false, nil
	}

	switch fa.Field {
	case "recv":
		if len(callNode.Args) != 0 {
			return nil, false, nil
		}

		val, err2 := cg.genDirectChanRecv(block, thisPtr, elemType)
		if err2 != nil {
			return nil, false, err2
		}

		if val == nil {
			return nil, false, nil
		}

		return val, true, nil
	case "send":
		if len(callNode.Args) != 1 {
			return nil, false, nil
		}

		valArg, err2 := cg.genExpr(block, callNode.Args[0])
		if err2 != nil {
			return nil, false, err2
		}

		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}

		valArg = cg.coerce(block, valArg, elemType)

		out, err2 := cg.genDirectChanSend(block, thisPtr, valArg, callNode.Args[0])
		if err2 != nil {
			return nil, false, err2
		}

		if out == nil {
			// send returns Unit; the caller (await Future[Unit]) doesn't
			// use the value but expects a non-nil result for the await
			// machinery.  Materialize the canonical Unit value.
			unitTy, lookupErr := cg.tinTypeToLLVM(&ast.SimpleType{Name: "sync::Unit"})
			if lookupErr != nil || unitTy == nil {
				return nil, false, nil
			}

			return constant.NewStruct(unitTy.(*irtypes.StructType), constant.NewInt(irtypes.I8, 0)), true, nil
		}

		return out, true, nil
	}

	return nil, false, nil
}

// activeSpawnFn returns the spawn function for the current context.
//
// All spawns use _tin_fiber_spawn_joinable (prejoined=1) by default so that a
// spawned fiber's slot cannot be ff_reclaimed and reused before the spawner
// calls _tin_fiber_join.  This is correct for:
//   - stored futures: `let f = spawn fn()` or `futures ++= spawn fn()` (awaited later)
//   - immediately awaited: `await spawn fn()` (auto-spawn path)
//   - non-coro context: test bodies, non-async main (TOCTOU fix)
//
// The only exception is a statement-level spawn (ExprStmt wrapping SpawnExpr)
// where the result is explicitly discarded.  In that case spawnFireForget=true
// allows _tin_fiber_spawn (prejoined=0) so the fiber can be ff_reclaimed at
// completion, keeping its slot available for reuse.
func (cg *CodeGen) activeSpawnFn() *ir.Func {
	if cg.stacktraceUsed {
		// Stacktrace-aware spawn variants (see
		// docs/plans/stacktrace-libunwind.md). The runtime signature
		// adds a uintptr_t caller_ip at the end; emitSpawnCall is
		// responsible for materializing it via llvm.returnaddress(0).
		if cg.spawnFireForget {
			return cg.fiberSpawnChainFn
		}

		return cg.fiberSpawnJoinableChainFn
	}

	if cg.spawnFireForget {
		return cg.fiberSpawnFn
	}

	return cg.fiberSpawnJoinableFn
}

// ensureLLVMReturnAddress lazily declares the i8*(i32) returnaddress
// intrinsic used at spawn sites to capture the caller's IP for the
// stacktrace spawn-chain.  Reusing one declaration across every spawn
// site keeps cg.mod's intrinsic list deduplicated.
func (cg *CodeGen) ensureLLVMReturnAddress() *ir.Func {
	if cg.llvmReturnAddressFn != nil {
		return cg.llvmReturnAddressFn
	}

	f := cg.mod.NewFunc("llvm.returnaddress", irtypes.I8Ptr,
		ir.NewParam("level", irtypes.I32))
	f.Blocks = nil
	cg.llvmReturnAddressFn = f

	return f
}

// emitSpawnCall is the single call-site entry point for fiber spawning.
// It selects the appropriate runtime variant via activeSpawnFn and, when
// stacktrace is reachable, materializes llvm.returnaddress(0) at the call
// site so the runtime can record this spawn's caller IP. Without this
// indirection, every spawn site would need to duplicate the
// stacktraceUsed branch.
func (cg *CodeGen) emitSpawnCall(block *ir.Block, hdl value.Value) value.Value {
	fn := cg.activeSpawnFn()
	if !cg.stacktraceUsed {
		return block.NewCall(fn, hdl)
	}

	retAddr := block.NewCall(cg.ensureLLVMReturnAddress(),
		constant.NewInt(irtypes.I32, 0))
	addrI64 := block.NewPtrToInt(retAddr, irtypes.I64)

	return block.NewCall(fn, hdl, addrI64)
}

// genSpawnExpr generates code for `spawn callExpr`.
// The callee must be a function marked {#async} (in coroCallable).
// Returns Future[T] wrapping the fiber PID.

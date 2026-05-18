package codegen

// coro.go - LLVM coroutine intrinsic support for the #async fiber system.
//
// llir/llvm v0.3.6 does not have a native "token" LLVM type. We implement a
// lightweight wrapper that satisfies the irtypes.Type interface so the emitted
// .ll file contains valid LLVM IR. The LLVM coroutine passes run automatically
// when clang processes the IR with -O1 or higher.

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// token type: LLVM pseudo-type used only by llvm.coro.* intrinsics

func (cg *CodeGen) llvmSizeOf(block *ir.Block, t irtypes.Type) value.Value {
	nullPtr := constant.NewNull(irtypes.NewPointer(t))
	gep := block.NewGetElementPtr(t, nullPtr, constant.NewInt(irtypes.I32, 1))

	return block.NewPtrToInt(gep, irtypes.I64)
}

// emitCoroComplete stores retVal (if non-void) and calls _tin_fiber_complete
// to hand the result to the drive loop.
//
// Result storage:
//   - _tin_inline_result_alloc(sz) is called instead of malloc directly.
//     When the inner $coro is being driven inline (genInlineAsyncDrive called
//     _tin_inline_result_mode_begin() before the ramp), this returns a TLS
//     pointer - no heap allocation.  For spawned fibers, it falls back to
//     malloc so the result survives beyond the worker loop iteration.
//
// The hdl parameter was removed from _tin_fiber_complete so that LLVM's
// coro-elide pass can see the inner $coro handle does not escape to any
// external function, enabling stack-allocation of inner coroutine frames.
func (cg *CodeGen) emitCoroComplete(block *ir.Block, retVal value.Value) {
	cg.ensureFiberRuntime()

	var resultI8Ptr value.Value
	if retVal == nil || irtypes.IsVoid(retVal.Type()) {
		resultI8Ptr = constant.NewNull(irtypes.I8Ptr)
	} else {
		// Store the result via the inline-result allocator.
		// - Inline drive: _tin_inline_result_mode_begin() was called -> TLS buffer (no malloc).
		// - Spawned fiber: mode not active -> malloc(sz) (result must outlive the coro).
		sz := cg.llvmSizeOf(block, retVal.Type())
		inlineAllocFn := cg.ensureExternDecl("_tin_inline_result_alloc", irtypes.I8Ptr,
			[]*ir.Param{ir.NewParam("sz", irtypes.I64)}, false)
		slot := block.NewCall(inlineAllocFn, sz)
		slotTyped := block.NewBitCast(slot, irtypes.NewPointer(retVal.Type()))
		block.NewStore(retVal, slotTyped)

		resultI8Ptr = slot
	}

	block.NewCall(cg.fiberCompleteFn, resultI8Ptr)
}

// genCoroFuncBody generates the LLVM IR body for the "$coro" ramp variant of n.
// coroName is "originalName$coro". The ramp suspends initially (returning the
// handle to the caller/scheduler), then runs the body on resume.
// captures and envStructType are non-nil only for spawn do: blocks that capture
// local variables; in that case unpackEnv restores them into the coro scope.
func (cg *CodeGen) genCoroFuncBody(n *ast.FuncDecl, coroName string, captures []closureCapture, envStructType *irtypes.StructType) error {
	// Look up the pre-declared $coro function.
	se, ok := cg.curScope.vars[coroName]
	if !ok {
		return nil
	}

	coroFn, ok := se.val.(*ir.Func)
	if !ok || len(coroFn.Blocks) > 0 {
		return nil // Already generated or not a function.
	}

	// LLVM's coro-split pass only processes functions marked presplitcoroutine.
	// Without this attribute the coroutine intrinsics survive to code-gen and
	// clang crashes during instruction selection.
	coroFn.FuncAttrs = append(coroFn.FuncAttrs, ir.AttrString("presplitcoroutine"))

	// Determine the original return type for body generation.
	var origRetType irtypes.Type = irtypes.Void

	if n.RetType != nil {
		var err error

		origRetType, err = cg.tinTypeToLLVM(n.RetType)
		if err != nil {
			return err
		}
	}

	// Save outer codegen context.
	prevFn := cg.curFn
	prevScope := cg.curScope
	prevInCoro := cg.inCoroFn
	prevCoroHdl := cg.curCoroHdl
	prevCoroID := cg.curCoroID
	prevCoroCleanup := cg.curCoroCleanup
	prevCoroFrame := cg.curCoroFrame
	prevCoroRetType := cg.curCoroRetType
	prevFnDeferRetAlloca := cg.curFnDeferRetAlloca
	prevDeferFnI8s := cg.pendingDeferFnI8s
	prevDeferFrames := cg.pendingDeferFrames
	prevDeferEnvs := cg.pendingDeferEnvs
	prevLabelCount := cg.labelCount
	prevMatchSubject := cg.matchSubject
	prevAutoYield := cg.curFnAutoYield
	prevYieldResumeBlocks := cg.yieldResumeBlocks
	prevCurBlock := cg.curBlock
	prevDiScope := cg.diCurrentScope

	cg.curBlock = nil
	cg.yieldResumeBlocks = make(map[*ir.Block]bool)
	cg.pendingDeferFnI8s = nil
	cg.pendingDeferFrames = nil
	cg.pendingDeferEnvs = nil
	cg.labelCount = 0
	cg.matchSubject = nil
	cg.curFnAutoYield = !hasTag(n.Tags, "no_autoyield")
	cg.curFn = coroFn
	cg.curScope = newScope(prevScope)
	cg.curScope.isFunctionBoundary = true

	// Emit DISubprogram for the coro function in debug builds.
	cg.emitDbgSubprogram(n, coroFn, cg.filename)

	// Emit coroutine prologue: entry -> coro.alloc -> coro.begin.
	entryBlk := coroFn.NewBlock("entry")

	cg.ensureFiberRuntime()
	frame, rampBlock := cg.emitCoroPrologue(entryBlk)

	// Recursive lambda self-ref: when the caller plumbed
	// `cg.lambdaSelfName` (genLambdaExpr's `#async` lambda emission
	// path), register a fat-fn-ptr value built from this coro's IR
	// func + its env arg under that name in the body scope so
	// recursive calls from within the coro body resolve through
	// callFatFn -> slot 0/1/2 -> the appropriate variant.  Mirrors
	// the sync + $colored variants' self-ref registration in
	// genLambdaExpr.  Cleared so nested async lambdas don't
	// inherit the outer binding.
	selfName := cg.lambdaSelfName
	cg.lambdaSelfName = ""

	if selfName != "" && n.RetType != nil && len(coroFn.Params) > 0 {
		envForSelf := coroFn.Params[0]
		// Look up the sync entry (registered earlier by
		// genLambdaExpr) to build the fat-fn-ptr.  Falls back to
		// using the coroFn itself if no sync entry is in scope
		// yet (defensive; should not happen for the
		// genLambdaExpr path).
		var syncFn *ir.Func

		if se, ok := cg.curScope.lookup(n.Name); ok {
			if f, ok2 := se.val.(*ir.Func); ok2 {
				syncFn = f
			}
		}

		if syncFn != nil {
			fatVal := cg.buildFatFnPtrValue(rampBlock, syncFn, envForSelf)
			fatSlot := rampBlock.NewAlloca(fatVal.Type())
			rampBlock.NewStore(fatVal, fatSlot)
			cg.curScope.set(selfName, &scopeEntry{val: fatSlot, isAlloc: true, noDeinit: true, noRelease: true})
		}
	}

	// Param offset: when the coro fn was predeclared with hasEnv=true,
	// Params[0] is the env pointer and user params start at index 1.
	// Detect by sig-arity difference vs the AST.
	nonVarArgCount := 0

	for _, astParam := range n.Params {
		if !astParam.IsVarArgs {
			nonVarArgCount++
		}
	}

	paramOffset := 0
	if len(coroFn.Params) > nonVarArgCount {
		paramOffset = 1
	}

	// ARC: retain every parameter before the initial suspend.
	//
	// The ramp function returns the coroutine handle to the scheduler before
	// any body code runs.  The caller's scope may release its local variables
	// immediately after spawn returns, so without a retain here there is a
	// window where the reference count can drop to zero and the value is freed
	// before the fiber ever reads it.  The matching release is the scope-exit
	// emitRelease at the end of the coroutine body.
	//
	// emitRetain handles all cases:
	//   - primitive RC-tracked types (string, []T, any)  -> _tin_retain
	//   - named structs with ARC-tracked fields          -> walkRCStructFields
	//   - named structs with C-level resources that
	//     define fn _fiber_retain                        -> calls that method
	//   - plain scalars / structs with no RC data        -> no-op
	{
		llParam := paramOffset

		for _, astParam := range n.Params {
			if astParam.IsVarArgs {
				continue
			}

			p := coroFn.Params[llParam]
			llParam++
			// Retain ARC-tracked data (strings, arrays, any, nested struct fields).
			cg.emitRetain(rampBlock, p)
			// Additionally call fn _fiber_retain for structs that manage C-level
			// resources outside the ARC system (e.g. Channel[T]).
			structName := cg.typeNameOf(p.Type())
			if structName == "" {
				continue
			}

			fiberRetainName := structName + "__fiber_retain"

			entry, ok := cg.curScope.lookup(fiberRetainName)
			if !ok {
				continue
			}

			fn, ok2 := entry.val.(*ir.Func)
			if !ok2 {
				continue
			}

			args := cg.adaptArgs(rampBlock, []value.Value{p}, fn.Sig)
			rampBlock.NewCall(fn, args...)
		}
	}

	// Emit initial suspend so the ramp returns the handle to the scheduler
	// before running any body code. The scheduler will resume on dequeue.
	bodyStart := cg.emitSuspendPoint(rampBlock, frame)

	// Unpack captured locals from env struct (spawn do: blocks only).
	// unpackEnv retains each RC-tracked value for the coro scope.
	// useEnvDirect=false: genCoroFuncBody frees the env after unpacking, so
	// we must copy values to local allocas (not use the env GEP directly).
	cg.unpackEnv(bodyStart, coroFn, envStructType, captures, false)

	// Detect closure-env layout (lambda coro variant): a leading i8*
	// dtor slot indicates the env was built via buildClosureEnv and is
	// RC-allocated, shared across the sync/colored/coro variants.  Do
	// NOT free() it here -- the RC dtor handles release.
	envIsClosureLayout := envStructType != nil && len(envStructType.Fields) == len(captures)+1 && envStructType.Fields[0] == irtypes.I8Ptr

	// ARC: release the env's own reference to each RC-tracked capture (the
	// matching retain was emitted in genSpawnDoBlock before buildEnv).
	// The coro scope now owns its own retain (from unpackEnv), so the env's
	// reference is no longer needed.  Also free the env struct itself.
	// Skip for closure-layout envs (lambda coro variant) -- the RC dtor
	// owns release of both the env block and its inner captures.
	if !envIsClosureLayout && len(captures) > 0 && envStructType != nil {
		for _, c := range captures {
			if !isRCTrackedType(c.llvmTy) {
				continue
			}

			if se, ok := cg.curScope.lookup(c.name); ok && se.isAlloc {
				loaded := bodyStart.NewLoad(c.llvmTy, se.val)
				cg.emitRelease(bodyStart, loaded)
			}
		}

		bodyStart.NewCall(cg.ensureFree(), coroFn.Params[0])
	}

	// Set up coroutine state for body code generation.
	cg.inCoroFn = true
	cg.curCoroHdl = frame.hdl
	cg.curCoroID = frame.id
	cg.curCoroCleanup = frame.cleanupEntry
	cg.curCoroFrame = frame
	cg.curCoroRetType = origRetType
	cg.usesAnyFiber = true

	// Set up defer return override slot for this coro body only when defer stmts
	// are present (mirrors genFuncDeclAs).  Always clear curFnDeferRetAlloca so
	// it doesn't bleed in from an outer function, causing cross-function SSA refs.
	if origRetType != nil && !irtypes.IsVoid(origRetType) && hasDeferStmt(n.Body) {
		slotType := irtypes.NewStruct(irtypes.I8, origRetType)
		slotAlloca := bodyStart.NewAlloca(slotType)
		validGep := bodyStart.NewGetElementPtr(slotType, slotAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		bodyStart.NewStore(constant.NewInt(irtypes.I8, 0), validGep)
		cg.curFnDeferRetAlloca = bodyStart.NewBitCast(slotAlloca, irtypes.I8Ptr)
	} else {
		cg.curFnDeferRetAlloca = nil
	}

	// Register self in scope for recursion.
	cg.curScope.set(coroName, &scopeEntry{val: coroFn, isAlloc: false})

	// Alloca parameters and register them in scope (same as genFuncDeclAs).
	// Note: RC-tracked parameters were already retained in the ramp block above
	// (before the initial suspend) so we do NOT emit another retain here.
	// The scope-exit release at body end provides the matching release.
	llIdx := paramOffset

	for _, astParam := range n.Params {
		if astParam.IsVarArgs {
			continue
		}

		p := coroFn.Params[llIdx]
		llIdx++
		alloca := bodyStart.NewAlloca(p.Type())
		bodyStart.NewStore(p, alloca)
		isRC := isRCTrackedType(p.Type())
		// Emit dbg.declare for this parameter in debug builds.
		cg.emitDbgDeclare(bodyStart, alloca, astParam.Name, n.Pos().Line, uint64(llIdx), astParam.Type, p.Type())
		// Parameters that have _fiber_retain called in the ramp block are co-owned
		// by the coro (the ramp increments the C-level RC). The scope-exit release
		// must call deinit to decrement that RC, so noDeinit must be false.
		// All other parameters use noDeinit=true because the caller still owns them.
		hasFiberRetain := false
		if structName := cg.typeNameOf(p.Type()); structName != "" {
			_, hasFiberRetain = cg.curScope.lookup(structName + "__fiber_retain")
		}

		cg.curScope.set(astParam.Name, &scopeEntry{val: alloca, isAlloc: true, isRC: isRC, noDeinit: !hasFiberRetain})
	}

	// Generate the function body. genReturn and genBody's addDefaultRet check
	// cg.inCoroFn and emit _tin_fiber_complete + coro.suspend instead of ret.
	if n.Body != nil {
		_, err := cg.genBody(bodyStart, n.Body, origRetType)
		if err != nil {
			return err
		}
	} else {
		// No body: immediately complete.
		cg.emitCoroComplete(bodyStart, nil)
		cg.emitFinalSuspend(bodyStart, frame)
	}

	// Ensure all call instructions have !dbg (required when DISubprogram is attached).
	cg.ensureAllCallsHaveDbg(coroFn)

	// Emit coroutine cleanup epilogue (coro.free + free + coro.end + ret hdl).
	cg.emitCoroEpilogue(frame)

	// Restore outer codegen context.
	cg.curFn = prevFn
	cg.curScope = prevScope
	cg.inCoroFn = prevInCoro
	cg.curCoroHdl = prevCoroHdl
	cg.curCoroID = prevCoroID
	cg.curCoroCleanup = prevCoroCleanup
	cg.curCoroFrame = prevCoroFrame
	cg.curCoroRetType = prevCoroRetType
	cg.curFnDeferRetAlloca = prevFnDeferRetAlloca
	cg.pendingDeferFnI8s = prevDeferFnI8s
	cg.pendingDeferFrames = prevDeferFrames
	cg.pendingDeferEnvs = prevDeferEnvs
	cg.labelCount = prevLabelCount
	cg.matchSubject = prevMatchSubject
	cg.curFnAutoYield = prevAutoYield
	cg.yieldResumeBlocks = prevYieldResumeBlocks
	cg.curBlock = prevCurBlock
	cg.diCurrentScope = prevDiScope

	return nil
}

// recoverRetVal builds the return value to use after _tin_panic returns from a
// recovered panic inside a coroutine body.  If a deferred thunk wrote an
// override value to curFnDeferRetAlloca, that value is used; otherwise the zero
// value of the coro's declared return type is used.  Returns nil for void.
func (cg *CodeGen) recoverRetVal(block *ir.Block) value.Value {
	rt := cg.curCoroRetType
	if rt == nil || irtypes.IsVoid(rt) {
		return nil
	}

	base := cg.zeroValue(rt)
	if cg.curFnDeferRetAlloca == nil {
		return base
	}

	slotType := irtypes.NewStruct(irtypes.I8, rt)
	slotPtr := block.NewBitCast(cg.curFnDeferRetAlloca, irtypes.NewPointer(slotType))
	validGep := block.NewGetElementPtr(slotType, slotPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	valid := block.NewLoad(irtypes.I8, validGep)
	isValid := block.NewICmp(enum.IPredNE, valid, constant.NewInt(irtypes.I8, 0))
	valGep := block.NewGetElementPtr(slotType, slotPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	overrideVal := block.NewLoad(rt, valGep)

	return block.NewSelect(isValid, overrideVal, base)
}

// emitInlineChanSuspend wires up the coro.suspend / resume / cleanup blocks
// for an inline channel retry loop (genDirectChanSend, genDirectChanRecv, or
// any future inline channel op).
//
// LLVM coroutine ABI contract encoded here (single place to update if it changes):
//
//	coro.suspend(none, false) returns i8:
//	  0  -> normal resume   -> jump back to retryBlk
//	  1  -> final cleanup   -> jump to coro cleanup entry
//	  default -> the "suspend" path; the outer function returns its handle
//
// doneBlk is marked in yieldResumeBlocks so the auto-yield pass at the next
// loop backedge sees that a real suspension point was just traversed and skips
// inserting a redundant yield.  cg.curBlock is updated to doneBlk.
func (cg *CodeGen) emitInlineChanSuspend(prefix string, yieldBlk, retryBlk, doneBlk *ir.Block) {
	sp := yieldBlk.NewCall(cg.coroSuspendFn, coroNone, constant.NewInt(irtypes.I1, 0))
	suspBlk := cg.newBlock(prefix + ".suspended")
	cleanupBlk := cg.newBlock(prefix + ".cleanup")
	yieldBlk.NewSwitch(sp, suspBlk,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), retryBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), cleanupBlk),
	)
	suspBlk.NewRet(cg.curCoroHdl)
	cleanupBlk.NewBr(cg.curCoroFrame.cleanupEntry)

	if cg.yieldResumeBlocks != nil {
		cg.yieldResumeBlocks[doneBlk] = true
	}

	cg.curBlock = doneBlk
}

// ensureFiberCheckPanicFn lazily declares _tin_fiber_check_panic() -> i8*.
// Returns the first retained panic message from a non-awaited fiber, or NULL.
func (cg *CodeGen) ensureFiberCheckPanicFn() *ir.Func {
	if cg.fiberCheckPanicFn != nil {
		return cg.fiberCheckPanicFn
	}

	cg.fiberCheckPanicFn = cg.ensureExternDecl("_tin_fiber_check_panic", irtypes.I8Ptr, nil, false)

	return cg.fiberCheckPanicFn
}

// ensurePanicFlagGlobal lazily declares _has_unhandled_panics as an external i32 global.
// Used by emitPanicCheck to avoid a function call on the hot path (flag == 0 case).
func (cg *CodeGen) ensurePanicFlagGlobal() *ir.Global {
	if cg.panicFlagGlobal != nil {
		return cg.panicFlagGlobal
	}

	g := cg.mod.NewGlobal("_has_unhandled_panics", irtypes.I32)
	g.Linkage = enum.LinkageExternal
	cg.panicFlagGlobal = g

	return g
}

// emitPanicCheck emits a two-level unhandled-panic check after a coro resume point.
//
// Fast path (common): atomic load of _has_unhandled_panics; if zero, jump to doneBlk.
// Slow path (rare):   call _tin_fiber_check_panic(); if null, jump to doneBlk; else panic.
//
// resumeBlk is terminated here. doneBlk must have no terminator yet.
func (cg *CodeGen) emitPanicCheck(resumeBlk *ir.Block, doneBlk *ir.Block, suffix string) {
	flagLoad := resumeBlk.NewLoad(irtypes.I32, cg.ensurePanicFlagGlobal())
	flagLoad.Atomic = true
	flagLoad.Ordering = enum.AtomicOrderingMonotonic
	flagLoad.Align = 4
	hasFlag := resumeBlk.NewICmp(enum.IPredNE, flagLoad, constant.NewInt(irtypes.I32, 0))
	slowBlk := cg.newBlock(suffix + ".slow")
	resumeBlk.NewCondBr(hasFlag, slowBlk, doneBlk)

	msg := slowBlk.NewCall(cg.ensureFiberCheckPanicFn())
	isNotNull := slowBlk.NewICmp(enum.IPredNE, msg, constant.NewNull(irtypes.I8Ptr))
	panicBlk := cg.newBlock(suffix + ".panic")
	slowBlk.NewCondBr(isNotNull, panicBlk, doneBlk)

	// Do NOT release msg - the defer thunk already balances the retain added by
	// _tin_fiber_check_panic (same as the await.panic path; see genAwaitExpr).
	panicBlk.NewCall(cg.ensurePanicFn(), msg)
	cg.emitCoroComplete(panicBlk, cg.recoverRetVal(panicBlk))
	cg.emitFinalSuspend(panicBlk, cg.curCoroFrame)
}

// genCallSiteYield emits a coro.suspend before calling a heavy or recursive
// function from inside a coroutine body.  Returns the block to continue
// emitting into (the resume block after the suspend point).
//
// Must only be called when cg.curCoroFrame != nil and cg.curFnAutoYield is true.
// After each resume, unhandled panics from fire-and-forget fibers are checked
// and re-raised.
//
// cg.curBlock is set to afterBlk so that the "if cg.curBlock != block {block = cg.curBlock}"
// pattern used in genStmt, genVarDecl, genReturn, etc. picks up the block advance.
func (cg *CodeGen) genCallSiteYield(from *ir.Block) *ir.Block {
	resume := cg.emitSuspendPoint(from, cg.curCoroFrame)
	afterBlk := cg.newBlock("callsite.yield.after")
	cg.emitPanicCheck(resume, afterBlk, "callsite.yield")
	cg.curBlock = afterBlk

	return afterBlk
}

// genColoredCallSiteYield is the $colored-body counterpart of
// genCallSiteYield: the body has no coro frame of its own, so the
// yield routes through the caller's TLS-tracked hdl via
// `_tin_fiber_yield_coro(_tin_current_coro_hdl())`.  No suspend
// intrinsic, no resume block split -- the runtime call simply returns
// once the scheduler hands the fiber back.  We still emit a panic
// check afterwards so fire-and-forget fibers' unhandled panics
// surface on the same per-iteration cadence as the $coro path.
//
// Caller contract is the same as genCallSiteYield: returns the
// (possibly advanced) block and updates cg.curBlock.
func (cg *CodeGen) genColoredCallSiteYield(from *ir.Block) *ir.Block {
	cg.emitColoredRuntimeYield(from)
	afterBlk := cg.newBlock("callsite.colored.yield.after")
	cg.emitColoredPanicCheck(from, afterBlk, "callsite.colored.yield")
	cg.curBlock = afterBlk

	return afterBlk
}

// emitColoredRuntimeYield emits the runtime yield call used by
// $colored bodies: `_tin_fiber_yield_coro(_tin_current_coro_hdl())`.
// The TLS lookup returns the current fiber's coro hdl (set by the
// scheduler before resuming); passing it to _tin_fiber_yield_coro
// suspends and reschedules.  No coro frame allocation, no intrinsics.
// Block is NOT terminated -- the yield is a regular call instruction.
func (cg *CodeGen) emitColoredRuntimeYield(block *ir.Block) {
	cg.ensureFiberRuntime()
	hdl := block.NewCall(cg.ensureCurrentCoroHdlFn())
	block.NewCall(cg.fiberYieldCoroFn, hdl)
}

// emitColoredPanicCheck is the non-coro counterpart of emitPanicCheck:
// terminates `from` with a conditional branch to a slow-path block
// that drains an unhandled panic flag (matching the $coro path's
// behavior after each resume).  Lands in `doneBlk` on the fast path
// and on the slow path after the optional panic recovery.
//
// Mirrors emitPanicCheck except the panic re-raise path emits a
// plain `_tin_panic` + `ret` instead of the coro-completion sequence
// (no frame to complete).
func (cg *CodeGen) emitColoredPanicCheck(from *ir.Block, doneBlk *ir.Block, suffix string) {
	flagLoad := from.NewLoad(irtypes.I32, cg.ensurePanicFlagGlobal())
	flagLoad.Atomic = true
	flagLoad.Ordering = enum.AtomicOrderingMonotonic
	flagLoad.Align = 4
	hasFlag := from.NewICmp(enum.IPredNE, flagLoad, constant.NewInt(irtypes.I32, 0))
	slowBlk := cg.newBlock(suffix + ".slow")
	from.NewCondBr(hasFlag, slowBlk, doneBlk)

	msg := slowBlk.NewCall(cg.ensureFiberCheckPanicFn())
	isNotNull := slowBlk.NewICmp(enum.IPredNE, msg, constant.NewNull(irtypes.I8Ptr))
	panicBlk := cg.newBlock(suffix + ".panic")
	slowBlk.NewCondBr(isNotNull, panicBlk, doneBlk)

	panicBlk.NewCall(cg.ensurePanicFn(), msg)
	panicBlk.NewUnreachable()
}

// ensureCurrentCoroHdlFn lazily declares _tin_current_coro_hdl() i8*.
// Runtime helper that returns the current fiber's coro hdl from TLS.
// Used by $colored bodies to drive yields through the caller's frame.
func (cg *CodeGen) ensureCurrentCoroHdlFn() *ir.Func {
	if cg.currentCoroHdlFn != nil {
		return cg.currentCoroHdlFn
	}

	cg.currentCoroHdlFn = cg.ensureExternDecl("_tin_current_coro_hdl", irtypes.I8Ptr, nil, false)

	return cg.currentCoroHdlFn
}

// genCallSiteYieldFor checks whether the named callee warrants a pre-call yield
// and, if so, calls genCallSiteYield.  Returns the (possibly updated) block to
// use for the actual call instruction.
//
// Conditions for emitting a yield:
//   - the callee is classified as AutoYield (heavy or recursive) in funcHeuristics
//   - the current function allows auto-yield (curFnAutoYield)
//   - EITHER curCoroFrame != nil (inside $coro variant)
//     OR curFnColoredSync (inside $colored variant) -- yields via runtime call
func (cg *CodeGen) genCallSiteYieldFor(block *ir.Block, calleeName string) *ir.Block {
	if !cg.curFnAutoYield {
		return block
	}

	if cg.curCoroFrame == nil && !cg.curFnColoredSync {
		return block
	}

	info, ok := cg.funcHeuristics[calleeName]
	if !ok || !info.AutoYield {
		return block
	}

	if cg.curFnColoredSync {
		return cg.genColoredCallSiteYield(block)
	}

	return cg.genCallSiteYield(block)
}

// genYieldAutoAt emits an automatic yield point at the backedge of a loop.
// `from` is the block at the end of the loop body; after yielding it resumes
// at `header` (the loop condition or post block).
// Only called when cg.curFnAutoYield is true.
//
// After each resume, checks for unhandled panics from fire-and-forget fibers
// (_tin_fiber_check_panic). If one is found, it is re-raised in the current
// fiber so it surfaces at the earliest possible loop iteration rather than
// only at scheduler shutdown.
func (cg *CodeGen) genYieldAutoAt(from *ir.Block, header *ir.Block) {
	if cg.yieldResumeBlocks[from] {
		// `from` is the continuation block of an explicit `yield` or `await`.
		// The fiber just executed one suspension this iteration; adding a second
		// autoyield at the backedge would force a redundant scheduler round-trip.
		from.NewBr(header)

		return
	}

	if cg.curFnColoredSync {
		// $colored body: no coro frame -- yield via runtime call.
		cg.emitColoredRuntimeYield(from)
		cg.emitColoredPanicCheck(from, header, "autoyield.colored")

		return
	}

	resume := cg.emitSuspendPoint(from, cg.curCoroFrame)
	cg.emitPanicCheck(resume, header, "autoyield")
}

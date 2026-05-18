package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) genAwaitMatch(block *ir.Block, s *ast.AwaitMatchStmt) (*ir.Block, error) {
	cg.prepareAwaitMatchGuards(s.Cases)
	cg.ensureFiberRuntime()

	n := len(s.Futures)

	// Evaluate each future expression once and extract its PID.
	slots := make([]awMatchSlot, n)

	for i, fnode := range s.Futures {
		fval, err := cg.genExpr(block, fnode)
		if err != nil {
			return nil, fmt.Errorf("await match: future %d: %w", i, err)
		}

		sname := structNameFromValue(fval)
		if sname == "" || len(sname) <= 8 || sname[:8] != "Future__" {
			return nil, fmt.Errorf("await match: expression at index %d is not a Future[T] (got type %s)", i, cg.fmtArgType(fval.Type()))
		}

		pidIdx := cg.fieldIndex(sname, "pid")
		if pidIdx < 0 {
			return nil, fmt.Errorf("await match: Future type %s has no pid field", sname)
		}

		pid := block.NewExtractValue(fval, uint64(pidIdx))

		retTypeName := sname[8:]

		var retLLVM irtypes.Type

		if retTypeName != "" && retTypeName != "Unit" {
			var rerr error

			retLLVM, rerr = cg.resolveSimpleType(retTypeName)
			if rerr != nil {
				retLLVM = nil
			}
		}

		slots[i] = awMatchSlot{val: fval, pid: pid, structName: sname, retType: retLLVM}
	}

	// Build a fixed-size [n x i64] PID array on the stack.
	pidArrayType := irtypes.NewArray(uint64(n), irtypes.I64)
	pidAlloca := block.NewAlloca(pidArrayType)

	for i, sl := range slots {
		gep := block.NewGetElementPtr(pidArrayType, pidAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
		block.NewStore(sl.pid, gep)
	}

	pidsPtr := block.NewGetElementPtr(pidArrayType, pidAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	nConst := constant.NewInt(irtypes.I64, int64(n))

	// Ensure runtime function declarations.
	pollAnySkipFn := cg.ensureExternDecl("_tin_fiber_poll_any_skip", irtypes.I64,
		[]*ir.Param{
			ir.NewParam("pids", irtypes.NewPointer(irtypes.I64)),
			ir.NewParam("n", irtypes.I64),
			ir.NewParam("skip", irtypes.I8Ptr),
		}, false)

	afterBlock := cg.newBlock("awmatch.after")

	// skipAlloca: [n x i8] bitmask tracking slots whose guards failed.
	skipType := irtypes.NewArray(uint64(n), irtypes.I8)
	skipAlloca := block.NewAlloca(skipType)

	// Zero-initialize skip mask.
	for i := 0; i < n; i++ {
		gep := block.NewGetElementPtr(skipType, skipAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(i)))
		block.NewStore(constant.NewInt(irtypes.I8, 0), gep)
	}

	skipPtr := block.NewGetElementPtr(skipType, skipAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))

	// WITH default: one non-blocking poll pass
	if s.Default != nil {
		defaultBlock := cg.newBlock("awmatch.default")

		// Poll: find first done, non-skipped slot with a passing guard.
		// Linear scan through case arms; fall through to default if nothing actionable.
		checkBlock := block

		for i, c := range s.Cases {
			slotPid := slots[c.SlotIdx].pid
			doneCheckBlock := cg.newBlock(fmt.Sprintf("awmatch.donecheck.%d", i))
			nextArmBlock := cg.newBlock(fmt.Sprintf("awmatch.nextarm.%d", i))

			// Check FIBER_DONE for this slot via poll_any_skip on a single-element array.
			// Simpler: call _tin_fiber_poll_any_skip which already handles the table lock.
			// We build a 1-element pid array for the single-slot check.
			// Alternatively emit _tin_fiber_get_done(pid) - but we don't have that.
			// Use a temporary alloca with just this pid and a zero skip mask.
			singlePidAlloca := checkBlock.NewAlloca(irtypes.I64)
			checkBlock.NewStore(slotPid, singlePidAlloca)
			singleSkipAlloca := checkBlock.NewAlloca(irtypes.I8)
			checkBlock.NewStore(constant.NewInt(irtypes.I8, 0), singleSkipAlloca)

			idx := checkBlock.NewCall(pollAnySkipFn, singlePidAlloca,
				constant.NewInt(irtypes.I64, 1), singleSkipAlloca)
			isDone := checkBlock.NewICmp(enum.IPredEQ, idx, constant.NewInt(irtypes.I64, 0))
			checkBlock.NewCondBr(isDone, doneCheckBlock, nextArmBlock)

			checkBlock = doneCheckBlock

			// Slot is done. Bind result and check guard if present.
			cg.curScope = newScope(cg.curScope)

			okBlk, bindErr := cg.bindAwaitMatchSlot(checkBlock, c, slots[c.SlotIdx])
			if bindErr != nil {
				cg.curScope = cg.curScope.parent

				return nil, bindErr
			}

			armEntryBlock := okBlk

			if c.Guard != nil {
				guardVal, err := cg.genExpr(armEntryBlock, c.Guard)
				if err != nil {
					cg.curScope = cg.curScope.parent

					return nil, err
				}

				guardPassBlock := cg.newBlock(fmt.Sprintf("awmatch.guardpass.%d", i))
				armEntryBlock.NewCondBr(cg.toBoolImplicit(armEntryBlock, guardVal), guardPassBlock, nextArmBlock)
				armEntryBlock = guardPassBlock
			}

			// Emit arm body.
			bodyBlock, _, err := cg.genStmt(armEntryBlock, c.Body)
			cg.curScope = cg.curScope.parent

			if err != nil {
				return nil, err
			}

			if bodyBlock != nil && bodyBlock.Term == nil {
				bodyBlock.NewBr(afterBlock)
			}

			checkBlock = nextArmBlock
		}

		// Nothing actionable: go to default.
		checkBlock.NewBr(defaultBlock)

		cg.curScope = newScope(cg.curScope)
		defBlock, _, err := cg.genStmt(defaultBlock, s.Default)
		cg.curScope = cg.curScope.parent

		if err != nil {
			return nil, err
		}

		if defBlock != nil && defBlock.Term == nil {
			defBlock.NewBr(afterBlock)
		}

		return afterBlock, nil
	}

	// WITHOUT default: blocking loop
	// Loop: join_any -> poll -> dispatch; re-loop if guard fails; panic if exhausted.

	// anyWaiterType mirrors TinAnyWaiter in fiber.c:
	// { i64 waiter_pid, i32 fired (atomic), i32 pad, i64 result_idx, i64* pids, i64 n }
	anyWaiterType := irtypes.NewStruct(irtypes.I64, irtypes.I32, irtypes.I32, irtypes.I64,
		irtypes.NewPointer(irtypes.I64), irtypes.I64)
	anyWaiterAlloca := block.NewAlloca(anyWaiterType)
	_ = anyWaiterAlloca

	joinAnyFn := cg.ensureExternDecl("_tin_fiber_join_any", irtypes.Void,
		[]*ir.Param{
			ir.NewParam("pids", irtypes.NewPointer(irtypes.I64)),
			ir.NewParam("n", irtypes.I64),
			ir.NewParam("skip", irtypes.I8Ptr),
			ir.NewParam("my_hdl", irtypes.I8Ptr),
			ir.NewParam("aw", irtypes.I8Ptr),
		}, false)

	syncAwaitAnyFn := cg.ensureExternDecl("_tin_fiber_sync_await_any", irtypes.I64,
		[]*ir.Param{
			ir.NewParam("pids", irtypes.NewPointer(irtypes.I64)),
			ir.NewParam("n", irtypes.I64),
			ir.NewParam("skip", irtypes.I8Ptr),
		}, false)

	loopBlock := cg.newBlock("awmatch.loop")
	block.NewBr(loopBlock)

	// loop body
	var resumeBlock *ir.Block

	if cg.inCoroFn {
		awPtr := loopBlock.NewBitCast(anyWaiterAlloca, irtypes.I8Ptr)
		loopBlock.NewCall(joinAnyFn, pidsPtr, nConst, skipPtr,
			cg.curCoroHdl, awPtr)
		resumeBlock = cg.emitSuspendPoint(loopBlock, cg.curCoroFrame)
	} else {
		// Non-async context: synchronous spin-wait.
		idx := loopBlock.NewCall(syncAwaitAnyFn, pidsPtr, nConst, skipPtr)
		_ = idx
		resumeBlock = loopBlock
	}

	// After resume: poll to find which slot fired.
	idx := resumeBlock.NewCall(pollAnySkipFn, pidsPtr, nConst, skipPtr)

	// Check exhaustion: idx == -1 means all slots skipped.
	exhaustedBlock := cg.newBlock("awmatch.exhausted")
	dispatchBlock := cg.newBlock("awmatch.dispatch")
	resumeBlock.NewCondBr(
		resumeBlock.NewICmp(enum.IPredEQ, idx, constant.NewInt(irtypes.I64, -1)),
		exhaustedBlock, dispatchBlock)

	// Exhaustion: panic.
	exhaustMsg := cg.newGlobalString("await match: all futures exhausted, no arm matched")
	exhaustedBlock.NewCall(cg.ensurePanicFn(), exhaustMsg)

	retType := cg.curFn.Sig.RetType
	if irtypes.IsVoid(retType) {
		exhaustedBlock.NewRet(nil)
	} else {
		exhaustedBlock.NewRet(cg.zeroValue(retType))
	}

	// Dispatch: if-else chain over case arms.
	checkBlock := dispatchBlock

	for i, c := range s.Cases {
		matchBlock := cg.newBlock(fmt.Sprintf("awmatch.arm.%d", i))
		noMatchBlock := cg.newBlock(fmt.Sprintf("awmatch.nomatch.%d", i))

		slotConst := constant.NewInt(irtypes.I64, int64(c.SlotIdx))
		isThisSlot := checkBlock.NewICmp(enum.IPredEQ, idx, slotConst)
		checkBlock.NewCondBr(isThisSlot, matchBlock, noMatchBlock)

		cg.curScope = newScope(cg.curScope)

		okBlk, bindErr := cg.bindAwaitMatchSlot(matchBlock, c, slots[c.SlotIdx])
		if bindErr != nil {
			cg.curScope = cg.curScope.parent

			return nil, bindErr
		}

		armEntry := okBlk

		// Guard check.
		if c.Guard != nil {
			guardVal, err := cg.genExpr(armEntry, c.Guard)
			if err != nil {
				cg.curScope = cg.curScope.parent

				return nil, err
			}

			guardPassBlock := cg.newBlock(fmt.Sprintf("awmatch.gpass.%d", i))
			guardFailBlock := cg.newBlock(fmt.Sprintf("awmatch.gfail.%d", i))
			armEntry.NewCondBr(cg.toBoolImplicit(armEntry, guardVal), guardPassBlock, guardFailBlock)

			// Guard fail: mark slot as skipped, re-loop.
			skipGep := guardFailBlock.NewGetElementPtr(skipType, skipAlloca,
				constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(c.SlotIdx)))
			guardFailBlock.NewStore(constant.NewInt(irtypes.I8, 1), skipGep)
			guardFailBlock.NewBr(loopBlock)

			armEntry = guardPassBlock
		}

		// Emit body.
		bodyBlock, _, err := cg.genStmt(armEntry, c.Body)
		cg.curScope = cg.curScope.parent

		if err != nil {
			return nil, err
		}

		if bodyBlock != nil && bodyBlock.Term == nil {
			bodyBlock.NewBr(afterBlock)
		}

		checkBlock = noMatchBlock
	}

	// No arm matched this idx (shouldn't happen if patterns are exhaustive per slot,
	// but handle gracefully: re-loop).
	checkBlock.NewBr(loopBlock)

	return afterBlock, nil
}

// awMatchSlot holds per-slot data for genAwaitMatch.
type awMatchSlot struct {
	val        value.Value
	pid        value.Value
	structName string
	retType    irtypes.Type // nil for void/Unit futures
}

// bindAwaitMatchSlot emits panic-check + result unboxing for one await match arm.
// Returns the block to continue emitting into (the "ok" block after panic check).
func (cg *CodeGen) bindAwaitMatchSlot(block *ir.Block, c ast.AwaitMatchCase, sl awMatchSlot) (*ir.Block, error) {
	// Panic check (same pattern as single await).
	pmsg := block.NewCall(cg.fiberGetPanicMsgFn, sl.pid)
	panicked := block.NewICmp(enum.IPredNE, pmsg, constant.NewNull(irtypes.I8Ptr))
	panicBlk := cg.newBlock(fmt.Sprintf("awmatch.panic.s%d", c.SlotIdx))
	okBlk := cg.newBlock(fmt.Sprintf("awmatch.ok.s%d", c.SlotIdx))
	block.NewCondBr(panicked, panicBlk, okBlk)

	panicBlk.NewCall(cg.ensurePanicFn(), pmsg)

	retType := cg.curFn.Sig.RetType
	if irtypes.IsVoid(retType) {
		panicBlk.NewRet(nil)
	} else {
		panicBlk.NewRet(cg.zeroValue(retType))
	}

	// Unbox result and bind to BindName (if not wildcard / void).
	if sl.retType != nil && c.BindName != "" {
		rawPtr := okBlk.NewCall(cg.fiberGetResultFn, sl.pid)
		typedPtr := okBlk.NewBitCast(rawPtr, irtypes.NewPointer(sl.retType))
		result := okBlk.NewLoad(sl.retType, typedPtr)
		okBlk.NewCall(cg.ensureFree(), rawPtr)
		alloca := okBlk.NewAlloca(sl.retType)
		okBlk.NewStore(result, alloca)
		cg.curScope.set(c.BindName, &scopeEntry{val: alloca, isAlloc: true})
	}

	return okBlk, nil
}

// genMatchType handles "match a.(type):" dispatch for tagged unions.
// Each case "case i T:" extracts the payload as variable i of type T.

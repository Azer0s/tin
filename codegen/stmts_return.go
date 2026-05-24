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

func (cg *CodeGen) genReturn(block *ir.Block, s *ast.ReturnStmt) error {
	// `return try expr` yields the success value (V from tryable[V, C]),
	// not the container - so the natural-feeling form is almost always a
	// type error in user code. Surface the gotcha with a hint pointing at
	// the constructor wrap they probably meant. The underlying
	// type-check still fires; this just adds context.
	if _, isTryExpr := s.Value.(*ast.TryExpr); isTryExpr {
		cg.warn(DiagReturnTry, s.Pos(),
			"`return try expr` returns the success value, not the container; did you mean `return Ok(try expr)` or `return Some(try expr)`?")
	}

	// Propagate "owning iface" up the call graph: if we're returning a
	// binding that we know carries an escape-promoted iface data block,
	// flag this function so callers' let-bindings inherit
	// ownsHeapIfaceData (see bindingOwnsHeapIfaceData).
	if cg.curFn != nil && s.Value != nil {
		if id, ok := s.Value.(*ast.Identifier); ok {
			if entry, ok2 := cg.curScope.lookup(id.Name); ok2 && entry.ownsHeapIfaceData {
				cg.fnReturnsOwningIface[cg.curFn.Name()] = true
			}
		}
	}

	// Record heap-promoted struct fields so the caller's receiving
	// binding can cascade-release them.  Without this, `return
	// Box{p: &x}` (where x is heap-promoted by escape analysis) leaves
	// the heap block dangling - x's own scope-exit skips release
	// (isEarlyHeap), the Box's per-struct release helper treats *T
	// fields as borrows, and the caller's binding never sees it.
	if cg.curFn != nil && s.Value != nil && len(cg.curFnEscapingVars) > 0 {
		if sl, ok := s.Value.(*ast.StructLit); ok {
			heapFields := cg.heapPromotedFieldIndices(sl)
			if len(heapFields) > 0 {
				prev := cg.fnReturnsHeapPromotedFields[cg.curFn.Name()]
				cg.fnReturnsHeapPromotedFields[cg.curFn.Name()] = mergeFieldIndices(prev, heapFields)
			}
		}
	}

	// In a coroutine body, return is replaced by _tin_fiber_complete + final suspend.
	if cg.inCoroFn {
		return cg.genCoroReturn(block, s)
	}

	// Self-TCO: intercept `return name(args...)` and rewrite as a loop-back.
	if cg.tcoFuncName != "" && s.Value != nil {
		if ce, ok := s.Value.(*ast.CallExpr); ok {
			if ident, ok2 := ce.Func.(*ast.Identifier); ok2 && ident.Name == cg.tcoFuncName {
				return cg.emitTCOLoopBack(block, ce)
			}
		}
	}

	// Mutual TCO: `return g(args...)` where g is a different Tin function with a
	// compatible non-RC return type. Emit a musttail call so LLVM turns it into
	// a sibling call, preventing stack growth in mutually-recursive cycles.
	if cg.mutualTCOEligible && s.Value != nil &&
		cg.curFnDeferRetAlloca == nil && len(cg.pendingDeferFnI8s) == 0 {
		if ce, ok := s.Value.(*ast.CallExpr); ok {
			if ident, ok2 := ce.Func.(*ast.Identifier); ok2 {
				if callee, eligible := cg.resolveMutualTCOCallee(ident.Name); eligible {
					return cg.emitMutualTCO(block, ce, callee)
				}
			}
		}
	}

	// Inside a defer thunk: 'return val' overrides the outer function's return value
	// by writing to the ret_slot parameter.
	if cg.curDeferRetSlotParam != nil {
		if s.Value != nil {
			retVal, err := cg.genExpr(block, s.Value)
			if err != nil {
				return err
			}
			// Coerce to the lambda's declared return type (e.g. None -> null *i64).
			if cg.curDeferThunkRetType != nil && !irtypes.IsVoid(cg.curDeferThunkRetType) {
				retVal = cg.coerce(block, retVal, cg.curDeferThunkRetType)
			}
			// Two sub-cases:
			// (a) Lambda return type is *T (pointer-to-outer-retType): only override if non-nil.
			//     The ret_slot struct is typed for T, so load through the pointer.
			// (b) Direct value: write it directly to the slot.
			if ptrTy, isPtr := retVal.Type().(*irtypes.PointerType); isPtr {
				// Case (a): `defer (fn() *T = ...)()` - non-nil pointer overrides outer return.
				innerType := ptrTy.ElemType
				slotType := irtypes.NewStruct(irtypes.I8, innerType)
				slotPtr := block.NewBitCast(cg.curDeferRetSlotParam, irtypes.NewPointer(slotType))
				// Check non-nil.
				isNilPtr := block.NewICmp(enum.IPredEQ, retVal, constant.NewNull(ptrTy))
				nilBlock := cg.curFn.NewBlock(fmt.Sprintf("defer.ret.nil.%d", cg.labelCount))
				overrideBlock := cg.curFn.NewBlock(fmt.Sprintf("defer.ret.override.%d", cg.labelCount))
				cg.labelCount++

				block.NewCondBr(isNilPtr, nilBlock, overrideBlock)
				// Override branch: load *retVal and write to slot.
				derefVal := overrideBlock.NewLoad(innerType, retVal)
				validGep := overrideBlock.NewGetElementPtr(slotType, slotPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
				overrideBlock.NewStore(constant.NewInt(irtypes.I8, 1), validGep)
				valGep := overrideBlock.NewGetElementPtr(slotType, slotPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
				overrideBlock.NewStore(derefVal, valGep)
				overrideBlock.NewRet(nil)
				// Nil branch: no override, just return.
				nilBlock.NewRet(nil)
			} else {
				// Case (b): plain `return val` in defer do: - write directly to slot.
				slotType := irtypes.NewStruct(irtypes.I8, retVal.Type())
				slotPtr := block.NewBitCast(cg.curDeferRetSlotParam, irtypes.NewPointer(slotType))
				validGep := block.NewGetElementPtr(slotType, slotPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
				block.NewStore(constant.NewInt(irtypes.I8, 1), validGep)
				valGep := block.NewGetElementPtr(slotType, slotPtr,
					constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
				block.NewStore(retVal, valGep)
				block.NewRet(nil)
			}
		} else {
			block.NewRet(nil)
		}

		return nil
	}

	if s.Value == nil {
		// Bare `return` in a non-void function: emit a Tin diagnostic
		// here instead of letting NewRet(nil) reach LLVM and surface as
		// a clang IR-level "value doesn't match function result type"
		// error from a temp .ll file.
		if cg.curFn != nil && !irtypes.IsVoid(cg.curFn.Sig.RetType) {
			return cg.nodeErr(s, "function returns %s but the return statement has no value",
				cg.fmtArgType(cg.curFn.Sig.RetType))
		}

		if err := cg.emitDefers(block); err != nil {
			return err
		}

		cg.emitAllScopeReleases(block, "")
		block.NewRet(nil)

		return nil
	}

	// Late heap promotion: if this return involves escaping vars, defer evaluation
	// until after defers so the post-defer stack values are used for the RC blocks.
	if len(cg.curFnEscapingVars) > 0 {
		promoted := retainedHeapVars(s.Value, cg.curFnEscapingAliases, cg.curFnEscapingVars)
		if len(promoted) > 0 {
			return cg.genLatePromotedReturn(block, s, promoted)
		}
	}

	cg.curBlock = block // sync before genExpr so we can detect block advances
	// TupleLit: pass the declared return type so fields get the right types.
	var val value.Value

	if tup, ok := s.Value.(*ast.TupleLit); ok && cg.curFn != nil && !irtypes.IsVoid(cg.curFn.Sig.RetType) {
		var err2 error

		val, err2 = cg.genTupleLit(block, tup, cg.curFn.Sig.RetType)
		if err2 != nil {
			return err2
		}
	} else {
		// Set returnTypeHint so ADT bare-constructor calls like `return Ok(x)`
		// can resolve against the declared return type. Restore after.
		prevHint := cg.returnTypeHint

		if cg.curFn != nil && !irtypes.IsVoid(cg.curFn.Sig.RetType) {
			cg.returnTypeHint = cg.curFn.Sig.RetType
		}

		var err2 error

		val, err2 = cg.genExpr(block, s.Value)
		cg.returnTypeHint = prevHint

		if err2 != nil {
			return err2
		}
	}
	// If genExpr advanced the current block (e.g. via coro chain call), use it.
	if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
	}

	if cg.curFn != nil {
		retType := cg.curFn.Sig.RetType
		if irtypes.IsVoid(retType) {
			if val != nil {
				return cg.nodeErr(s, "void function cannot return a value")
			}
		} else {
			// Bare `return` in a non-void function: catch here with a
			// targeted Tin diagnostic instead of letting LLVM emit
			// `ret void` and surface a clang IR-level error.
			if val == nil {
				return cg.nodeErr(s, "function returns %s but the return statement has no value", cg.fmtArgType(retType))
			}

			// `return ident` skips the source's scope-exit release via
			// retSkipName below; in that mode buildPtrToTraitBorrow
			// must NOT retain (would leak +1 since the source never
			// decrements).  Coerce while the flag is on, then clear.
			cg.coerceLastErr = nil
			if _, isIdent := s.Value.(*ast.Identifier); isIdent {
				prevTransfer := cg.coerceTransfersSource
				cg.coerceTransfersSource = true
				val = cg.coerce(block, val, retType)
				cg.coerceTransfersSource = prevTransfer
			} else {
				val = cg.coerce(block, val, retType)
			}
			// Surface a richer diagnostic stashed by coerce (e.g.
			// value-form coerce of an impl whose receiver is *Self) in
			// place of the generic type-mismatch fall-through below.
			if cg.coerceLastErr != nil {
				return cg.nodeErr(s, "%v", cg.coerceLastErr)
			}

			if !val.Type().Equal(retType) {
				// Render in user-facing source syntax (Foo[i64], not
				// the LLVM-mangled %Foo__i64). fmtArgType handles every
				// common shape; fall back to prettyStructName via the
				// raw struct name only when fmtArgType yields nothing.
				gotName := cg.fmtArgType(val.Type())
				if gotName == "" || gotName == "<nil>" {
					gotName = cg.diagStructName(cg.typeNameOf(val.Type()))
				}

				wantName := cg.fmtArgType(retType)
				if wantName == "" || wantName == "<nil>" {
					wantName = cg.diagStructName(cg.typeNameOf(retType))
				}

				if astDecl, ok := cg.funcDecls[cg.curFn.Name()]; ok && astDecl.RetType != nil {
					wantName = astDecl.RetType.String()
				}

				return cg.nodeErr(s, "cannot return value of type %s as %s", gotName, wantName)
			}
		}
	}

	if err := cg.emitDefers(block); err != nil {
		return err
	}
	// After running defers, check if any deferred function wrote an override return value.
	if cg.curFnDeferRetAlloca != nil && cg.curFn != nil && !irtypes.IsVoid(cg.curFn.Sig.RetType) {
		retType := cg.curFn.Sig.RetType
		slotType := irtypes.NewStruct(irtypes.I8, retType)
		slotPtr := block.NewBitCast(cg.curFnDeferRetAlloca, irtypes.NewPointer(slotType))
		validGep := block.NewGetElementPtr(slotType, slotPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		valid := block.NewLoad(irtypes.I8, validGep)
		isValid := block.NewICmp(enum.IPredNE, valid, constant.NewInt(irtypes.I8, 0))
		valGep := block.NewGetElementPtr(slotType, slotPtr,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		overrideVal := block.NewLoad(retType, valGep)
		val = block.NewSelect(isValid, overrideVal, val)
	}
	// ARC: release all RC locals except the one being returned
	// (to transfer its rc=1 ownership to the caller).
	retSkipName := ""
	if ident, ok := s.Value.(*ast.Identifier); ok {
		retSkipName = ident.Name
		// Returning a borrowed Identifier (typically a non-reassigning
		// RC param under the new model -- the body never retained, the
		// caller's binding holds the rc share).  Without a retain here
		// the caller's receiving binding and the caller's source
		// binding would share one rc share, double-releasing on the
		// two scope exits.  Owned identifiers carry their own +1 and
		// the skipName above transfers it cleanly; only Borrowed ones
		// need the extra retain.
		if e, has := cg.curScope.lookup(ident.Name); has && e.isAlloc && e.isRC &&
			e.ownership == ownershipBorrowed {
			if !cg.emitOwningPtrRetainIfApplicable(block, val) {
				cg.emitRetain(block, val)
			}
		}
	} else if isCopyExpr(s.Value) && !cg.isFreshBytesAlloc(val) && !cg.isFreshCallResult(val) &&
		!cg.isDerefOfRawVoidPtrCast(s.Value) && !isFreshSliceExpr(s.Value) {
		// Returning a borrowed value (field access, index) whose RC lifetime is
		// tied to a local/parameter that will be released by emitAllScopeReleases.
		// Retain first so the caller gets one owned reference, then scope cleanup
		// decrements the RC back to a net-neutral result.
		// Exceptions:
		//   - [T;N] as string calls _tin_bytes_from_buf (rc=1 already)
		//   - `n as Trait` lowers to a coerce[T] call returning rc=1; the
		//     call result is already owned, retaining would over-count.
		//   - `*(rawvoid as *T)` is a move out of foreign memory (e.g. a
		//     channel's per-thread recv scratch buffer that already
		//     transferred RC into the slot); retaining would leave a +1
		//     no scope cleanup decrements, leaking every received value.
		//
		// Bare *<named struct> / *<iface> values are NOT covered by
		// emitRetain (Tin's calling convention treats them as borrows
		// for parameters, so emitRetain skips them to keep entry/exit
		// balanced).  When the return path needs ownership transfer,
		// retain the heap block explicitly via emitOwningPtrRetainIfApplicable
		// before falling through to emitRetain for the rest.  Pre-fix,
		// `return h.iface_field` left the iface RC unchanged while the
		// caller's scope-exit release decremented it -- use-after-free
		// after a few iterations of any pattern that returns a *Trait
		// field from an aggregate.
		if !cg.emitOwningPtrRetainIfApplicable(block, val) {
			cg.emitRetain(block, val)
		}
	}

	// Returning a trait fat-pointer by value: the scope's synthetic
	// `.iface_data_*` release entry would otherwise free the heap
	// block backing data_ptr before the caller ever sees it. Retain
	// data_ptr here so the upcoming scope release decrements rc to 1
	// instead of 0, leaving the caller with an owned iface they will
	// release on drop. Mirrors how `retSkipName` keeps a returning
	// named binding alive past scope cleanup.
	//
	// Gated on the function actually having registered a live
	// `.iface_data_*` entry via coerceToTrait.  Without the gate,
	// pass-through wrappers like `fn make() Err = return errors::new("x")`
	// over-retain (the inner errors::new already returned rc=1 and there's
	// no matching scope release in the wrapper to compensate), and
	// channel.recv-style functions that just deref a borrowed iface
	// pointer leak every received value.
	if val != nil {
		if _, ok := cg.isTraitFatPtr(val.Type()); ok && cg.hasLiveIfaceDataScopeEntry() {
			dataPtr := block.NewExtractValue(val, 0)
			block.NewCall(cg.ensureRetain(), dataPtr)
		}
	}
	// Returning an ADT by value whose active variant payload holds
	// a freshly-coerced trait fat-ptr: the iface heap block was
	// alloc'd rc=1 by coerceToTrait and a synthetic `.iface_data_*`
	// scope-release entry registered. Without compensation that
	// release fires inside this function and frees the iface - caller
	// gets a dangling pointer in the returned Result, which crashes
	// the moment the caller's data_release_val tries to dispatch
	// through the iface's vtable thunk.
	//
	// Suppress the synthetic iface-data scope releases for this
	// function exit. The iface is transferred to the caller via the
	// ADT, so the caller's data_release_val (fired by
	// emitCallArgRelease's ADT path) becomes the sole owner that
	// drops rc to 0. Limited to ADT returns where some variant has a
	// fat-ptr field, so functions that don't transfer ifaces aren't
	// affected.
	if val != nil && cg.isDataType(val.Type()) && cg.adtHasFatPtrField(val.Type()) {
		cg.suppressIfaceDataScopeReleases()
	}

	cg.emitAllScopeReleases(block, retSkipName)

	// Tin-level type-mismatch check on the return value: surface the
	// error here so users see a Tin source location and message instead
	// of an LLVM IR-level type-mismatch from clang on the temp .ll.
	if cg.curFn != nil && val != nil && !irtypes.IsVoid(cg.curFn.Sig.RetType) {
		if !val.Type().Equal(cg.curFn.Sig.RetType) {
			return cg.nodeErr(s,
				"return type mismatch: expected %s, got %s; if the called method has a wildcard return type, ensure its trait bound declares the wildcard slot (e.g. `tryable[T, Result[_, E]]`) so the value can be reconstructed in the enclosing function's type",
				cg.fmtArgType(cg.curFn.Sig.RetType), cg.fmtArgType(val.Type()))
		}
	}

	block.NewRet(val)

	return nil
}

// genCoroReturn generates the coroutine-specific terminator for a return statement.
// Instead of ret, it boxes the return value, calls _tin_fiber_complete, and
// emits the final coro.suspend which leads to cleanup.
func (cg *CodeGen) genCoroReturn(block *ir.Block, s *ast.ReturnStmt) error {
	var retVal value.Value

	if s.Value != nil {
		cg.curBlock = block // sync before genExpr so we can detect block advances

		var err error
		// TupleLit: hand the coroutine's *original* return type to
		// the tuple generator so trait-pointer fields keep their
		// real LLVM type instead of being silently widened to i64
		// during inference (the LLVM coro signature is i8*, but the
		// stored payload uses the user-declared shape).
		if tup, isTup := s.Value.(*ast.TupleLit); isTup && cg.curCoroRetType != nil {
			retVal, err = cg.genTupleLit(block, tup, cg.curCoroRetType)
		} else {
			// Set returnTypeHint so ADT bare-constructor calls like
			// `return Err(e)` disambiguate against the declared
			// return type when multiple Result instantiations are in
			// scope. Mirrors the sync-return path in genReturn - the
			// LLVM coro signature is i8*, so we have to use the saved
			// curCoroRetType instead of cg.curFn.Sig.RetType here.
			prevHint := cg.returnTypeHint

			if cg.curCoroRetType != nil && !irtypes.IsVoid(cg.curCoroRetType) {
				cg.returnTypeHint = cg.curCoroRetType
			}

			retVal, err = cg.genExpr(block, s.Value)
			cg.returnTypeHint = prevHint
		}

		if err != nil {
			return err
		}
		// If genExpr advanced the current block (e.g. via coro chain call), use it.
		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}
		// Coerce to the original return type (not i8*).
		if cg.curCoroRetType != nil && !irtypes.IsVoid(cg.curCoroRetType) && retVal != nil {
			retVal = cg.coerce(block, retVal, cg.curCoroRetType)
		}
	}

	if err := cg.emitDefers(block); err != nil {
		return err
	}

	retSkipName := ""

	if s.Value != nil {
		if ident, ok := s.Value.(*ast.Identifier); ok {
			retSkipName = ident.Name
			// Mirror genReturn's borrowed-Identifier retain so a
			// coroutine that yields a non-reassigning RC param to its
			// awaiter transfers its own rc share.  Without this, the
			// awaiter's receiving binding and the caller's source
			// binding share one rc share and double-release on their
			// two scope exits.
			if e, has := cg.curScope.lookup(ident.Name); has && e.isAlloc && e.isRC &&
				e.ownership == ownershipBorrowed && retVal != nil {
				if !cg.emitOwningPtrRetainIfApplicable(block, retVal) {
					cg.emitRetain(block, retVal)
				}
			}
		}
	}

	cg.emitAllScopeReleases(block, retSkipName)
	cg.emitCoroComplete(block, retVal)
	cg.emitFinalSuspend(block, cg.curCoroFrame)

	return nil
}

// genYieldStmt emits a yield point inside an {#async} coroutine body.
// In the normal (non-coro) variant of the same function, yield is a no-op.
// Returns the continuation block where execution resumes after the panic check.
func (cg *CodeGen) genYieldStmt(block *ir.Block) (*ir.Block, error) {
	if !cg.inCoroFn {
		// In the sync version of an {#async} function, yield is a no-op.
		return block, nil
	}

	cg.ensureFiberRuntime()
	// Notify the scheduler that we want to be re-enqueued.
	block.NewCall(cg.fiberYieldCoroFn, cg.curCoroHdl)
	// Suspend the coroutine; returns the resume block.
	resumeBlk := cg.emitSuspendPoint(block, cg.curCoroFrame)

	doneBlk := cg.newBlock("yield.done")
	cg.emitPanicCheck(resumeBlk, doneBlk, "yield")

	// Track doneBlk so genYieldAutoAt suppresses the redundant auto-yield when
	// the loop backedge lands on this continuation block.
	if cg.yieldResumeBlocks != nil {
		cg.yieldResumeBlocks[doneBlk] = true
	}

	return doneBlk, nil
}

// genAwaitStmt emits an await point inside an {#async} coroutine body.
// In the normal (non-coro) variant, await is a no-op.
func (cg *CodeGen) genAwaitStmt(block *ir.Block, pidVal value.Value) (*ir.Block, error) {
	if !cg.inCoroFn {
		// Non-coroutine context (e.g., main): run scheduler until the fiber completes.
		cg.ensureFiberRuntime()

		if pidVal == nil {
			return block, nil
		}

		pid64 := cg.coerce(block, pidVal, irtypes.I64)
		syncAwaitFn := cg.ensureExternDecl("_tin_fiber_sync_await", irtypes.Void,
			[]*ir.Param{ir.NewParam("pid", irtypes.I64)}, false)
		block.NewCall(syncAwaitFn, pid64)

		return block, nil
	}

	cg.ensureFiberRuntime()

	if pidVal != nil && !pidVal.Type().Equal(irtypes.I64) {
		pidVal = cg.coerce(block, pidVal, irtypes.I64)
	}

	if pidVal == nil {
		return block, nil
	}
	// Register this fiber as a waiter for pid.
	block.NewCall(cg.fiberJoinFn, pidVal, cg.curCoroHdl)
	// Suspend; the scheduler will re-enqueue us when pid completes.
	resumeBlk := cg.emitSuspendPoint(block, cg.curCoroFrame)

	return resumeBlk, nil
}

// genBuiltinLen implements the len(expr) built-in: returns the i64 length of
// strings, dynamic arrays, or the constant size of static arrays.
func (cg *CodeGen) genBuiltinLen(block *ir.Block, arg ast.Node) (value.Value, error) {
	val, err := cg.genExpr(block, arg)
	if err != nil {
		return nil, err
	}

	t := val.Type()

	var length value.Value

	// String fat-ptr {i8*, i64}: extract field 1.
	if isStringType(t) {
		length = cg.extractStringLen(block, val)
	} else if isFatArrayPtr(t) {
		// Dynamic array fat-ptr {T*, i64}: extract field 1.
		st := t.(*irtypes.StructType)
		alloca := block.NewAlloca(st)
		block.NewStore(val, alloca)
		gep := block.NewGetElementPtr(st, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
		length = block.NewLoad(irtypes.I64, gep)
	} else if at, ok := t.(*irtypes.ArrayType); ok {
		// Static array [N x T]: constant length (no RC).
		return constant.NewInt(irtypes.I64, int64(at.Len)), nil
	} else {
		return nil, fmt.Errorf("len() not supported for type %s", t)
	}

	// ARC: release the argument if it is a temporary RC allocation
	// (e.g. len(a |> filter(f)) where the filtered array is a fresh allocation).
	if isRCTrackedType(t) && !isCopyExpr(arg) {
		cg.emitRelease(block, val)
	}

	return length, nil
}

// genBuiltinCopy implements the copy(expr) built-in: returns a fresh,
// independently-owned duplicate of a string or dynamic array.  Modifying
// the returned value does not affect the source.  For RC-tracked element
// types, each element gets a retain so the new buffer's release does not
// take down elements still owned by the source.
func (cg *CodeGen) genBuiltinCopy(block *ir.Block, arg ast.Node) (value.Value, error) {
	val, err := cg.genExpr(block, arg)
	if err != nil {
		return nil, err
	}

	// Pick up any continuation block that `arg` evaluation parked us
	// in (an `await` inside `copy(...)` advances cg.curBlock); without
	// this all the subsequent IR lands in the original block and the
	// emission verifier flags the dead use of the await's result.
	if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
	}

	t := val.Type()

	// Named struct: route through the per-struct deep-copy intrinsic
	// (same machinery the auto-copy dispatch uses).  Skips ADT and
	// cLayoutStruct since their layouts aren't navigable by the
	// generic field walker; users wanting `copy()` on those today get
	// a clear error pointing at the open follow-ups.
	if st, ok := t.(*irtypes.StructType); ok && st.Name() != "" &&
		!isStringType(st) && !isFatArrayPtr(st) && !isAnyType(st) &&
		!isTraitFatPtrShape(st) && !isFatFnPtr(st) && !isAtomType(st) {
		if cg.isDataType(st) {
			return nil, fmt.Errorf("copy() not yet supported for ADT (data) types - tracked as a follow-up")
		}

		if cg.cLayoutStructs[st.Name()] {
			return nil, fmt.Errorf("copy() not yet supported for cLayoutStruct types")
		}

		if cg.structTypeFor(CanonKey(st.Name())) == nil {
			return nil, fmt.Errorf("copy() cannot resolve struct type %s", st.Name())
		}

		fn := cg.ensureStructDeepCopyFn(st.Name(), st)

		return block.NewCall(fn, val), nil
	}

	if !isStringType(t) && !isFatArrayPtr(t) {
		return nil, fmt.Errorf("copy() not supported for type %s", t)
	}

	st := t.(*irtypes.StructType)
	elemPtrType := st.Fields[0].(*irtypes.PointerType)
	elemT := elemPtrType.ElemType

	srcPtr := block.NewExtractValue(val, 0)
	srcLen := block.NewExtractValue(val, 1)

	// sizeof(elemT) via GEP trick.
	nullElemPtr := constant.NewNull(irtypes.NewPointer(elemT))
	sizeGep := block.NewGetElementPtr(elemT, nullElemPtr, constant.NewInt(irtypes.I64, 1))
	elemSize := block.NewPtrToInt(sizeGep, irtypes.I64)
	totalBytes := block.NewMul(srcLen, elemSize)

	// For strings, allocate len+1 so the result is NUL-terminated and stays
	// drop-in compatible with C-extern boundaries that read past .len.
	var allocBytes value.Value = totalBytes
	if isStringType(t) {
		allocBytes = block.NewAdd(totalBytes, constant.NewInt(irtypes.I64, 1))
	}

	newI8Ptr := block.NewCall(cg.ensureRCAlloc(), allocBytes)
	newPtr := block.NewBitCast(newI8Ptr, irtypes.NewPointer(elemT))

	srcI8Ptr := block.NewBitCast(srcPtr, irtypes.I8Ptr)
	block.NewCall(cg.ensureMemcpy(), newI8Ptr, srcI8Ptr, totalBytes, constant.NewInt(irtypes.I1, 0))

	if isStringType(t) {
		nullByte := block.NewGetElementPtr(irtypes.I8, newI8Ptr, totalBytes)
		block.NewStore(constant.NewInt(irtypes.I8, 0), nullByte)
	}

	// Retain every RC-tracked element so the copy independently owns its
	// references; without this the copy's release would tear down content
	// still held by the source.
	_, elemIsPtr := elemT.(*irtypes.PointerType)
	if cg.elemNeedsRelease(elemT) || isRCTrackedType(elemT) || elemIsPtr {
		cg.emitRetainElemSlice(block, newI8Ptr, srcLen, elemT)
	}

	if isRCTrackedType(t) && !isCopyExpr(arg) {
		cg.emitRelease(block, val)
	}

	return cg.buildFatArrayValue(block, elemT, newPtr, srcLen, srcLen), nil
}

// genBuiltinCap implements the cap(expr) built-in: returns the i64
// capacity (allocated headroom) of a string or dynamic array.  For
// an owned/growable slice cap >= len; immortal / borrowed views
// (string literals, fieldnames(), atom-array globals) encode
// cap == -1 so the runtime cap-check on `++=` can reject them
// before allocating.  Static arrays have cap == len.
func (cg *CodeGen) genBuiltinCap(block *ir.Block, arg ast.Node) (value.Value, error) {
	val, err := cg.genExpr(block, arg)
	if err != nil {
		return nil, err
	}

	if cg.curBlock != nil && cg.curBlock != block {
		block = cg.curBlock
	}

	t := val.Type()

	var capacity value.Value

	if isStringType(t) || isFatArrayPtr(t) {
		st := t.(*irtypes.StructType)
		alloca := block.NewAlloca(st)
		block.NewStore(val, alloca)
		gep := block.NewGetElementPtr(st, alloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))
		capacity = block.NewLoad(irtypes.I64, gep)
	} else if at, ok := t.(*irtypes.ArrayType); ok {
		return constant.NewInt(irtypes.I64, int64(at.Len)), nil
	} else {
		return nil, fmt.Errorf("cap() not supported for type %s", t)
	}

	if isRCTrackedType(t) && !isCopyExpr(arg) {
		cg.emitRelease(block, val)
	}

	return capacity, nil
}

// Defer chain helpers

// ensureDeferChain lazily declares the runtime defer-chain functions and
// initializes the TinDeferEntry LLVM struct type.
func (cg *CodeGen) genBuiltinDefault(block *ir.Block, arg ast.Node) (value.Value, error) {
	// Handle default(typeof(expr)): generate expr to discover its LLVM type,
	// then return the zero value for that type. The generated code for the
	// inner expression is dead but LLVM will optimize it away.
	if call, ok := arg.(*ast.CallExpr); ok {
		if fnID, ok2 := call.Func.(*ast.Identifier); ok2 && fnID.Name == "typeof" && len(call.Args) == 1 {
			val, err := cg.genExpr(block, call.Args[0])
			if err != nil {
				return nil, err
			}

			return cg.zeroValue(val.Type()), nil
		}
	}

	// The argument is a type used as an expression - typically an Identifier.
	var typeExpr ast.TypeExpr

	switch a := arg.(type) {
	case *ast.Identifier:
		typeExpr = &ast.SimpleType{Name: a.Name}
	default:
		return constant.NewInt(irtypes.I64, 0), nil
	}

	llvmType, err := cg.tinTypeToLLVM(typeExpr)
	if err != nil {
		return constant.NewInt(irtypes.I64, 0), nil
	}

	switch t := llvmType.(type) {
	case *irtypes.IntType:
		return constant.NewInt(t, 0), nil
	case *irtypes.FloatType:
		return constant.NewFloat(t, 0), nil
	case *irtypes.PointerType:
		return constant.NewNull(t), nil
	case *irtypes.StructType:
		// Zero-initialize each field.
		fields := make([]constant.Constant, len(t.Fields))
		for i, f := range t.Fields {
			switch ft := f.(type) {
			case *irtypes.IntType:
				fields[i] = constant.NewInt(ft, 0)
			case *irtypes.FloatType:
				fields[i] = constant.NewFloat(ft, 0)
			case *irtypes.PointerType:
				fields[i] = constant.NewNull(ft)
			default:
				fields[i] = constant.NewUndef(ft)
			}
		}

		return constant.NewStruct(t, fields...), nil
	default:
		return constant.NewUndef(llvmType), nil
	}
}

// panic builtin

// genBuiltinPanic implements panic(msg): runs the runtime defer chain and
// terminates the program.  The call does not return; a NewUnreachable

// expandMacro evaluates a macro call, choosing the appropriate strategy:
//   - Complex macros (block body): CTFE - compile to a temp binary, run with timeout,
//     parse stdout as the expansion result.
//   - Simple macros (expression body): AST substitution - fast, no subprocess.
//
// callPos is the source position of the macro CALL site - used to retag
// macro-body nodes so codegen-time pos lookups (sourcepos in particular)
// report the caller's location, not the macro definition line.

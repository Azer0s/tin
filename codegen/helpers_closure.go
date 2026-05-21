package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

func (cg *CodeGen) pushClosureCtx(f *ir.Func) closureCtx {
	prev := closureCtx{
		fn:                cg.curFn,
		scope:             cg.curScope,
		curBlock:          cg.curBlock,
		deferFnI8s:        cg.pendingDeferFnI8s,
		deferFrames:       cg.pendingDeferFrames,
		deferEnvs:         cg.pendingDeferEnvs,
		deferRetSlotParam: cg.curDeferRetSlotParam,
		fnDeferRetAlloca:  cg.curFnDeferRetAlloca,
		deferThunkRetType: cg.curDeferThunkRetType,
		inCoroFn:          cg.inCoroFn,
		autoYield:         cg.curFnAutoYield,
		coloredSync:       cg.curFnColoredSync,
		coroHdl:           cg.curCoroHdl,
		coroID:            cg.curCoroID,
		coroCleanup:       cg.curCoroCleanup,
		coroFrame:         cg.curCoroFrame,
		coroRetType:       cg.curCoroRetType,
		fnAstBody:         cg.curFnAstBody,
		movedBindings:     cg.movedBindings,
	}

	cg.curFn = f
	cg.curBlock = nil
	cg.pendingDeferFnI8s = nil
	cg.pendingDeferFrames = nil
	cg.pendingDeferEnvs = nil
	cg.curDeferRetSlotParam = nil
	cg.curFnDeferRetAlloca = nil
	cg.curDeferThunkRetType = nil
	// Reset to nil; the closure/thunk caller is responsible for setting
	// curFnAstBody to the body that will actually be emitted so the
	// cLayout escape walker scopes its name lookups to that body.
	cg.curFnAstBody = nil
	// Each closure / thunk emission starts with its own moved-bindings
	// set so the outer fn's moves do not poison this body's reads.
	cg.movedBindings = nil
	// Thunks and closures are plain functions, not coroutines.  Reset
	// every "we're inside a fiber/coroutine body" flag so the closure's
	// own body emits as a plain sync fn -- otherwise yield-insertion
	// machinery would emit llvm.coro.suspend or _tin_fiber_yield_coro
	// referencing the OUTER fn's coro frame, producing cross-function
	// SSA references that crash coro-split / verify-ir.
	cg.inCoroFn = false
	cg.curFnAutoYield = false
	cg.curFnColoredSync = false
	cg.curCoroHdl = nil
	cg.curCoroID = nil
	cg.curCoroCleanup = nil
	cg.curCoroFrame = nil
	cg.curCoroRetType = nil

	global := prev.scope
	for global.parent != nil {
		global = global.parent
	}

	cg.curScope = newScope(global)

	return prev
}

// popClosureCtx restores the function context saved by pushClosureCtx.
func (cg *CodeGen) popClosureCtx(prev closureCtx) {
	cg.curFn = prev.fn
	cg.curScope = prev.scope
	cg.curBlock = prev.curBlock
	cg.pendingDeferFnI8s = prev.deferFnI8s
	cg.pendingDeferFrames = prev.deferFrames
	cg.pendingDeferEnvs = prev.deferEnvs
	cg.curDeferRetSlotParam = prev.deferRetSlotParam
	cg.curFnDeferRetAlloca = prev.fnDeferRetAlloca
	cg.curDeferThunkRetType = prev.deferThunkRetType
	cg.inCoroFn = prev.inCoroFn
	cg.curFnAutoYield = prev.autoYield
	cg.curFnColoredSync = prev.coloredSync
	cg.curCoroHdl = prev.coroHdl
	cg.curCoroID = prev.coroID
	cg.curCoroCleanup = prev.coroCleanup
	cg.curCoroFrame = prev.coroFrame
	cg.curCoroRetType = prev.coroRetType
	cg.curFnAstBody = prev.fnAstBody
	cg.movedBindings = prev.movedBindings
}

// buildEnv heap-allocates an env struct for the given captures and stores each
// captured value into it. Returns the i8* pointer to the env and the struct
// type (nil struct type and null pointer when there are no captures).
func (cg *CodeGen) buildEnv(block *ir.Block, captures []closureCapture) (value.Value, *irtypes.StructType) {
	if len(captures) == 0 {
		return constant.NewNull(irtypes.I8Ptr), nil
	}

	fields := make([]irtypes.Type, len(captures))
	for i, c := range captures {
		fields[i] = c.llvmTy
	}

	envStructType := irtypes.NewStruct(fields...)
	// sizeof(*envStructType) via GEP trick: null + 1 element then ptrtoint.
	nullPtr := constant.NewNull(irtypes.NewPointer(envStructType))
	oneGEP := block.NewGetElementPtr(envStructType, nullPtr, constant.NewInt(irtypes.I32, 1))
	envSize := block.NewPtrToInt(oneGEP, irtypes.I64)
	envI8 := block.NewCall(cg.ensureMalloc(), envSize)

	envTypedPtr := block.NewBitCast(envI8, irtypes.NewPointer(envStructType))
	for i, c := range captures {
		gep := block.NewGetElementPtr(envStructType, envTypedPtr,
			constant.NewInt(irtypes.I32, 0),
			constant.NewInt(irtypes.I32, int64(i)))
		block.NewStore(c.val, gep)
	}

	return envI8, envStructType
}

// posStr formats a source position as "filename:line:col".
// If node is non-nil its position is used; otherwise cg.currentPos is used.
// Returns just the filename when no line info is available.

func (cg *CodeGen) buildClosureEnv(block *ir.Block, captures []closureCapture, dtorFn *ir.Func) (value.Value, *irtypes.StructType) {
	if len(captures) == 0 {
		return constant.NewNull(irtypes.I8Ptr), nil
	}
	// Field 0: i8* dtor; fields 1..N: captures.
	fields := make([]irtypes.Type, len(captures)+1)

	fields[0] = irtypes.I8Ptr
	for i, c := range captures {
		fields[i+1] = c.llvmTy
	}

	envStructType := irtypes.NewStruct(fields...)

	// Compute size via GEP trick.
	nullPtr := constant.NewNull(irtypes.NewPointer(envStructType))
	oneGEP := block.NewGetElementPtr(envStructType, nullPtr, constant.NewInt(irtypes.I32, 1))
	envSize := block.NewPtrToInt(oneGEP, irtypes.I64)

	// Use _tin_rc_alloc so the env lifetime is reference-counted.
	envI8 := block.NewCall(cg.ensureRCAlloc(), envSize)
	envTypedPtr := block.NewBitCast(envI8, irtypes.NewPointer(envStructType))

	// Store dtor pointer as field 0.
	dtorGep := block.NewGetElementPtr(envStructType, envTypedPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	if dtorFn != nil {
		dtorI8 := block.NewBitCast(dtorFn, irtypes.I8Ptr)
		block.NewStore(dtorI8, dtorGep)
	} else {
		block.NewStore(constant.NewNull(irtypes.I8Ptr), dtorGep)
	}

	// Store captures at fields 1..N and retain RC-tracked ones.
	// emitStructFieldRetain handles the `*Trait_iface` and `*TinStruct`
	// cases that plain emitRetain skips (param-borrow convention) -- a
	// closure that outlives its outer scope must own its captured
	// pointer outright or the env would dangle the moment the caller
	// releases its own borrow.
	for i, c := range captures {
		gep := block.NewGetElementPtr(envStructType, envTypedPtr,
			constant.NewInt(irtypes.I32, 0),
			constant.NewInt(irtypes.I32, int64(i+1)))
		block.NewStore(c.val, gep)

		needRetain := isRCTrackedType(c.llvmTy)
		// Also retain pointer-to-trait-iface / pointer-to-Tin-struct
		// captures: the param-borrow convention skips them in
		// isRCTrackedType, but a closure that outlives the caller's
		// scope MUST hold its own +1 reference, otherwise the caller's
		// scope-exit release frees the heap block before the closure
		// fires.
		if !needRetain {
			if pt, ok := c.llvmTy.(*irtypes.PointerType); ok {
				if innerSt, ok2 := pt.ElemType.(*irtypes.StructType); ok2 && innerSt.Name() != "" {
					if cg.structTypeFor(CanonKey(innerSt.Name())) != nil || isTraitFatPtrShape(innerSt) {
						needRetain = true
					}
				}
			}
		}

		if needRetain && !c.skipRetain {
			cg.emitStructFieldRetain(block, c.val)
		}
	}

	return envI8, envStructType
}

// unpackClosureEnv unpacks captures from a closure env (built by buildClosureEnv).
// GEP indices are offset by 1 to skip the dtor pointer at field 0.
// Uses env field GEPs directly (useEnvDirect=true semantics): mutations to
// captured variables persist across closure calls.
func (cg *CodeGen) unpackClosureEnv(entry *ir.Block, f *ir.Func, envStructType *irtypes.StructType, captures []closureCapture) {
	if len(captures) == 0 || envStructType == nil {
		return
	}

	envRaw := f.Params[0]

	envTypedPtr := entry.NewBitCast(envRaw, irtypes.NewPointer(envStructType))
	for i, c := range captures {
		// Field index = i+1 (offset by 1 to skip dtor slot at field 0).
		gep := entry.NewGetElementPtr(envStructType, envTypedPtr,
			constant.NewInt(irtypes.I32, 0),
			constant.NewInt(irtypes.I32, int64(i+1)))
		// Retain RC-tracked captures so each closure call holds its own reference.
		if isRCTrackedType(c.llvmTy) {
			loaded := entry.NewLoad(c.llvmTy, gep)
			cg.emitRetain(entry, loaded)
		}
		// Use env GEP directly so mutations persist across calls (counter closures, etc.)
		cg.curScope.set(c.name, &scopeEntry{val: gep, isAlloc: true, noDeinit: true})
	}
}

// unpackEnv unpacks captured values from the env struct into the current scope.
// byRef captures load the stored alloca pointer; non-byRef captures use the env
// field GEP directly (useEnvDirect=true, for lambdas whose env persists across
// calls) or copy the value to a local alloca (useEnvDirect=false, for coro/defer
// thunks that free the env after unpacking).
func (cg *CodeGen) unpackEnv(entry *ir.Block, f *ir.Func, envStructType *irtypes.StructType, captures []closureCapture, useEnvDirect bool) {
	if len(captures) == 0 || envStructType == nil {
		return
	}

	envRaw := f.Params[0]

	envTypedPtr := entry.NewBitCast(envRaw, irtypes.NewPointer(envStructType))
	// Detect closure-env layout: when field 0 is i8* (the dtor slot
	// prepended by buildClosureEnv), captures start at field 1.
	// spawn-do envs (built by buildEnv) have no dtor slot, so the
	// offset stays at 0.
	fieldOffset := 0
	if len(envStructType.Fields) == len(captures)+1 && envStructType.Fields[0] == irtypes.I8Ptr {
		fieldOffset = 1
	}

	for i, c := range captures {
		gep := entry.NewGetElementPtr(envStructType, envTypedPtr,
			constant.NewInt(irtypes.I32, 0),
			constant.NewInt(irtypes.I32, int64(i+fieldOffset)))
		if c.byRef {
			allocaPtr := entry.NewLoad(c.llvmTy, gep)
			// noRelease: the captured variable is owned by the outer scope; the
			// defer thunk must not release it at scope exit.  Only the outer scope
			// (which allocated the variable) is responsible for releasing it.
			cg.curScope.set(c.name, &scopeEntry{val: allocaPtr, isAlloc: true, noDeinit: true, noRelease: true})
		} else if useEnvDirect {
			// Use the env field GEP directly so mutations persist across calls
			// (e.g. counter closures).  The env is heap-allocated and outlives
			// individual invocations.
			// ARC: retain RC-tracked captures so each call holds its own reference.
			if isRCTrackedType(c.llvmTy) {
				loaded := entry.NewLoad(c.llvmTy, gep)
				cg.emitRetain(entry, loaded)
			}

			cg.curScope.set(c.name, &scopeEntry{val: gep, isAlloc: true, noDeinit: true})
		} else {
			alloca := entry.NewAlloca(c.llvmTy)
			loaded := entry.NewLoad(c.llvmTy, gep)
			entry.NewStore(loaded, alloca)
			// ARC: retain RC-tracked captures so that each invocation holds its own ref.
			if isRCTrackedType(c.llvmTy) {
				cg.emitRetain(entry, loaded)
			}
			// noDeinit: the captured variable is a borrow from the enclosing scope.
			cg.curScope.set(c.name, &scopeEntry{val: alloca, isAlloc: true, noDeinit: true})
		}
	}
}

// callPrintTrait tries to call the print trait method on val, returning the
// resulting string value and true, or (nil, false) if not applicable.
// Handles both concrete-struct dispatch and print fat-pointer dispatch.

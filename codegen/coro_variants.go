package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"

	"github.com/Azer0s/tin/ast"
)

// that call sites inside other coro functions can forward-reference it.
// If hasEnv is true, an i8* env pointer is prepended as the first parameter.
func (cg *CodeGen) predeclareCoroVariant(n *ast.FuncDecl, tinName string, hasEnv bool) error {
	coroName := coroVersionName(tinName)
	// Check if already declared.
	if _, already := cg.curScope.vars[coroName]; already {
		return nil
	}
	// Build param list.
	var params []*ir.Param
	if hasEnv {
		params = append(params, ir.NewParam("env", irtypes.I8Ptr))
	}

	for _, p := range n.Params {
		if p.IsVarArgs {
			continue
		}

		pt, err := cg.tinTypeToLLVM(p.Type)
		if err != nil {
			return fmt.Errorf("predeclareCoroVariant %s param %s: %w", tinName, p.Name, err)
		}

		params = append(params, ir.NewParam(p.Name, pt))
	}
	// Ramp function always returns i8* (the coroutine handle).
	f := cg.mod.NewFunc(coroName, irtypes.I8Ptr, params...)
	f.Blocks = nil
	cg.curScope.set(coroName, &scopeEntry{val: f, isAlloc: false})

	return nil
}

// $colored variant predeclaration

// coloredVersionName returns name + "$colored".
func coloredVersionName(name string) string { return name + "$colored" }

// predeclareColoredVariant pre-declares "name$colored(...) T" on the
// module so that call sites inside other $colored or $coro bodies
// can forward-reference it.  Same signature as the plain sync entry
// (returns T, takes the same params).  Idempotent: a second call for
// the same name is a no-op.
func (cg *CodeGen) predeclareColoredVariant(n *ast.FuncDecl, tinName string, hasEnv bool) error {
	coloredName := coloredVersionName(tinName)
	if _, already := cg.curScope.vars[coloredName]; already {
		return nil
	}

	var params []*ir.Param
	if hasEnv {
		params = append(params, ir.NewParam("env", irtypes.I8Ptr))
	}

	for _, p := range n.Params {
		if p.IsVarArgs {
			continue
		}

		pt, err := cg.tinTypeToLLVM(p.Type)
		if err != nil {
			return fmt.Errorf("predeclareColoredVariant %s param %s: %w", tinName, p.Name, err)
		}

		params = append(params, ir.NewParam(p.Name, pt))
	}

	var retType irtypes.Type = irtypes.Void

	if n.RetType != nil {
		rt, err := cg.tinTypeToLLVM(n.RetType)
		if err != nil {
			return fmt.Errorf("predeclareColoredVariant %s retType: %w", tinName, err)
		}

		retType = rt
	}

	f := cg.mod.NewFunc(coloredName, retType, params...)
	f.Blocks = nil
	cg.curScope.set(coloredName, &scopeEntry{val: f, isAlloc: false})

	return nil
}

// $colored body generation

// genColoredFuncBody emits the $colored variant body for `n` into
// the pre-declared `<coloredName>` IR function.  Same body shape as
// the plain sync emission in genFuncDeclAs except that
// curFnAutoYield is on (loop back-edges + heavy-call sites yield)
// and curFnColoredSync gates the yield-emission switch in
// genYieldAutoAt / genCallSiteYieldFor so the yield lowers to a
// runtime call (`_tin_fiber_yield_coro(_tin_current_coro_hdl())`)
// instead of an LLVM coro.suspend intrinsic.  No coro frame is
// allocated; the body borrows the caller's frame via TLS.
//
// Coverage matches the sync emission path: param allocas with ARC
// retain, scope-exit release via genBody, where-list match subject,
// debug info.  Skipped relative to sync: TCO + mutual-TCO (a tail
// call between colored variants would need to preserve the yield
// instrumentation invariant; conservatively disable until we have a
// concrete need + test).  Skipped relative to $coro: ramp prologue,
// coro intrinsics, fiber complete, final suspend.
func (cg *CodeGen) genColoredFuncBody(n *ast.FuncDecl, coloredName string) error {
	se, ok := cg.curScope.vars[coloredName]
	if !ok {
		return nil
	}

	coloredFn, ok := se.val.(*ir.Func)
	if !ok || len(coloredFn.Blocks) > 0 {
		return nil
	}

	if n.Body == nil {
		coloredFn.Blocks = nil

		return nil
	}

	var retType irtypes.Type = irtypes.Void

	if n.RetType != nil {
		rt, err := cg.tinTypeToLLVM(n.RetType)
		if err != nil {
			return err
		}

		retType = rt
	}

	prevFn := cg.curFn
	prevScope := cg.curScope
	prevBlock := cg.curBlock
	prevAutoYield := cg.curFnAutoYield
	prevColoredSync := cg.curFnColoredSync
	prevInCoroFn := cg.inCoroFn
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
	prevYieldResume := cg.yieldResumeBlocks
	prevDiScope := cg.diCurrentScope
	prevEscapingVars := cg.curFnEscapingVars
	prevEscapingAliases := cg.curFnEscapingAliases
	// Each variant of the function body emits independently; the
	// moved-bindings set from the sync pass must not leak into the
	// colored pass or every read after a `move` in the sync body
	// would fire a spurious use-after-move when the colored body
	// codegens.
	prevMovedBindings := cg.movedBindings
	cg.movedBindings = nil

	cg.curBlock = nil
	cg.pendingDeferFnI8s = nil
	cg.pendingDeferFrames = nil
	cg.pendingDeferEnvs = nil
	cg.labelCount = 0
	cg.matchSubject = nil
	cg.yieldResumeBlocks = make(map[*ir.Block]bool)
	cg.curFnAutoYield = !hasTag(n.Tags, "no_autoyield")
	cg.curFnColoredSync = true
	// $colored bodies do NOT set inCoroFn: that flag specifically
	// gates coro-frame-using lowerings (genReturn -> coro completion,
	// emitSuspendPoint, etc.) that require curCoroFrame.  Call
	// routing checks (resolveColoredCallee, genCallSiteYieldFor)
	// consult curFnColoredSync directly.
	cg.inCoroFn = false
	// No coro frame -- yields go through the runtime hdl lookup.
	cg.curCoroHdl = nil
	cg.curCoroID = nil
	cg.curCoroCleanup = nil
	cg.curCoroFrame = nil
	cg.curCoroRetType = retType
	cg.curFnEscapingVars, cg.curFnEscapingAliases = findEscapingAddressTakenVars(n.Body)

	cg.curFn = coloredFn
	cg.curScope = newScope(prevScope)
	cg.curScope.isFunctionBoundary = true

	defer func() {
		cg.curFn = prevFn
		cg.curScope = prevScope
		cg.curBlock = prevBlock
		cg.curFnAutoYield = prevAutoYield
		cg.curFnColoredSync = prevColoredSync
		cg.inCoroFn = prevInCoroFn
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
		cg.yieldResumeBlocks = prevYieldResume
		cg.diCurrentScope = prevDiScope
		cg.curFnEscapingVars = prevEscapingVars
		cg.curFnEscapingAliases = prevEscapingAliases
		cg.movedBindings = prevMovedBindings
	}()

	entry := coloredFn.NewBlock("entry")

	if cg.filename != "" {
		if cg.fnSourceFiles == nil {
			cg.fnSourceFiles = map[string]string{}
		}

		cg.fnSourceFiles[coloredFn.Name()] = cg.filename
	}

	cg.emitDbgSubprogram(n, coloredFn, cg.filename)

	if cg.debugMode && n.Pos().Line != 0 {
		cg.currentPos = n.Pos()
	}
	// Defer-return slot.
	if !irtypes.IsVoid(retType) && hasDeferStmt(n.Body) {
		slotType := irtypes.NewStruct(irtypes.I8, retType)
		slotAlloca := entry.NewAlloca(slotType)
		validGep := entry.NewGetElementPtr(slotType, slotAlloca,
			constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
		entry.NewStore(constant.NewInt(irtypes.I8, 0), validGep)
		cg.curFnDeferRetAlloca = entry.NewBitCast(slotAlloca, irtypes.I8Ptr)
	} else {
		cg.curFnDeferRetAlloca = nil
	}
	// Register self in scope so recursive calls inside the body resolve.
	cg.curScope.set(coloredName, &scopeEntry{val: coloredFn, isAlloc: false})

	// Param allocas + ARC retain, mirroring genFuncDeclAs.
	var firstParamAlloca *ir.InstAlloca

	llIdx := 0

	for _, astParam := range n.Params {
		if astParam.IsVarArgs {
			if astParam.Name != "" {
				null := constant.NewNull(irtypes.NewPointer(irtypes.I8))
				cg.curScope.set(astParam.Name, &scopeEntry{val: null, isAlloc: false})
			}

			continue
		}

		p := coloredFn.Params[llIdx]
		llIdx++
		alloca := entry.NewAlloca(p.Type())
		entry.NewStore(p, alloca)
		isRC := isRCTrackedType(p.Type())
		cg.emitRetain(entry, p)
		cg.emitDbgDeclare(entry, alloca, astParam.Name, n.Pos().Line, uint64(llIdx), astParam.Type, p.Type())
		cg.curScope.set(astParam.Name, &scopeEntry{
			val: alloca, isAlloc: true, isRC: isRC, noDeinit: true,
			isUnsigned: isUnsignedTinType(astParam.Type), scalarTypeName: scalar8BitTypeName(astParam.Type),
			tinType: astParam.Type, declPos: n.Pos(),
		})

		if llIdx == 1 {
			firstParamAlloca = alloca
		}
	}
	// Where-list bodies match against the first param.
	if _, isWhere := n.Body.(*ast.WhereList); isWhere && firstParamAlloca != nil {
		loadInst := entry.NewLoad(firstParamAlloca.ElemType, firstParamAlloca)
		cg.attachCurrentDbgLoc(loadInst)
		cg.matchSubject = loadInst
	}

	_, err := cg.genBody(entry, n.Body, retType)

	cg.ensureAllCallsHaveDbg(coloredFn)

	if err != nil {
		return err
	}

	return nil
}

// $coro body generation helpers

// llvmSizeOf returns the size of an LLVM type as an i64 value using the GEP

package codegen

// coro.go - LLVM coroutine intrinsic support for the #async fiber system.
//
// llir/llvm v0.3.6 does not have a native "token" LLVM type. We implement a
// lightweight wrapper that satisfies the irtypes.Type interface so the emitted
// .ll file contains valid LLVM IR. The LLVM coroutine passes run automatically
// when clang processes the IR with -O1 or higher.

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// --------------------------------------------------------------------------
// token type: LLVM pseudo-type used only by llvm.coro.* intrinsics
// --------------------------------------------------------------------------

type coroTokenType struct{}

func (t *coroTokenType) Equal(other irtypes.Type) bool {
	_, ok := other.(*coroTokenType)

	return ok
}
func (t *coroTokenType) String() string   { return "token" }
func (t *coroTokenType) LLString() string { return "token" }
func (t *coroTokenType) SetName(_ string) {}
func (t *coroTokenType) Name() string     { return "" }

var coroTokType irtypes.Type = &coroTokenType{}

// coroNoneVal is the `token none` constant used in coro.suspend / coro.end.
type coroNoneVal struct{}

func (v *coroNoneVal) Type() irtypes.Type { return coroTokType }
func (v *coroNoneVal) Ident() string      { return "none" }
func (v *coroNoneVal) LLString() string   { return "token none" }
func (v *coroNoneVal) String() string     { return "token none" }

var coroNone value.Value = &coroNoneVal{}

// --------------------------------------------------------------------------
// Lazy intrinsic / runtime declarations
// --------------------------------------------------------------------------

func (cg *CodeGen) ensureCoroIntrinsics() {
	if cg.coroIDFn != nil {
		return
	}
	cg.coroIDFn = cg.ensureIntrinsic("llvm.coro.id", coroTokType, []*ir.Param{
		ir.NewParam("", irtypes.I32),
		ir.NewParam("", irtypes.I8Ptr),
		ir.NewParam("", irtypes.I8Ptr),
		ir.NewParam("", irtypes.I8Ptr),
	})
	cg.coroAllocFn = cg.ensureIntrinsic("llvm.coro.alloc", irtypes.I1, []*ir.Param{
		ir.NewParam("", coroTokType),
	})
	cg.coroSizeFn = cg.ensureIntrinsic("llvm.coro.size.i64", irtypes.I64, nil)
	cg.coroBeginFn = cg.ensureIntrinsic("llvm.coro.begin", irtypes.I8Ptr, []*ir.Param{
		ir.NewParam("", coroTokType),
		ir.NewParam("", irtypes.I8Ptr),
	})
	cg.coroSuspendFn = cg.ensureIntrinsic("llvm.coro.suspend", irtypes.I8, []*ir.Param{
		ir.NewParam("", coroTokType),
		ir.NewParam("", irtypes.I1),
	})
	cg.coroEndFn = cg.ensureIntrinsic("llvm.coro.end", irtypes.Void, []*ir.Param{
		ir.NewParam("", irtypes.I8Ptr),
		ir.NewParam("", irtypes.I1),
		ir.NewParam("", coroTokType),
	})
	cg.coroFreeFn = cg.ensureIntrinsic("llvm.coro.free", irtypes.I8Ptr, []*ir.Param{
		ir.NewParam("", coroTokType),
		ir.NewParam("", irtypes.I8Ptr),
	})
	cg.coroResumeFn = cg.ensureIntrinsic("llvm.coro.resume", irtypes.Void, []*ir.Param{
		ir.NewParam("", irtypes.I8Ptr),
	})
	cg.coroDoneFn = cg.ensureIntrinsic("llvm.coro.done", irtypes.I1, []*ir.Param{
		ir.NewParam("", irtypes.I8Ptr),
	})
	cg.coroDestroyFn = cg.ensureIntrinsic("llvm.coro.destroy", irtypes.Void, []*ir.Param{
		ir.NewParam("", irtypes.I8Ptr),
	})
}

// ensureIntrinsic declares an LLVM intrinsic on the module (declaration only).
func (cg *CodeGen) ensureIntrinsic(name string, ret irtypes.Type, params []*ir.Param) *ir.Func {
	for _, f := range cg.mod.Funcs {
		if f.Name() == name {
			return f
		}
	}
	f := cg.mod.NewFunc(name, ret, params...)
	f.Blocks = nil

	return f
}

func (cg *CodeGen) ensureFiberRuntime() {
	if cg.fiberSpawnFn != nil {
		return
	}
	cg.fiberSpawnFn = cg.ensureExternDecl("_tin_fiber_spawn", irtypes.I64,
		[]*ir.Param{ir.NewParam("hdl", irtypes.I8Ptr)}, false)
	cg.fiberCompleteFn = cg.ensureExternDecl("_tin_fiber_complete", irtypes.Void,
		[]*ir.Param{ir.NewParam("res", irtypes.I8Ptr)}, false)
	cg.fiberJoinFn = cg.ensureExternDecl("_tin_fiber_join", irtypes.Void,
		[]*ir.Param{ir.NewParam("pid", irtypes.I64), ir.NewParam("hdl", irtypes.I8Ptr)}, false)
	cg.fiberGetResultFn = cg.ensureExternDecl("_tin_fiber_get_result", irtypes.I8Ptr,
		[]*ir.Param{ir.NewParam("pid", irtypes.I64)}, false)
	cg.fiberGetPanicMsgFn = cg.ensureExternDecl("_tin_fiber_get_panic_msg", irtypes.I8Ptr,
		[]*ir.Param{ir.NewParam("pid", irtypes.I64)}, false)
	cg.fiberYieldCoroFn = cg.ensureExternDecl("_tin_fiber_yield_coro", irtypes.Void,
		[]*ir.Param{ir.NewParam("hdl", irtypes.I8Ptr)}, false)
	cg.coroTakeResultFn = cg.ensureExternDecl("_tin_coro_take_result", irtypes.I8Ptr, nil, false)
	cg.fiberInitFn = cg.ensureExternDecl("_tin_fiber_init", irtypes.Void, nil, false)
	cg.fiberRunFn = cg.ensureExternDecl("_tin_fiber_run", irtypes.Void, nil, false)
	cg.ioInitFn = cg.ensureExternDecl("_tin_io_init", irtypes.Void, nil, false)
	// Auto-load stdlib/sync so Future[t] and Awaitable[t] are available
	// for spawn/await codegen without requiring an explicit `use sync`.
	// Error is stored; if sync fails to load, wrapPidInFuture will report it.
	cg.syncLoadErr = cg.ensureSyncModule()
}

// ensureSyncModule loads the stdlib/sync package once so that Future[t],
// Awaitable[t], and Unit are available in scope for fiber codegen.
// Returns the load error so callers can report it if Future[T] wrapping later fails.
func (cg *CodeGen) ensureSyncModule() error {
	if cg.syncModuleLoaded {
		return nil
	}
	cg.syncModuleLoaded = true

	return cg.loadPackage("sync")
}

// --------------------------------------------------------------------------
// Coroutine prologue helper
// --------------------------------------------------------------------------

type coroFrame struct {
	hdl             value.Value // i8* coroutine handle
	id              value.Value // token coroutine id
	cleanupEntry    *ir.Block   // block to branch to for cleanup
	finalSuspendBlk *ir.Block   // single shared block holding coro.suspend(true)
}

// emitCoroPrologue emits the standard coroutine prologue into entry.
// Returns a coroFrame and the block where the function body should be generated.
//
//	entry -> [coro.alloc] -> coro.begin  (body starts here)
func (cg *CodeGen) emitCoroPrologue(entry *ir.Block) (*coroFrame, *ir.Block) {
	cg.ensureCoroIntrinsics()

	// _tin_coro_malloc / _tin_coro_free use a per-thread frame pool to avoid
	// system malloc/free calls on the hot path.  The pool prefixes each frame
	// with its size (8 bytes) so frames can be matched by size on reuse.
	// The LLVM frame pointer is ptr+8 (skipping the size prefix).
	coroMallocFn := cg.ensureExternDecl("_tin_coro_malloc", irtypes.I8Ptr,
		[]*ir.Param{ir.NewParam("size", irtypes.I64)}, false)

	id := entry.NewCall(cg.coroIDFn,
		constant.NewInt(irtypes.I32, 0),
		constant.NewNull(irtypes.I8Ptr),
		constant.NewNull(irtypes.I8Ptr),
		constant.NewNull(irtypes.I8Ptr),
	)
	needAlloc := entry.NewCall(cg.coroAllocFn, id)

	allocBlk := cg.newBlock("coro.alloc")
	beginBlk := cg.newBlock("coro.begin")
	entry.NewCondBr(needAlloc, allocBlk, beginBlk)

	sz := allocBlk.NewCall(cg.coroSizeFn)
	mem := allocBlk.NewCall(coroMallocFn, sz)
	allocBlk.NewBr(beginBlk)

	phi := beginBlk.NewPhi(
		ir.NewIncoming(mem, allocBlk),
		ir.NewIncoming(constant.NewNull(irtypes.I8Ptr), entry),
	)
	hdl := beginBlk.NewCall(cg.coroBeginFn, id, phi)

	cleanupBlk := cg.newBlock("coro.cleanup")

	// finalBlk holds the single coro.suspend(true) for this coroutine.
	// LLVM requires exactly one final suspend per coroutine; all return paths
	// branch here instead of emitting coro.suspend(true) inline.
	finalBlk := cg.newBlock("coro.final")
	sp := finalBlk.NewCall(cg.coroSuspendFn, coroNone, constant.NewInt(irtypes.I1, 1))
	deadBlk := cg.newBlock("coro.after.final")
	finalBlk.NewSwitch(sp, cleanupBlk,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), deadBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), cleanupBlk),
	)
	deadBlk.NewUnreachable()

	return &coroFrame{hdl: hdl, id: id, cleanupEntry: cleanupBlk, finalSuspendBlk: finalBlk}, beginBlk
}

// emitCoroEpilogue emits the cleanup epilogue into cleanupBlk.
// LLVM coroutine lowering requires coro.end BEFORE coro.free:
// the coro.free intrinsic returns null if coro.end was already called,
// which tells the cleanup block whether to free memory.
func (cg *CodeGen) emitCoroEpilogue(frame *coroFrame) {
	b := frame.cleanupEntry
	// Use _tin_coro_free (pool-aware) instead of free() directly.
	// _tin_coro_free reads the size prefix stored 8 bytes before the LLVM frame
	// pointer and either returns the frame to the per-thread pool or falls back
	// to free().  Passing null (coro-elided / stack-allocated frame) is a no-op.
	coroFreeFn := cg.ensureExternDecl("_tin_coro_free", irtypes.Void,
		[]*ir.Param{ir.NewParam("ptr", irtypes.I8Ptr)}, false)
	// coro.end must come first - it marks the coroutine as done.
	b.NewCall(cg.coroEndFn, frame.hdl, constant.NewInt(irtypes.I1, 0), coroNone)
	// coro.free returns the memory to release (null if coro-elided).
	mem := b.NewCall(cg.coroFreeFn, frame.id, frame.hdl)
	b.NewCall(coroFreeFn, mem)
	b.NewRet(frame.hdl)
}

// emitSuspendPoint emits a coro.suspend in block.
// Returns: resumeBlock (execution continues there after re-resume).
// The cleanupBlock destination must be set by the caller; we branch directly
// to frame.cleanupEntry here.
func (cg *CodeGen) emitSuspendPoint(block *ir.Block, frame *coroFrame) *ir.Block {
	sp := block.NewCall(cg.coroSuspendFn, coroNone, constant.NewInt(irtypes.I1, 0))
	resumeBlk := cg.newBlock("coro.resume")
	cleanBrBlk := cg.newBlock("coro.cleanup.br")
	suspBlk := cg.newBlock("coro.suspended")

	block.NewSwitch(sp, suspBlk,
		ir.NewCase(constant.NewInt(irtypes.I8, 0), resumeBlk),
		ir.NewCase(constant.NewInt(irtypes.I8, 1), cleanBrBlk),
	)
	suspBlk.NewRet(frame.hdl) // coroutine is suspended; ramp returns hdl
	cleanBrBlk.NewBr(frame.cleanupEntry)

	return resumeBlk
}

// emitFinalSuspend branches to the coroutine's shared final-suspend block.
// LLVM requires exactly one coro.suspend(true) per coroutine; all return paths
// call this function which branches to frame.finalSuspendBlk (created once in
// emitCoroPrologue).
func (cg *CodeGen) emitFinalSuspend(block *ir.Block, frame *coroFrame) {
	block.NewBr(frame.finalSuspendBlk)
}

// --------------------------------------------------------------------------
// Callgraph coloring
// --------------------------------------------------------------------------

// isAsyncTag returns true when tags contains "async".
func isAsyncTag(tags []string) bool {
	for _, t := range tags {
		if t == "async" {
			return true
		}
	}

	return false
}

// coroVersionName returns name + "$coro".
func coroVersionName(name string) string { return name + "$coro" }

// recordCallees walks an AST node tree and appends every directly-called
// function name to *out. This is used to build the call graph before coloring.
func recordCallees(n ast.Node, out *[]string) {
	if n == nil {
		return
	}
	switch v := n.(type) {
	case *ast.CallExpr:
		if id, ok := v.Func.(*ast.Identifier); ok {
			*out = append(*out, id.Name)
		}
		recordCallees(v.Func, out)
		for _, a := range v.Args {
			recordCallees(a, out)
		}
	case *ast.Block:
		for _, s := range v.Stmts {
			recordCallees(s, out)
		}
	case *ast.ExprStmt:
		recordCallees(v.Expr, out)
	case *ast.VarDecl:
		recordCallees(v.Value, out)
	case *ast.ReturnStmt:
		recordCallees(v.Value, out)
	case *ast.AssignStmt:
		recordCallees(v.Target, out)
		recordCallees(v.Value, out)
	case *ast.AugAssignStmt:
		recordCallees(v.Target, out)
		recordCallees(v.Value, out)
	case *ast.IfStmt:
		recordCallees(v.Cond, out)
		for _, s := range v.Then.Stmts {
			recordCallees(s, out)
		}
		for _, elif := range v.ElseIfs {
			recordCallees(elif.Cond, out)
			for _, s := range elif.Body.Stmts {
				recordCallees(s, out)
			}
		}
		if v.Else != nil {
			for _, s := range v.Else.Stmts {
				recordCallees(s, out)
			}
		}
	case *ast.ForStmt:
		recordCallees(v.Cond, out)
		recordCallees(v.Init, out)
		recordCallees(v.Post, out)
		recordCallees(v.Iter, out)
		if v.Body != nil {
			for _, s := range v.Body.Stmts {
				recordCallees(s, out)
			}
		}
	case *ast.BinExpr:
		recordCallees(v.Left, out)
		recordCallees(v.Right, out)
	case *ast.UnaryExpr:
		recordCallees(v.Expr, out)
	case *ast.IndexExpr:
		recordCallees(v.Expr, out)
		recordCallees(v.Index, out)
	case *ast.FieldAccess:
		recordCallees(v.Expr, out)
	case *ast.PipeExpr:
		recordCallees(v.Left, out)
		recordCallees(v.Right, out)
	case *ast.TernaryExpr:
		recordCallees(v.Cond, out)
		recordCallees(v.Then, out)
		recordCallees(v.Else, out)
	case *ast.EchoStmt:
		recordCallees(v.Value, out)
	case *ast.DeferStmt:
		recordCallees(v.Call, out)
	case *ast.SpawnExpr:
		recordCallees(v.Call, out)
		if v.DoBlock != nil {
			for _, s := range v.DoBlock.Stmts {
				recordCallees(s, out)
			}
		}
	case *ast.AwaitExpr:
		recordCallees(v.Future, out)
	case *ast.LambdaExpr:
		if v.Body != nil {
			recordCallees(v.Body, out)
		}
	case *ast.WhereList:
		for _, wc := range v.Clauses {
			recordCallees(wc.Body, out)
		}
	}
}

// buildCallGraphEntry builds call-graph entries for a single function declaration
// and all its struct methods.
func (cg *CodeGen) buildCallGraphEntry(name string, body ast.Node) {
	if body == nil {
		return
	}
	var callees []string
	recordCallees(body, &callees)
	// deduplicate
	seen := map[string]bool{}
	for _, c := range callees {
		if !seen[c] {
			seen[c] = true
			cg.callGraph[name] = append(cg.callGraph[name], c)
		}
	}
}

// colorCallGraph runs BFS from all {#async} roots and marks every reachable
// Tin function as needing a $coro duplicate.
func (cg *CodeGen) colorCallGraph() {
	worklist := make([]string, 0)
	for name, decl := range cg.funcDecls {
		if isAsyncTag(decl.Tags) {
			worklist = append(worklist, name)
		}
	}
	for len(worklist) > 0 {
		name := worklist[0]
		worklist = worklist[1:]
		if cg.coroCallable[name] {
			continue
		}
		cg.coroCallable[name] = true
		for _, callee := range cg.callGraph[name] {
			if _, ok := cg.funcDecls[callee]; ok && !cg.coroCallable[callee] {
				worklist = append(worklist, callee)
			}
		}
	}
	// fn main() is compiled to _tin_user_main at IR level.  If the user
	// marked it {#async}, colorCallGraph sets coroCallable["main"] but
	// genFuncDeclAs checks coroCallable["_tin_user_main"] (the IR name).
	// Sync the two so the $coro variant is actually generated.
	if cg.coroCallable["main"] {
		cg.coroCallable["_tin_user_main"] = true
	}
}

// --------------------------------------------------------------------------
// $coro variant predeclaration
// --------------------------------------------------------------------------

// predeclareCoroVariant pre-declares "name$coro(...) i8*" on the module so
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

// --------------------------------------------------------------------------
// $coro body generation helpers
// --------------------------------------------------------------------------

// llvmSizeOf returns the size of an LLVM type as an i64 value using the GEP
// null-pointer trick: sizeof(T) = ptrtoint(GEP (T*)null, 1).
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
//     pointer — no heap allocation.  For spawned fibers, it falls back to
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
		// - Inline drive: _tin_inline_result_mode_begin() was called → TLS buffer (no malloc).
		// - Spawned fiber: mode not active → malloc(sz) (result must outlive the coro).
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

	// Emit coroutine prologue: entry -> coro.alloc -> coro.begin.
	entryBlk := coroFn.NewBlock("entry")
	cg.ensureFiberRuntime()
	frame, rampBlock := cg.emitCoroPrologue(entryBlk)

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
		llParam := 0
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
	cg.unpackEnv(bodyStart, coroFn, envStructType, captures)

	// ARC: release the env's own reference to each RC-tracked capture (the
	// matching retain was emitted in genSpawnDoBlock before buildEnv).
	// The coro scope now owns its own retain (from unpackEnv), so the env's
	// reference is no longer needed.  Also free the env struct itself.
	if len(captures) > 0 && envStructType != nil {
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

	// Set up defer return override slot for this coro body (mirrors genFuncDecl).
	// Without this, cg.curFnDeferRetAlloca would bleed in from the outer function,
	// causing cross-function SSA value references in the coro's IR.
	if origRetType != nil && !irtypes.IsVoid(origRetType) {
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
	llIdx := 0
	for _, astParam := range n.Params {
		if astParam.IsVarArgs {
			continue
		}
		p := coroFn.Params[llIdx]
		llIdx++
		alloca := bodyStart.NewAlloca(p.Type())
		bodyStart.NewStore(p, alloca)
		isRC := isRCTrackedType(p.Type())
		cg.curScope.set(astParam.Name, &scopeEntry{val: alloca, isAlloc: true, isRC: isRC, noDeinit: true})
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
//	  0  → normal resume   → jump back to retryBlk
//	  1  → final cleanup   → jump to coro cleanup entry
//	  default → the "suspend" path; the outer function returns its handle
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

// genYieldAutoAt emits an automatic yield point at the backedge of a loop.
// `from` is the block at the end of the loop body; after yielding it resumes
// at `header` (the loop condition or post block).
// Only called when cg.curFnAutoYield is true.
func (cg *CodeGen) genYieldAutoAt(from *ir.Block, header *ir.Block) {
	if cg.yieldResumeBlocks[from] {
		// `from` is the resume block of an explicit `yield` statement.
		// The fiber just executed one suspension this iteration; adding a second
		// autoyield at the backedge would force a redundant scheduler round-trip.
		from.NewBr(header)

		return
	}
	resume := cg.emitSuspendPoint(from, cg.curCoroFrame)
	resume.NewBr(header)
}

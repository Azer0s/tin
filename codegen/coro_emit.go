package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

// Coroutine prologue helper

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

// externYieldsAfter reports whether the named runtime extern leaves
// the calling fiber in a pending-park state, requiring a
// coro.suspend afterwards so the worker observes the park and
// switches to another fiber.  The list is small and tightly
// auditable -- channel send/recv emit their own inline suspends,
// fiber_join handles its own waiter dance, and the remaining
// park-on-call externs are timer / future / IO primitives.
func externYieldsAfter(name string) bool {
	switch name {
	case "_tin_sleep_ms":
		return true
	}

	return false
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

// ensureCoroWrapperFor returns (creating on first call) a per-fn
// coroutine wrapper that calls srcFn synchronously and packages its
// result through the standard coro completion path.  Used to fill
// fat-fn-ptr slot 0 for sync fns under the 4-slot ABI so `spawn f()`
// on a fn-value always finds a coro ramp regardless of whether `f`
// was declared `fn(...)` or `fn{#async}(...)`.
//
// Wrapper shape:
//
//	define i8* @<srcFn>$coro_wrap(<params...>) presplitcoroutine {
//	entry:    coro.id / alloc / begin
//	          initial suspend (returns hdl to caller)
//	body:     %r = call @<srcFn>(<params...>)
//	          _tin_fiber_complete(box(%r))
//	          br final
//	final:    coro.suspend(true) / cleanup / coro.end
//	}
//
// Idempotent via cg.curScope lookup on the wrapper name.
func (cg *CodeGen) ensureCoroWrapperFor(srcFn *ir.Func) *ir.Func {
	wrapperName := srcFn.Name() + "$coro_wrap"

	if cg.coroWrappers == nil {
		cg.coroWrappers = map[string]*ir.Func{}
	}

	if existing, ok := cg.coroWrappers[wrapperName]; ok {
		return existing
	}

	wrapperParams := make([]*ir.Param, 0, len(srcFn.Sig.Params))
	for i, pt := range srcFn.Sig.Params {
		wrapperParams = append(wrapperParams, ir.NewParam(fmt.Sprintf("p%d", i), pt))
	}

	wrapper := cg.mod.NewFunc(wrapperName, irtypes.I8Ptr, wrapperParams...)
	// Default external linkage -- the cache guarantees one definition per
	// LLVM module, and ThinLTO will dedup across translation units.  Marking
	// internal caused references from sibling fns to lose the definition
	// after ThinLTO partition (undefined `__shim_xxx$coro_wrap` at link).
	wrapper.FuncAttrs = append(wrapper.FuncAttrs, ir.AttrString("presplitcoroutine"))

	cg.coroWrappers[wrapperName] = wrapper

	prevFn := cg.curFn
	prevInCoro := cg.inCoroFn
	prevCoroHdl := cg.curCoroHdl
	prevCoroID := cg.curCoroID
	prevCoroCleanup := cg.curCoroCleanup
	prevCoroFrame := cg.curCoroFrame
	prevCoroRetType := cg.curCoroRetType
	prevCurBlock := cg.curBlock

	cg.curFn = wrapper

	entry := wrapper.NewBlock("entry")

	cg.ensureFiberRuntime()
	frame, ramp := cg.emitCoroPrologue(entry)

	cg.curCoroHdl = frame.hdl
	cg.curCoroID = frame.id
	cg.curCoroCleanup = frame.cleanupEntry
	cg.curCoroFrame = frame
	cg.curCoroRetType = srcFn.Sig.RetType
	cg.inCoroFn = true

	for _, p := range wrapper.Params {
		cg.emitRetain(ramp, p)
	}

	body := cg.emitSuspendPoint(ramp, frame)

	callArgs := make([]value.Value, len(wrapper.Params))
	for i, p := range wrapper.Params {
		callArgs[i] = p
	}
	// Prefer the $colored variant of srcFn when one was emitted, so a
	// spawned sync fn cooperates at the same coloring points a bare
	// cooperative-context call would.  Falls through to srcFn when no
	// colored variant exists (e.g. srcFn was not in coloredCallable).
	target := srcFn
	if colored := cg.lookupColoredVariant(srcFn); colored != nil {
		target = colored
	}

	if irtypes.IsVoid(srcFn.Sig.RetType) {
		body.NewCall(target, callArgs...)
		cg.emitCoroComplete(body, nil)
	} else {
		result := body.NewCall(target, callArgs...)
		cg.emitCoroComplete(body, result)
	}

	cg.emitFinalSuspend(body, frame)
	cg.emitCoroEpilogue(frame)

	cg.curFn = prevFn
	cg.inCoroFn = prevInCoro
	cg.curCoroHdl = prevCoroHdl
	cg.curCoroID = prevCoroID
	cg.curCoroCleanup = prevCoroCleanup
	cg.curCoroFrame = prevCoroFrame
	cg.curCoroRetType = prevCoroRetType
	cg.curBlock = prevCurBlock

	return wrapper
}

// Callgraph coloring

// isAsyncTag returns true when tags contains "async".
func isAsyncTag(tags []string) bool {
	for _, t := range tags {
		if t == "async" {
			return true
		}
	}

	return false
}

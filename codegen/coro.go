package codegen

// coro.go - LLVM coroutine intrinsic support for the #async fiber system.
//
// llir/llvm v0.3.6 does not have a native "token" LLVM type. We implement a
// lightweight wrapper that satisfies the irtypes.Type interface so the emitted
// .ll file contains valid LLVM IR. The LLVM coroutine passes run automatically
// when clang processes the IR with -O1 or higher.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// token type: LLVM pseudo-type used only by llvm.coro.* intrinsics

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

// Lazy intrinsic / runtime declarations

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
	for _, f := range cg.allFuncs() {
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
	cg.fiberSpawnJoinableFn = cg.ensureExternDecl("_tin_fiber_spawn_joinable", irtypes.I64,
		[]*ir.Param{ir.NewParam("hdl", irtypes.I8Ptr)}, false)
	// Stacktrace-aware spawn variants (Phase 4 of docs/plans/stacktrace-libunwind.md).
	// Codegen routes here only when cg.stacktraceUsed; the runtime captures
	// _current_fib's pid+generation as the new fiber's parent so a later
	// stacktrace() can walk the spawn chain across fiber boundaries.
	cg.fiberSpawnChainFn = cg.ensureExternDecl("_tin_fiber_spawn_chain", irtypes.I64,
		[]*ir.Param{
			ir.NewParam("hdl", irtypes.I8Ptr),
			ir.NewParam("caller_ip", irtypes.I64),
		}, false)
	cg.fiberSpawnJoinableChainFn = cg.ensureExternDecl("_tin_fiber_spawn_joinable_chain", irtypes.I64,
		[]*ir.Param{
			ir.NewParam("hdl", irtypes.I8Ptr),
			ir.NewParam("caller_ip", irtypes.I64),
		}, false)
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
	// Auto-load runtime/builtin/ so language-defined traits (tryable,
	// awaitable in the future, operator traits, etc.) are always in scope.
	// Failure here is non-fatal: if the directory does not exist (e.g. a
	// custom build), language features that depend on those traits surface
	// their own errors at use sites.
	_ = cg.ensureRuntimeBuiltinModules()

	// Auto-load sync so Future[t] and Awaitable[t] are available
	// for spawn/await codegen without requiring an explicit `use sync`.
	// Error is stored; if sync fails to load, wrapPidInFuture will report it.
	cg.syncLoadErr = cg.ensureSyncModule()
}

// ensureSyncModule loads the sync package once so that Future[t],
// Awaitable[t], and Unit are available in scope for fiber codegen.
// Returns the load error so callers can report it if Future[T] wrapping later fails.
func (cg *CodeGen) ensureSyncModule() error {
	if cg.syncModuleLoaded {
		return nil
	}

	cg.syncModuleLoaded = true

	return cg.loadPackage("sync")
}

// ensureRuntimeBuiltinModules walks runtime/builtin/ and loads every
// .tin file found so the traits defined there are in scope for every
// program. Idempotent. Missing directory is treated as "no built-ins"
// rather than an error so a stripped-down compiler build still works.
func (cg *CodeGen) ensureRuntimeBuiltinModules() error {
	if cg.runtimeBuiltinLoaded {
		return nil
	}

	cg.runtimeBuiltinLoaded = true

	dir := cg.runtimeBuiltinBase()

	entries, err := os.ReadDir(dir)
	if err != nil {
		// No directory == no built-ins. Not an error.
		return nil
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tin") {
			continue
		}

		full := filepath.Join(dir, e.Name())

		dedupeKey := "file:" + full
		if cg.importedPkgs[dedupeKey] {
			continue
		}

		cg.importedPkgs[dedupeKey] = true

		src, err := os.ReadFile(full)
		if err != nil {
			return fmt.Errorf("read %s: %w", full, err)
		}

		pkgName := strings.TrimSuffix(e.Name(), ".tin")
		if err := cg.loadPackageFromSource("builtin::"+pkgName, pkgName, full); err != nil {
			return fmt.Errorf("load %s: %w", full, err)
		}

		_ = src
	}

	return nil
}

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
// Tin function as needing a $coro duplicate.  Also populates
// cg.coloredCallable with the union of {#async}-reachable fns and boxed-fn
// reachable fns -- those need a $colored sync variant so callers in
// cooperative context can route to a yielding body without changing the
// sync signature.  See docs/internals/fn-coloring.md (Colored variants).
func (cg *CodeGen) colorCallGraph() {
	coroWorklist := make([]string, 0)

	for name, decl := range cg.funcDecls {
		if isAsyncTag(decl.Tags) {
			coroWorklist = append(coroWorklist, name)
		}
	}

	for len(coroWorklist) > 0 {
		name := coroWorklist[0]
		coroWorklist = coroWorklist[1:]

		if cg.coroCallable[name] {
			continue
		}

		cg.coroCallable[name] = true
		for _, callee := range cg.callGraph[name] {
			if _, ok := cg.funcDecls[callee]; ok && !cg.coroCallable[callee] {
				coroWorklist = append(coroWorklist, callee)
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
	// Build coloredCallable.  Roots: every coroCallable fn (so any sync
	// callee reached from {#async} is available in colored form, letting
	// the caller's $coro body route through the colored variant for
	// cooperation) plus every boxed fn (slot 1 of the fat-fn-ptr needs
	// a colored body).  BFS through cg.callGraph: a colored body routes
	// its sync callees to their colored variants, so the closure
	// captures every sync helper reachable from cooperative context.
	// isColorableFn returns true when name resolves to a Tin fn with a
	// real body we can emit a $colored variant from.  Skipped:
	//   - externs (no Tin body; the $colored stub would be declared
	//     but never defined, linker rejects).
	//   - #no_autoyield-tagged fns (yields are suppressed, so the
	//     colored body would be byte-identical to the sync entry --
	//     slot 1 falls back to slot 0 via lookupColoredVariant=nil).
	isColorableFn := func(name string) bool {
		decl, ok := cg.funcDecls[name]
		if !ok {
			return false
		}

		if decl.IsExtern != "" || decl.Body == nil {
			return false
		}

		return !hasTag(decl.Tags, "no_autoyield")
	}

	// Populate coloredCallable for EVERY colorable Tin fn (top-level,
	// struct method, package-loaded).  Method calls (`obj.foo()`) and
	// qualified calls (`pkg::foo()`) aren't recorded by recordCallees,
	// so a BFS-only approach would silently lose cooperation through
	// those call sites.  Over-emission cost is bounded: a $colored
	// body for a yield-free fn is byte-identical to its sync entry;
	// the linker / LLVM global-DCE drops unreferenced symbols.
	for name := range cg.funcDecls {
		if isColorableFn(name) {
			cg.coloredCallable[name] = true
		}
	}

	if cg.coloredCallable["main"] {
		cg.coloredCallable["_tin_user_main"] = true
	}
}

// collectBoxedFns walks the AST and populates cg.boxedFns with names
// of fns that appear as VALUES anywhere in the program (referenced
// without an immediate call).  These are roots for coloredCallable
// alongside {#async}-reachable fns: a boxed fn can be invoked via
// slot 1 of its fat-fn-ptr from any cooperative caller, so it needs a
// colored variant to yield at coloring points.
//
// Detection: every Identifier whose name resolves to a known fn AND
// which is NOT the .Func of a CallExpr is treated as boxed.  This
// over-approximates (the value may flow somewhere that never
// cooperatively invokes it) but errs in the safe direction: an unused
// colored emission is dead code the linker drops.
func (cg *CodeGen) collectBoxedFns(prog *ast.Program) {
	if cg.boxedFns == nil {
		cg.boxedFns = map[string]bool{}
	}

	for _, stmt := range prog.Stmts {
		cg.walkBoxedRefs(stmt, false)
	}
}

// walkBoxedRefs is the recursive worker for collectBoxedFns.  The
// `inCalleePos` flag is true when the current node sits in the .Func
// slot of a parent CallExpr -- an identifier in callee position is a
// direct call, not a value reference, and is suppressed.
func (cg *CodeGen) walkBoxedRefs(n ast.Node, inCalleePos bool) {
	if n == nil {
		return
	}

	switch v := n.(type) {
	case *ast.Identifier:
		if inCalleePos {
			return
		}

		if _, isFn := cg.funcDecls[v.Name]; isFn {
			cg.boxedFns[v.Name] = true
		}
	case *ast.CallExpr:
		cg.walkBoxedRefs(v.Func, true)
		for _, a := range v.Args {
			cg.walkBoxedRefs(a, false)
		}
	case *ast.Block:
		for _, s := range v.Stmts {
			cg.walkBoxedRefs(s, false)
		}
	case *ast.FuncDecl:
		if v.Body != nil {
			cg.walkBoxedRefs(v.Body, false)
		}
	case *ast.TestDecl:
		if v.Body != nil {
			cg.walkBoxedRefs(v.Body, false)
		}
	case *ast.StructDecl:
		for _, m := range v.Methods {
			if m.Body != nil {
				cg.walkBoxedRefs(m.Body, false)
			}
		}
	case *ast.BinExpr:
		cg.walkBoxedRefs(v.Left, false)
		cg.walkBoxedRefs(v.Right, false)
	case *ast.UnaryExpr:
		cg.walkBoxedRefs(v.Expr, false)
	case *ast.FieldAccess:
		cg.walkBoxedRefs(v.Expr, false)
	case *ast.IndexExpr:
		cg.walkBoxedRefs(v.Expr, false)
		cg.walkBoxedRefs(v.Index, false)
	case *ast.AsExpr:
		cg.walkBoxedRefs(v.Expr, false)
	case *ast.PipeExpr:
		cg.walkBoxedRefs(v.Left, false)
		cg.walkBoxedRefs(v.Right, false)
	case *ast.TernaryExpr:
		cg.walkBoxedRefs(v.Cond, false)
		cg.walkBoxedRefs(v.Then, false)
		cg.walkBoxedRefs(v.Else, false)
	case *ast.AwaitExpr:
		cg.walkBoxedRefs(v.Future, false)
	case *ast.SpawnExpr:
		cg.walkBoxedRefs(v.Call, false)
		if v.DoBlock != nil {
			for _, s := range v.DoBlock.Stmts {
				cg.walkBoxedRefs(s, false)
			}
		}
	case *ast.LambdaExpr:
		if v.Body != nil {
			cg.walkBoxedRefs(v.Body, false)
		}
	case *ast.ArrayLit:
		for _, e := range v.Elems {
			cg.walkBoxedRefs(e, false)
		}
	case *ast.StructLit:
		for _, f := range v.Fields {
			cg.walkBoxedRefs(f.Value, false)
		}

		for _, e := range v.Positional {
			cg.walkBoxedRefs(e, false)
		}
	case *ast.TupleLit:
		for _, e := range v.Elems {
			cg.walkBoxedRefs(e, false)
		}
	case *ast.VarDecl:
		cg.walkBoxedRefs(v.Value, false)
	case *ast.TopLevelVar:
		cg.walkBoxedRefs(v.Value, false)
	case *ast.ReturnStmt:
		cg.walkBoxedRefs(v.Value, false)
	case *ast.AssignStmt:
		cg.walkBoxedRefs(v.Target, false)
		cg.walkBoxedRefs(v.Value, false)
	case *ast.AugAssignStmt:
		cg.walkBoxedRefs(v.Target, false)
		cg.walkBoxedRefs(v.Value, false)
	case *ast.ExprStmt:
		cg.walkBoxedRefs(v.Expr, false)
	case *ast.IfStmt:
		cg.walkBoxedRefs(v.Cond, false)
		if v.Then != nil {
			for _, s := range v.Then.Stmts {
				cg.walkBoxedRefs(s, false)
			}
		}

		for _, ei := range v.ElseIfs {
			cg.walkBoxedRefs(ei.Cond, false)
			if ei.Body != nil {
				for _, s := range ei.Body.Stmts {
					cg.walkBoxedRefs(s, false)
				}
			}
		}

		if v.Else != nil {
			for _, s := range v.Else.Stmts {
				cg.walkBoxedRefs(s, false)
			}
		}
	case *ast.ForStmt:
		cg.walkBoxedRefs(v.Cond, false)
		cg.walkBoxedRefs(v.Init, false)
		cg.walkBoxedRefs(v.Post, false)
		cg.walkBoxedRefs(v.Iter, false)
		if v.Body != nil {
			for _, s := range v.Body.Stmts {
				cg.walkBoxedRefs(s, false)
			}
		}
	case *ast.DeferStmt:
		cg.walkBoxedRefs(v.Call, false)
	case *ast.EchoStmt:
		cg.walkBoxedRefs(v.Value, false)
	case *ast.WhereList:
		for _, wc := range v.Clauses {
			cg.walkBoxedRefs(wc.Body, false)
		}
	case *ast.MatchStmt:
		cg.walkBoxedRefs(v.Expr, false)
		for _, c := range v.Cases {
			if c.Guard != nil {
				cg.walkBoxedRefs(c.Guard, false)
			}

			if c.Body != nil {
				cg.walkBoxedRefs(c.Body, false)
			}
		}
	case *ast.AwaitMatchStmt:
		for _, f := range v.Futures {
			cg.walkBoxedRefs(f, false)
		}

		for _, c := range v.Cases {
			if c.Guard != nil {
				cg.walkBoxedRefs(c.Guard, false)
			}

			if c.Body != nil {
				cg.walkBoxedRefs(c.Body, false)
			}
		}

		if v.Default != nil {
			cg.walkBoxedRefs(v.Default, false)
		}
	case *ast.TryExpr:
		cg.walkBoxedRefs(v.Inner, false)
	case *ast.SliceExpr:
		cg.walkBoxedRefs(v.Expr, false)
		cg.walkBoxedRefs(v.Start, false)
		cg.walkBoxedRefs(v.End, false)
	case *ast.RangeExpr:
		cg.walkBoxedRefs(v.Start, false)
		cg.walkBoxedRefs(v.End, false)
	case *ast.ArrayFillLit:
		cg.walkBoxedRefs(v.Value, false)
	case *ast.AddrExpr:
		cg.walkBoxedRefs(v.Val, false)
	case *ast.DerefExpr:
		cg.walkBoxedRefs(v.Expr, false)
	case *ast.AddressOfExpr:
		cg.walkBoxedRefs(v.Expr, false)
	case *ast.InterpolatedString:
		for _, p := range v.Parts {
			if p.IsExpr {
				cg.walkBoxedRefs(p.Expr, false)
			}
		}
	case *ast.TaggedBlock:
		if v.Body != nil {
			cg.walkBoxedRefs(v.Body, false)
		}
	}
}

// $coro variant predeclaration

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
	envIsClosureLayout := false
	if envStructType != nil && len(envStructType.Fields) == len(captures)+1 && envStructType.Fields[0] == irtypes.I8Ptr {
		envIsClosureLayout = true
	}

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

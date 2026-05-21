package codegen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

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
	// Stacktrace-aware spawn variants (see docs/plans/stacktrace-libunwind.md).
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

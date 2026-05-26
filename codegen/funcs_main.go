package codegen

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) genImplicitMain(stmts []ast.Node) error {
	if bodyContainsSpawnOrAwait(stmts) && len(stmts) > 0 {
		cg.warn(DiagAsyncMain, stmts[0].Pos(),
			"top-level statements use 'spawn' or 'await' but there is no async main(); "+
				"each await at the top level creates a temporary fiber, which is slower and "+
				"bypasses inline channel optimizations. Fix: wrap your code in 'fn{#async} main() = ...' instead")
	}

	f := cg.newCMainWrapper(false)
	entry := f.NewBlock("entry")

	prevFn := cg.curFn
	prevScope := cg.curScope
	cg.curFn = f
	cg.curScope = newScope(cg.curScope)

	// Attach a DISubprogram so lldb/gdb can resolve `main` to the user's
	// source file. The implicit main spans all top-level statements; use the
	// first statement's line as the scope line.
	mainLine := 1
	if len(stmts) > 0 && stmts[0].Pos().Line > 0 {
		mainLine = stmts[0].Pos().Line
	}

	prevDbgScope := cg.diCurrentScope
	cg.emitDbgSubprogramForSynthetic(f, "main", mainLine)

	defer func() { cg.diCurrentScope = prevDbgScope }()

	// Seed currentPos so that preamble instructions (fiber init, var inits)
	// carry the first statement's source line rather than line 0. Without this
	// `br set -n main` in lldb lands on line 0 before the user's code.
	if cg.debugMode && len(stmts) > 0 {
		cg.currentPos = stmts[0].Pos()
	}

	// Emit fiber init if the program uses any fiber features.
	entry = cg.emitFiberMainWrap(entry)

	// Register the deinit dispatcher with libc atexit BEFORE running
	// any user code. See codegen.go's main wrapper for rationale.
	entry = cg.emitDeinitAllAtexit(entry)

	// Register per-type-id any-release helpers so that any-boxed
	// structs run their deinit on scope exit instead of just freeing
	// the heap block.
	entry = cg.emitAnyDispatchRegistrations(entry)

	// Emit top-level var runtime initializations (deferred from pre-pass 1.7).
	var err error

	entry, err = cg.emitTopLevelVarInits(entry)
	if err != nil {
		return err
	}

	cg.emitPkgInitFns(entry)

	// Seed cg.mutatedNames from the union of every top-level statement so
	// the if-condition folder treats reassigned top-level lets as non-const.
	// Without this, `let alive bool = true` followed by `alive = false`
	// later in the implicit main produces phantom "always true" warnings on
	// any `if alive:` between the binding and the first mutation.
	prevMutated := cg.mutatedNames
	cg.mutatedNames = collectMutatedNamesFromStmts(stmts)

	defer func() { cg.mutatedNames = prevMutated }()

	for _, stmt := range stmts {
		entry, _, err = cg.genStmt(entry, stmt)
		if err != nil {
			return err
		}

		if entry == nil {
			break
		}
	}

	if entry != nil && entry.Term == nil {
		_ = cg.emitDefers(entry)
		cg.emitAllScopeReleases(entry, "")
		cg.emitFiberMainEnd(entry)
		entry.NewRet(constant.NewInt(irtypes.I32, 0))
	}

	cg.ensureAllCallsHaveDbg(f)

	cg.curFn = prevFn
	cg.curScope = prevScope

	return nil
}

// genTestRunner generates one __tin_test_N function per TestDecl, plus a
// main() that:
//  1. Initializes top-level var globals (topLevelVarInits).
//  2. Calls _tin_run_test(desc, fn_ptr) for each test.
//  3. Returns the exit code from _tin_test_finish(total_count).
//
// Top-level statements that would form the implicit main are NOT executed;
// only test blocks run.
//
// _tin_run_test and _tin_test_finish are C helpers in runtime.c that use
// setjmp/longjmp to isolate test failures and accumulate pass/fail counts.
func (cg *CodeGen) genTestRunner() error {
	stringType, err := cg.tinTypeToLLVM(&ast.SimpleType{Name: "string"})
	if err != nil {
		return err
	}

	// Declare C runtime helpers.
	// void _tin_run_test(string desc, i8* fn)
	runTestFn := cg.ensureExternDecl("_tin_run_test", irtypes.Void,
		[]*ir.Param{
			ir.NewParam("desc", stringType),
			ir.NewParam("fn", irtypes.I8Ptr),
		}, false)

	// i64 _tin_test_finish(i64 total)
	finishFn := cg.ensureExternDecl("_tin_test_finish", irtypes.I64,
		[]*ir.Param{ir.NewParam("total", irtypes.I64)},
		false)

	// Generate one void function per test.
	testFuncs := make([]*ir.Func, len(cg.testDecls))
	for i, td := range cg.testDecls {
		name := fmt.Sprintf("__tin_test_%d", i)
		fn := cg.mod.NewFunc(name, irtypes.Void)
		entry := fn.NewBlock("entry")

		prevFn := cg.curFn
		prevScope := cg.curScope
		prevCurBlock := cg.curBlock
		prevDeferFnI8s := cg.pendingDeferFnI8s
		prevDeferFrames := cg.pendingDeferFrames
		prevDeferEnvs := cg.pendingDeferEnvs
		prevFnAstBody := cg.curFnAstBody
		prevBorrowSet := cg.currentFnBorrowSet
		prevMovedBindings := cg.movedBindings
		cg.curFn = fn
		cg.curScope = newScope(cg.curScope)
		cg.curBlock = nil
		cg.pendingDeferFnI8s = nil
		cg.pendingDeferFrames = nil
		cg.pendingDeferEnvs = nil
		cg.labelCount = 0
		cg.curFnAstBody = td.Body
		cg.currentFnBorrowSet = cg.analyzeFunctionBorrows(td.Body)
		cg.movedBindings = nil
		prevImplicitMoves := cg.implicitMoveSites
		cg.implicitMoveSites = computeImplicitMoveSites(td.Body)

		prevMutated := cg.mutatedNames
		cg.mutatedNames = collectMutatedNames(td.Body)

		terminated, err := cg.genBody(entry, td.Body, irtypes.Void)
		if err != nil {
			return fmt.Errorf("test %q: %w", td.Desc, err)
		}
		// Ensure the entry block is terminated.
		if !terminated {
			for _, b := range fn.Blocks {
				if b.Term == nil {
					_ = cg.emitDefers(b)
					cg.emitAllScopeReleases(b, "")
					b.NewRet(nil)
				}
			}
		}

		cg.curFn = prevFn
		cg.curScope = prevScope
		cg.curBlock = prevCurBlock
		cg.pendingDeferFnI8s = prevDeferFnI8s
		cg.pendingDeferFrames = prevDeferFrames
		cg.pendingDeferEnvs = prevDeferEnvs
		cg.curFnAstBody = prevFnAstBody
		cg.currentFnBorrowSet = prevBorrowSet
		cg.movedBindings = prevMovedBindings
		cg.implicitMoveSites = prevImplicitMoves
		cg.mutatedNames = prevMutated

		testFuncs[i] = fn
	}

	// Generate main().
	mainFn := cg.newCMainWrapper(false)
	entry := mainFn.NewBlock("entry")

	prevFn := cg.curFn
	prevScope := cg.curScope
	cg.curFn = mainFn
	cg.curScope = newScope(cg.curScope)

	// Initialize fiber runtime (workers + I/O thread) so tests can use spawn/await.
	cur := cg.emitFiberMainWrap(entry)

	// Register the deinit dispatcher with libc atexit BEFORE running
	// any test code (matches the pattern in genImplicitMain / the
	// codegen.go main wrapper).
	cur = cg.emitDeinitAllAtexit(cur)

	// Register per-type-id any-release helpers so that any-boxed
	// structs in tests run their deinit on scope exit.
	cur = cg.emitAnyDispatchRegistrations(cur)

	// Initialize top-level var globals so tests can reference them.
	cur, err = cg.emitTopLevelVarInits(cur)
	if err != nil {
		return err
	}

	cg.emitPkgInitFns(cur)

	// Call _tin_run_test for each test.
	if cur != nil {
		for i, td := range cg.testDecls {
			descVal := cg.buildStringFatPtr(cur, td.Desc)
			fnPtr := cur.NewBitCast(testFuncs[i], irtypes.I8Ptr)
			cg.callExtern(cur, runTestFn, descVal, fnPtr)
		}

		// Drain the run queue and shut down workers.
		cg.emitFiberMainEnd(cur)

		// Release RC-tracked locals (e.g. from topLevelVarInits).
		cg.emitAllScopeReleases(cur, "")

		// Deinit top-level globals: registered with atexit at the top
		// of the test runner main; runs automatically on clean exit.
		// Inline emit removed (was duplicating the atexit hook).

		// Call _tin_test_finish(N) -> i64 exit code.
		total := constant.NewInt(irtypes.I64, int64(len(cg.testDecls)))
		rc64 := cur.NewCall(finishFn, total)
		rc32 := cur.NewTrunc(rc64, irtypes.I32)
		cur.NewRet(rc32)
	}

	cg.curFn = prevFn
	cg.curScope = prevScope

	return nil
}

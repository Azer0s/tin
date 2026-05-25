package codegen

import (
	"fmt"
	"strings"

	"github.com/llir/llvm/ir"
)

func (cg *CodeGen) pushBreakTarget(afterBlock *ir.Block) {
	cg.breakStack = append(cg.breakStack, afterBlock)
	cg.breakUsedStack = append(cg.breakUsedStack, false)
	// Record the scope before the loop body so that break can release
	// all variables declared inside the loop up to (not including) this scope.
	var outerScope *scope
	if cg.curScope != nil {
		outerScope = cg.curScope.parent
	}

	cg.breakScopeStack = append(cg.breakScopeStack, outerScope)
}

// popBreakTarget removes the innermost break target after loop body generation.
// Returns true if any break statement was emitted to this target.
func (cg *CodeGen) popBreakTarget() bool {
	if len(cg.breakStack) == 0 {
		return false
	}

	used := cg.breakUsedStack[len(cg.breakUsedStack)-1]
	cg.breakStack = cg.breakStack[:len(cg.breakStack)-1]
	cg.breakUsedStack = cg.breakUsedStack[:len(cg.breakUsedStack)-1]
	cg.breakScopeStack = cg.breakScopeStack[:len(cg.breakScopeStack)-1]

	return used
}

// currentBreakTarget returns the innermost loop's after-block, or nil if not in a loop.
func (cg *CodeGen) currentBreakTarget() *ir.Block {
	if len(cg.breakStack) == 0 {
		return nil
	}

	return cg.breakStack[len(cg.breakStack)-1]
}

// currentBreakScope returns the scope before the innermost loop body, or nil.
// On break, variables in scopes from cg.curScope up to (not including) this scope
// must be released before branching to the break target.
func (cg *CodeGen) currentBreakScope() *scope {
	if len(cg.breakScopeStack) == 0 {
		return nil
	}

	return cg.breakScopeStack[len(cg.breakScopeStack)-1]
}

// markBreakUsed records that a break was emitted to the current break target.
func (cg *CodeGen) markBreakUsed() {
	if len(cg.breakUsedStack) > 0 {
		cg.breakUsedStack[len(cg.breakUsedStack)-1] = true
	}
}

// pkgStructKey returns the canonical map key / LLVM IR name for a struct named
// "name" that is being compiled under the current package.  When currentPkg is
// set (i.e. we are inside loadPackageFromSource), the returned key is
// "pkgName__name" so that structs from different packages never collide even
// when they share the same short name.  For user-level structs (currentPkg="")
// the bare name is returned unchanged.
func (cg *CodeGen) pkgStructKey(name string) string {
	if cg.currentPkg != "" {
		key := cg.currentPkg + "__" + name
		displayPkg := cg.currentPkgPath

		if displayPkg == "" {
			displayPkg = cg.currentPkg
		}

		cg.recordDisplay(CanonKey(key), displayPkg+"::"+name)

		return key
	}

	return name
}

// newBlock creates a uniquely-named basic block in the current function.
// Sequential if/for/match statements in the same function reuse label base
// names (e.g. "if.merge") which produces duplicate labels in the IR and
// confuses LLVM's loop-deletion pass.  Always routing through this helper
// ensures every block in a function has a distinct name.
func (cg *CodeGen) newBlock(base string) *ir.Block {
	id := cg.labelCount
	cg.labelCount++

	return cg.curFn.NewBlock(fmt.Sprintf("%s.%d", base, id))
}

// SetTestMode enables test-mode compilation: test blocks are compiled into
// test functions and a test-runner main() is generated.
func (cg *CodeGen) SetTestMode(v bool)         { cg.testMode = v }
func (cg *CodeGen) SetNoRuntimeChecks(v bool)  { cg.noRuntimeChecks = v }
func (cg *CodeGen) SetVerboseMatchInfo(v bool) { cg.verboseMatchInfo = v }

// SetExplainOwnership enables the per-binding ownership report.  The
// spec is "*" to print every binding, "fnName" to filter by function
// name, or "file.tin:fnName" to filter by both.  The report is
// emitted to stderr at the end of codegen for the current compilation
// unit.  Driven by the `--explain-ownership[=spec]` CLI flag.
func (cg *CodeGen) SetExplainOwnership(spec string) { cg.explainOwnershipSpec = spec }

// SetNoWarnAsyncMain is the -Wno-async-main hook (kept for back-compat with
// existing callers; new code should use SetWarnSuppress(DiagAsyncMain)).
func (cg *CodeGen) SetNoWarnAsyncMain(v bool) {
	if v {
		cg.SetWarnSuppress(DiagAsyncMain)
	}
}

// SetNoWarnUnusedMatchArms is the -Wno-unused-match-arms hook.
func (cg *CodeGen) SetNoWarnUnusedMatchArms(v bool) {
	if v {
		cg.SetWarnSuppress(DiagUnusedMatchArms)
	}
}

// SetNoWarnBoolAnalysis is the -Wno-bool-analysis hook.
func (cg *CodeGen) SetNoWarnBoolAnalysis(v bool) {
	if v {
		cg.SetWarnSuppress(DiagBoolAnalysis)
	}
}
func (cg *CodeGen) SetVerboseDemorgan(v bool)  { cg.verboseDemorgan = v }
func (cg *CodeGen) SetEmitHeaderPath(p string) { cg.emitHeaderPath = p }
func (cg *CodeGen) SetUseDoubleForF128(v bool) { cg.useDoubleForF128 = v }
func (cg *CodeGen) SetTargetTriple(triple string) {
	if triple != "" {
		cg.mod.TargetTriple = triple
	}
}
func (cg *CodeGen) SetVerboseHeuristics(v bool)                     { cg.verboseHeuristics = v }
func (cg *CodeGen) SetProgressFunc(fn func(string))                 { cg.progressFn = fn }
func (cg *CodeGen) SetTCOReportFunc(fn func(caller, callee string)) { cg.tcoReportFn = fn }

// SetPureFoldDisabled toggles compile-time evaluation of #pure calls.
// When true, both tier-1 (AST evaluator) and tier-2 (cached .so dispatch)
// are short-circuited, and every #pure call codegens as a regular runtime
// invocation. The user-visible behavior of #pure (purity contract,
// alwaysinline, readnone, no_recurse depth limit) is unchanged - only
// the constant-folding optimization is suppressed. Driven by the
// `--no-pure-fold` CLI flag.
func (cg *CodeGen) SetPureFoldDisabled(v bool) { cg.pureFoldDisabled = v }

// SetPureFoldBudget overrides the per-call evaluation budget cap used by
// the CTFE evaluator. Pass 0 to keep the default (defaultPureFoldBudget);
// any negative value is treated as 0. Each call to evalNode consumes one
// unit; the budget is reset at the top-level entry into the evaluator.
// On exhaustion the call falls back to runtime evaluation just as if
// the body weren't foldable.
func (cg *CodeGen) SetPureFoldBudget(n int) {
	if n < 0 {
		n = 0
	}

	cg.pureFoldBudget = n
}

// StacktraceUsed reports whether any reachable call site referenced the
// `stacktrace()` builtin. cmd/tin/main.go consults this after Generate() returns
// to decide whether to link libunwind, emit unwind tables, and pass
// `-rdynamic` (the conditional-emission story in
// docs/plans/stacktrace-libunwind.md). Stable through the rest of the
// build; once set true it stays true.
func (cg *CodeGen) StacktraceUsed() bool { return cg.stacktraceUsed }

// PkgModules returns per-package LLVM modules (excluding cg.mod) in
// deterministic alphabetical name order. Empty when no `use` decls
// loaded any pkg, OR when mergeRoutedPkgMods has folded everything
// back into cg.mod (which happens at the end of Generate today).
//
// cmd/tin/main.go uses this to drive per-pkg .o compilation in parallel; the
// linker then combines them with cg.mod into the final binary.
// Currently returns nil because the merge step still folds everything
// into cg.mod for the legacy single-module compile path; once cmd/tin/main.go
// is wired to compile each module separately, the merge step gets
// removed and this returns the live per-pkg modules.
func (cg *CodeGen) PkgModules() []*ir.Module {
	if len(cg.pkgMods) == 0 {
		return nil
	}

	out := make([]*ir.Module, 0, len(cg.pkgMods))
	for _, name := range cg.pkgModNames() {
		if m := cg.pkgMods[name]; m != nil && hasPkgContent(m) {
			out = append(out, m)
		}
	}

	return out
}

// PkgModuleNames returns the names paired with PkgModules() in the
// same order. Used by the build driver to label per-pkg .ll / .o
// artifacts.
func (cg *CodeGen) PkgModuleNames() []string {
	if len(cg.pkgMods) == 0 {
		return nil
	}

	out := make([]string, 0, len(cg.pkgMods))
	for _, name := range cg.pkgModNames() {
		if m := cg.pkgMods[name]; m != nil && hasPkgContent(m) {
			out = append(out, name)
		}
	}

	return out
}

// hasPkgContent reports whether m carries any IR worth compiling. Pkg
// modules that only declared types (everything DCE'd away) skip the
// per-pkg .o emit so we don't waste a clang invocation on a no-op.
func hasPkgContent(m *ir.Module) bool {
	return len(m.Funcs) > 0 || len(m.Globals) > 0 || len(m.Aliases) > 0
}

// progress fires the optional progress callback with msg.  Callers use it to
// report named pass boundaries, per-function events, imports, CTFE, and macros.
func (cg *CodeGen) progress(msg string) {
	if cg.progressFn != nil {
		cg.progressFn(msg)
	}
}
func (cg *CodeGen) SetDebugMode(v bool) { cg.debugMode = v }

// HasTests reports whether the source contained at least one test block.
// Only meaningful after Generate has been called.
func (cg *CodeGen) HasTests() bool { return len(cg.testDecls) > 0 }

// targetIsAMD64 reports whether the module's target triple is an x86-64 target.
// Used in place of runtime.GOARCH so that cross-compilation works correctly:
// the decision is based on the compilation target, not the host.
func (cg *CodeGen) targetIsAMD64() bool {
	return strings.HasPrefix(cg.mod.TargetTriple, "x86_64")
}

// targetIsARM64 reports whether the module's target triple is an ARM64 target.
func (cg *CodeGen) targetIsARM64() bool {
	return strings.HasPrefix(cg.mod.TargetTriple, "arm64") ||
		strings.HasPrefix(cg.mod.TargetTriple, "aarch64")
}

// newModuleWithTriple creates a new LLVM IR module pre-populated with the
// target triple that clang will actually use, preventing the
// "overriding the module target triple" warning.
//
// clang -dumpmachine (and llc --version) return the darwin-style triple
// (e.g. arm64-apple-darwin25.1.0) but clang normalizes it to the macosx-style
// triple (e.g. arm64-apple-macosx26.0.0) when compiling LLVM IR.  Setting a
// darwin-style triple in the module therefore always triggers the override
// warning.  The only reliable way to get the exact string clang will use is to

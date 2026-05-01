package codegen

// pkgmod.go - foundation for incremental compilation step 2.
//
// Goal: each Tin package eventually owns its own *ir.Module so the per-pkg
// .ll files can be compiled independently and cached. This file introduces
// the routing primitives (pkgMod, echoTypeInActive) without flipping any
// call sites yet. Existing code keeps writing into cg.mod via the existing
// activeMod swap-pointer pattern; once the primitives prove sound, call
// sites get migrated wave by wave (globals -> declares -> bodies -> vtables).
//
// Contract: byte-identical IR text post-split is NOT a goal - the sum of
// per-pkg modules is allowed to differ in section ordering or temp-name
// numbering from the legacy single-module text. The hard contract is a
// bit-identical linked binary across runs (already enforced by the
// determinism test on cg.mod's IR; per-pkg modules will fold into the
// same contract once they participate).

import (
	"fmt"
	"os"
	"sort"

	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"
)

// EntryPkgName is the sentinel package name used to route IR for the
// program's entry compilation unit (the file passed to `tin run`/`tin ir`)
// into a dedicated per-pkg module. Distinct from cg.currentPkg = "" which
// is also overloaded as a struct-naming signal in pkgStructKey, so we keep
// the routing identity orthogonal: routing uses this sentinel; struct
// naming keeps the existing empty-string convention.
const EntryPkgName = "__entry__"

// pkgMod returns the per-package LLVM module for name, lazily creating it
// on first call. The returned module shares cg.mod's target triple and
// data layout so clang can co-link the per-pkg objects into one binary.
//
// Panics on empty name: every call site that routes through pkgMod must
// have already decided which package owns the IR being emitted. Code that
// runs before package context exists (interpreter init, REPL boot) should
// continue to write into cg.mod directly.
func (cg *CodeGen) pkgMod(name string) *ir.Module {
	if name == "" {
		panic("codegen: pkgMod called with empty package name; " +
			"use EntryPkgName for the program entry unit or set " +
			"currentPkg before routing through pkgMod")
	}

	if cg.pkgMods == nil {
		cg.pkgMods = map[string]*ir.Module{}
	}

	if m, ok := cg.pkgMods[name]; ok {
		return m
	}

	m := ir.NewModule()
	m.TargetTriple = cg.mod.TargetTriple
	m.DataLayout = cg.mod.DataLayout
	cg.pkgMods[name] = m

	return m
}

// echoTypeInActive ensures that any named struct type referenced (directly
// or transitively) by t has a TypeDef entry in cg.activeModule(). LLVM
// type identity is per-module: a `%MyStruct` reference in module B is
// only valid if module B itself defines `%MyStruct = type { ... }`.
//
// Idempotent per (target-module, type-name): repeat calls for the same
// name into the same module are no-ops. Anonymous types pass through
// unchanged. Returns t (callers can write `field := cg.echoTypeInActive(t)`
// for self-documenting flow even though identity is preserved).
//
// Currently a stub: foundation phase wires the name and signature; the
// walker that actually copies named StructType bodies across modules is
// added when the first per-pkg routing wave lands. Calling it today is
// safe but does no work, because every active module is still cg.mod.
func (cg *CodeGen) echoTypeInActive(t irtypes.Type) irtypes.Type {
	target := cg.activeModule()
	if target == cg.mod {
		return t
	}

	st, ok := t.(*irtypes.StructType)
	if !ok || st.Name() == "" {
		return t
	}

	if cg.echoedTypes == nil {
		cg.echoedTypes = map[*ir.Module]map[string]bool{}
	}

	perMod, ok := cg.echoedTypes[target]
	if !ok {
		perMod = map[string]bool{}
		cg.echoedTypes[target] = perMod
	}

	if perMod[st.Name()] {
		return t
	}

	perMod[st.Name()] = true
	target.TypeDefs = append(target.TypeDefs, st)

	return t
}

// pkgModNames returns the package names that have an emitted per-pkg
// module, sorted for deterministic linker / driver invocation. Used by
// the parallel-compile driver once routing is live.
func (cg *CodeGen) pkgModNames() []string {
	out := make([]string, 0, len(cg.pkgMods))
	for n := range cg.pkgMods {
		out = append(out, n)
	}

	sort.Strings(out)

	return out
}

// debugPkgMods is a developer-only formatter for inspecting the current
// per-pkg module map (used from log lines when bisecting routing bugs).
func (cg *CodeGen) debugPkgMods() string {
	if len(cg.pkgMods) == 0 {
		return "<no per-pkg modules>"
	}

	out := ""
	for _, n := range cg.pkgModNames() {
		out += fmt.Sprintf("  %s: %d funcs, %d globals, %d typedefs\n",
			n, len(cg.pkgMods[n].Funcs), len(cg.pkgMods[n].Globals),
			len(cg.pkgMods[n].TypeDefs))
	}

	return out
}

// allFuncs iterates every function defined by codegen so far across
// cg.mod and every per-pkg module. Used by passes that need to walk
// "every fn in the program" - e.g. coro variant emission, the
// stacktrace post-pass, the user-main / has-main lookup. Today this is
// equivalent to iterating cg.mod.Funcs (no call site routes to per-pkg
// modules yet); after the routing flip lands, callers that use this
// helper continue to see all fns regardless of which module they live
// in.
//
// Iteration order: cg.mod.Funcs first (deterministic), then per-pkg
// modules in alphabetical pkg-name order. Order matters for
// determinism (the byte-identical IR contract).
func (cg *CodeGen) allFuncs() []*ir.Func {
	if len(cg.pkgMods) == 0 {
		return cg.mod.Funcs
	}

	out := make([]*ir.Func, 0, len(cg.mod.Funcs)+16)
	out = append(out, cg.mod.Funcs...)

	for _, name := range cg.pkgModNames() {
		m := cg.pkgMods[name]
		if m == nil {
			continue
		}

		out = append(out, m.Funcs...)
	}

	return out
}

// debugDumpUnterminated prints names of fns whose blocks lack
// terminators across cg.mod and per-pkg modules. Used to bisect
// per-pkg routing bugs that surface as llir/llvm serialization
// panics ("missing terminator in basic block").
//goland:noinspection GoUnusedFunction
func (cg *CodeGen) debugDumpUnterminated() {
	check := func(prefix string, m *ir.Module) {
		for _, f := range m.Funcs {
			for _, bb := range f.Blocks {
				if bb.Term == nil {
					fmt.Fprintf(os.Stderr, "[unterminated] mod=%s fn=%s bb=%s insts=%d\n",
						prefix, f.Name(), bb.Name(), len(bb.Insts))
				}
			}
		}
	}

	check("cg.mod", cg.mod)
	for _, name := range cg.pkgModNames() {
		check("pkg:"+name, cg.pkgMods[name])
	}
}

// mergeRoutedPkgMods folds every per-pkg module's content back into
// cg.mod so the existing single-module serialization path keeps working
// while we migrate call sites off cg.mod one wave at a time.
//
// Each pkg module's funcs / globals / typedefs / aliases are appended
// to cg.mod's slices; pkg-mod IR objects continue to point to their
// original parent (an llir/llvm Func's Parent field), but llir/llvm's
// LLString walks cg.mod's slices directly, so the serialized output is
// the union as if everything had been emitted into cg.mod from the
// start. This is a transient bridge - once every emit site routes
// through activeModule() and the build pipeline compiles per-pkg .o
// files separately, this merge goes away and pkg modules feed clang
// directly.
//
// Idempotent: pkg modules that get merged once are cleared so a second
// call (e.g. test mode + REPL mode entering Generate's exit branches in
// turn) doesn't double-append.
func (cg *CodeGen) mergeRoutedPkgMods() {
	if len(cg.pkgMods) == 0 {
		return
	}

	for _, name := range cg.pkgModNames() {
		m := cg.pkgMods[name]
		if m == nil {
			continue
		}

		cg.mod.Funcs = append(cg.mod.Funcs, m.Funcs...)
		cg.mod.Globals = append(cg.mod.Globals, m.Globals...)
		cg.mod.TypeDefs = append(cg.mod.TypeDefs, m.TypeDefs...)
		cg.mod.Aliases = append(cg.mod.Aliases, m.Aliases...)

		m.Funcs = nil
		m.Globals = nil
		m.TypeDefs = nil
		m.Aliases = nil
	}
}

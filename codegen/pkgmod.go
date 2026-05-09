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
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
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
	// Stable SourceFilename so opt-emitted bitcode is reproducible
	// across builds; otherwise the random temp .ll path leaks into
	// the binary's symbol table.
	m.SourceFilename = "tin/pkg:" + name
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
//
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

// addCrossModuleDeclares walks each per-pkg module's IR and adds
// `declare`-style stubs for any function or global referenced from
// inside that module but defined in a different module. Required for
// per-pkg .o compile: without an explicit declare in the consumer's
// IR, the LLVM verifier rejects the cross-module reference.
//
// Symbols searched: every operand of every instruction (call callee,
// fn pointer, GEP base, load/store ptr) plus every global initializer
// (vtable globals reference fn pointers via constant exprs). For each
// operand pointing to a fn / global that lives in a different module,
// emit a matching declare/extern global into the consumer module.
// Idempotent per (consumer, symbol) via a per-module declared-set.
//
// This is the prerequisite for stripping mergeRoutedPkgMods: once
// every per-pkg module is self-sufficient (declares everything it
// references but doesn't define), main.go can serialize each pkg
// module to its own .ll and compile in parallel.
func (cg *CodeGen) addCrossModuleDeclares() {
	if len(cg.pkgMods) == 0 && len(cg.monoMods) == 0 {
		return
	}

	// Build a global-pointer -> owning-module index by walking every
	// module's Globals once. *ir.Global has no Parent field (unlike
	// *ir.Func), so module ownership has to be reconstructed from the
	// containers. Mono modules (step 5) participate alongside per-pkg
	// modules.
	globalOwner := map[*ir.Global]*ir.Module{}
	for _, g := range cg.mod.Globals {
		globalOwner[g] = cg.mod
	}

	for _, name := range cg.pkgModNames() {
		m := cg.pkgMods[name]
		if m == nil {
			continue
		}

		for _, g := range m.Globals {
			globalOwner[g] = m
		}
	}

	for _, m := range cg.monoMods {
		for _, g := range m.Globals {
			globalOwner[g] = m
		}
	}

	// Process cg.mod too - entry-program main / runtime helpers
	// frequently reference per-pkg fns (e.g. `assert::not_ok` from
	// inside a test body that codegen emitted directly into cg.mod).
	cg.addCrossModuleDeclaresFor(cg.mod, globalOwner)

	for _, name := range cg.pkgModNames() {
		m := cg.pkgMods[name]
		if m == nil {
			continue
		}

		cg.addCrossModuleDeclaresFor(m, globalOwner)
	}

	// Mono modules need cross-module declares too: their relocated fn
	// bodies still reference strings, vtables, and other helpers that
	// live in the original pkg modules.
	for _, m := range cg.monoMods {
		cg.addCrossModuleDeclaresFor(m, globalOwner)
	}
}

// addCrossModuleDeclaresFor processes a single consumer module.
// declaredFuncs / declaredGlobals track which extern symbols already
// have a declare in this module so repeated references don't append
// duplicates.
func (cg *CodeGen) addCrossModuleDeclaresFor(m *ir.Module, globalOwner map[*ir.Global]*ir.Module) {
	declaredFuncs := map[string]bool{}
	for _, f := range m.Funcs {
		declaredFuncs[f.Name()] = true
	}

	declaredGlobals := map[string]bool{}
	for _, g := range m.Globals {
		declaredGlobals[g.Name()] = true
	}

	// Helper: ensure decl exists for an external function reference.
	declareFunc := func(extFn *ir.Func) {
		if extFn == nil || extFn.Parent == m || declaredFuncs[extFn.Name()] {
			return
		}

		// LLVM requires distinct param names within a function (or all
		// empty / numeric defaults). Anonymous params from intrinsics
		// (e.g. @llvm.coro.id) all serialize as "0" via p.Name(), which
		// collide on declare; synthesize unique p<i> names instead so
		// the declare verifies.
		used := map[string]bool{}
		params := make([]*ir.Param, len(extFn.Params))

		for i, p := range extFn.Params {
			name := p.Name()
			if name == "" || used[name] {
				name = fmt.Sprintf("p%d", i)
			}

			used[name] = true
			params[i] = ir.NewParam(name, p.Type())
		}

		decl := m.NewFunc(extFn.Name(), extFn.Sig.RetType, params...)
		decl.Sig.Variadic = extFn.Sig.Variadic
		decl.Blocks = nil // declare-only
		declaredFuncs[extFn.Name()] = true
	}

	// Helper: ensure decl exists for an external global reference.
	declareGlobal := func(extG *ir.Global) {
		if extG == nil || globalOwner[extG] == m || declaredGlobals[extG.Name()] {
			return
		}

		// Internal / private globals in the source module are STB_LOCAL
		// and don't cross object boundaries; cross-module references
		// would fail to link. Promote the source linkage to weak_odr
		// so the symbol is exported by the source .o, then emit an
		// external declare in the consumer.
		if extG.Linkage == enum.LinkageInternal ||
			extG.Linkage == enum.LinkagePrivate ||
			extG.Linkage == enum.LinkageNone {
			extG.Linkage = enum.LinkageWeakODR
		}

		decl := m.NewGlobal(extG.Name(), extG.ContentType)
		decl.Linkage = enum.LinkageExternal
		decl.Immutable = extG.Immutable
		declaredGlobals[extG.Name()] = true
	}

	// Walk every instruction in every block of every fn defined in m.
	for _, f := range m.Funcs {
		for _, bb := range f.Blocks {
			for _, inst := range bb.Insts {
				walkInstOperands(inst, declareFunc, declareGlobal)
			}

			if bb.Term != nil {
				walkTermOperands(bb.Term, declareFunc, declareGlobal)
			}
		}
	}

	// Also walk global initializers - vtable globals reference fn
	// pointers via constant expressions.
	for _, g := range m.Globals {
		if g.Init != nil {
			walkConstant(g.Init, declareFunc, declareGlobal)
		}
	}
}

// walkInstOperands inspects every operand of inst and calls the
// appropriate declare callback for any *ir.Func or *ir.Global it sees.
// Operands include the call callee, GEP base, load/store ptr, etc.
func walkInstOperands(inst ir.Instruction, df func(*ir.Func), dg func(*ir.Global)) {
	for _, op := range inst.Operands() {
		walkValue(*op, df, dg)
	}
}

// walkTermOperands does the same for terminators (br, condbr, ret).
func walkTermOperands(term ir.Terminator, df func(*ir.Func), dg func(*ir.Global)) {
	for _, op := range term.Operands() {
		walkValue(*op, df, dg)
	}
}

// walkValue dispatches on a value's runtime type, recursing into
// constant expressions to find embedded fn / global references.
func walkValue(v value.Value, df func(*ir.Func), dg func(*ir.Global)) {
	switch x := v.(type) {
	case *ir.Func:
		df(x)
	case *ir.Global:
		dg(x)
	case constant.Constant:
		walkConstant(x, df, dg)
	}
}

// walkConstant unwraps constant expressions (bitcast, GEP, ptrtoint,
// trunc, sub, struct, array, blockaddress) to find leaf fn / global
// references inside.
func walkConstant(c constant.Constant, df func(*ir.Func), dg func(*ir.Global)) {
	switch x := c.(type) {
	case *ir.Func:
		df(x)
	case *ir.Global:
		dg(x)
	case *constant.ExprBitCast:
		walkConstant(x.From, df, dg)
	case *constant.ExprGetElementPtr:
		walkConstant(x.Src, df, dg)

		for _, idx := range x.Indices {
			walkConstant(idx, df, dg)
		}
	case *constant.ExprPtrToInt:
		walkConstant(x.From, df, dg)
	case *constant.ExprIntToPtr:
		walkConstant(x.From, df, dg)
	case *constant.ExprTrunc:
		walkConstant(x.From, df, dg)
	case *constant.ExprSub:
		walkConstant(x.X, df, dg)
		walkConstant(x.Y, df, dg)
	case *constant.Struct:
		for _, field := range x.Fields {
			walkConstant(field, df, dg)
		}
	case *constant.Array:
		for _, elem := range x.Elems {
			walkConstant(elem, df, dg)
		}
	case *constant.BlockAddress:
		// blockaddress's Func is a Constant (the parent fn).
		if fn, ok := x.Func.(*ir.Func); ok {
			df(fn)
		}
	}
}

// echoSharedTypeDefs copies cg.mod.TypeDefs into every per-pkg module.
// LLVM type identity is per-module: a `%MyStruct*` in pkg B's IR is
// only meaningful if B's TypeDefs also defines `%MyStruct`. Without
// the echo, the assembler errors on unresolved type references when
// each pkg compiles separately.
//
// Over-copying (every pkg gets every type, even ones it doesn't use)
// is safe: typedefs are zero-cost at runtime, the linker dedups
// identical layouts, and dead-typedef pruning is a clang concern not
// ours. The cost is a few extra bytes in each .ll text, dwarfed by
// the actual code.
//
// Idempotent per (pkg-module, typedef): repeat calls don't add
// duplicates because the per-pkg dedup set is rebuilt each time.
func (cg *CodeGen) echoSharedTypeDefs() {
	if len(cg.mod.TypeDefs) == 0 {
		return
	}

	if len(cg.pkgMods) == 0 && len(cg.monoMods) == 0 {
		return
	}

	echo := func(m *ir.Module) {
		if m == nil {
			return
		}

		have := map[irtypes.Type]bool{}
		for _, t := range m.TypeDefs {
			have[t] = true
		}

		for _, t := range cg.mod.TypeDefs {
			if !have[t] {
				m.TypeDefs = append(m.TypeDefs, t)
				have[t] = true
			}
		}
	}

	for _, name := range cg.pkgModNames() {
		echo(cg.pkgMods[name])
	}

	for _, m := range cg.monoMods {
		echo(m)
	}
}

// finalizePerPkgModules prepares every per-pkg module for independent
// compilation: adds cross-module declares for any fn/global referenced
// from a pkg module but defined elsewhere, then echoes the shared
// TypeDefs so each pkg module is type-self-sufficient.
//
// Replaces mergeRoutedPkgMods on the parallel-compile path. After
// this runs, each per-pkg module is a self-contained LLVM IR module
// that can be serialized and compiled to its own `.o`.
func (cg *CodeGen) finalizePerPkgModules() {
	if len(cg.pkgMods) == 0 {
		return
	}

	cg.echoSharedTypeDefs()
	cg.addCrossModuleDeclares()
}

// mergePkgModsIntoMain folds every per-pkg LLVM module's funcs, globals,
// and typedefs back into cg.mod, then empties the pkgMods + monoMods
// maps.  Used by the REPL entry-point return path: the REPL has no
// separate compile step that would link per-pkg .o files into the cell
// .so, so every definition has to live in cg.mod.
//
// Dedup-on-name-conflict policy: when both modules define a symbol
// with the same name, prefer the entry that has a body (definition)
// over an empty-blocks declare.  A naive name-only skip would silently
// keep the declare and drop the define, producing dlopen
// "symbol not found" errors when the cell calls the symbol.
//
// Determinism: pkgMods are walked in alphabetical name order
// (pkgModNames is sorted) and monoMods in alphabetical hash order, so
// the resulting cg.mod ordering is reproducible across runs.
//
// Per-module @llvm.used rekey: emitLlvmUsedRoots, which runs AFTER
// this merge, would otherwise see leftover pkgMod entries in
// cg.llvmUsedRoots/Funcs and emit a per-pkg @llvm.used global into a
// module that is about to be discarded.  We rekey those entries from
// the pkgMod/monoMod pointer to cg.mod so the subsequent emit produces
// a single combined @llvm.used in cg.mod with every pin entry.
func (cg *CodeGen) mergePkgModsIntoMain() {
	if len(cg.pkgMods) == 0 && len(cg.monoMods) == 0 {
		return
	}

	knownTypes := map[string]*irtypes.StructType{}

	for _, t := range cg.mod.TypeDefs {
		if st, ok := t.(*irtypes.StructType); ok && st.Name() != "" {
			knownTypes[st.Name()] = st
		}
	}

	knownFuncs := map[string]*ir.Func{}

	for _, f := range cg.mod.Funcs {
		if f.Name() != "" {
			knownFuncs[f.Name()] = f
		}
	}

	knownGlobals := map[string]*ir.Global{}

	for _, g := range cg.mod.Globals {
		if g.Name() != "" {
			knownGlobals[g.Name()] = g
		}
	}

	mergeFrom := func(m *ir.Module) {
		if m == nil {
			return
		}

		for _, t := range m.TypeDefs {
			st, ok := t.(*irtypes.StructType)
			if !ok || st.Name() == "" {
				cg.mod.TypeDefs = append(cg.mod.TypeDefs, t)

				continue
			}

			if _, dup := knownTypes[st.Name()]; dup {
				continue
			}

			cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)
			knownTypes[st.Name()] = st
		}

		for _, g := range m.Globals {
			name := g.Name()
			if name == "" {
				cg.mod.Globals = append(cg.mod.Globals, g)

				continue
			}

			existing, dup := knownGlobals[name]
			if !dup {
				cg.mod.Globals = append(cg.mod.Globals, g)
				knownGlobals[name] = g

				continue
			}
			// Prefer the entry with an initializer (definition) over an
			// extern-only declare.  Identical-name globals between
			// cg.mod and a pkgMod are normal: cg.mod often holds a
			// declare for a runtime helper that ends up defined inside
			// a pkg module.
			if existing.Init == nil && g.Init != nil {
				for i, eg := range cg.mod.Globals {
					if eg == existing {
						cg.mod.Globals[i] = g

						break
					}
				}

				knownGlobals[name] = g
			}
		}

		for _, f := range m.Funcs {
			name := f.Name()
			if name == "" {
				cg.mod.Funcs = append(cg.mod.Funcs, f)

				continue
			}

			existing, dup := knownFuncs[name]
			if !dup {
				cg.mod.Funcs = append(cg.mod.Funcs, f)
				knownFuncs[name] = f

				continue
			}
			// Prefer the entry that has blocks (definition) over an
			// empty-blocks declare.  Same pattern as globals: cg.mod
			// frequently holds a declare for a symbol that turns out
			// to be defined in a pkg module.
			if len(existing.Blocks) == 0 && len(f.Blocks) > 0 {
				for i, ef := range cg.mod.Funcs {
					if ef == existing {
						cg.mod.Funcs[i] = f

						break
					}
				}

				knownFuncs[name] = f
			}
		}
	}

	rekeyUsed := func(m *ir.Module) {
		if m == nil {
			return
		}

		if entries, ok := cg.llvmUsedRoots[m]; ok {
			seen := map[*ir.Global]bool{}
			for _, g := range cg.llvmUsedRoots[cg.mod] {
				seen[g] = true
			}

			for _, g := range entries {
				if !seen[g] {
					cg.llvmUsedRoots[cg.mod] = append(cg.llvmUsedRoots[cg.mod], g)
					seen[g] = true
				}
			}

			delete(cg.llvmUsedRoots, m)
		}

		if entries, ok := cg.llvmUsedFuncs[m]; ok {
			seen := map[*ir.Func]bool{}
			for _, f := range cg.llvmUsedFuncs[cg.mod] {
				seen[f] = true
			}

			for _, f := range entries {
				if !seen[f] {
					cg.llvmUsedFuncs[cg.mod] = append(cg.llvmUsedFuncs[cg.mod], f)
					seen[f] = true
				}
			}

			delete(cg.llvmUsedFuncs, m)
		}
	}

	for _, name := range cg.pkgModNames() {
		m := cg.pkgMods[name]
		mergeFrom(m)
		rekeyUsed(m)
	}

	cg.pkgMods = map[string]*ir.Module{}

	monoHashes := make([]string, 0, len(cg.monoMods))
	for h := range cg.monoMods {
		monoHashes = append(monoHashes, h)
	}

	sort.Strings(monoHashes)

	for _, h := range monoHashes {
		m := cg.monoMods[h]
		mergeFrom(m)
		rekeyUsed(m)
	}

	cg.monoMods = map[string]*ir.Module{}
}

package codegen

// ctfe_emit.go - text-level slicer that pulls a single #pure function out of
// the whole-program LLVM IR text and emits a self-contained sub-module the
// linker can compile to its own .so. See ctfe_cache.go for the cache layout.
//
// Strategy: stream the program IR line by line. Pass through types, globals,
// and pre-existing `declare` lines; convert every `define` block that is NOT
// the target into a `declare` (signature only, no body); emit the target's
// `define` block intact. The result is a valid stand-alone module that
// references its callees as external symbols, ready to be resolved at
// dlopen time.
//
// Each per-fn slice contains both the original Tin function (with its
// internal Tin-ABI) and a parallel `__tin_pure_shim_<name>` C-callable
// wrapper produced by emitPureFnCtfeShims. dlsym hits the shim, which
// reuses the existing #interop marshal helpers (tin_interop_str_in/out,
// tin_interop_slice_in/out, bool widening) before delegating to the
// internal entry — the same path the user-tagged #interop pipeline uses.

import (
	"fmt"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"

	"github.com/Azer0s/tin/ast"
)

// activeModule returns the LLVM module that should receive new IR objects.
// Outside CTFE shim emission this is always cg.mod (the user binary).
// During emitPureFnCtfeShims it is swapped to cg.shimMod so the shims and
// every supporting `declare` they emit land there instead, leaving cg.mod
// unaware of the CTFE pipeline.
func (cg *CodeGen) activeModule() *ir.Module {
	if cg.activeMod != nil {
		return cg.activeMod
	}

	return cg.mod
}

// ensureShimMod lazily creates the CTFE shim module. Same target triple as
// cg.mod so clang can compile both halves of a per-fn .so together.
func (cg *CodeGen) ensureShimMod() *ir.Module {
	if cg.shimMod == nil {
		cg.shimMod = ir.NewModule()
		cg.shimMod.TargetTriple = cg.mod.TargetTriple
		cg.shimMod.DataLayout = cg.mod.DataLayout
	}

	return cg.shimMod
}

// declareInActive returns a `declare` of src's signature inside the active
// module. Used by the CTFE shim emit when the wrapper (in cg.shimMod) needs
// to reference a function whose definition lives in cg.mod. Subsequent
// calls for the same name in the same active module return the cached
// declare so we never emit duplicates.
func (cg *CodeGen) declareInActive(src *ir.Func) *ir.Func {
	target := cg.activeModule()
	if target == src.Parent {
		return src
	}

	if cg.runtimeHelperCache == nil {
		cg.runtimeHelperCache = map[*ir.Module]map[string]*ir.Func{}
	}

	perMod, ok := cg.runtimeHelperCache[target]
	if !ok {
		perMod = map[string]*ir.Func{}
		cg.runtimeHelperCache[target] = perMod
	}

	if f, ok := perMod[src.Name()]; ok {
		return f
	}

	params := make([]*ir.Param, len(src.Params))
	for i, p := range src.Params {
		params[i] = ir.NewParam(p.Name(), p.Type())
	}

	f := target.NewFunc(src.Name(), src.Sig.RetType, params...)
	perMod[src.Name()] = f

	return f
}

// ensureRuntimeHelper looks up (or declares) a runtime-helper function in
// the active module. Each target module gets its own declare so the wrapper
// body's call-sites resolve to a function in the SAME module. Replaces the
// scope-cached single-module pattern that used to live in interop.go's
// ensureXxx helpers.
func (cg *CodeGen) ensureRuntimeHelper(name string, retType irtypes.Type, params ...*ir.Param) *ir.Func {
	target := cg.activeModule()

	if cg.runtimeHelperCache == nil {
		cg.runtimeHelperCache = map[*ir.Module]map[string]*ir.Func{}
	}

	perMod, ok := cg.runtimeHelperCache[target]
	if !ok {
		perMod = map[string]*ir.Func{}
		cg.runtimeHelperCache[target] = perMod
	}

	if f, ok := perMod[name]; ok {
		return f
	}

	f := target.NewFunc(name, retType, params...)
	perMod[name] = f

	return f
}

// pureFnShimPrefix names the auto-generated #interop-style wrapper attached
// to every wrappable #pure function for CTFE dispatch. Symbol pattern:
// `__tin_pure_shim_<fn_name>`. Distinct from the user-facing #interop
// wrapper symbol so we never collide with the original function name.
const pureFnShimPrefix = "__tin_pure_shim_"

// pureFnShimName returns the deterministic dispatch symbol for fn.
func pureFnShimName(fnName string) string { return pureFnShimPrefix + fnName }

// PureFnShimName is the public alias of pureFnShimName for callers outside
// the codegen package (main.go's cache emitter). Centralising this avoids
// the `__tin_pure_shim_<name>` literal getting duplicated across files
// where one rename would silently desynchronise the cache lookup against
// the emitted symbol.
func PureFnShimName(fnName string) string { return pureFnShimName(fnName) }

// emitPureFnCtfeShims walks every #pure function declared in the program
// and, when its signature passes the #interop validator, emits a
// `__tin_pure_shim_<name>` wrapper alongside it. The shim re-uses
// emitInteropWrapperWithName so the marshal logic stays in one place.
//
// In the main binary the shim has internal linkage (clang DCEs it as
// dead code at -O2 because nothing in main references the shim symbol);
// in the per-fn .so cache slice the slicer promotes it to external so
// dlsym can find it.
func (cg *CodeGen) emitPureFnCtfeShims() error {
	if cg.pureFnShims == nil {
		cg.pureFnShims = map[string]bool{}
	}

	// Route every NewFunc/NewGlobal inside emitInteropWrapperWithName and
	// the ensureXxx helpers into a sibling shim module instead of cg.mod.
	cg.activeMod = cg.ensureShimMod()
	defer func() { cg.activeMod = nil }()

	for name, fd := range cg.funcDecls {
		if !hasTag(fd.Tags, "pure") {
			continue
		}
		// Methods are surfaced under names like "Struct_method"; the
		// #interop pipeline rejects struct methods (v1) so we follow
		// suit and skip them here too.
		if strings.Contains(name, "_") && fd.TraitQualifier != "" {
			continue
		}
		// User already tagged it #interop — wrapper already emitted by
		// emitInteropWrappers under the bare name; reuse THAT one.
		if hasTag(fd.Tags, "interop") {
			cg.pureFnShims[name] = true

			continue
		}
		// Skip generic / async / extern fns — validateInteropFunc would
		// reject them anyway. Cheap pre-checks let us avoid building the
		// wrapper for impossible cases.
		if len(fd.TypeParams) > 0 || hasTag(fd.Tags, "async") || fd.IsExtern != "" {
			continue
		}
		// Run the full #interop validator to gate types we can actually
		// wrap. Failures are non-fatal — the fn just doesn't get a CTFE
		// shim, and the dispatch path silently falls back to AST eval.
		if err := cg.validateInteropFunc(fd); err != nil {
			continue
		}

		shimName := pureFnShimName(name)
		if err := cg.emitInteropWrapperWithName(fd, shimName); err != nil {
			return fmt.Errorf("ctfe shim for %q: %w", name, err)
		}
		// Shim lives in cg.shimMod (we routed activeMod above); the
		// sibling module is compiled into the per-fn .so. The slicer
		// promotes the shim's linkage to external so dlsym resolves it.
		for _, f := range cg.shimMod.Funcs {
			if f.Name() == shimName {
				f.Linkage = enum.LinkageInternal
				break
			}
		}

		cg.pureFnShims[name] = true
	}

	return nil
}

// PureFnArtefact is one slice of the compilation output ready to be cached
// and dispatched independently. Name is the user-visible function name (the
// dlsym symbol exported by the .so); Hash is the Merkle key
// (.build/pure-fn/<Hash>/bin.so); IRText is the self-contained sub-module
// .ll text the linker compiles.
//
// The function is exported with its native C-ABI signature — the same one
// the #interop wrapper machinery would produce for primitive params and
// returns. Callers dispatch via cgo shape entries (see ctfe_dispatch.go);
// non-primitive args/returns will eventually route through the full
// emitInteropWrapperFor pipeline (string, slice, bool widening) instead of
// reinventing marshalling here.
type PureFnArtefact struct {
	Name   string
	Hash   string
	IRText string
}

// PureFnsForCache walks every #pure function declared in the program and
// returns one PureFnArtefact per fn. Fns whose Merkle hash cannot be computed
// (generic, references unresolvable callees) are skipped silently — the AST
// evaluator + main binary already cover the same fold path.
//
// The cache slice combines two source-of-truth modules:
//   - cg.mod:     definition of the original Tin internal entry, plus
//                 every type/global/declare it transitively references
//   - cg.shimMod: the `__tin_pure_shim_<name>` wrapper produced by
//                 emitPureFnCtfeShims, with its own redeclares of
//                 runtime helpers and the internal-entry forward decl
//
// The result is a self-contained .ll the linker compiles into one .so.
func (cg *CodeGen) PureFnsForCache() []PureFnArtefact {
	if cg.mod == nil {
		return nil
	}

	mainText := cg.mod.String()

	shimText := ""
	if cg.shimMod != nil {
		shimText = cg.shimMod.String()
	}

	var out []PureFnArtefact

	for name, fd := range cg.funcDecls {
		if !hasTag(fd.Tags, "pure") {
			continue
		}
		// Generic functions cannot be sliced — their LLVM IR is per-instantiation.
		if len(fd.TypeParams) > 0 {
			continue
		}

		hash := cg.ctfeFnHash(fd)
		if hash == "" {
			continue
		}

		shimName := pureFnShimName(name)
		if !cg.pureFnShims[name] {
			continue
		}

		// Slice the internal Tin entry out of cg.mod (plus everything
		// reachable). The shim itself is in cg.shimMod, NOT cg.mod, so
		// the slicer never sees it from this side.
		mainSlice := sliceIRForFuncs(mainText, []string{name})
		if mainSlice == "" {
			continue
		}

		// Slice the shim out of shimMod. Because the shim's `define`
		// references the internal entry via a forward declare we already
		// emitted into shimMod (see declareInActive in emitInteropWrapperWithName),
		// the slicer keeps that declare too.
		shimSlice := sliceIRForFuncs(shimText, []string{shimName})
		if shimSlice == "" {
			continue
		}

		// Combine. The forward declare for the internal entry that
		// shimSlice carries collides with mainSlice's `define` for the
		// same symbol — strip the duplicate `declare` line. Same for
		// any runtime-helper declares both halves emit independently.
		ir := mergeSliceModules(mainSlice, shimSlice)
		ir = promoteSymbolToExternal(ir, shimName)

		out = append(out, PureFnArtefact{
			Name:   name,
			Hash:   hash,
			IRText: ir,
		})
	}

	return out
}

// mergeSliceModules concatenates two LLVM IR module texts into a single
// valid .ll. Drops duplicate target triple / datalayout headers, and any
// `declare` line whose symbol is already defined OR previously declared in
// either half (LLVM rejects two declares for the same symbol when their
// attribute lists differ even slightly).
//
// When the two halves disagree on the signature/attribute text of a
// duplicate declare we panic instead of silently picking the first: this
// is an internal-invariant violation (both halves come from the same
// wrapper-emit machinery, so they MUST emit identical declares for shared
// runtime helpers) and silent divergence here can produce a .so whose
// caller and callee disagree on calling convention.
func mergeSliceModules(mainSlice, shimSlice string) string {
	// Collect every symbol that already has a `define` in either half;
	// shim-side declares for those names are pure duplicates.
	defined := map[string]bool{}

	for _, line := range strings.Split(mainSlice+"\n"+shimSlice, "\n") {
		if strings.HasPrefix(line, "define ") {
			if name := extractDefineName(line); name != "" {
				defined[name] = true
			}
		}
	}

	var b strings.Builder

	// Track the full text of declares we've emitted so a second declare
	// for the same symbol can be compared against the first instead of
	// dropped blindly.
	declared := map[string]string{}

	emit := func(line string) {
		trimmed := strings.TrimSpace(line)

		switch {
		case strings.HasPrefix(trimmed, "target triple"),
			strings.HasPrefix(trimmed, "target datalayout"):
			if b.Len() == 0 {
				b.WriteString(line)
				b.WriteByte('\n')
			}

			return
		case strings.HasPrefix(trimmed, "declare "):
			name := extractDeclareName(line)
			if name != "" {
				if defined[name] {
					return
				}

				if prev, seen := declared[name]; seen {
					if normaliseDeclare(prev) != normaliseDeclare(line) {
						panic(fmt.Sprintf("ctfe shim merge: divergent declares for %s\n  first:  %s\n  second: %s",
							name, strings.TrimSpace(prev), strings.TrimSpace(line)))
					}

					return
				}

				declared[name] = line
			}
		}

		b.WriteString(line)
		b.WriteByte('\n')
	}

	for _, line := range strings.Split(mainSlice, "\n") {
		emit(line)
	}

	for _, line := range strings.Split(shimSlice, "\n") {
		emit(line)
	}

	return b.String()
}

// normaliseDeclare returns a comparable form of an LLVM declare line by
// collapsing whitespace runs AND truncating at the closing paren of the
// argument list. Attribute lists (`alwaysinline readnone nounwind` etc.)
// after that paren are advisory annotations the IR builder attaches to
// some sides of an emit but not others — they don't affect calling
// convention or ABI, so an attribute-only divergence is NOT a sign of a
// bug. By contrast, a divergence in return type or arg types between
// the two halves WOULD produce a wrong .so, and the panic surrounding
// this comparison catches that.
func normaliseDeclare(line string) string {
	trimmed := strings.TrimSpace(line)

	if rparen := strings.LastIndexByte(trimmed, ')'); rparen >= 0 {
		trimmed = trimmed[:rparen+1]
	}

	var b strings.Builder

	prevSpace := false

	for _, r := range trimmed {
		if r == ' ' || r == '\t' {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}

			continue
		}

		prevSpace = false

		b.WriteRune(r)
	}

	return b.String()
}

// extractDeclareName pulls the symbol name out of a `declare` line such as
//
//	declare i64 @foo(i64) alwaysinline
func extractDeclareName(line string) string {
	at := strings.Index(line, " @")
	if at < 0 {
		return ""
	}

	rest := line[at+2:]

	end := strings.IndexAny(rest, "( \t")
	if end < 0 {
		return ""
	}

	return strings.TrimSpace(rest[:end])
}

// sliceIRForFuncs is sliceIRForFunc generalized to keep multiple functions'
// definitions intact while still converting every other `define` block to a
// `declare`. Used by the per-fn .so emit to keep the original Tin entry
// alongside its `__tin_pure_shim_<name>` wrapper.
func sliceIRForFuncs(fullIR string, targets []string) string {
	keep := make(map[string]bool, len(targets))
	for _, t := range targets {
		keep[t] = true
	}

	var (
		out         strings.Builder
		inDefine    bool
		isTarget    bool
		curBody     strings.Builder
		curSig      string
		foundAny    bool
	)

	for _, line := range strings.Split(fullIR, "\n") {
		if !inDefine {
			if strings.HasPrefix(line, "define ") {
				inDefine = true
				curSig = line
				curBody.Reset()
				curBody.WriteString(line)
				curBody.WriteByte('\n')
				isTarget = keep[extractDefineName(line)]

				if isTarget {
					foundAny = true
				}

				continue
			}

			out.WriteString(line)
			out.WriteByte('\n')

			continue
		}

		curBody.WriteString(line)
		curBody.WriteByte('\n')

		if line == "}" {
			inDefine = false
			if isTarget {
				out.WriteString(curBody.String())
			} else {
				out.WriteString(defineToDeclare(curSig))
				out.WriteByte('\n')
			}
		}
	}

	if !foundAny {
		return ""
	}

	return out.String()
}

// promoteSymbolToExternal drops the `internal` linkage qualifier from the
// `define` line of fnName so dlsym can find it after compile. No-op when
// the function is already external.
func promoteSymbolToExternal(ir, fnName string) string {
	needle := "define internal "

	for i := 0; i+len(needle) <= len(ir); {
		idx := strings.Index(ir[i:], needle)
		if idx < 0 {
			break
		}

		idx += i

		// Inspect the rest of the define line for `@<fnName>(`.
		eol := strings.IndexByte(ir[idx:], '\n')
		if eol < 0 {
			eol = len(ir) - idx
		}

		line := ir[idx : idx+eol]
		if strings.Contains(line, " @"+fnName+"(") {
			return ir[:idx] + "define " + ir[idx+len(needle):]
		}

		i = idx + eol
	}

	return ir
}

// sliceIRForFunc returns a valid stand-alone LLVM IR module containing only
// the definition of targetFn. Every other `define` in the input becomes a
// matching `declare` (extern reference) so the slice links cleanly. Returns
// "" if the target function's `define` is not found in the input.
func sliceIRForFunc(fullIR, targetFn string) string {
	var (
		out         strings.Builder
		inDefine    bool
		isTarget    bool
		curBody     strings.Builder
		curSig      string
		foundTarget bool
	)

	for _, line := range strings.Split(fullIR, "\n") {
		if !inDefine {
			if strings.HasPrefix(line, "define ") {
				inDefine = true
				curSig = line
				curBody.Reset()
				curBody.WriteString(line)
				curBody.WriteByte('\n')

				if name := extractDefineName(line); name == targetFn {
					isTarget = true
					foundTarget = true
				} else {
					isTarget = false
				}

				continue
			}
			// pass-through lines outside any define: target triple, types,
			// globals, pre-existing declares, comments, blank lines.
			out.WriteString(line)
			out.WriteByte('\n')

			continue
		}

		curBody.WriteString(line)
		curBody.WriteByte('\n')

		if line == "}" {
			inDefine = false
			if isTarget {
				out.WriteString(curBody.String())
			} else {
				out.WriteString(defineToDeclare(curSig))
				out.WriteByte('\n')
			}
		}
	}

	if !foundTarget {
		return ""
	}

	return out.String()
}

// extractDefineName pulls the function name out of a `define` line such as:
//
//	define i64 @foo(i64 %x) alwaysinline readnone nounwind {
//
// Returns "" if no `@name(` token appears.
func extractDefineName(line string) string {
	at := strings.Index(line, " @")
	if at < 0 {
		return ""
	}

	rest := line[at+2:]

	paren := strings.IndexByte(rest, '(')
	if paren < 0 {
		return ""
	}

	return strings.TrimSpace(rest[:paren])
}

// defineToDeclare converts a function definition signature line ending in
// `{` into a `declare` line. Drops the trailing `{` (and any whitespace
// before it) and rewrites the leading `define` keyword.
//
// LLVM's grammar permits these tokens between `define` and the return
// type, in any order:
//
//   linkage:        internal private external weak linkonce linkonce_odr
//                   weak_odr appending common available_externally
//                   extern_weak
//   visibility:     hidden protected
//   DLL storage:    dllimport dllexport
//   thread-locals:  thread_local
//   preemption:     dso_local dso_preemptable
//
// All of these are LEGAL on a define and ILLEGAL on a declare. We sweep
// every contiguous prefix-token of that set off the rewritten declare
// line so the slicer can't produce LLVM IR that fails to parse.
func defineToDeclare(sig string) string {
	sig = strings.TrimRight(sig, " \t")
	sig = strings.TrimSuffix(sig, "{")
	sig = strings.TrimRight(sig, " \t")
	sig = strings.Replace(sig, "define ", "declare ", 1)

	// Iteratively strip qualifier tokens that follow `declare ` until we
	// hit something that isn't on the disallowed list (the return type,
	// or an attribute group / dllimport-style with arguments).
	const prefix = "declare "

	rest := strings.TrimPrefix(sig, prefix)
	for {
		// Peel off the next whitespace-delimited token.
		end := 0
		for end < len(rest) && rest[end] != ' ' && rest[end] != '\t' {
			end++
		}

		token := rest[:end]
		if !isDefineOnlyQualifier(token) {
			break
		}
		// Skip the token + its trailing whitespace.
		i := end
		for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t') {
			i++
		}

		rest = rest[i:]
	}

	return prefix + rest
}

// isDefineOnlyQualifier reports whether tok is a function-definition
// qualifier that LLVM rejects on `declare`.
func isDefineOnlyQualifier(tok string) bool {
	switch tok {
	case "internal", "private", "external",
		"weak", "linkonce", "linkonce_odr",
		"weak_odr", "appending", "common",
		"available_externally", "extern_weak",
		"hidden", "protected",
		"dllimport", "dllexport",
		"thread_local",
		"dso_local", "dso_preemptable":
		return true
	}

	return false
}

// debugDumpFingerprint is exposed only to ease diagnosing hash mismatches:
// returns the canonical fingerprint text for fd, suitable for diffing against
// another build. Not used in the production cache path.
func (cg *CodeGen) debugDumpFingerprint(fd *ast.FuncDecl) string {
	return fmt.Sprintf("hash=%s\n%s", cg.ctfeFnHash(fd), ctfeFnFingerprint(fd))
}

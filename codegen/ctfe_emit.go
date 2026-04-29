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

	"github.com/llir/llvm/ir/enum"

	"github.com/Azer0s/tin/ast"
)

// pureFnShimPrefix names the auto-generated #interop-style wrapper attached
// to every wrappable #pure function for CTFE dispatch. Symbol pattern:
// `__tin_pure_shim_<fn_name>`. Distinct from the user-facing #interop
// wrapper symbol so we never collide with the original function name.
const pureFnShimPrefix = "__tin_pure_shim_"

// pureFnShimName returns the deterministic dispatch symbol for fn.
func pureFnShimName(fnName string) string { return pureFnShimPrefix + fnName }

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
		// Mark the shim internal so clang DCEs it from the main binary.
		// The slicer promotes it back to external for the per-fn .so.
		for _, f := range cg.mod.Funcs {
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
func (cg *CodeGen) PureFnsForCache() []PureFnArtefact {
	if cg.mod == nil {
		return nil
	}

	full := cg.mod.String()

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

		// Slice the original function plus, when present, the
		// `__tin_pure_shim_<name>` C-callable entry that dlsym will find.
		// Skip cache emission entirely if no shim exists (the function's
		// signature wasn't wrappable; CTFE dispatch would have nothing
		// to call).
		shimName := pureFnShimName(name)
		if !cg.pureFnShims[name] {
			continue
		}

		ir := sliceIRForFuncs(full, []string{name, shimName})
		if ir == "" {
			continue
		}

		// The shim must be reachable from outside the .so for dlsym to
		// find it. The original Tin entry stays internal (called only
		// from the shim within the .so).
		ir = promoteSymbolToExternal(ir, shimName)

		out = append(out, PureFnArtefact{
			Name:   name,
			Hash:   hash,
			IRText: ir,
		})
	}

	return out
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
// before it) and rewrites the leading `define` keyword. Linkage qualifiers
// like `internal` / `private` are illegal on `declare` lines and are
// stripped here.
func defineToDeclare(sig string) string {
	sig = strings.TrimRight(sig, " \t")
	sig = strings.TrimSuffix(sig, "{")
	sig = strings.TrimRight(sig, " \t")
	sig = strings.Replace(sig, "define ", "declare ", 1)

	// Strip any linkage qualifier that is legal on define but not declare.
	for _, qual := range []string{
		"declare internal ", "declare private ", "declare external ",
		"declare weak ", "declare linkonce ", "declare linkonce_odr ",
		"declare weak_odr ", "declare appending ", "declare common ",
	} {
		if strings.HasPrefix(sig, qual) {
			sig = "declare " + sig[len(qual):]
			break
		}
	}

	return sig
}

// debugDumpFingerprint is exposed only to ease diagnosing hash mismatches:
// returns the canonical fingerprint text for fd, suitable for diffing against
// another build. Not used in the production cache path.
func (cg *CodeGen) debugDumpFingerprint(fd *ast.FuncDecl) string {
	return fmt.Sprintf("hash=%s\n%s", cg.ctfeFnHash(fd), ctfeFnFingerprint(fd))
}

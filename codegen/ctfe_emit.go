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

import (
	"fmt"
	"strings"

	"github.com/Azer0s/tin/ast"
)

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

		ir := sliceIRForFunc(full, name)
		if ir == "" {
			continue
		}

		// The function must be reachable from outside the .so for dlsym to
		// find it. Promote its define from the default `internal` linkage
		// (set during main codegen) back to external/default visibility.
		ir = promoteSymbolToExternal(ir, name)

		out = append(out, PureFnArtefact{
			Name:   name,
			Hash:   hash,
			IRText: ir,
		})
	}

	return out
}

// canI64Adapter reports whether fd's full signature is expressible through
// the primitive-only cgo dispatch entries (i1/i8/i16/i32/i64 — signed and
// unsigned in source). Floats/strings/arrays/structs need to route through
// emitInteropWrapperFor's marshal helpers and are out of scope for this
// shape gate.
func canI64Adapter(fd *ast.FuncDecl) bool {
	if fd.RetType == nil {
		return false
	}

	if i64BackingType(fd.RetType) == "" {
		return false
	}

	for _, p := range fd.Params {
		if i64BackingType(p.Type) == "" {
			return false
		}
	}

	return true
}

// i64BackingType returns the LLVM IR type name for a Tin type that fits in an
// i64 register (i1 / i8 / i16 / i32 / i64 — including unsigned variants).
// Returns "" for types we cannot dispatch through the i64 cgo entries.
func i64BackingType(t ast.TypeExpr) string {
	st, ok := t.(*ast.SimpleType)
	if !ok {
		return ""
	}

	switch st.Name {
	case "bool":
		return "i1"
	case "i8", "u8", "byte", "char":
		return "i8"
	case "i16", "u16":
		return "i16"
	case "i32", "u32":
		return "i32"
	case "i64", "u64":
		return "i64"
	}

	return ""
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

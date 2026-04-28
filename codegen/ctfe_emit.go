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
// and dispatched independently. Name is the user-visible function name; Hash
// is the Merkle key (.build/pure-fn/<Hash>/bin.so); IRText is the
// self-contained sub-module .ll text the linker compiles.
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

		out = append(out, PureFnArtefact{Name: name, Hash: hash, IRText: ir})
	}

	return out
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
func defineToDeclare(sig string) string {
	sig = strings.TrimRight(sig, " \t")
	sig = strings.TrimSuffix(sig, "{")
	sig = strings.TrimRight(sig, " \t")
	sig = strings.Replace(sig, "define ", "declare ", 1)

	return sig
}

// debugDumpFingerprint is exposed only to ease diagnosing hash mismatches:
// returns the canonical fingerprint text for fd, suitable for diffing against
// another build. Not used in the production cache path.
func (cg *CodeGen) debugDumpFingerprint(fd *ast.FuncDecl) string {
	return fmt.Sprintf("hash=%s\n%s", cg.ctfeFnHash(fd), ctfeFnFingerprint(fd))
}

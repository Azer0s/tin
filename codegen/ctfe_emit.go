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
//
// AdapterSym is the name of the uniform-signature dispatch entry point
// emitted into the same .so when the function's params/return are all
// 64-bit-or-narrower integer types. Empty when no adapter was emitted
// (the caller must skip dlopen-dispatch for that fn). Adapter signature:
//
//	int64_t AdapterSym(int64_t* args, int64_t nargs)
//
// — args is a caller-owned buffer of nargs i64 values, the function call
// is performed, and the i64 result is returned.
type PureFnArtefact struct {
	Name       string
	Hash       string
	IRText     string
	AdapterSym string
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

		adapterSym, adapterIR := buildI64Adapter(fd, hash)
		if adapterIR != "" {
			ir += adapterIR
		}

		out = append(out, PureFnArtefact{
			Name:       name,
			Hash:       hash,
			IRText:     ir,
			AdapterSym: adapterSym,
		})
	}

	return out
}

// buildI64Adapter returns the symbol name and the LLVM IR text for a uniform
// dispatch trampoline that calls fd via the standard tin_ctfe protocol:
//
//	int64_t tin_ctfe_<hash>(int64_t* args, int64_t nargs)
//
// Only emitted when every parameter type and the return type fit in i64
// (i8/i16/i32/i64 plus their unsigned counterparts and bool). Returns
// ("", "") when fd's signature is outside that subset (string, struct,
// float, etc.) — callers will fall back to non-dispatched evaluation.
//
// The adapter is intentionally tiny: load each arg from the args buffer,
// truncate or zero-extend to the param type, call the function, extend the
// result back to i64. clang inlines through it at -O2 in the cached .so.
func buildI64Adapter(fd *ast.FuncDecl, hash string) (string, string) {
	if !canI64Adapter(fd) {
		return "", ""
	}

	sym := "tin_ctfe_" + hash

	var b strings.Builder

	fmt.Fprintf(&b, "\ndefine i64 @%s(i64* %%args, i64 %%nargs) {\nentry:\n", sym)

	var callArgs []string
	for i, p := range fd.Params {
		argTy := i64BackingType(p.Type)
		fmt.Fprintf(&b, "\t%%a%d_ptr = getelementptr i64, i64* %%args, i64 %d\n", i, i)
		fmt.Fprintf(&b, "\t%%a%d_i64 = load i64, i64* %%a%d_ptr\n", i, i)

		if argTy == "i64" {
			callArgs = append(callArgs, fmt.Sprintf("i64 %%a%d_i64", i))
		} else {
			fmt.Fprintf(&b, "\t%%a%d = trunc i64 %%a%d_i64 to %s\n", i, i, argTy)
			callArgs = append(callArgs, fmt.Sprintf("%s %%a%d", argTy, i))
		}
	}

	retTy := i64BackingType(fd.RetType)
	fmt.Fprintf(&b, "\t%%result = call %s @%s(%s)\n", retTy, fd.Name, strings.Join(callArgs, ", "))

	if retTy == "i64" {
		b.WriteString("\tret i64 %result\n")
	} else {
		fmt.Fprintf(&b, "\t%%result_i64 = zext %s %%result to i64\n", retTy)
		b.WriteString("\tret i64 %result_i64\n")
	}

	b.WriteString("}\n")

	return sym, b.String()
}

// canI64Adapter reports whether fd's full signature is expressible as the
// uniform tin_ctfe (i64*, i64) -> i64 protocol. We accept i1/i8/i16/i32/i64
// (signed and unsigned in source) plus a non-void return; floats/strings/
// arrays/structs require richer marshalling and are out of scope for the
// MVP adapter.
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
// Returns "" for types we cannot dispatch through the i64 adapter (float,
// string, struct, array, pointer, void, ...).
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

package codegen

// House-style checks: naming conventions plus a small file-level pass
// for trailing whitespace and a missing EOF newline. Gated behind
// -Wstyle (default-off; joins -Wall).

import (
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/Azer0s/tin/ast"
)

// snakeCaseRe accepts snake_case (with optional leading underscore for
// the conventional "private" prefix) and the bare `_` placeholder.
// Trailing digits are fine: `parse_u8`, `decode_v2`. Identifiers ending
// in `_` are allowed too -- they're occasionally used to avoid keyword
// collisions.
var snakeCaseRe = regexp.MustCompile(`^_?[a-z][a-z0-9_]*$|^_$`)

// pascalCaseRe accepts PascalCase: an uppercase letter followed by any
// mix of letters and digits. ALLCAPS is rejected as a reminder to use
// PascalCase for type names.
var pascalCaseRe = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)

// checkStyle runs over prog flagging declarations that don't match the
// house style, then makes one pass over the source file for whitespace
// hygiene. Skipped when the diagnostic is disabled so we don't pay for
// regex matches and a file read on every build.
func (cg *CodeGen) checkStyle(prog *ast.Program) {
	if !cg.diagEnabled(DiagStyle) {
		return
	}

	for _, s := range prog.Stmts {
		walkAST(s, func(n ast.Node) {
			switch e := n.(type) {
			case *ast.FuncDecl:
				cg.checkFuncStyle(e)
			case *ast.VarDecl:
				cg.checkVarDeclStyle(e)
			case *ast.StructDecl:
				cg.checkTypeNameStyle(e.Name, "struct", e.Pos())
			case *ast.TraitDecl:
				cg.checkTypeNameStyle(e.Name, "trait", e.Pos())
			case *ast.EnumDecl:
				cg.checkTypeNameStyle(e.Name, "enum", e.Pos())
			case *ast.TypeDecl:
				cg.checkTypeNameStyle(e.Name, "type", e.Pos())
			case *ast.UnionDecl:
				cg.checkTypeNameStyle(e.Name, "union", e.Pos())
			case *ast.DataDecl:
				cg.checkTypeNameStyle(e.Name, "data", e.Pos())
			}
		})
	}

	cg.checkSourceWhitespace()
}

// checkFuncStyle flags a function whose Tin-side name doesn't follow
// snake_case, plus any parameter with the same problem. Extern wrappers
// declare a Tin-side identifier separate from the C symbol they bind to
// (`fn open_fd() = extern("_open")`), so both halves get judged on
// their own merits -- only the Tin name is style-checked.
func (cg *CodeGen) checkFuncStyle(fn *ast.FuncDecl) {
	if fn.Name != "" && !snakeCaseRe.MatchString(fn.Name) {
		cg.warn(DiagStyle, fn.Pos(),
			"function %q is not snake_case", fn.Name)
	}

	for _, p := range fn.Params {
		if p.Name == "" || p.Name == "this" {
			continue
		}

		if !snakeCaseRe.MatchString(p.Name) {
			cg.warn(DiagStyle, fn.Pos(),
				"parameter %q in fn %q is not snake_case", p.Name, fn.Name)
		}
	}
}

// checkVarDeclStyle flags a let/const binding whose name isn't
// snake_case. We don't try to enforce SCREAMING_SNAKE for top-level
// constants -- the convention isn't universally adopted in Tin code,
// and demanding it would be more noise than signal.
func (cg *CodeGen) checkVarDeclStyle(v *ast.VarDecl) {
	if v.Name == "" {
		return
	}

	if !snakeCaseRe.MatchString(v.Name) {
		kind := "let"
		if v.IsConst {
			kind = "const"
		}

		cg.warn(DiagStyle, v.Pos(),
			"%s %q is not snake_case", kind, v.Name)
	}
}

// checkTypeNameStyle flags a type-level declaration whose name isn't
// PascalCase. Single-letter names are allowed (`type T = ...` is
// occasionally seen as a stand-in identifier in tutorial code).
func (cg *CodeGen) checkTypeNameStyle(name, kind string, pos ast.Pos) {
	if name == "" || len(name) == 1 {
		return
	}

	if !pascalCaseRe.MatchString(name) {
		cg.warn(DiagStyle, pos,
			"%s %q is not PascalCase", kind, name)
	}
}

// checkSourceWhitespace flags trailing spaces/tabs at end of any line
// and a missing terminating newline.  Iterates every distinct user
// source file we have a record of (collected from struct/func decls)
// so the check covers the whole program -- using cg.filename alone
// would only see the last package compiled, since that field is
// mutated during per-package code generation.
func (cg *CodeGen) checkSourceWhitespace() {
	seen := map[string]bool{}

	addFile := func(f string) {
		if f != "" {
			seen[f] = true
		}
	}

	addFile(cg.filename)

	for _, f := range cg.structDeclFiles {
		addFile(f)
	}

	files := make([]string, 0, len(seen))
	for f := range seen {
		files = append(files, f)
	}

	sort.Strings(files)

	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}

		cg.checkOneFileWhitespace(file, string(data))
	}
}

func (cg *CodeGen) checkOneFileWhitespace(file, src string) {
	if len(src) > 0 && !strings.HasSuffix(src, "\n") {
		lastLine := strings.Count(src, "\n") + 1

		cg.warnInFile(file, DiagStyle, ast.Pos{Line: lastLine, Col: 1},
			"file is missing a trailing newline")
	}

	lines := strings.Split(src, "\n")

	for i, line := range lines {
		if i == len(lines)-1 && line == "" {
			continue
		}

		stripped := strings.TrimRight(line, " \t")
		if stripped == line {
			continue
		}

		col := len(stripped) + 1

		cg.warnInFile(file, DiagStyle, ast.Pos{Line: i + 1, Col: col},
			"trailing whitespace")
	}
}

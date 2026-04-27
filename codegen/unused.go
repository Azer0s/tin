package codegen

import (
	irtypes "github.com/llir/llvm/ir/types"

	"github.com/Azer0s/tin/ast"
)

// isVoidType reports whether t is a void or zero-bit return type. Calls
// returning such a type are not subject to the discarded-result warning.
func isVoidType(t irtypes.Type) bool {
	if t == nil {
		return true
	}

	if _, ok := t.(*irtypes.VoidType); ok {
		return true
	}
	// `Unit` is sometimes lowered to {} or i1 that's never read.
	if st, ok := t.(*irtypes.StructType); ok && len(st.Fields) == 0 {
		return true
	}

	return false
}

// isCalleePure reports whether a CallExpr's target function is tagged
// `#pure`. Pure calls have no observable side effects, so discarding their
// result is always a mistake (the call could be deleted entirely).
func (cg *CodeGen) isCalleePure(c *ast.CallExpr) bool {
	id, ok := c.Func.(*ast.Identifier)
	if !ok {
		return false
	}
	// Built-ins like len/sizeof are inherently pure.
	if isPureBuiltin(id.Name) {
		return true
	}

	for _, fd := range cg.funcDecls {
		if fd.Name == id.Name && hasTag(fd.Tags, "pure") {
			return true
		}
	}

	return false
}

// callDisplayName returns a short human-readable description of a call site
// for use in diagnostic messages.
func callDisplayName(c *ast.CallExpr) string {
	switch fn := c.Func.(type) {
	case *ast.Identifier:
		return fn.Name
	case *ast.FieldAccess:
		return fn.Field
	}

	return "<call>"
}

// checkAllUnused walks every top-level FuncDecl (including struct methods)
// and emits unused-let / unused-param warnings for names that are never
// read in the body. Default-off; gated by -W<name>, -Wall, -Wpedantic.
//
// Also runs the unused-import scan over top-level UseDecls, which is
// default-on (a stale `use` is essentially dead weight in the file).
func (cg *CodeGen) checkAllUnused(prog *ast.Program) {
	for _, n := range prog.Stmts {
		switch v := n.(type) {
		case *ast.FuncDecl:
			cg.checkUnusedInFunc(v)
		case *ast.StructDecl:
			for _, m := range v.Methods {
				cg.checkUnusedInFunc(m)
			}
		}
	}

	cg.checkUnusedImports(prog)
}

// checkUnusedImports warns for `use pkg` / `use { name } from pkg` /
// `use "./file"` declarations whose imported names are never referenced
// anywhere else in the program.
func (cg *CodeGen) checkUnusedImports(prog *ast.Program) {
	// Collect every name referenced anywhere - identifiers, scope-access
	// roots (pkg::), and field-access roots (pkg.).
	used := map[string]bool{}

	visit := func(n ast.Node) {
		switch v := n.(type) {
		case *ast.Identifier:
			used[v.Name] = true
		case *ast.ScopeAccess:
			if len(v.Path) > 0 {
				used[v.Path[0]] = true
			}
		case *ast.FieldAccess:
			if id, ok := v.Expr.(*ast.Identifier); ok {
				used[id.Name] = true
			}
		}
	}

	for _, n := range prog.Stmts {
		walkAST(n, visit)
	}

	for _, n := range prog.Stmts {
		ud, ok := n.(*ast.UseDecl)
		if !ok || ud.IsExtern {
			continue
		}

		if ud.FromSyntax && len(ud.Names) > 0 {
			// `use { a, b } from pkg`: each name lands in scope directly.
			for _, name := range ud.Names {
				if !used[name] {
					cg.warn(DiagUnusedImport, ud.Pos(),
						"imported name %q is never used", name)
				}
			}

			continue
		}

		// `use pkg` / `use "./file"` brings the package handle into scope
		// under its base name (last `::` or `/` segment).
		base := importBaseName(ud.Path)
		if base == "" || used[base] {
			continue
		}

		cg.warn(DiagUnusedImport, ud.Pos(),
			"import %q is never used", base)
	}
}

// importBaseName returns the bare identifier under which a `use` declaration
// is referenced. For `use io` it's "io"; for `use std::math` it's "math";
// for `use "./foo/bar"` it's "bar".
func importBaseName(path string) string {
	clean := path
	if i := lastIndexAny(clean, "/:"); i >= 0 {
		clean = clean[i+1:]
	}

	return clean
}

func lastIndexAny(s, chars string) int {
	out := -1

	for i := 0; i < len(s); i++ {
		for j := 0; j < len(chars); j++ {
			if s[i] == chars[j] && i > out {
				out = i
			}
		}
	}

	return out
}

func (cg *CodeGen) checkUnusedInFunc(fn *ast.FuncDecl) {
	if fn.Body == nil {
		return
	}
	// Externs and virtual decls have no body to scan.
	if fn.IsExtern != "" || fn.IsVirtual {
		return
	}

	// Collect every identifier referenced anywhere in the body. This
	// over-approximates "use" - both reads and writes count - so a binding
	// that is only ever assigned to and never read still avoids the warning.
	// A stricter "never read" check would require scope-aware tracking that
	// the simple AST walker can't model in the presence of shadowing.
	used := map[string]bool{}

	walkAST(fn.Body, func(n ast.Node) {
		if id, ok := n.(*ast.Identifier); ok {
			used[id.Name] = true
		}
	})

	for _, p := range fn.Params {
		if p.Name == "" || p.Name == "_" || p.Name == "this" {
			continue
		}

		if used[p.Name] {
			continue
		}

		cg.warn(DiagUnusedParam, fn.Pos(),
			"parameter %q is never read; rename to `_` if intentional", p.Name)
	}

	walkAST(fn.Body, func(n ast.Node) {
		v, ok := n.(*ast.VarDecl)
		if !ok {
			return
		}

		if v.Name == "" || v.Name == "_" || v.IsConst {
			return
		}

		if used[v.Name] {
			return
		}

		cg.warn(DiagUnusedLet, v.Pos(),
			"let-binding %q is never read; rename to `_` if intentional", v.Name)
	})
}

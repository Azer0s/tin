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

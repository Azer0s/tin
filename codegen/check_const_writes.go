package codegen

// check_const_writes.go - static analysis pass that warns when a
// program takes the address of a top-level `const` and then writes
// through that pointer.
//
// Top-level const is placed in read-only storage by codegen (the
// global is emitted with an Init constant and tracked as immutable
// for ARC purposes). Writing through a pointer alias of one is
// well-formed at the LLVM IR level but is undefined behavior at the
// language level - the same status as modifying a `const` global in C.
//
// The pass runs once per program, after parse and before codegen
// emits IR. It is intraprocedural per function body and uses a small
// flow-insensitive taint:
//
//   1. Collect every top-level decl tagged `const` (TopLevelVar with
//      IsConst=true).
//   2. Walk every function body. Within a function, mark a binding as
//      "tainted" when its initializer is `&top_level_const` OR when
//      it is bound to another tainted binding (alias chain of any
//      depth).
//   3. Warn when the program does `*p = ...`, `*p += ...`, or `*p++`
//      on a tainted p. Also warn for AddressOfExpr {SomeConst} used
//      directly as the LHS of a write (rare in practice but easy to
//      cover).
//
// The pass is intentionally simple: no inter-procedural propagation
// (that would force an extra fixpoint pass and would produce noisy
// diagnostics on common idioms like passing a pointer to a `read`
// helper that doesn't actually write). Users who write through a
// helper they wrote themselves can audit at the call site.

import (
	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) checkAllWritesToTopLevelConst(prog *ast.Program) {
	if cg.diagSuppressed(DiagWriteToConst) {
		return
	}

	consts := collectTopLevelConsts(prog)
	if len(consts) == 0 {
		return
	}

	for _, n := range prog.Stmts {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}

		cg.checkFuncForConstWrites(fn, consts)
	}
}

// collectTopLevelConsts returns the set of top-level decl names that
// were declared with `const`. Only TopLevelVar decls qualify - block-
// scope const is local and never escapes through a global pointer.
func collectTopLevelConsts(prog *ast.Program) map[string]bool {
	consts := map[string]bool{}

	for _, n := range prog.Stmts {
		if tv, ok := n.(*ast.TopLevelVar); ok && tv.IsConst {
			consts[tv.Name] = true
		}
	}

	return consts
}

// checkFuncForConstWrites walks fn's body, tracking which bindings
// alias `&top_level_const` and emitting DiagWriteToConst when one is
// written through.
func (cg *CodeGen) checkFuncForConstWrites(fn *ast.FuncDecl, consts map[string]bool) {
	tainted := map[string]bool{}

	var walk func(node ast.Node)
	walk = func(node ast.Node) {
		switch v := node.(type) {
		case *ast.Block:
			for _, s := range v.Stmts {
				walk(s)
			}
		case *ast.VarDecl:
			if v.Value != nil {
				walk(v.Value)

				if isConstAddr(v.Value, consts) || isTaintedAlias(v.Value, tainted) {
					tainted[v.Name] = true
				}
			}
		case *ast.AssignStmt:
			// Detect *p = ... where p is tainted.
			if d, ok := v.Target.(*ast.DerefExpr); ok {
				if id, ok2 := d.Expr.(*ast.Identifier); ok2 && tainted[id.Name] {
					cg.warn(DiagWriteToConst, v.Pos(),
						"writing through a pointer aliased to top-level const (via %q): "+
							"top-level consts live in read-only storage; "+
							"this is undefined behavior. Drop the alias, or change the const to var if mutation is required.",
						id.Name)
				}
				// Direct write: `*&CONST = ...`. Rare but trivial to catch.
				if ao, ok2 := d.Expr.(*ast.AddressOfExpr); ok2 {
					if id, ok3 := ao.Expr.(*ast.Identifier); ok3 && consts[id.Name] {
						cg.warn(DiagWriteToConst, v.Pos(),
							"writing to top-level const %q directly: "+
								"top-level consts live in read-only storage; "+
								"this is undefined behavior.",
							id.Name)
					}
				}
			}

			walk(v.Value)
			walk(v.Target)
		case *ast.AugAssignStmt:
			if d, ok := v.Target.(*ast.DerefExpr); ok {
				if id, ok2 := d.Expr.(*ast.Identifier); ok2 && tainted[id.Name] {
					cg.warn(DiagWriteToConst, v.Pos(),
						"compound-assign through a pointer aliased to top-level const (via %q): "+
							"top-level consts live in read-only storage; "+
							"this is undefined behavior.",
						id.Name)
				}
			}

			walk(v.Value)
			walk(v.Target)
		case *ast.PostfixStmt:
			if d, ok := v.Expr.(*ast.DerefExpr); ok {
				if id, ok2 := d.Expr.(*ast.Identifier); ok2 && tainted[id.Name] {
					cg.warn(DiagWriteToConst, v.Pos(),
						"%s through a pointer aliased to top-level const (via %q): "+
							"top-level consts live in read-only storage; "+
							"this is undefined behavior.",
						v.Op, id.Name)
				}
			}
		case *ast.IfStmt:
			walk(v.Cond)
			walk(v.Then)
			walk(v.Else)
		case *ast.ForStmt:
			walk(v.Init)
			walk(v.Cond)
			walk(v.Post)
			walk(v.Iter)
			walk(v.Body)
		case *ast.MatchStmt:
			walk(v.Expr)
			for _, c := range v.Cases {
				walk(c.Body)
			}

			walk(v.Default)
		case *ast.ReturnStmt:
			walk(v.Value)
		case *ast.ExprStmt:
			walk(v.Expr)
		case *ast.CallExpr:
			for _, a := range v.Args {
				walk(a)
			}
		case *ast.BinExpr:
			walk(v.Left)
			walk(v.Right)
		case *ast.UnaryExpr:
			walk(v.Expr)
		case *ast.AddressOfExpr:
			walk(v.Expr)
		case *ast.DerefExpr:
			walk(v.Expr)
		}
	}

	walk(fn.Body)
}

// isConstAddr returns true when expr is `&IDENT` and IDENT names a
// top-level const declared in this program.
func isConstAddr(expr ast.Node, consts map[string]bool) bool {
	ao, ok := expr.(*ast.AddressOfExpr)
	if !ok {
		return false
	}

	id, ok := ao.Expr.(*ast.Identifier)
	if !ok {
		return false
	}

	return consts[id.Name]
}

// isTaintedAlias returns true when expr is an Identifier that names a
// previously-tainted binding (so `let p2 = p1` propagates the taint
// when p1 was already tainted).
func isTaintedAlias(expr ast.Node, tainted map[string]bool) bool {
	id, ok := expr.(*ast.Identifier)
	if !ok {
		return false
	}

	return tainted[id.Name]
}

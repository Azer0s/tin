package codegen

// resolve.go - pre-generation semantic checks that require a full AST walk.
//
// checkDuplicateDecls is invoked by Generate() before any IR is emitted.
// It reports the first duplicate variable declaration found in the same scope.
// Shadowing (declaring a name that exists in an *outer* scope) is allowed.

import (
	"fmt"
	"strings"

	"github.com/Azer0s/tin/ast"
)

// checkDuplicateDecls is the entry point.  It checks the top-level (module)
// scope and every nested block/function/lambda body for duplicate `let` names.
func checkDuplicateDecls(nodes []ast.Node) error {
	// Duplicate `use` decls at module scope: importing the same package
	// twice (or pulling the same name from a package twice) is almost
	// always a copy-paste mistake.  Catch it before codegen folds the
	// dedup -- silently ignoring the second `use` made typos like
	// `use rseult; use result` look fine until something downstream broke.
	if err := checkDuplicateUseDecls(nodes); err != nil {
		return err
	}

	// Top-level module scope.
	if err := checkNodeListDecls(nodes); err != nil {
		return err
	}

	// Recurse into all nested scopes.
	for _, n := range nodes {
		if err := walkForDuplicates(n); err != nil {
			return err
		}
	}

	return nil
}

// checkSelectiveImportQualifiers rejects `pkg::Name` references when
// the surrounding file only imported `pkg` via `use { ... } from pkg`.
// Selective imports bring the named symbols into scope as bare names
// but do NOT register the package itself as a namespace -- writing
// `pkg::X` reaches for a qualifier the file never opted into, and is
// almost always a stale habit from before the selective import was
// added.  Files that want both forms must say both: `use pkg` and
// `use { X } from pkg`.
func checkSelectiveImportQualifiers(nodes []ast.Node) error {
	// First pass: classify each top-level UseDecl.
	pkgImported := map[string]bool{}
	selectivePkgs := map[string]bool{}

	for _, node := range nodes {
		ud, ok := node.(*ast.UseDecl)
		if !ok || ud.IsExtern || ud.IsFile {
			continue
		}

		if ud.FromSyntax {
			selectivePkgs[ud.Path] = true
		} else {
			pkgImported[ud.Path] = true
		}
	}

	// Set of packages that are reachable ONLY through selective imports.
	selectiveOnly := map[string]bool{}

	for p := range selectivePkgs {
		if !pkgImported[p] {
			selectiveOnly[p] = true
		}
	}

	if len(selectiveOnly) == 0 {
		return nil
	}

	// Second pass: walk the program and reject any ScopeAccess whose
	// first segment names a selective-only package.  We do not try to
	// rewrite or downgrade the error: the file declared its intent by
	// importing selectively, so adding `use pkg` is the right fix.
	var visitErr error

	visit := func(n ast.Node) {
		if visitErr != nil {
			return
		}

		sa, ok := n.(*ast.ScopeAccess)
		if !ok || len(sa.Path) < 1 {
			return
		}

		pkg := sa.Path[0]
		if idx := strings.Index(pkg, "::"); idx >= 0 {
			pkg = pkg[:idx]
		}

		if !selectiveOnly[pkg] {
			return
		}

		visitErr = fmt.Errorf("%d:%d: `%s::...` cannot be used because the file only imported %q selectively (`use { ... } from %s`); add `use %s` to opt into the qualified form, or rewrite as a bare name",
			sa.Pos().Line, sa.Pos().Col, pkg, pkg, pkg, pkg)
	}

	for _, n := range nodes {
		walkAST(n, visit)

		if visitErr != nil {
			return visitErr
		}
	}

	return nil
}

// checkDuplicateUseDecls scans the module-level node list and rejects
// repeated package or selective-name imports.  Allowed:
//   - `use foo` paired with `use { X } from foo` (package alias + selective)
//   - `use { X } from foo` paired with `use { Y } from foo` (different names)
//
// Rejected:
//   - `use foo` ... `use foo`     (duplicate package import)
//   - `use { X } from foo` ... `use { X } from foo` (duplicate selective)
func checkDuplicateUseDecls(nodes []ast.Node) error {
	seenPkg := make(map[string]int)
	seenSel := make(map[string]int) // key = "<pkg>::<name>"

	for _, node := range nodes {
		ud, ok := node.(*ast.UseDecl)
		if !ok || ud.IsExtern {
			continue
		}

		if !ud.FromSyntax {
			// Plain `use foo` -- track the package path.
			if prev, dup := seenPkg[ud.Path]; dup {
				return fmt.Errorf("%d:%d: duplicate import %q (previously imported at line %d)",
					ud.Pos().Line, ud.Pos().Col, ud.Path, prev)
			}

			seenPkg[ud.Path] = ud.Pos().Line

			continue
		}

		// Selective `use { a, b } from foo` -- track each name independently.
		for _, name := range ud.Names {
			key := ud.Path + "::" + name
			if prev, dup := seenSel[key]; dup {
				return fmt.Errorf("%d:%d: duplicate import of %q from %q (previously imported at line %d)",
					ud.Pos().Line, ud.Pos().Col, name, ud.Path, prev)
			}

			seenSel[key] = ud.Pos().Line
		}
	}

	return nil
}

// declSeen maps a variable name to the line where it was first declared in
// the current scope level.
type declSeen map[string]int

// recordDecl adds name to seen or returns an error if already present.
func recordDecl(seen declSeen, name string, line int, col int) error {
	if name == "_" || name == "" {
		return nil
	}

	if prev, ok := seen[name]; ok {
		return fmt.Errorf("%d:%d: variable %q already declared in this scope (previously declared at line %d)",
			line, col, name, prev)
	}

	seen[name] = line

	return nil
}

// checkNodeListDecls checks a flat list of nodes (a block's statements, or
// the module-level node list) for duplicate declarations at this scope level.
// It does NOT descend into nested blocks; walkForDuplicates handles recursion.
func checkNodeListDecls(nodes []ast.Node) error {
	seen := make(declSeen)

	for _, node := range nodes {
		if err := checkDeclNode(node, seen); err != nil {
			return err
		}
	}

	return nil
}

// checkDeclNode extracts declared variable names from a single statement node
// and checks them against seen.  Only direct declaration forms are handled;
// nested expressions and sub-blocks are skipped here (handled by walkForDuplicates).
func checkDeclNode(node ast.Node, seen declSeen) error {
	switch s := node.(type) {
	case *ast.VarDecl:
		return recordDecl(seen, s.Name, s.Pos().Line, s.Pos().Col)

	case *ast.ArrayDestructDecl:
		for _, name := range s.Names {
			name = strings.TrimPrefix(name, "...")

			if err := recordDecl(seen, name, s.Pos().Line, s.Pos().Col); err != nil {
				return err
			}
		}

	case *ast.StructDestructDecl:
		// VarNames overrides Names when aliasing: let {x: a, y: b} T = ...
		varNames := s.VarNames
		if len(varNames) == 0 {
			varNames = s.Names
		}

		for _, name := range varNames {
			if err := recordDecl(seen, name, s.Pos().Line, s.Pos().Col); err != nil {
				return err
			}
		}

	case *ast.TupleDestructDecl:
		for _, name := range s.Names {
			if err := recordDecl(seen, name, s.Pos().Line, s.Pos().Col); err != nil {
				return err
			}
		}
	}

	return nil
}

// walkForDuplicates recursively visits every scope-creating construct (blocks,
// function/lambda/test bodies) and validates each for duplicate declarations.
// It does not descend into struct/trait declarations at the type level, but it
// does walk method bodies.
func walkForDuplicates(node ast.Node) error {
	if node == nil {
		return nil
	}

	switch n := node.(type) {
	case *ast.Block:
		if n == nil {
			return nil
		}

		// Check this block's own declarations, then recurse into its statements.
		if err := checkNodeListDecls(n.Stmts); err != nil {
			return err
		}

		for _, s := range n.Stmts {
			if err := walkForDuplicates(s); err != nil {
				return err
			}
		}

	case *ast.FuncDecl:
		if n == nil {
			return nil
		}

		return walkForDuplicates(n.Body)

	case *ast.LambdaExpr:
		if n == nil {
			return nil
		}

		return walkForDuplicates(n.Body)

	case *ast.TestDecl:
		if n == nil {
			return nil
		}

		return walkForDuplicates(n.Body)

	case *ast.StructDecl:
		if n == nil {
			return nil
		}

		for _, m := range n.Methods {
			if err := walkForDuplicates(m); err != nil {
				return err
			}
		}

	case *ast.IfStmt:
		if n == nil {
			return nil
		}

		if n.Then != nil {
			if err := walkForDuplicates(n.Then); err != nil {
				return err
			}
		}

		if n.Else != nil {
			if err := walkForDuplicates(n.Else); err != nil {
				return err
			}
		}

		for _, elif := range n.ElseIfs {
			if elif.Body != nil {
				if err := walkForDuplicates(elif.Body); err != nil {
					return err
				}
			}
		}

	case *ast.ForStmt:
		if n == nil || n.Body == nil {
			return nil
		}

		return walkForDuplicates(n.Body)

	case *ast.MatchStmt:
		if n == nil {
			return nil
		}

		for _, arm := range n.Cases {
			if arm.Body != nil {
				if err := walkForDuplicates(arm.Body); err != nil {
					return err
				}
			}
		}

		if n.Default != nil {
			if err := walkForDuplicates(n.Default); err != nil {
				return err
			}
		}

	case *ast.WhereList:
		if n == nil {
			return nil
		}

		for _, c := range n.Clauses {
			if err := walkForDuplicates(c.Body); err != nil {
				return err
			}
		}

	case *ast.AwaitMatchStmt:
		if n == nil {
			return nil
		}

		for _, arm := range n.Cases {
			if arm.Body != nil {
				if err := walkForDuplicates(arm.Body); err != nil {
					return err
				}
			}
		}

	case *ast.SpawnExpr:
		if n == nil {
			return nil
		}

		if n.DoBlock != nil {
			return walkForDuplicates(n.DoBlock)
		}

		return walkForDuplicates(n.Call)

	// Statement wrappers - walk their values to find lambdas inside.
	case *ast.VarDecl:
		return walkForDuplicates(n.Value)

	case *ast.ArrayDestructDecl:
		return walkForDuplicates(n.Value)

	case *ast.StructDestructDecl:
		return walkForDuplicates(n.Value)

	case *ast.TupleDestructDecl:
		return walkForDuplicates(n.Value)

	case *ast.AssignStmt:
		return walkForDuplicates(n.Value)

	case *ast.AugAssignStmt:
		return walkForDuplicates(n.Value)

	case *ast.ReturnStmt:
		return walkForDuplicates(n.Value)

	case *ast.ExprStmt:
		return walkForDuplicates(n.Expr)

	case *ast.EchoStmt:
		return walkForDuplicates(n.Value)

	case *ast.DeferStmt:
		return walkForDuplicates(n.Call)

	// Expressions that can embed lambdas.
	case *ast.CallExpr:
		if err := walkForDuplicates(n.Func); err != nil {
			return err
		}

		for _, a := range n.Args {
			if err := walkForDuplicates(a); err != nil {
				return err
			}
		}

	case *ast.BinExpr:
		if err := walkForDuplicates(n.Left); err != nil {
			return err
		}

		return walkForDuplicates(n.Right)

	case *ast.UnaryExpr:
		return walkForDuplicates(n.Expr)

	case *ast.AwaitExpr:
		return walkForDuplicates(n.Future)

	case *ast.TernaryExpr:
		if err := walkForDuplicates(n.Cond); err != nil {
			return err
		}

		if err := walkForDuplicates(n.Then); err != nil {
			return err
		}

		return walkForDuplicates(n.Else)

	case *ast.PipeExpr:
		if err := walkForDuplicates(n.Left); err != nil {
			return err
		}

		return walkForDuplicates(n.Right)
	}

	return nil
}

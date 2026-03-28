package codegen

import (
	"fmt"
	"strings"

	"github.com/Azer0s/tin/ast"
)

// ExpandProgramMacros collects all MacroDecl nodes from prog, then returns a
// new Program where every macro call has been replaced with its expansion.
// MacroDecl nodes are removed from the output (they have been inlined).
func (cg *CodeGen) ExpandProgramMacros(prog *ast.Program) (*ast.Program, error) {
	// Register macros first so expandMacroToAST can find them.
	for _, stmt := range prog.Stmts {
		if m, ok := stmt.(*ast.MacroDecl); ok {
			cg.macros[m.Name] = m
		}
	}
	// Expand every statement.
	var stmts []ast.Node
	for _, stmt := range prog.Stmts {
		expanded, err := cg.expandNodeMacros(stmt)
		if err != nil {
			return nil, err
		}
		stmts = append(stmts, expanded)
	}
	return &ast.Program{Stmts: stmts}, nil
}

// expandMacroToAST expands one macro call and returns the resulting AST node.
// It reuses the existing CTFE and substitution machinery.
func (cg *CodeGen) expandMacroToAST(macro *ast.MacroDecl, args []ast.Node) (ast.Node, error) {
	if len(args) != len(macro.Params) {
		return nil, fmt.Errorf("macro %s: expected %d args, got %d",
			macro.Name, len(macro.Params), len(args))
	}
	// Complex macros (block body): CTFE path - returns ast.Node directly.
	if isMacroComplex(macro) {
		return cg.ctfeExpandMacro(macro, args)
	}
	// Simple macros: AST substitution.
	subst := make(map[string]ast.Node, len(macro.Params))
	for i, p := range macro.Params {
		subst[p] = args[i]
	}
	body := macro.Body
	if es, ok := body.(*ast.ExprStmt); ok {
		body = es.Expr
	}
	expanded := substituteMacroNode(body, subst)
	// Backtick body: parse content as Tin source.
	if btl, ok := expanded.(*ast.BacktickLit); ok {
		node, err := parseExprString(btl.Content)
		if err != nil {
			return nil, fmt.Errorf("macro %s: backtick parse error: %w", macro.Name, err)
		}
		return node, nil
	}
	return expanded, nil
}

// expandNodeMacros recursively walks node, expanding any macro calls it finds.
func (cg *CodeGen) expandNodeMacros(node ast.Node) (ast.Node, error) {
	if node == nil {
		return nil, nil
	}
	switch n := node.(type) {

	case *ast.CallExpr:
		// Check whether this is a macro call.
		if id, ok := n.Func.(*ast.Identifier); ok && strings.HasSuffix(id.Name, "!") {
			macro := cg.lookupMacro(id.Name)
			if macro != nil {
				// Expand args first (nested macro calls).
				expandedArgs := make([]ast.Node, len(n.Args))
				for i, a := range n.Args {
					ea, err := cg.expandNodeMacros(a)
					if err != nil {
						return nil, err
					}
					expandedArgs[i] = ea
				}
				return cg.expandMacroToAST(macro, expandedArgs)
			}
		}
		// Regular call: recurse into args.
		newArgs := make([]ast.Node, len(n.Args))
		for i, a := range n.Args {
			ea, err := cg.expandNodeMacros(a)
			if err != nil {
				return nil, err
			}
			newArgs[i] = ea
		}
		newFunc, err := cg.expandNodeMacros(n.Func)
		if err != nil {
			return nil, err
		}
		return &ast.CallExpr{Func: newFunc, Args: newArgs, TypeArgs: n.TypeArgs}, nil

	case *ast.FuncDecl:
		newBody, err := cg.expandNodeMacros(n.Body)
		if err != nil {
			return nil, err
		}
		out := *n
		out.Body = newBody
		return &out, nil

	case *ast.Block:
		newStmts := make([]ast.Node, 0, len(n.Stmts))
		for _, s := range n.Stmts {
			es, err := cg.expandNodeMacros(s)
			if err != nil {
				return nil, err
			}
			newStmts = append(newStmts, es)
		}
		return &ast.Block{Stmts: newStmts}, nil

	case *ast.ExprStmt:
		newExpr, err := cg.expandNodeMacros(n.Expr)
		if err != nil {
			return nil, err
		}
		return &ast.ExprStmt{Expr: newExpr}, nil

	case *ast.EchoStmt:
		newVal, err := cg.expandNodeMacros(n.Value)
		if err != nil {
			return nil, err
		}
		return &ast.EchoStmt{Value: newVal}, nil

	case *ast.ReturnStmt:
		if n.Value == nil {
			return n, nil
		}
		newVal, err := cg.expandNodeMacros(n.Value)
		if err != nil {
			return nil, err
		}
		return &ast.ReturnStmt{Value: newVal}, nil

	case *ast.VarDecl:
		if n.Value == nil {
			return n, nil
		}
		newVal, err := cg.expandNodeMacros(n.Value)
		if err != nil {
			return nil, err
		}
		out := *n
		out.Value = newVal
		return &out, nil

	case *ast.AssignStmt:
		newVal, err := cg.expandNodeMacros(n.Value)
		if err != nil {
			return nil, err
		}
		return &ast.AssignStmt{Target: n.Target, Value: newVal}, nil

	case *ast.AugAssignStmt:
		newVal, err := cg.expandNodeMacros(n.Value)
		if err != nil {
			return nil, err
		}
		return &ast.AugAssignStmt{Target: n.Target, Op: n.Op, Value: newVal}, nil

	case *ast.IfStmt:
		newCond, err := cg.expandNodeMacros(n.Cond)
		if err != nil {
			return nil, err
		}
		newThen, err := cg.expandBlockMacros(n.Then)
		if err != nil {
			return nil, err
		}
		newElseIfs := make([]ast.ElseIfClause, len(n.ElseIfs))
		for i, ei := range n.ElseIfs {
			newEICond, err := cg.expandNodeMacros(ei.Cond)
			if err != nil {
				return nil, err
			}
			newEIBody, err := cg.expandBlockMacros(ei.Body)
			if err != nil {
				return nil, err
			}
			newElseIfs[i] = ast.ElseIfClause{Cond: newEICond, Body: newEIBody}
		}
		newElse, err := cg.expandBlockMacros(n.Else)
		if err != nil {
			return nil, err
		}
		return &ast.IfStmt{Cond: newCond, Then: newThen, ElseIfs: newElseIfs, Else: newElse}, nil

	case *ast.ForStmt:
		newInit, err := cg.expandNodeMacros(n.Init)
		if err != nil {
			return nil, err
		}
		newCond, err := cg.expandNodeMacros(n.Cond)
		if err != nil {
			return nil, err
		}
		newPost, err := cg.expandNodeMacros(n.Post)
		if err != nil {
			return nil, err
		}
		newIter, err := cg.expandNodeMacros(n.Iter)
		if err != nil {
			return nil, err
		}
		newBody, err := cg.expandBlockMacros(n.Body)
		if err != nil {
			return nil, err
		}
		out := *n
		out.Init = newInit
		out.Cond = newCond
		out.Post = newPost
		out.Iter = newIter
		out.Body = newBody
		return &out, nil

	case *ast.BinExpr:
		newLeft, err := cg.expandNodeMacros(n.Left)
		if err != nil {
			return nil, err
		}
		newRight, err := cg.expandNodeMacros(n.Right)
		if err != nil {
			return nil, err
		}
		return &ast.BinExpr{Left: newLeft, Right: newRight, Op: n.Op}, nil

	case *ast.UnaryExpr:
		newExpr, err := cg.expandNodeMacros(n.Expr)
		if err != nil {
			return nil, err
		}
		return &ast.UnaryExpr{Expr: newExpr, Op: n.Op, Post: n.Post}, nil

	case *ast.TernaryExpr:
		newCond, err := cg.expandNodeMacros(n.Cond)
		if err != nil {
			return nil, err
		}
		newThen, err := cg.expandNodeMacros(n.Then)
		if err != nil {
			return nil, err
		}
		newElse, err := cg.expandNodeMacros(n.Else)
		if err != nil {
			return nil, err
		}
		return &ast.TernaryExpr{Cond: newCond, Then: newThen, Else: newElse}, nil

	case *ast.FieldAccess:
		newExpr, err := cg.expandNodeMacros(n.Expr)
		if err != nil {
			return nil, err
		}
		out := *n
		out.Expr = newExpr
		return &out, nil

	case *ast.IndexExpr:
		newExpr, err := cg.expandNodeMacros(n.Expr)
		if err != nil {
			return nil, err
		}
		newIdx, err := cg.expandNodeMacros(n.Index)
		if err != nil {
			return nil, err
		}
		return &ast.IndexExpr{Expr: newExpr, Index: newIdx}, nil

	case *ast.TestDecl:
		newBody, err := cg.expandNodeMacros(n.Body)
		if err != nil {
			return nil, err
		}
		return &ast.TestDecl{Desc: n.Desc, Body: newBody}, nil

	default:
		return node, nil
	}
}

// expandBlockMacros is a helper that expands macros inside a *ast.Block (may be nil).
func (cg *CodeGen) expandBlockMacros(b *ast.Block) (*ast.Block, error) {
	if b == nil {
		return nil, nil
	}
	expanded, err := cg.expandNodeMacros(b)
	if err != nil {
		return nil, err
	}
	if eb, ok := expanded.(*ast.Block); ok {
		return eb, nil
	}
	return b, nil
}

// lookupMacro tries to find a macro by the full name (e.g. "square!") in cg.macros.
func (cg *CodeGen) lookupMacro(name string) *ast.MacroDecl {
	if m, ok := cg.macros[name]; ok {
		return m
	}
	// Also try stripping/adding ! to handle both "square!" and "square" keys.
	if strings.HasSuffix(name, "!") {
		base := name[:len(name)-1]
		if m, ok := cg.macros[base+"!"]; ok {
			return m
		}
		if m, ok := cg.macros[base]; ok {
			return m
		}
	}
	return nil
}

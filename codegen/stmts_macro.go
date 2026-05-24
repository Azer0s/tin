package codegen

import (
	"fmt"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) expandMacro(block *ir.Block, macro *ast.MacroDecl, args []ast.Node, callPos ast.Pos) (value.Value, error) {
	if len(args) != len(macro.Params) {
		return nil, fmt.Errorf("macro %s: expected %d args, got %d",
			macro.Name, len(macro.Params), len(args))
	}
	// Complex (block body) macros: compile and run at compile time.
	if isMacroComplex(macro) {
		resultNode, err := cg.ctfeExpandMacro(macro, args)
		if err != nil {
			return nil, err
		}

		retagMacroBody(resultNode, args, callPos)

		return cg.genExpr(block, resultNode)
	}
	// Simple expression macros: AST substitution (fast path).
	cg.progress("macro " + strings.TrimSuffix(macro.Name, "!"))

	subst := make(map[string]ast.Node, len(macro.Params))
	for i, p := range macro.Params {
		subst[p] = args[i]
	}

	body := macro.Body
	// Unwrap ExprStmt and ReturnStmt wrappers so the body is a bare expression.
	if es, ok := body.(*ast.ExprStmt); ok {
		body = es.Expr
	}

	if rs, ok := body.(*ast.ReturnStmt); ok && rs.Value != nil {
		body = rs.Value
	}

	expanded := substituteMacroNode(body, subst)
	// Backtick literal body: parse the content as tin source, substitute params, then codegen.
	if btl, ok := expanded.(*ast.BacktickLit); ok {
		node, err := parseExprString(btl.Content)
		if err != nil {
			return nil, fmt.Errorf("macro %s: backtick parse error: %w", macro.Name, err)
		}
		// Substitute params into the parsed tree (backtick was an opaque string).
		node = substituteMacroNode(node, subst)
		retagMacroBody(node, args, callPos)

		return cg.genExpr(block, node)
	}

	retagMacroBody(expanded, args, callPos)

	return cg.genExpr(block, expanded)
}

// substituteMacroNode replaces identifier nodes matching a macro parameter
// with the corresponding argument AST node.
func substituteMacroNode(node ast.Node, subst map[string]ast.Node) ast.Node {
	if node == nil {
		return nil
	}

	switch n := node.(type) {
	case *ast.Identifier:
		if replacement, ok := subst[n.Name]; ok {
			return replacement
		}

		return n
	case *ast.BinExpr:
		return &ast.BinExpr{
			Left:  substituteMacroNode(n.Left, subst),
			Right: substituteMacroNode(n.Right, subst),
			Op:    n.Op,
		}
	case *ast.UnaryExpr:
		return &ast.UnaryExpr{
			Expr: substituteMacroNode(n.Expr, subst),
			Op:   n.Op,
		}
	case *ast.CallExpr:
		newArgs := make([]ast.Node, len(n.Args))
		for i, a := range n.Args {
			newArgs[i] = substituteMacroNode(a, subst)
		}

		return &ast.CallExpr{
			Func:     substituteMacroNode(n.Func, subst),
			Args:     newArgs,
			TypeArgs: n.TypeArgs,
		}
	case *ast.FieldAccess:
		return &ast.FieldAccess{
			Expr:  substituteMacroNode(n.Expr, subst),
			Field: n.Field,
			IsPtr: n.IsPtr,
		}
	case *ast.IndexExpr:
		return &ast.IndexExpr{
			Expr:  substituteMacroNode(n.Expr, subst),
			Index: substituteMacroNode(n.Index, subst),
		}
	case *ast.TernaryExpr:
		return &ast.TernaryExpr{
			Cond: substituteMacroNode(n.Cond, subst),
			Then: substituteMacroNode(n.Then, subst),
			Else: substituteMacroNode(n.Else, subst),
		}
	case *ast.ExprStmt:
		return &ast.ExprStmt{Expr: substituteMacroNode(n.Expr, subst)}
	case *ast.ReturnStmt:
		if n.Value != nil {
			return &ast.ReturnStmt{Value: substituteMacroNode(n.Value, subst)}
		}

		return n
	case *ast.Block:
		// Copy the original to preserve the embedded `base` (position
		// info).  Building from scratch with a struct literal would
		// drop the span and any diagnostic later raised against the
		// substituted body would point at (0,0).
		out := *n
		out.Stmts = make([]ast.Node, len(n.Stmts))

		for i, s := range n.Stmts {
			out.Stmts[i] = substituteMacroNode(s, subst)
		}

		return &out
	case *ast.MatchStmt:
		out := *n
		out.Expr = substituteMacroNode(n.Expr, subst)
		out.Cases = make([]ast.MatchCase, len(n.Cases))

		for i, c := range n.Cases {
			var body *ast.Block
			if b, ok := substituteMacroNode(c.Body, subst).(*ast.Block); ok {
				body = b
			}

			out.Cases[i] = ast.MatchCase{
				Pattern: substituteMacroNode(c.Pattern, subst),
				Guard:   substituteMacroNode(c.Guard, subst),
				VarName: c.VarName,
				Body:    body,
			}
		}

		if n.Default != nil {
			if b, ok := substituteMacroNode(n.Default, subst).(*ast.Block); ok {
				out.Default = b
			}
		}

		return &out
	case *ast.IfStmt:
		out := *n
		out.Cond = substituteMacroNode(n.Cond, subst)

		if b, ok := substituteMacroNode(n.Then, subst).(*ast.Block); ok {
			out.Then = b
		}

		if n.Else != nil {
			if b, ok := substituteMacroNode(n.Else, subst).(*ast.Block); ok {
				out.Else = b
			}
		}

		out.ElseIfs = make([]ast.ElseIfClause, len(n.ElseIfs))

		for i, ei := range n.ElseIfs {
			var body *ast.Block
			if b, ok := substituteMacroNode(ei.Body, subst).(*ast.Block); ok {
				body = b
			}

			out.ElseIfs[i] = ast.ElseIfClause{
				Cond: substituteMacroNode(ei.Cond, subst),
				Body: body,
			}
		}

		return &out
	case *ast.VarDecl:
		out := *n
		out.Value = substituteMacroNode(n.Value, subst)

		return &out
	case *ast.AssignStmt:
		out := *n
		out.Target = substituteMacroNode(n.Target, subst)
		out.Value = substituteMacroNode(n.Value, subst)

		return &out
	}

	return node
}

// markOutParamVarsHeapOwned marks variables passed as &varName (address-of) to
// a call that has N*S (N>=2) write-back parameters. After such a call, varName
// may hold a heap-allocated borrow wrapper (depth-1 chain), so it must be
// released at scope exit. We mark isHeapOwned=true and heapOwnedDepth=N-1.
//
// This fixes the leak where a void-returning extern with **S out-params writes
// a new RC-allocated borrow wrapper into the caller's *S variable, but the scope
// release would skip it because the void function was never in heapPromotingFns.

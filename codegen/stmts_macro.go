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
		// Inside backticks, param substitution happens ONLY through
		// the explicit `{paramname}` (or `{expr-using-param}`) form.
		// Bare `n` in the backtick body is left untouched and will
		// surface as an undefined-identifier error at codegen, so
		// authors can't accidentally treat the backtick like an
		// expression body.  `{n * 4000}` evaluates the substitution
		// (and falls through to constant folding) at codegen time.
		content := interpolateMacroBacktick(btl.Content, macro.Params, args)

		node, err := parseExprString(content)
		if err != nil {
			return nil, fmt.Errorf("macro %s: backtick parse error: %w", macro.Name, err)
		}

		retagMacroBody(node, args, callPos)

		return cg.genExpr(block, node)
	}

	retagMacroBody(expanded, args, callPos)

	return cg.genExpr(block, expanded)
}

// interpolateMacroBacktick walks the backtick body string and replaces
// every `{expr}` segment with `expr-with-paramnames-substituted` as
// rendered tin source.  Each param name inside the brace span is
// swapped for the corresponding argument's PrintExpr form before the
// surrounding context is reassembled.  The result is a tin-source
// string that the macro-expansion pipeline reparses and codegens.
//
// `{` and `}` outside of an identifier-bearing region are passed
// through unchanged (e.g. struct-lit braces in `Value{re: 0.0,
// im: {n} as f64}` survive).  Only `{` immediately followed by a
// valid expression chunk before its matching `}` is treated as an
// interpolation.  Nested braces are not supported and would surface as
// a parse error from the eventual reparse.
func interpolateMacroBacktick(content string, params []string, args []ast.Node) string {
	subst := map[string]string{}

	for i, p := range params {
		if i < len(args) {
			subst[p] = ast.PrintExpr(args[i])
		}
	}

	var out strings.Builder

	i := 0

	isIdentCont := func(b byte) bool {
		return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') ||
			(b >= '0' && b <= '9') || b == '_'
	}

	for i < len(content) {
		c := content[i]
		if c != '{' {
			out.WriteByte(c)

			i++

			continue
		}
		// Struct-literal / block braces are preceded by an identifier
		// character (`Value{...}`, `T{...}`).  Interpolation braces
		// stand alone (`{n}` after whitespace or punctuation).  Use
		// the preceding char to disambiguate.
		if i > 0 && isIdentCont(content[i-1]) {
			out.WriteByte(c)

			i++

			continue
		}
		// Scan to the matching `}` at depth 0.
		depth := 1
		j := i + 1

		for j < len(content) && depth > 0 {
			switch content[j] {
			case '{':
				depth++
			case '}':
				depth--
			}

			if depth == 0 {
				break
			}

			j++
		}

		if depth != 0 || j >= len(content) {
			// Unbalanced; pass through verbatim.
			out.WriteByte(c)

			i++

			continue
		}
		// content[i+1:j] is the inner expression.
		inner := content[i+1 : j]
		out.WriteString(substituteParamsInSource(inner, subst))

		i = j + 1
	}

	return out.String()
}

// substituteParamsInSource scans `src` for whole-word identifiers
// matching a key in `subst` and replaces them with the mapped source.
// Identifiers inside string literals or back-tick spans are left
// alone.  Used by interpolateMacroBacktick to resolve `{n * 4000}`
// into `4 * 4000` when n=4.
func substituteParamsInSource(src string, subst map[string]string) string {
	var out strings.Builder

	isIdentStart := func(b byte) bool {
		return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_'
	}
	isIdentCont := func(b byte) bool {
		return isIdentStart(b) || (b >= '0' && b <= '9')
	}

	i := 0

	for i < len(src) {
		c := src[i]
		// Skip string literals so identifiers inside them stay literal.
		if c == '"' {
			out.WriteByte(c)

			i++

			for i < len(src) && src[i] != '"' {
				if src[i] == '\\' && i+1 < len(src) {
					out.WriteByte(src[i])
					out.WriteByte(src[i+1])

					i += 2

					continue
				}

				out.WriteByte(src[i])

				i++
			}

			if i < len(src) {
				out.WriteByte(src[i])

				i++
			}

			continue
		}

		if !isIdentStart(c) {
			out.WriteByte(c)

			i++

			continue
		}

		j := i + 1
		for j < len(src) && isIdentCont(src[j]) {
			j++
		}

		ident := src[i:j]
		if repl, ok := subst[ident]; ok {
			out.WriteString(repl)
		} else {
			out.WriteString(ident)
		}

		i = j
	}

	return out.String()
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

// genSuffixCall is the codegen path for a SuffixCallExpr produced by
// the parser when it sees `<literal>_<suffix>` (optionally with
// `(extras...)` and a `!`). Resolves the suffix to a `#suffix@<kind>`
// macro, builds args = [literal, ...extras], dispatches to
// expandMacro, and emits the result.
func (cg *CodeGen) genSuffixCall(block *ir.Block, s *ast.SuffixCallExpr) (value.Value, error) {
	macro := cg.lookupSuffixMacro(s.SuffixName)
	if macro == nil {
		return nil, cg.nodeErr(s,
			"no suffix macro `%s` in scope; import it with `use { %s } from ...` or use the fully-qualified `<lit>_pkg::%s` form",
			s.SuffixName, strings.TrimSuffix(s.SuffixName, "!"), s.SuffixName)
	}

	kind := suffixLiteralKind(s.Literal)
	if kind == "" {
		return nil, cg.nodeErr(s, "suffix macro `%s` applied to non-literal expression", s.SuffixName)
	}

	if !macroAcceptsSuffixKind(macro, kind) {
		return nil, cg.nodeErr(s,
			"suffix macro `%s` does not accept @%s literals; declared kinds: %s",
			s.SuffixName, kind, macroDeclaredSuffixKinds(macro))
	}

	args := append([]ast.Node{s.Literal}, s.ExtraArgs...)
	if len(args) != len(macro.Params) {
		return nil, cg.nodeErr(s,
			"suffix macro `%s` expects %d arg(s) (1 literal + %d extras), got %d total",
			s.SuffixName, len(macro.Params), len(macro.Params)-1, len(args))
	}

	return cg.expandMacro(block, macro, args, s.Pos())
}

// markOutParamVarsHeapOwned marks variables passed as &varName (address-of) to
// a call that has N*S (N>=2) write-back parameters. After such a call, varName
// may hold a heap-allocated borrow wrapper (depth-1 chain), so it must be
// released at scope exit. We mark isHeapOwned=true and heapOwnedDepth=N-1.
//
// This fixes the leak where a void-returning extern with **S out-params writes
// a new RC-allocated borrow wrapper into the caller's *S variable, but the scope
// release would skip it because the void function was never in heapPromotingFns.

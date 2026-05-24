package parser

import (
	"strconv"
	"strings"

	"github.com/Azer0s/tin/ast"
	"github.com/Azer0s/tin/lexer"
)

func (p *Parser) parseLambda() (*ast.LambdaExpr, error) {
	if _, err := p.expect(lexer.KW_FN); err != nil {
		return nil, err
	}

	// Optional inline tags: fn{#async} / fn{#pure} etc.  Mirrors
	// parseFuncDecl so lambdas can carry the same control tags as
	// named fns.  #async on a lambda asks codegen to emit a real
	// $coro variant alongside the sync + colored bodies, so a
	// spawned lambda cooperates at its own coloring points rather
	// than going through the synth coro wrapper that targets slot 1.
	var tags []string
	if p.check(lexer.LBRACE) {
		tags = p.parseTags()
	}

	typeParams, _ := p.parseTypeParams()

	params, err := p.parseParams()
	if err != nil {
		return nil, err
	}

	var retType ast.TypeExpr
	if !p.match(lexer.ASSIGN, lexer.NEWLINE, lexer.EOF, lexer.COMMA, lexer.RPAREN) {
		retType, err = p.parseTypeExpr()
		if err != nil {
			return nil, err
		}
	}

	var body ast.Node

	if p.check(lexer.ASSIGN) {
		p.advance()

		body, err = p.parseFuncBody()
		if err != nil {
			return nil, err
		}
	}

	return &ast.LambdaExpr{TypeParams: typeParams, Params: params, RetType: retType, Body: body, Tags: tags}, nil
}

func (p *Parser) parseArrayLit() (ast.Node, error) {
	p.advance() // consume [
	p.skipWhitespace()

	// Empty array.
	if p.check(lexer.RBRACKET) {
		p.advance()

		return &ast.ArrayLit{Elems: nil}, nil
	}

	// Parse first element.
	first, err := p.parseExpr()
	if err != nil {
		return nil, err
	}

	// Fill syntax: [value; count] - fill array with `count` copies of `value`.
	if p.check(lexer.SEMI) {
		p.advance() // consume ;

		countTok, err2 := p.expect(lexer.INT_LIT)
		if err2 != nil {
			return nil, err2
		}

		count, _ := strconv.Atoi(countTok.Literal)

		if _, err3 := p.expect(lexer.RBRACKET); err3 != nil {
			return nil, err3
		}

		return &ast.ArrayFillLit{Value: first, Count: count}, nil
	}

	// Regular array literal: collect remaining elements.
	elems := []ast.Node{first}

	if p.check(lexer.COMMA) {
		p.advance()
	}

	p.skipWhitespace()

	for !p.check(lexer.RBRACKET) && !p.check(lexer.EOF) {
		elem, err2 := p.parseExpr()
		if err2 != nil {
			return nil, err2
		}

		elems = append(elems, elem)

		if p.check(lexer.COMMA) {
			p.advance()
		}

		p.skipWhitespace()
	}

	if _, err := p.expect(lexer.RBRACKET); err != nil {
		return nil, err
	}

	return &ast.ArrayLit{Elems: elems}, nil
}

// indexExprTypeName returns the base type name from an IndexExpr whose Expr is
// an Identifier or ScopeAccess (i.e. a generic struct type). The second return
// value is false if the Expr is not a plain type name (e.g. an array variable).
func (p *Parser) indexExprTypeName(idx *ast.IndexExpr) (string, bool) {
	switch inner := idx.Expr.(type) {
	case *ast.Identifier:
		return inner.Name, true
	case *ast.ScopeAccess:
		return strings.Join(inner.Path, "::"), true
	}

	return "", false
}

// indexExprTypeArgs converts the Index of an IndexExpr (which encodes type
// args as a comma-separated Identifier string for the simple case, or as a
// nested IndexExpr / ScopeAccess for nested generics) into a []ast.TypeExpr.
func (p *Parser) indexExprTypeArgs(idx *ast.IndexExpr) []ast.TypeExpr {
	switch ix := idx.Index.(type) {
	case *ast.Identifier:
		parts := strings.Split(ix.Name, ",")
		result := make([]ast.TypeExpr, 0, len(parts))

		for _, part := range parts {
			result = append(result, &ast.SimpleType{Name: strings.TrimSpace(part)})
		}

		return result
	}

	// Single non-Identifier type arg: convert via the generic node->type-expr
	// path (covers nested IndexExpr like `G[i64]` and qualified names).
	if te := typeNodeToTypeExpr(idx.Index); te != nil {
		return []ast.TypeExpr{te}
	}

	return nil
}

// typeNodeToString converts an AST node that names a type (Identifier,
// ScopeAccess, IndexExpr) into its source-level string. Nested generics
// like `G[i64]` round-trip as `G[i64]`. Returns "" if the node isn't a
// recognized type-naming shape.
func typeNodeToString(n ast.Node) string {
	switch v := n.(type) {
	case *ast.Identifier:
		return v.Name
	case *ast.ScopeAccess:
		return strings.Join(v.Path, "::")
	case *ast.IndexExpr:
		base := typeNodeToString(v.Expr)
		if base == "" {
			return ""
		}

		argStr := typeNodeToString(v.Index)
		if argStr == "" {
			return ""
		}

		return base + "[" + argStr + "]"
	case *ast.TypeRefNode:
		// Source-syntax-like fn-type encoding so the round trip survives
		// IR mangling: `fn(p1,p2,...)ret`.  Mirrors typeExprCanonicalKey
		// in the codegen so a parser-emitted key matches the codegen's
		// canonical form when the AST is later re-resolved.
		return typeExprSourceForm(v.Type)
	}

	return ""
}

// typeExprSourceForm produces a source-syntax key string for a TypeExpr.
// Parser-side mirror of cg.typeExprCanonicalKey for the cases the parser
// needs to emit (currently only FuncType plus its nested type composition).
// Stays in lockstep with the codegen encoding so a key from either side
// decodes the same way in parseTypeParamStr.
func typeExprSourceForm(te ast.TypeExpr) string {
	switch t := te.(type) {
	case nil:
		return ""
	case *ast.SimpleType:
		return t.Name
	case *ast.PointerType:
		return "*" + typeExprSourceForm(t.Elem)
	case *ast.ArrayType:
		if t.Size < 0 {
			return "[]" + typeExprSourceForm(t.Elem)
		}

		return ""
	case *ast.GenericType:
		parts := make([]string, len(t.TypeParams))
		for i, tp := range t.TypeParams {
			parts[i] = typeExprSourceForm(tp)
		}

		return t.Name + "[" + strings.Join(parts, ",") + "]"
	case *ast.FuncType:
		parts := make([]string, len(t.Params))
		for i, p := range t.Params {
			parts[i] = typeExprSourceForm(p)
		}

		prefix := "fn"
		if t.IsAsync {
			prefix = "fn#async"
		}

		out := prefix + "(" + strings.Join(parts, ",") + ")"

		if t.RetType != nil {
			if _, isVoid := t.RetType.(*ast.VoidType); !isVoid {
				out += typeExprSourceForm(t.RetType)
			}
		}

		return out
	}

	return ""
}

// typeNodeToTypeExpr lifts a type-shaped AST node (as parsed in expression
// position) into a TypeExpr. Mirrors typeNodeToString but produces the
// structured type rather than its source form.
func typeNodeToTypeExpr(n ast.Node) ast.TypeExpr {
	switch v := n.(type) {
	case *ast.Identifier:
		return &ast.SimpleType{Name: v.Name}
	case *ast.ScopeAccess:
		return &ast.SimpleType{Name: strings.Join(v.Path, "::")}
	case *ast.TypeRefNode:
		return v.Type
	case *ast.IndexExpr:
		baseName := ""

		switch be := v.Expr.(type) {
		case *ast.Identifier:
			baseName = be.Name
		case *ast.ScopeAccess:
			baseName = strings.Join(be.Path, "::")
		}

		if baseName == "" {
			return nil
		}

		params := []ast.TypeExpr{}
		// Multi-arg encoding: comma-joined identifier.
		if argID, ok := v.Index.(*ast.Identifier); ok && strings.Contains(argID.Name, ",") {
			for _, part := range strings.Split(argID.Name, ",") {
				params = append(params, &ast.SimpleType{Name: strings.TrimSpace(part)})
			}
		} else {
			inner := typeNodeToTypeExpr(v.Index)
			if inner == nil {
				return nil
			}

			params = append(params, inner)
		}

		return &ast.GenericType{Name: baseName, TypeParams: params}
	}

	return nil
}

func (p *Parser) parseStructLit(typeName string) (ast.Node, error) {
	pos := p.curPos()
	p.advance() // consume {
	p.skipWhitespace()

	lit := &ast.StructLit{TypeName: typeName}
	lit.SetPos(pos)

	for !p.check(lexer.RBRACE) && !p.check(lexer.EOF) {
		// Named: "name: value" or positional: "value"
		if p.check(lexer.IDENT) && p.peekAt(1).Type == lexer.COLON {
			field := p.advance().Literal
			p.advance() // :

			val, err := p.parseExpr()
			if err != nil {
				return nil, err
			}

			lit.Fields = append(lit.Fields, ast.StructLitField{Name: field, Value: val})
		} else {
			val, err := p.parseExpr()
			if err != nil {
				return nil, err
			}

			lit.Positional = append(lit.Positional, val)
		}

		if p.check(lexer.COMMA) {
			p.advance()
		}

		p.skipWhitespace()
	}

	if _, err := p.expect(lexer.RBRACE); err != nil {
		return nil, err
	}

	return lit, nil
}

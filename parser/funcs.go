package parser

import (
	"fmt"

	"github.com/Azer0s/tin/ast"
	"github.com/Azer0s/tin/lexer"
)

// Function declaration

func (p *Parser) parseFuncDecl(tags []string, isStatic bool) (*ast.FuncDecl, error) {
	pos := p.curPos()
	if _, err := p.expect(lexer.KW_FN); err != nil {
		return nil, err
	}

	// Optional inline tags: fn{#pure #recurse}
	if p.check(lexer.LBRACE) {
		moreTags := p.parseTags()
		tags = append(tags, moreTags...)
	}

	// Optional name (lambdas are anonymous)
	// Forms:
	//   fn name(...)            – regular method
	//   fn ::name(...)          – alias-trait implementation
	//   fn trait::method(...)   – qualified trait-method implementation
	//   fn trait[T]::method(...)– generic qualified implementation
	var name string
	var traitQualifier string
	if p.check(lexer.DCOLON) {
		p.advance() // consume ::
		// Alias-trait impl: ::traitName (may carry type arg: ::implicit[[char]])
		if p.check(lexer.IDENT) {
			name = p.advance().Literal
		}
	} else if p.check(lexer.IDENT) {
		saved := p.pos
		candidate := p.advance().Literal
		// Collect optional type arguments: [T, ...]
		typeArgStr := ""
		if p.check(lexer.LBRACKET) {
			start := p.pos
			depth := 0
			for p.pos < len(p.tokens) {
				t := p.tokens[p.pos]
				if t.Type == lexer.LBRACKET {
					depth++
				} else if t.Type == lexer.RBRACKET {
					depth--
					if depth == 0 {
						p.pos++ // consume ]
						break
					}
				} else if t.Type == lexer.EOF {
					break
				}
				p.pos++
			}
			for i := start; i < p.pos; i++ {
				typeArgStr += p.tokens[i].Literal
			}
		}
		if p.check(lexer.DCOLON) {
			// traitname[args]::method
			p.advance() // consume ::
			if typeArgStr != "" {
				traitQualifier = candidate + typeArgStr
			} else {
				traitQualifier = candidate
			}
			if p.check(lexer.IDENT) {
				name = p.advance().Literal
			}
		} else {
			// plain name - restore and re-read cleanly
			p.pos = saved
			name = p.advance().Literal
		}
	}

	// Optional generic type params [t, r]
	typeParams, _ := p.parseTypeParams()

	// Parameters
	params, err := p.parseParams()
	if err != nil {
		return nil, err
	}

	// Return type (optional)
	var retType ast.TypeExpr
	if !p.match(lexer.ASSIGN, lexer.NEWLINE, lexer.EOF, lexer.COMMA, lexer.RPAREN, lexer.KW_WHERE) {
		retType, err = p.parseTypeExpr()
		if err != nil {
			return nil, err
		}
	}

	// Generic type constraints: "where t is Labeled+Sized, r is Printable"
	// Appear BEFORE the `=` body separator.  One `where` keyword may be
	// followed by multiple comma-separated bindings; multiple `where` keywords
	// are also accepted for readability
	var constraints []ast.TypeConstraint
	parseOneConstraint := func() bool {
		if !(p.check(lexer.IDENT) && p.peekAt(1).Type == lexer.KW_IS) {
			return false
		}
		typeParam := p.advance().Literal // e.g. "t"
		p.advance()                      // consume "is"
		var traits []ast.TypeExpr
		// Each trait may be a simple name or a generic like iter[i64]
		if isTypeToken(p.peek()) || p.check(lexer.IDENT) {
			te, err2 := p.parseTypeSingle()
			if err2 == nil {
				traits = append(traits, te)
			}
			for p.check(lexer.PLUS) {
				p.advance() // consume +
				if isTypeToken(p.peek()) || p.check(lexer.IDENT) {
					te2, err3 := p.parseTypeSingle()
					if err3 == nil {
						traits = append(traits, te2)
					}
				}
			}
		}
		constraints = append(constraints, ast.TypeConstraint{TypeParam: typeParam, Traits: traits})
		return true
	}
	for p.check(lexer.KW_WHERE) {
		saved := p.pos
		p.advance() // consume "where"
		if !parseOneConstraint() {
			p.pos = saved
			break
		}
		// Additional constraints after commas (still in the same `where` clause)
		for p.check(lexer.COMMA) {
			p.advance() // consume ","
			if !parseOneConstraint() {
				break
			}
		}
	}

	// extern body: = extern("symbol")  OR  virtual marker: = virtual
	if p.check(lexer.ASSIGN) {
		p.advance()
		if p.check(lexer.KW_VIRTUAL) {
			p.advance() // consume "virtual"
			return &ast.FuncDecl{
				Name: name, TraitQualifier: traitQualifier,
				TypeParams: typeParams, Constraints: constraints, Params: params,
				RetType: retType, Tags: tags, IsStatic: isStatic,
				IsVirtual: true,
			}, nil
		}
		if p.check(lexer.KW_EXTERN) {
			p.advance()
			if _, err := p.expect(lexer.LPAREN); err != nil {
				return nil, err
			}
			symTok, err := p.expect(lexer.STRING_LIT)
			if err != nil {
				return nil, fmt.Errorf("extern symbol must be a string literal, e.g. extern(\"name\")")
			}
			sym := symTok.Literal
			if _, err := p.expect(lexer.RPAREN); err != nil {
				return nil, err
			}
			return &ast.FuncDecl{
				Name: name, TraitQualifier: traitQualifier,
				TypeParams: typeParams, Constraints: constraints, Params: params,
				RetType: retType, Tags: tags, IsStatic: isStatic,
				IsExtern: sym,
			}, nil
		}

		body, err := p.parseFuncBody()
		if err != nil {
			return nil, err
		}
		_ = pos
		return &ast.FuncDecl{
			Name: name, TraitQualifier: traitQualifier,
			TypeParams: typeParams, Constraints: constraints, Params: params,
			RetType: retType, Body: body, Tags: tags, IsStatic: isStatic,
		}, nil
	}

	// fn with no body (forward declaration / extern / trait virtual)
	_ = pos
	return &ast.FuncDecl{
		Name: name, TraitQualifier: traitQualifier,
		TypeParams: typeParams, Constraints: constraints, Params: params,
		RetType: retType, Tags: tags, IsStatic: isStatic,
	}, nil
}

// parseFuncBody parses the body after the `=` sign
// It has already been consumed by the caller
// Handles: single-line expr, indented block, indented where list
func (p *Parser) parseFuncBody() (ast.Node, error) {
	if p.check(lexer.NEWLINE) {
		p.advance() // consume NEWLINE
		p.skipNewlines()
		if p.check(lexer.INDENT) {
			// Peek inside to determine block vs where-list
			if p.peekAt(1).Type == lexer.KW_WHERE {
				return p.parseWhereBlock()
			}
			return p.parseBlock()
		}
		return &ast.Block{}, nil
	}
	// Single expression / statement on same line - may be SEMI-separated multi-statement
	first, err := p.parseStatement()
	if err != nil {
		return nil, err
	}
	if !p.check(lexer.SEMI) {
		return first, nil
	}
	// Multiple statements separated by semicolons on one line: wrap in a Block
	var stmts []ast.Node
	if first != nil {
		stmts = append(stmts, first)
	}
	for p.check(lexer.SEMI) {
		p.advance() // consume SEMI
		if p.check(lexer.NEWLINE) || p.check(lexer.EOF) || p.check(lexer.DEDENT) {
			break
		}
		stmt, err2 := p.parseStatement()
		if err2 != nil {
			return nil, err2
		}
		if stmt != nil {
			stmts = append(stmts, stmt)
		}
	}
	return &ast.Block{Stmts: stmts}, nil
}

// parseWhereBlock consumes INDENT, parses where clauses, consumes DEDENT
func (p *Parser) parseWhereBlock() (*ast.WhereList, error) {
	if _, err := p.expect(lexer.INDENT); err != nil {
		return nil, err
	}
	wl := &ast.WhereList{}
	p.skipNewlines()
	for p.check(lexer.KW_WHERE) {
		wc, err := p.parseWhereClause()
		if err != nil {
			return nil, err
		}
		wl.Clauses = append(wl.Clauses, wc)
		p.skipNewlines()
	}
	if p.check(lexer.DEDENT) {
		p.advance()
	}
	return wl, nil
}

func (p *Parser) parseWhereClause() (ast.WhereClause, error) {
	pos := p.curPos()
	if _, err := p.expect(lexer.KW_WHERE); err != nil {
		return ast.WhereClause{}, err
	}

	var cond ast.Node
	// Wildcard _
	if p.check(lexer.IDENT) && p.peek().Literal == "_" {
		p.advance()
		cond = nil // wildcard
	} else if p.check(lexer.ATOM_LIT) {
		cond = &ast.AtomLit{Name: p.advance().Literal}
	} else {
		var err error
		cond, err = p.parseExpr()
		if err != nil {
			return ast.WhereClause{}, err
		}
	}

	if _, err := p.expect(lexer.COLON); err != nil {
		return ast.WhereClause{}, err
	}

	var body ast.Node
	var err error
	if p.check(lexer.NEWLINE) {
		p.advance()
		p.skipNewlines()
		if p.check(lexer.INDENT) {
			body, err = p.parseBlock()
		} else {
			body = &ast.Block{}
		}
	} else {
		body, err = p.parseStatement()
	}
	if err != nil {
		return ast.WhereClause{}, err
	}
	return ast.WhereClause{Pos: pos, Cond: cond, Body: body}, nil
}

// Block parsing

// parseBlock consumes INDENT, zero or more statements, then DEDENT
func (p *Parser) parseBlock() (*ast.Block, error) {
	pos := p.curPos()
	if _, err := p.expect(lexer.INDENT); err != nil {
		return nil, err
	}
	b := &ast.Block{}
	_ = pos
	p.skipSemisAndNewlines()
	for !p.check(lexer.DEDENT) && !p.check(lexer.EOF) {
		stmt, err := p.parseStatement()
		if err != nil {
			return nil, err
		}
		if stmt != nil {
			b.Stmts = append(b.Stmts, stmt)
		}
		p.skipSemisAndNewlines()
	}
	if p.check(lexer.DEDENT) {
		p.advance()
	}
	return b, nil
}

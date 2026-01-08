// Package parser implements a recursive-descent parser for the tin language.
// It consumes the INDENT/DEDENT token stream produced by the lexer and
// builds the AST defined in the ast package.
package parser

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Azer0s/tin/ast"
	"github.com/Azer0s/tin/lexer"
)

// Parser holds the token stream and current position.
type Parser struct {
	tokens []lexer.Token
	pos    int
}

// New creates a Parser over the given token slice.
func New(tokens []lexer.Token) *Parser {
	return &Parser{tokens: tokens}
}

// ── Navigation helpers ────────────────────────────────────────────────────────

func (p *Parser) peek() lexer.Token {
	if p.pos >= len(p.tokens) {
		return lexer.Token{Type: lexer.EOF}
	}
	return p.tokens[p.pos]
}

func (p *Parser) peekAt(offset int) lexer.Token {
	i := p.pos + offset
	if i < 0 || i >= len(p.tokens) {
		return lexer.Token{Type: lexer.EOF}
	}
	return p.tokens[i]
}

func (p *Parser) advance() lexer.Token {
	tok := p.peek()
	if p.pos < len(p.tokens) {
		p.pos++
	}
	return tok
}

func (p *Parser) check(t lexer.TokenType) bool { return p.peek().Type == t }

func (p *Parser) match(ts ...lexer.TokenType) bool {
	for _, t := range ts {
		if p.check(t) {
			return true
		}
	}
	return false
}

func (p *Parser) expect(t lexer.TokenType) (lexer.Token, error) {
	tok := p.peek()
	if tok.Type != t {
		return tok, fmt.Errorf("expected %s, got %s (%q) at %d:%d",
			t, tok.Type, tok.Literal, tok.Line, tok.Col)
	}
	return p.advance(), nil
}

func (p *Parser) skipNewlines() {
	for p.check(lexer.NEWLINE) {
		p.advance()
	}
}

// skipWhitespace skips NEWLINE, INDENT, and DEDENT tokens.
// Use this inside parenthesised lists where indentation is not significant.
func (p *Parser) skipWhitespace() {
	for p.match(lexer.NEWLINE, lexer.INDENT, lexer.DEDENT) {
		p.advance()
	}
}

func (p *Parser) curPos() ast.Pos {
	t := p.peek()
	return ast.Pos{Line: t.Line, Col: t.Col}
}

func (p *Parser) errorf(f string, a ...any) error {
	t := p.peek()
	return fmt.Errorf(f+" (at %d:%d)", append(a, t.Line, t.Col)...)
}

// ── Entry point ───────────────────────────────────────────────────────────────

// Parse builds and returns the complete AST for the token stream.
func (p *Parser) Parse() (*ast.Program, error) {
	prog := &ast.Program{}
	p.skipNewlines()
	for !p.check(lexer.EOF) {
		node, err := p.parseTopLevel()
		if err != nil {
			return nil, err
		}
		if node != nil {
			prog.Stmts = append(prog.Stmts, node)
		}
		p.skipNewlines()
	}
	return prog, nil
}

// ── Top-level declarations ────────────────────────────────────────────────────

func (p *Parser) parseTopLevel() (ast.Node, error) {
	// Collect leading control tags: fn{#pure #recurse} …
	tags := p.parseTags()

	switch p.peek().Type {
	case lexer.KW_FN:
		return p.parseFuncDecl(tags, false)
	case lexer.KW_MACRO:
		return p.parseMacroDecl(tags)
	case lexer.KW_STRUCT:
		return p.parseStructDecl(tags)
	case lexer.KW_TRAIT:
		return p.parseTraitDecl()
	case lexer.KW_TYPE:
		return p.parseTypeDecl()
	case lexer.KW_ENUM:
		return p.parseEnumDecl()
	case lexer.KW_UNION:
		return p.parseUnionDecl()
	case lexer.KW_DATA:
		return p.parseDataDecl()
	case lexer.KW_USE:
		return p.parseUseDecl()
	case lexer.KW_EXPORT:
		return p.parseExportDecl()
	case lexer.KW_STATIC:
		p.advance()
		return p.parseFuncDecl(tags, true)
	default:
		return p.parseStatement()
	}
}

// parseTags consumes optional {#tag …} before a declaration keyword.
func (p *Parser) parseTags() []string {
	var tags []string
	// Two forms: fn{#pure} or just leading control tags on the fn line
	if p.check(lexer.LBRACE) {
		// Peek ahead to see if this is a {#tag} block
		saved := p.pos
		p.advance() // consume {
		if p.check(lexer.CONTROL_TAG) {
			for p.check(lexer.CONTROL_TAG) {
				tags = append(tags, p.advance().Literal)
				// optional @fn / @field qualifier
				if p.check(lexer.AT) {
					p.advance()
					p.advance() // qualifier ident
				}
			}
			if p.check(lexer.RBRACE) {
				p.advance()
			}
		} else {
			p.pos = saved // not a tag block – restore
		}
	}
	return tags
}

// ── Function declaration ──────────────────────────────────────────────────────

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

	// Optional name (lambdas are anonymous).
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
			// plain name — restore and re-read cleanly
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
	// are also accepted for readability.
	var constraints []ast.TypeConstraint
	parseOneConstraint := func() bool {
		if !(p.check(lexer.IDENT) && p.peekAt(1).Type == lexer.KW_IS) {
			return false
		}
		typeParam := p.advance().Literal // e.g. "t"
		p.advance()                      // consume "is"
		var traits []ast.TypeExpr
		// Each trait may be a simple name or a generic like iter[i64].
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
		// Additional constraints after commas (still in the same `where` clause).
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

// parseFuncBody parses the body after the `=` sign.
// It has already been consumed by the caller.
// Handles: single-line expr, indented block, indented where list.
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
	// Single expression / statement on same line
	return p.parseStatement()
}

// parseWhereBlock consumes INDENT, parses where clauses, consumes DEDENT.
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

// ── Block parsing ─────────────────────────────────────────────────────────────

// parseBlock consumes INDENT, zero or more statements, then DEDENT.
func (p *Parser) parseBlock() (*ast.Block, error) {
	pos := p.curPos()
	if _, err := p.expect(lexer.INDENT); err != nil {
		return nil, err
	}
	b := &ast.Block{}
	_ = pos
	p.skipNewlines()
	for !p.check(lexer.DEDENT) && !p.check(lexer.EOF) {
		stmt, err := p.parseStatement()
		if err != nil {
			return nil, err
		}
		if stmt != nil {
			b.Stmts = append(b.Stmts, stmt)
		}
		p.skipNewlines()
	}
	if p.check(lexer.DEDENT) {
		p.advance()
	}
	return b, nil
}

// ── Statements ────────────────────────────────────────────────────────────────

func (p *Parser) parseStatement() (ast.Node, error) {
	tags := p.parseTags()
	_ = tags

	switch p.peek().Type {
	case lexer.KW_LET, lexer.KW_CONST:
		return p.parseVarDecl()
	case lexer.KW_FN:
		return p.parseFuncDecl(tags, false)
	case lexer.KW_STRUCT:
		return p.parseStructDecl(tags)
	case lexer.KW_TYPE:
		return p.parseTypeDecl()
	case lexer.KW_ENUM:
		return p.parseEnumDecl()
	case lexer.KW_RETURN:
		return p.parseReturnStmt()
	case lexer.KW_BREAK:
		p.advance()
		return &ast.BreakStmt{}, nil
	case lexer.KW_DEFER:
		return p.parseDeferStmt()
	case lexer.KW_IF:
		return p.parseIfStmt()
	case lexer.KW_FOR:
		return p.parseForStmt()
	case lexer.KW_MATCH:
		return p.parseMatchStmt()
	case lexer.KW_ECHO:
		return p.parseEchoStmt()
	case lexer.KW_USE:
		return p.parseUseDecl()
	case lexer.KW_EXPORT:
		return p.parseExportDecl()
	case lexer.KW_WHERE:
		wc, err := p.parseWhereClause()
		if err != nil {
			return nil, err
		}
		return &ast.WhereList{Clauses: []ast.WhereClause{wc}}, nil
	case lexer.LBRACE:
		// { #tag } { body }  tagged block
		if p.check(lexer.LBRACE) && p.peekAt(1).Type == lexer.CONTROL_TAG {
			return p.parseTaggedBlock()
		}
		return p.parseExprStatement()
	case lexer.NEWLINE, lexer.DEDENT:
		return nil, nil
	default:
		return p.parseExprStatement()
	}
}

func (p *Parser) parseVarDecl() (*ast.VarDecl, error) {
	pos := p.curPos()
	isConst := p.peek().Type == lexer.KW_CONST
	p.advance() // consume let/const

	nameTok, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, err
	}

	// Optional type annotation
	var typ ast.TypeExpr
	if !p.match(lexer.ASSIGN, lexer.NEWLINE, lexer.EOF, lexer.SEMI) {
		typ, err = p.parseTypeExpr()
		if err != nil {
			return nil, err
		}
	}

	// Optional initializer
	var val ast.Node
	if p.check(lexer.ASSIGN) {
		p.advance()
		val, err = p.parseExpr()
		if err != nil {
			return nil, err
		}
	}
	_ = pos
	return &ast.VarDecl{Name: nameTok.Literal, Type: typ, Value: val, IsConst: isConst}, nil
}

func (p *Parser) parseReturnStmt() (*ast.ReturnStmt, error) {
	p.advance() // consume return
	if p.match(lexer.NEWLINE, lexer.DEDENT, lexer.EOF) {
		return &ast.ReturnStmt{}, nil
	}
	val, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	return &ast.ReturnStmt{Value: val}, nil
}

func (p *Parser) parseDeferStmt() (*ast.DeferStmt, error) {
	p.advance() // consume defer
	call, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	return &ast.DeferStmt{Call: call}, nil
}

func (p *Parser) parseEchoStmt() (*ast.EchoStmt, error) {
	p.advance() // consume echo
	val, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	return &ast.EchoStmt{Value: val}, nil
}

func (p *Parser) parseIfStmt() (*ast.IfStmt, error) {
	p.advance() // consume if
	cond, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(lexer.COLON); err != nil {
		return nil, err
	}
	// body
	var thenBlock *ast.Block
	if p.check(lexer.NEWLINE) {
		p.advance()
		p.skipNewlines()
		if p.check(lexer.INDENT) {
			thenBlock, err = p.parseBlock()
		} else {
			thenBlock = &ast.Block{}
		}
	} else {
		stmt, err2 := p.parseStatement()
		if err2 != nil {
			return nil, err2
		}
		thenBlock = &ast.Block{Stmts: []ast.Node{stmt}}
	}
	if err != nil {
		return nil, err
	}

	stmt := &ast.IfStmt{Cond: cond, Then: thenBlock}

	// else / else if
	p.skipNewlines()
	for p.check(lexer.KW_ELSE) {
		p.advance()
		if p.check(lexer.KW_IF) {
			p.advance()
			eicond, err2 := p.parseExpr()
			if err2 != nil {
				return nil, err2
			}
			if _, err2 := p.expect(lexer.COLON); err2 != nil {
				return nil, err2
			}
			var eiBlock *ast.Block
			if p.check(lexer.NEWLINE) {
				p.advance()
				p.skipNewlines()
				if p.check(lexer.INDENT) {
					eiBlock, err2 = p.parseBlock()
					if err2 != nil {
						return nil, err2
					}
				} else {
					eiBlock = &ast.Block{}
				}
			} else {
				es, err2 := p.parseStatement()
				if err2 != nil {
					return nil, err2
				}
				eiBlock = &ast.Block{Stmts: []ast.Node{es}}
			}
			stmt.ElseIfs = append(stmt.ElseIfs, ast.ElseIfClause{Cond: eicond, Body: eiBlock})
			p.skipNewlines()
		} else {
			if _, err2 := p.expect(lexer.COLON); err2 != nil {
				return nil, err2
			}
			var elseBlock *ast.Block
			if p.check(lexer.NEWLINE) {
				p.advance()
				p.skipNewlines()
				if p.check(lexer.INDENT) {
					elseBlock, err = p.parseBlock()
					if err != nil {
						return nil, err
					}
				} else {
					elseBlock = &ast.Block{}
				}
			} else {
				es, err2 := p.parseStatement()
				if err2 != nil {
					return nil, err2
				}
				elseBlock = &ast.Block{Stmts: []ast.Node{es}}
			}
			stmt.Else = elseBlock
			break
		}
	}
	return stmt, nil
}

func (p *Parser) parseForStmt() (*ast.ForStmt, error) {
	p.advance() // consume for
	stmt := &ast.ForStmt{}

	// Expect "let varName [type]"
	if p.check(lexer.KW_LET) {
		p.advance()
	}
	nameTok, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, err
	}
	stmt.VarName = nameTok.Literal

	// Optional type annotation for the loop variable
	if !p.match(lexer.SEMI, lexer.KW_IN, lexer.ASSIGN, lexer.COLON) {
		stmt.VarType, err = p.parseTypeExpr()
		if err != nil {
			return nil, err
		}
	}

	if p.check(lexer.SEMI) {
		// C-style (no initializer): for let i T; cond; post:
		stmt.Kind = ast.ForCStyle
		p.advance() // consume ;
		stmt.Cond, err = p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(lexer.SEMI); err != nil {
			return nil, err
		}
		stmt.Post, err = p.parseExprStatement()
		if err != nil {
			return nil, err
		}
	} else if p.check(lexer.KW_IN) {
		// for let i T in iter:
		stmt.Kind = ast.ForIn
		p.advance() // consume in
		stmt.Iter, err = p.parseExpr()
		if err != nil {
			return nil, err
		}
	} else if p.check(lexer.ASSIGN) {
		p.advance() // consume =
		initExpr, err2 := p.parseExpr()
		if err2 != nil {
			return nil, err2
		}
		if p.check(lexer.SEMI) {
			// C-style with initializer: for let i T = init; cond; post:
			stmt.Kind = ast.ForCStyle
			stmt.Init = &ast.VarDecl{Name: stmt.VarName, Type: stmt.VarType, Value: initExpr}
			p.advance() // consume ;
			stmt.Cond, err = p.parseExpr()
			if err != nil {
				return nil, err
			}
			if _, err := p.expect(lexer.SEMI); err != nil {
				return nil, err
			}
			stmt.Post, err = p.parseExprStatement()
			if err != nil {
				return nil, err
			}
		} else {
			// for let i T = start..end:
			stmt.Kind = ast.ForIn
			stmt.Iter = initExpr
		}
	}

	if _, err := p.expect(lexer.COLON); err != nil {
		return nil, err
	}

	// Body
	if p.check(lexer.NEWLINE) {
		p.advance()
		p.skipNewlines()
		if p.check(lexer.INDENT) {
			stmt.Body, err = p.parseBlock()
			if err != nil {
				return nil, err
			}
		} else {
			stmt.Body = &ast.Block{}
		}
	} else {
		s, err2 := p.parseStatement()
		if err2 != nil {
			return nil, err2
		}
		stmt.Body = &ast.Block{Stmts: []ast.Node{s}}
	}
	return stmt, nil
}

func (p *Parser) parseMatchStmt() (*ast.MatchStmt, error) {
	p.advance() // consume match
	expr, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	// match a.(type): → set IsType
	isType := false
	if ta, ok := expr.(*ast.TypeAssertExpr); ok && ta.IsType {
		isType = true
	}
	if _, err := p.expect(lexer.COLON); err != nil {
		return nil, err
	}

	stmt := &ast.MatchStmt{Expr: expr, IsType: isType}
	if p.check(lexer.NEWLINE) {
		p.advance()
	}
	p.skipNewlines()
	if p.check(lexer.INDENT) {
		p.advance()
		p.skipNewlines()
		for !p.check(lexer.DEDENT) && !p.check(lexer.EOF) {
			if p.check(lexer.KW_DEFAULT) {
				p.advance()
				if _, err := p.expect(lexer.COLON); err != nil {
					return nil, err
				}
				if p.check(lexer.NEWLINE) {
					p.advance()
					p.skipNewlines()
					if p.check(lexer.INDENT) {
						stmt.Default, err = p.parseBlock()
						if err != nil {
							return nil, err
						}
					}
				} else if !p.check(lexer.EOF) && !p.check(lexer.DEDENT) {
					// Inline body: default: return foo
					s, err2 := p.parseStatement()
					if err2 != nil {
						return nil, err2
					}
					if s != nil {
						stmt.Default = &ast.Block{Stmts: []ast.Node{s}}
					}
				}
			} else if p.check(lexer.KW_CASE) {
				mc, err2 := p.parseMatchCase()
				if err2 != nil {
					return nil, err2
				}
				stmt.Cases = append(stmt.Cases, mc)
			} else {
				break
			}
			p.skipNewlines()
		}
		if p.check(lexer.DEDENT) {
			p.advance()
		}
	}
	return stmt, nil
}

func (p *Parser) parseMatchCase() (ast.MatchCase, error) {
	pos := p.curPos()
	if _, err := p.expect(lexer.KW_CASE); err != nil {
		return ast.MatchCase{}, err
	}

	mc := ast.MatchCase{Pos: pos}

	// case varName TypeExpr: OR case expr:
	// If next is ident followed by a type, it's "case i i8"
	if p.check(lexer.IDENT) && !isTypeToken(p.peekAt(1)) {
		// Just an expression pattern
		mc.Pattern, _ = p.parseExpr()
	} else if p.check(lexer.IDENT) {
		mc.VarName = p.advance().Literal
		if !p.match(lexer.COLON, lexer.NEWLINE) {
			t, err := p.parseTypeExpr()
			if err != nil {
				return ast.MatchCase{}, err
			}
			mc.VarType = t
		}
	} else {
		var err error
		mc.Pattern, err = p.parseExpr()
		if err != nil {
			return ast.MatchCase{}, err
		}
	}

	if _, err := p.expect(lexer.COLON); err != nil {
		return ast.MatchCase{}, err
	}
	if p.check(lexer.NEWLINE) {
		p.advance()
		p.skipNewlines()
		if p.check(lexer.INDENT) {
			var err error
			mc.Body, err = p.parseBlock()
			if err != nil {
				return ast.MatchCase{}, err
			}
		}
	} else if !p.check(lexer.EOF) && !p.check(lexer.KW_CASE) && !p.check(lexer.KW_DEFAULT) {
		// Inline body: case foo: return bar
		stmt, err := p.parseStatement()
		if err != nil {
			return ast.MatchCase{}, err
		}
		mc.Body = &ast.Block{Stmts: []ast.Node{stmt}}
	}
	return mc, nil
}

func (p *Parser) parseTaggedBlock() (*ast.TaggedBlock, error) {
	tags := p.parseTags()
	if p.check(lexer.LBRACE) {
		p.advance()
	}
	var stmts []ast.Node
	p.skipNewlines()
	for !p.check(lexer.RBRACE) && !p.check(lexer.EOF) {
		s, err := p.parseStatement()
		if err != nil {
			return nil, err
		}
		if s != nil {
			stmts = append(stmts, s)
		}
		p.skipNewlines()
	}
	if p.check(lexer.RBRACE) {
		p.advance()
	}
	return &ast.TaggedBlock{Tags: tags, Body: &ast.Block{Stmts: stmts}}, nil
}

// parseExprStatement handles assignments, augmented assignments, postfixes,
// and bare expression statements.
func (p *Parser) parseExprStatement() (ast.Node, error) {
	expr, err := p.parseExpr()
	if err != nil {
		return nil, err
	}

	// Postfix ++/--
	if p.check(lexer.INC) {
		op := p.advance().Literal
		return &ast.PostfixStmt{Expr: expr, Op: op}, nil
	}

	// Assignment =
	if p.check(lexer.ASSIGN) {
		p.advance()
		val, err2 := p.parseExpr()
		if err2 != nil {
			return nil, err2
		}
		return &ast.AssignStmt{Target: expr, Value: val}, nil
	}

	// Augmented assignment +=, -=, *=, /=, %=, ++=
	if aug, ok := augOp(p.peek().Type); ok {
		p.advance()
		val, err2 := p.parseExpr()
		if err2 != nil {
			return nil, err2
		}
		return &ast.AugAssignStmt{Target: expr, Op: aug, Value: val}, nil
	}

	return &ast.ExprStmt{Expr: expr}, nil
}

func augOp(t lexer.TokenType) (string, bool) {
	switch t {
	case lexer.PLUSEQ:
		return "+=", true
	case lexer.MINUSEQ:
		return "-=", true
	case lexer.STAREQ:
		return "*=", true
	case lexer.SLASHEQ:
		return "/=", true
	case lexer.PERCENTEQ:
		return "%=", true
	case lexer.APPENDEQ:
		return "++=", true
	}
	return "", false
}

// ── Struct / Trait / Type / Enum / Union / Data declarations ──────────────────

func (p *Parser) parseStructDecl(tags []string) (*ast.StructDecl, error) {
	p.advance() // consume struct

	// Optional {#tags} before name
	if p.check(lexer.LBRACE) {
		moreTags := p.parseTags()
		tags = append(tags, moreTags...)
	}

	nameTok, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, err
	}
	typeParams, _ := p.parseTypeParams()

	// Optional trait implementations: struct Foo(TraitA, TraitB[T]) =
	var impls []ast.TypeExpr
	if p.check(lexer.LPAREN) {
		p.advance()
		for !p.check(lexer.RPAREN) && !p.check(lexer.EOF) {
			ti, err2 := p.parseTypeExpr()
			if err2 != nil {
				return nil, err2
			}
			impls = append(impls, ti)
			if p.check(lexer.COMMA) {
				p.advance()
			}
		}
		if _, err2 := p.expect(lexer.RPAREN); err2 != nil {
			return nil, err2
		}
	}

	if _, err := p.expect(lexer.ASSIGN); err != nil {
		return nil, err
	}

	decl := &ast.StructDecl{
		Name: nameTok.Literal, TypeParams: typeParams,
		Implements: impls, Tags: tags,
	}

	// Parse body (fields + methods)
	if p.check(lexer.NEWLINE) {
		p.advance()
		p.skipNewlines()
		if p.check(lexer.INDENT) {
			p.advance()
			p.skipNewlines()
			for !p.check(lexer.DEDENT) && !p.check(lexer.EOF) {
				item, err2 := p.parseStructItem()
				if err2 != nil {
					return nil, err2
				}
				switch v := item.(type) {
				case *ast.StructField:
					decl.Fields = append(decl.Fields, *v)
				case *ast.FuncDecl:
					decl.Methods = append(decl.Methods, v)
				}
				p.skipNewlines()
			}
			if p.check(lexer.DEDENT) {
				p.advance()
			}
		}
	}
	return decl, nil
}

func (p *Parser) parseStructItem() (any, error) {
	isStatic := false
	if p.check(lexer.KW_STATIC) {
		isStatic = true
		p.advance()
	}
	if p.check(lexer.KW_FN) {
		fn, err := p.parseFuncDecl(nil, isStatic)
		return fn, err
	}
	// Field: name type [forward]
	nameTok, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, err
	}
	var typ ast.TypeExpr
	isForward := false
	if !p.match(lexer.NEWLINE, lexer.DEDENT, lexer.EOF) {
		typ, err = p.parseTypeExpr()
		if err != nil {
			return nil, err
		}
	}
	if p.check(lexer.KW_FORWARD) {
		isForward = true
		p.advance()
	}
	return &ast.StructField{Name: nameTok.Literal, Type: typ, IsForward: isForward}, nil
}

func (p *Parser) parseTraitDecl() (*ast.TraitDecl, error) {
	p.advance() // consume trait
	// optional generic param [k]
	var traitTypeParams []string
	if p.check(lexer.LBRACKET) {
		traitTypeParams, _ = p.parseTypeParams()
	}
	nameTok, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, err
	}
	typeParams, _ := p.parseTypeParams()

	decl := &ast.TraitDecl{Name: nameTok.Literal, TypeParams: typeParams}
	_ = traitTypeParams

	// "trait print as fn() [char]"
	if p.check(lexer.KW_AS) {
		p.advance()
		decl.IsAlias = true
		decl.AliasType, err = p.parseTypeExpr()
		if err != nil {
			return nil, err
		}
		return decl, nil
	}

	if !p.check(lexer.ASSIGN) {
		return decl, nil
	}
	p.advance() // consume =

	if p.check(lexer.NEWLINE) {
		p.advance()
		p.skipNewlines()
		if p.check(lexer.INDENT) {
			p.advance()
			p.skipNewlines()
			for !p.check(lexer.DEDENT) && !p.check(lexer.EOF) {
				if p.check(lexer.KW_FN) {
					fn, err2 := p.parseFuncDecl(nil, false)
					if err2 != nil {
						return nil, err2
					}
					decl.Methods = append(decl.Methods, fn)
				} else if p.check(lexer.IDENT) {
					// forward field: "name type forward"
					fname := p.advance().Literal
					ftype, err2 := p.parseTypeExpr()
					if err2 != nil {
						return nil, err2
					}
					if p.check(lexer.KW_FORWARD) {
						p.advance()
					}
					decl.ForwardFields = append(decl.ForwardFields, ast.StructField{Name: fname, Type: ftype})
				} else {
					p.advance() // skip unexpected tokens
				}
				p.skipNewlines()
			}
			if p.check(lexer.DEDENT) {
				p.advance()
			}
		}
	}
	return decl, nil
}

func (p *Parser) parseTypeDecl() (*ast.TypeDecl, error) {
	p.advance() // consume type
	nameTok, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, err
	}
	typeParams, _ := p.parseTypeParams()
	if _, err := p.expect(lexer.ASSIGN); err != nil {
		return nil, err
	}
	typ, err := p.parseTypeExpr()
	if err != nil {
		return nil, err
	}

	decl := &ast.TypeDecl{Name: nameTok.Literal, TypeParams: typeParams, Type: typ}

	// optional "override = fn ..."
	if p.check(lexer.KW_OVERRIDE) {
		p.advance()
		if _, err := p.expect(lexer.ASSIGN); err != nil {
			return nil, err
		}
		if p.check(lexer.NEWLINE) {
			p.advance()
			p.skipNewlines()
			if p.check(lexer.INDENT) {
				p.advance()
				p.skipNewlines()
				for !p.check(lexer.DEDENT) && !p.check(lexer.EOF) {
					if p.check(lexer.KW_FN) {
						fn, err2 := p.parseFuncDecl(nil, false)
						if err2 != nil {
							return nil, err2
						}
						decl.Overrides = append(decl.Overrides, fn)
					}
					p.skipNewlines()
				}
				if p.check(lexer.DEDENT) {
					p.advance()
				}
			}
		}
	}
	return decl, nil
}

func (p *Parser) parseEnumDecl() (*ast.EnumDecl, error) {
	p.advance() // consume enum
	decl := &ast.EnumDecl{}

	// "enum atom status" or "enum i32 weather"
	if p.check(lexer.IDENT) && p.peek().Literal == "atom" {
		decl.IsAtom = true
		p.advance()
	} else if isTypeKeyword(p.peek()) {
		decl.BaseType, _ = p.parseTypeExpr()
	}

	nameTok, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, err
	}
	decl.Name = nameTok.Literal

	if _, err := p.expect(lexer.ASSIGN); err != nil {
		return nil, err
	}

	if p.check(lexer.NEWLINE) {
		p.advance()
		p.skipNewlines()
		if p.check(lexer.INDENT) {
			p.advance()
			p.skipNewlines()
			for !p.check(lexer.DEDENT) && !p.check(lexer.EOF) {
				mem, err2 := p.parseEnumMember(decl.IsAtom)
				if err2 != nil {
					return nil, err2
				}
				decl.Members = append(decl.Members, mem)
				p.skipNewlines()
			}
			if p.check(lexer.DEDENT) {
				p.advance()
			}
		}
	}
	return decl, nil
}

func (p *Parser) parseEnumMember(isAtom bool) (ast.EnumMember, error) {
	mem := ast.EnumMember{IsAtom: isAtom}
	if isAtom {
		if p.check(lexer.ATOM_LIT) {
			mem.Name = p.advance().Literal
		} else if p.check(lexer.IDENT) {
			mem.Name = p.advance().Literal
		}
	} else {
		nameTok, err := p.expect(lexer.IDENT)
		if err != nil {
			return mem, err
		}
		mem.Name = nameTok.Literal
	}

	if p.check(lexer.COLON) {
		p.advance()
		var err error
		mem.Value, err = p.parseExpr()
		if err != nil {
			return mem, err
		}
	}
	if p.check(lexer.COMMA) {
		p.advance()
	}
	return mem, nil
}

func (p *Parser) parseUnionDecl() (*ast.UnionDecl, error) {
	p.advance() // consume union
	nameTok, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(lexer.ASSIGN); err != nil {
		return nil, err
	}
	decl := &ast.UnionDecl{Name: nameTok.Literal}

	// "as_i8 i8 | as_string string" vs "i8 | string"
	for {
		var fieldName string
		if p.check(lexer.IDENT) && !isTypeKeyword(p.peek()) {
			// Could be "as_i8 i8" (named) or just type name matching a non-primitive
			// Check if next is a type: named field followed by type
			saved := p.pos
			candidate := p.advance().Literal
			if isTypeToken(p.peek()) || p.check(lexer.IDENT) {
				fieldName = candidate
				decl.IsNamed = true
			} else {
				p.pos = saved
			}
		}
		typ, err2 := p.parseTypeExpr()
		if err2 != nil {
			return nil, err2
		}
		decl.Members = append(decl.Members, ast.UnionMember{FieldName: fieldName, Type: typ})
		if !p.check(lexer.BITOR) {
			break
		}
		p.advance()
	}
	return decl, nil
}

func (p *Parser) parseDataDecl() (*ast.DataDecl, error) {
	p.advance() // consume data
	nameTok, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, err
	}
	typeParams, _ := p.parseTypeParams()
	if _, err := p.expect(lexer.ASSIGN); err != nil {
		return nil, err
	}

	decl := &ast.DataDecl{Name: nameTok.Literal, TypeParams: typeParams}
	// "t | None"  or "SomeType | None"
	for {
		if p.check(lexer.NONE_LIT) || (p.check(lexer.IDENT) && p.peek().Literal == "None") {
			p.advance()
			decl.Variants = append(decl.Variants, ast.DataVariant{Name: "None"})
		} else {
			// Use parseTypeSingle (not parseTypeExpr) so that | is treated as
			// variant separator, not as a union type operator.
			typ, err2 := p.parseTypeSingle()
			if err2 != nil {
				return nil, err2
			}
			decl.Variants = append(decl.Variants, ast.DataVariant{Type: typ})
		}
		if !p.check(lexer.BITOR) {
			break
		}
		p.advance()
	}
	return decl, nil
}

func (p *Parser) parseUseDecl() (*ast.UseDecl, error) {
	p.advance() // consume use
	decl := &ast.UseDecl{}

	if p.check(lexer.KW_EXTERN) {
		decl.IsExtern = true
		p.advance()
		if _, err := p.expect(lexer.LPAREN); err != nil {
			return nil, err
		}
		p.skipWhitespace()
		for !p.check(lexer.RPAREN) && !p.check(lexer.EOF) {
			imp, err := p.parseUseImport()
			if err != nil {
				return nil, err
			}
			decl.Imports = append(decl.Imports, imp)
			if p.check(lexer.COMMA) {
				p.advance()
			}
			p.skipWhitespace()
		}
		if _, err := p.expect(lexer.RPAREN); err != nil {
			return nil, err
		}
	} else {
		// Build path from scope-access tokens: "use io" / "use std::math"
		var parts []string
		if p.check(lexer.IDENT) {
			parts = append(parts, p.advance().Literal)
		}
		for p.check(lexer.DCOLON) {
			p.advance()
			parts = append(parts, p.advance().Literal)
		}
		decl.Path = strings.Join(parts, "::")
	}
	return decl, nil
}

func (p *Parser) parseUseImport() (ast.UseImport, error) {
	imp := ast.UseImport{}
	// Parse the local (Tin) name first.
	if p.check(lexer.IDENT) {
		imp.LocalName = p.advance().Literal
		imp.ExternName = imp.LocalName // default: C name == Tin name
	}
	// Optional ("exportedName") after the local name allows renaming:
	//   malloc("mymallocname") as fn(size_t) *void
	// means LocalName="malloc", ExternName="mymallocname"
	if p.check(lexer.LPAREN) {
		p.advance()
		if p.check(lexer.STRING_LIT) {
			imp.ExternName = p.advance().Literal
		}
		if _, err := p.expect(lexer.RPAREN); err != nil {
			return imp, err
		}
	}
	if p.check(lexer.KW_AS) {
		p.advance()
		typ, err := p.parseTypeExpr()
		if err != nil {
			return imp, err
		}
		imp.Type = typ
	}
	return imp, nil
}

func (p *Parser) parseExportDecl() (*ast.ExportDecl, error) {
	p.advance() // consume export
	decl := &ast.ExportDecl{}
	if p.check(lexer.LBRACE) {
		p.advance()
		for !p.check(lexer.RBRACE) && !p.check(lexer.EOF) {
			if p.check(lexer.IDENT) {
				decl.Names = append(decl.Names, p.advance().Literal)
			}
			if p.check(lexer.COMMA) {
				p.advance()
			}
		}
		if _, err := p.expect(lexer.RBRACE); err != nil {
			return nil, err
		}
	}
	if p.check(lexer.KW_AS) {
		p.advance()
		if p.check(lexer.IDENT) {
			decl.AsName = p.advance().Literal
		}
	}
	return decl, nil
}

func (p *Parser) parseMacroDecl(tags []string) (*ast.MacroDecl, error) {
	p.advance() // consume macro
	if p.check(lexer.LBRACE) {
		moreTags := p.parseTags()
		tags = append(tags, moreTags...)
	}
	nameTok, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, err
	}
	// macro may have "!" in name: try!
	name := nameTok.Literal
	if p.check(lexer.NOT) {
		p.advance()
		name += "!"
	}
	// params
	var params []string
	if p.check(lexer.LPAREN) {
		p.advance()
		for !p.check(lexer.RPAREN) && !p.check(lexer.EOF) {
			if p.check(lexer.IDENT) {
				params = append(params, p.advance().Literal)
			}
			if p.check(lexer.COMMA) {
				p.advance()
			}
		}
		if _, err := p.expect(lexer.RPAREN); err != nil {
			return nil, err
		}
	}
	if _, err := p.expect(lexer.ASSIGN); err != nil {
		return nil, err
	}
	body, err := p.parseFuncBody()
	if err != nil {
		return nil, err
	}
	return &ast.MacroDecl{Name: name, Tags: tags, Params: params, Body: body}, nil
}

// ── Expressions ───────────────────────────────────────────────────────────────

// Expression precedence (lowest → highest):
//
//	pipe |>
//	ternary ? :
//	or  ||
//	and &&
//	bitor |
//	xor ^
//	bitand &
//	equality == !=
//	comparison < <= > >=
//	shift << >>
//	additive + - ++
//	multiplicative * / %
//	unary ! - ~ & *
//	postfix ++ -- . -> :: [] () as is .(Type)
//	primary
func (p *Parser) parseExpr() (ast.Node, error) { return p.parsePipe() }

func (p *Parser) parsePipe() (ast.Node, error) {
	left, err := p.parseRange()
	if err != nil {
		return nil, err
	}
	for p.check(lexer.PIPE) || (p.check(lexer.NEWLINE) && p.peekAt(1).Type == lexer.PIPE) {
		// |> may span newlines (a\n|> f)
		if p.check(lexer.NEWLINE) {
			if p.peekAt(1).Type != lexer.PIPE {
				break
			}
			p.advance() // consume newline
		}
		if p.check(lexer.PIPE) {
			p.advance()
		}
		right, err2 := p.parseTernary()
		if err2 != nil {
			return nil, err2
		}
		left = &ast.PipeExpr{Left: left, Right: right}
	}
	return left, nil
}

func (p *Parser) parseRange() (ast.Node, error) {
	left, err := p.parseTernary()
	if err != nil {
		return nil, err
	}
	if p.check(lexer.RANGE) {
		p.advance()
		right, err2 := p.parseTernary()
		if err2 != nil {
			return nil, err2
		}
		return &ast.BinExpr{Left: left, Op: "..", Right: right}, nil
	}
	return left, nil
}

func (p *Parser) parseTernary() (ast.Node, error) {
	cond, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.check(lexer.QUESTION) {
		p.advance()
		then, err2 := p.parseOr()
		if err2 != nil {
			return nil, err2
		}
		if _, err2 := p.expect(lexer.COLON); err2 != nil {
			return nil, err2
		}
		els, err2 := p.parseOr()
		if err2 != nil {
			return nil, err2
		}
		return &ast.TernaryExpr{Cond: cond, Then: then, Else: els}, nil
	}
	return cond, nil
}

func (p *Parser) parseOr() (ast.Node, error) {
	return p.parseBinary(p.parseAnd, lexer.OR)
}

func (p *Parser) parseAnd() (ast.Node, error) {
	return p.parseBinary(p.parseBitOr, lexer.AND)
}

func (p *Parser) parseBitOr() (ast.Node, error) {
	return p.parseBinary(p.parseBitXor, lexer.BITOR)
}

func (p *Parser) parseBitXor() (ast.Node, error) {
	return p.parseBinary(p.parseBitAnd, lexer.XOR)
}

func (p *Parser) parseBitAnd() (ast.Node, error) {
	return p.parseBinary(p.parseEquality, lexer.AMP)
}

func (p *Parser) parseEquality() (ast.Node, error) {
	return p.parseBinary(p.parseComparison, lexer.EQEQ, lexer.NEQ)
}

func (p *Parser) parseComparison() (ast.Node, error) {
	return p.parseBinary(p.parseShift, lexer.LT, lexer.LTEQ, lexer.GT, lexer.GTEQ)
}

func (p *Parser) parseShift() (ast.Node, error) {
	return p.parseBinary(p.parseAdditive, lexer.SHL, lexer.SHR)
}

func (p *Parser) parseAdditive() (ast.Node, error) {
	left, err := p.parseMultiplicative()
	if err != nil {
		return nil, err
	}
	for p.match(lexer.PLUS, lexer.MINUS, lexer.INC) {
		// ++ is both a binary concat operator and a postfix increment.
		// Only treat it as binary concat when followed by a valid expression
		// start; otherwise leave it for parseExprStatement's postfix check.
		if p.peek().Type == lexer.INC && !isExprStart(p.peekAt(1)) {
			break
		}
		op := p.advance().Literal
		right, err2 := p.parseMultiplicative()
		if err2 != nil {
			return nil, err2
		}
		left = &ast.BinExpr{Left: left, Op: op, Right: right}
	}
	return left, nil
}

// isExprStart returns true if tok can begin a primary expression.
func isExprStart(tok lexer.Token) bool {
	switch tok.Type {
	case lexer.IDENT, lexer.INT_LIT, lexer.FLOAT_LIT, lexer.STRING_LIT,
		lexer.BOOL_LIT, lexer.CHAR_LIT, lexer.NONE_LIT,
		lexer.LPAREN, lexer.LBRACKET, lexer.MINUS, lexer.NOT,
		lexer.STAR, lexer.AMP, lexer.KW_FN, lexer.KW_SIZEOF, lexer.KW_ADDR:
		return true
	}
	return isTypeKeyword(tok)
}

func (p *Parser) parseMultiplicative() (ast.Node, error) {
	return p.parseBinary(p.parseUnary, lexer.STAR, lexer.SLASH, lexer.PERCENT)
}

func (p *Parser) parseBinary(sub func() (ast.Node, error), ops ...lexer.TokenType) (ast.Node, error) {
	left, err := sub()
	if err != nil {
		return nil, err
	}
	for p.match(ops...) {
		op := p.advance().Literal
		right, err2 := sub()
		if err2 != nil {
			return nil, err2
		}
		left = &ast.BinExpr{Left: left, Op: op, Right: right}
	}
	return left, nil
}

func (p *Parser) parseUnary() (ast.Node, error) {
	if p.match(lexer.NOT, lexer.MINUS, lexer.TILDE) {
		op := p.advance().Literal
		expr, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &ast.UnaryExpr{Op: op, Expr: expr}, nil
	}
	// Dereference: *expr
	if p.check(lexer.STAR) {
		p.advance()
		expr, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &ast.DerefExpr{Expr: expr}, nil
	}
	// Address-of: &expr
	if p.check(lexer.AMP) {
		p.advance()
		expr, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &ast.AddressOfExpr{Expr: expr}, nil
	}
	return p.parsePostfix()
}

func (p *Parser) parsePostfix() (ast.Node, error) {
	expr, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}

	for {
		switch p.peek().Type {
		case lexer.DOT:
			p.advance()
			// .(Type) or .(type)
			if p.check(lexer.LPAREN) {
				p.advance()
				if p.check(lexer.KW_TYPE) || p.peek().Literal == "type" {
					p.advance()
					if _, err2 := p.expect(lexer.RPAREN); err2 != nil {
						return nil, err2
					}
					expr = &ast.TypeAssertExpr{Expr: expr, IsType: true}
					continue
				}
				typ, err2 := p.parseTypeExpr()
				if err2 != nil {
					return nil, err2
				}
				if _, err2 := p.expect(lexer.RPAREN); err2 != nil {
					return nil, err2
				}
				expr = &ast.TypeAssertExpr{Expr: expr, Type: typ}
				continue
			}
			field, err2 := p.expect(lexer.IDENT)
			if err2 != nil {
				return nil, err2
			}
			// Method call: expr.field(args)
			if p.check(lexer.LPAREN) {
				args, err3 := p.parseArgList()
				if err3 != nil {
					return nil, err3
				}
				expr = &ast.CallExpr{
					Func: &ast.FieldAccess{Expr: expr, Field: field.Literal},
					Args: args,
				}
			} else {
				expr = &ast.FieldAccess{Expr: expr, Field: field.Literal}
			}

		case lexer.ARROW:
			p.advance()
			field, err2 := p.expect(lexer.IDENT)
			if err2 != nil {
				return nil, err2
			}
			if p.check(lexer.LPAREN) {
				args, err3 := p.parseArgList()
				if err3 != nil {
					return nil, err3
				}
				expr = &ast.CallExpr{
					Func: &ast.FieldAccess{Expr: expr, Field: field.Literal, IsPtr: true},
					Args: args,
				}
			} else {
				expr = &ast.FieldAccess{Expr: expr, Field: field.Literal, IsPtr: true}
			}

		case lexer.DCOLON:
			p.advance()
			field, err2 := p.expect(lexer.IDENT)
			if err2 != nil {
				return nil, err2
			}
			// Build scope access from existing expr + new segment
			if sa, ok := expr.(*ast.ScopeAccess); ok {
				sa.Path = append(sa.Path, field.Literal)
				if p.check(lexer.LPAREN) {
					args, err3 := p.parseArgList()
					if err3 != nil {
						return nil, err3
					}
					expr = &ast.CallExpr{Func: sa, Args: args}
				}
			} else if id, ok := expr.(*ast.Identifier); ok {
				sa := &ast.ScopeAccess{Path: []string{id.Name, field.Literal}}
				if p.check(lexer.LPAREN) {
					args, err3 := p.parseArgList()
					if err3 != nil {
						return nil, err3
					}
					expr = &ast.CallExpr{Func: sa, Args: args}
				} else {
					expr = sa
				}
			}

		case lexer.LBRACKET:
			p.advance()
			idx, err2 := p.parseExpr()
			if err2 != nil {
				return nil, err2
			}
			if _, err2 := p.expect(lexer.RBRACKET); err2 != nil {
				return nil, err2
			}
			expr = &ast.IndexExpr{Expr: expr, Index: idx}

		case lexer.NOT:
			// Macro call syntax: ident!(args) — only when followed by '('
			if p.peekAt(1).Type != lexer.LPAREN {
				return expr, nil
			}
			p.advance() // consume !
			if id, ok := expr.(*ast.Identifier); ok {
				id.Name += "!"
			}
			args, err2 := p.parseArgList()
			if err2 != nil {
				return nil, err2
			}
			expr = &ast.CallExpr{Func: expr, Args: args}

		case lexer.LPAREN:
			// Function call
			args, err2 := p.parseArgList()
			if err2 != nil {
				return nil, err2
			}
			expr = &ast.CallExpr{Func: expr, Args: args}

		case lexer.KW_AS:
			p.advance()
			typ, err2 := p.parseTypeExpr()
			if err2 != nil {
				return nil, err2
			}
			expr = &ast.AsExpr{Expr: expr, Type: typ}

		case lexer.KW_IS:
			p.advance()
			isExpr := &ast.IsExpr{Expr: expr}
			if p.check(lexer.NONE_LIT) || (p.check(lexer.IDENT) && p.peek().Literal == "None") {
				p.advance()
				isExpr.IsNone = true
			} else {
				if p.check(lexer.IDENT) && isTypeToken(p.peekAt(1)) {
					isExpr.VarName = p.advance().Literal
				}
				if !p.match(lexer.NEWLINE, lexer.COLON, lexer.EOF) {
					var err2 error
					isExpr.Type, err2 = p.parseTypeExpr()
					if err2 != nil {
						return nil, err2
					}
				}
			}
			expr = isExpr

		default:
			return expr, nil
		}
	}
}

func (p *Parser) parseArgList() ([]ast.Node, error) {
	if _, err := p.expect(lexer.LPAREN); err != nil {
		return nil, err
	}
	var args []ast.Node
	for !p.check(lexer.RPAREN) && !p.check(lexer.EOF) {
		arg, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		args = append(args, arg)
		if p.check(lexer.COMMA) {
			p.advance()
		}
	}
	if _, err := p.expect(lexer.RPAREN); err != nil {
		return nil, err
	}
	return args, nil
}

func (p *Parser) parsePrimary() (ast.Node, error) {
	tok := p.peek()
	pos := p.curPos()
	_ = pos

	switch tok.Type {
	case lexer.INT_LIT:
		p.advance()
		v, _ := strconv.ParseInt(tok.Literal, 0, 64)
		return &ast.IntLit{Value: v}, nil

	case lexer.FLOAT_LIT:
		p.advance()
		v, _ := strconv.ParseFloat(tok.Literal, 64)
		return &ast.FloatLit{Value: v}, nil

	case lexer.STRING_LIT:
		p.advance()
		return parseStringInterp(tok.Literal)

	case lexer.CHAR_LIT:
		p.advance()
		var b byte
		if len(tok.Literal) > 0 {
			b = tok.Literal[0]
		}
		return &ast.CharLit{Value: b}, nil

	case lexer.BOOL_LIT:
		p.advance()
		return &ast.BoolLit{Value: tok.Literal == "true"}, nil

	case lexer.ATOM_LIT:
		p.advance()
		return &ast.AtomLit{Name: tok.Literal}, nil

	case lexer.NONE_LIT:
		p.advance()
		return &ast.NoneLit{}, nil

	case lexer.KW_FN:
		// Lambda expression
		return p.parseLambda()

	case lexer.KW_SIZEOF:
		p.advance()
		if _, err := p.expect(lexer.LPAREN); err != nil {
			return nil, err
		}
		typ, err := p.parseTypeExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(lexer.RPAREN); err != nil {
			return nil, err
		}
		return &ast.SizeofExpr{Type: typ}, nil

	case lexer.KW_ADDR:
		p.advance()
		if _, err := p.expect(lexer.LPAREN); err != nil {
			return nil, err
		}
		inner, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(lexer.RPAREN); err != nil {
			return nil, err
		}
		return &ast.AddrExpr{Val: inner}, nil

	case lexer.KW_DEFAULT:
		p.advance()
		if _, err := p.expect(lexer.LPAREN); err != nil {
			return nil, err
		}
		typ, err := p.parseTypeExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(lexer.RPAREN); err != nil {
			return nil, err
		}
		return &ast.DefaultExpr{Type: typ}, nil

	case lexer.LPAREN:
		p.advance()
		inner, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		// handle (let x = expr; cond) ? ... : ... style  (macro helper)
		if p.check(lexer.SEMI) {
			p.advance()
			cond, err2 := p.parseExpr()
			if err2 != nil {
				return nil, err2
			}
			_ = cond
			// simplified: just return inner
		}
		if _, err := p.expect(lexer.RPAREN); err != nil {
			return nil, err
		}
		return inner, nil

	case lexer.LBRACKET:
		return p.parseArrayLit()

	case lexer.IDENT:
		name := p.advance().Literal
		// struct literal: name{...}
		if p.check(lexer.LBRACE) {
			return p.parseStructLit(name)
		}
		return &ast.Identifier{Name: name}, nil

	case lexer.KW_LET:
		// inline let (for ternary macro usage)
		return p.parseVarDecl()
	}

	// Type keywords used as identifiers / type names in expressions
	if isTypeKeyword(tok) {
		p.advance()
		return &ast.Identifier{Name: tok.Literal}, nil
	}

	return nil, p.errorf("unexpected token %s (%q)", tok.Type, tok.Literal)
}

func (p *Parser) parseLambda() (*ast.LambdaExpr, error) {
	if _, err := p.expect(lexer.KW_FN); err != nil {
		return nil, err
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
	return &ast.LambdaExpr{TypeParams: typeParams, Params: params, RetType: retType, Body: body}, nil
}

func (p *Parser) parseArrayLit() (ast.Node, error) {
	p.advance() // consume [
	var elems []ast.Node
	for !p.check(lexer.RBRACKET) && !p.check(lexer.EOF) {
		elem, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		elems = append(elems, elem)
		if p.check(lexer.COMMA) {
			p.advance()
		}
	}
	if _, err := p.expect(lexer.RBRACKET); err != nil {
		return nil, err
	}
	return &ast.ArrayLit{Elems: elems}, nil
}

func (p *Parser) parseStructLit(typeName string) (ast.Node, error) {
	p.advance() // consume {
	lit := &ast.StructLit{TypeName: typeName}
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
	}
	if _, err := p.expect(lexer.RBRACE); err != nil {
		return nil, err
	}
	return lit, nil
}

// ── Type expressions ──────────────────────────────────────────────────────────

// parseTypeExpr parses a type annotation.
func (p *Parser) parseTypeExpr() (ast.TypeExpr, error) {
	return p.parseTypeUnion()
}

func (p *Parser) parseTypeUnion() (ast.TypeExpr, error) {
	first, err := p.parseTypeSingle()
	if err != nil {
		return nil, err
	}
	if !p.check(lexer.BITOR) {
		return first, nil
	}
	union := &ast.UnionTypeExpr{Types: []ast.TypeExpr{first}}
	for p.check(lexer.BITOR) {
		p.advance()
		next, err2 := p.parseTypeSingle()
		if err2 != nil {
			return nil, err2
		}
		union.Types = append(union.Types, next)
	}
	return union, nil
}

func (p *Parser) parseTypeSingle() (ast.TypeExpr, error) {
	// const *T
	isConst := false
	if p.check(lexer.KW_CONST) {
		isConst = true
		p.advance()
	}
	// *T – pointer
	if p.check(lexer.STAR) {
		p.advance()
		elem, err := p.parseTypeSingle()
		if err != nil {
			return nil, err
		}
		return &ast.PointerType{Elem: elem, IsConst: isConst}, nil
	}
	// [T] or [T; N] – array
	if p.check(lexer.LBRACKET) {
		p.advance()
		// Empty brackets = void/unknown
		if p.check(lexer.RBRACKET) {
			p.advance()
			return &ast.ArrayType{Elem: &ast.SimpleType{Name: "void"}, Size: -1}, nil
		}
		elem, err := p.parseTypeExpr()
		if err != nil {
			return nil, err
		}
		size := -1
		if p.check(lexer.SEMI) {
			p.advance()
			if p.check(lexer.INT_LIT) {
				n, _ := strconv.Atoi(p.advance().Literal)
				size = n
			}
		}
		if _, err := p.expect(lexer.RBRACKET); err != nil {
			return nil, err
		}
		return &ast.ArrayType{Elem: elem, Size: size}, nil
	}
	// fn(T...) R – function type
	if p.check(lexer.KW_FN) {
		return p.parseFuncType()
	}
	// void
	if p.check(lexer.KW_EXTERN) {
		// skip "extern" used as void-like placeholder
		p.advance()
		return &ast.SimpleType{Name: "void"}, nil
	}

	// None used as a type (e.g. in `data maybe[t] = t | None`)
	if p.check(lexer.NONE_LIT) {
		p.advance()
		return &ast.SimpleType{Name: "None"}, nil
	}

	// Named type, possibly generic: name[T, R]
	if !p.match(lexer.IDENT) && !isTypeKeyword(p.peek()) {
		return nil, p.errorf("expected type, got %s (%q)", p.peek().Type, p.peek().Literal)
	}
	name := p.advance().Literal

	// Generic type args: name[T, R]
	if p.check(lexer.LBRACKET) {
		p.advance()
		var args []ast.TypeExpr
		for !p.check(lexer.RBRACKET) && !p.check(lexer.EOF) {
			t, err := p.parseTypeExpr()
			if err != nil {
				return nil, err
			}
			args = append(args, t)
			if p.check(lexer.COMMA) {
				p.advance()
			}
		}
		if _, err := p.expect(lexer.RBRACKET); err != nil {
			return nil, err
		}
		return &ast.GenericType{Name: name, TypeParams: args}, nil
	}
	return &ast.SimpleType{Name: name}, nil
}

func (p *Parser) parseFuncType() (ast.TypeExpr, error) {
	p.advance() // consume fn
	if _, err := p.expect(lexer.LPAREN); err != nil {
		return nil, err
	}
	ft := &ast.FuncType{}
	for !p.check(lexer.RPAREN) && !p.check(lexer.EOF) {
		// optional param name (ignored in type)
		if p.check(lexer.IDENT) && isTypeToken(p.peekAt(1)) {
			p.advance() // skip name
		}
		if p.check(lexer.DOTDOTDOT) {
			ft.IsVarArgs = true
			p.advance()
			if !p.check(lexer.RPAREN) {
				t, err2 := p.parseTypeExpr()
				if err2 != nil {
					return nil, err2
				}
				ft.Params = append(ft.Params, t)
			}
			break
		}
		t, err := p.parseTypeExpr()
		if err != nil {
			return nil, err
		}
		ft.Params = append(ft.Params, t)
		if p.check(lexer.COMMA) {
			p.advance()
		}
	}
	if _, err := p.expect(lexer.RPAREN); err != nil {
		return nil, err
	}
	// return type
	if !p.match(lexer.NEWLINE, lexer.EOF, lexer.RPAREN, lexer.COMMA, lexer.RBRACKET, lexer.ASSIGN, lexer.COLON) {
		var err error
		ft.RetType, err = p.parseTypeSingle()
		if err != nil {
			return nil, err
		}
	}
	return ft, nil
}

// ── Parameter list ────────────────────────────────────────────────────────────

func (p *Parser) parseParams() ([]ast.Param, error) {
	if _, err := p.expect(lexer.LPAREN); err != nil {
		return nil, err
	}
	var params []ast.Param
	for !p.check(lexer.RPAREN) && !p.check(lexer.EOF) {
		param, err := p.parseParam()
		if err != nil {
			return nil, err
		}
		params = append(params, param)
		if p.check(lexer.COMMA) {
			p.advance()
		}
	}
	if _, err := p.expect(lexer.RPAREN); err != nil {
		return nil, err
	}
	return params, nil
}

func (p *Parser) parseParam() (ast.Param, error) {
	param := ast.Param{}
	if p.check(lexer.KW_CONST) {
		param.IsConst = true
		p.advance()
	}
	if p.check(lexer.DOTDOTDOT) {
		param.IsVarArgs = true
		p.advance()
		return param, nil
	}
	// name (optional for anonymous params like "const *char")
	if p.check(lexer.IDENT) && !isTypeKeyword(p.peek()) {
		// Could be name followed by type, or just a type-like name
		// Heuristic: if next token is a type token or IDENT, consume as name
		savedPos := p.pos
		candidate := p.advance().Literal
		if p.match(lexer.RPAREN, lexer.COMMA, lexer.DOTDOTDOT) {
			// only one ident - treat as type name
			p.pos = savedPos
		} else {
			param.Name = candidate
		}
	}
	if !p.match(lexer.RPAREN, lexer.COMMA, lexer.DOTDOTDOT) {
		var err error
		param.Type, err = p.parseTypeExpr()
		if err != nil {
			return param, err
		}
	}
	return param, nil
}

// parseTypeParams parses optional generic type parameters [t] or [t, r].
func (p *Parser) parseTypeParams() ([]string, error) {
	if !p.check(lexer.LBRACKET) {
		return nil, nil
	}
	p.advance()
	var params []string
	for !p.check(lexer.RBRACKET) && !p.check(lexer.EOF) {
		if p.check(lexer.IDENT) {
			params = append(params, p.advance().Literal)
		}
		if p.check(lexer.COMMA) {
			p.advance()
		}
	}
	if _, err := p.expect(lexer.RBRACKET); err != nil {
		return nil, err
	}
	return params, nil
}

// ── String interpolation ──────────────────────────────────────────────────────

// parseStringInterp splits a raw string literal into an AST node.
// If it contains {expr} patterns it returns an InterpolatedString,
// otherwise a plain StringLit.
func parseStringInterp(s string) (ast.Node, error) {
	if !strings.Contains(s, "{") {
		return &ast.StringLit{Value: s}, nil
	}
	var parts []ast.StringPart
	for len(s) > 0 {
		idx := strings.Index(s, "{")
		if idx < 0 {
			parts = append(parts, ast.StringPart{Str: s})
			break
		}
		if idx > 0 {
			parts = append(parts, ast.StringPart{Str: s[:idx]})
		}
		s = s[idx+1:]
		end := strings.Index(s, "}")
		if end < 0 {
			// No closing brace – treat rest as literal
			parts = append(parts, ast.StringPart{Str: "{" + s})
			break
		}
		exprSrc := s[:end]
		s = s[end+1:]

		// Re-lex and re-parse the embedded expression
		l := newInlineLexer(exprSrc)
		toks, err := l.Tokenize()
		if err != nil {
			return nil, fmt.Errorf("interpolation error in {%s}: %v", exprSrc, err)
		}
		rp := New(toks)
		expr, err := rp.parseExpr()
		if err != nil {
			return nil, fmt.Errorf("interpolation error in {%s}: %v", exprSrc, err)
		}
		parts = append(parts, ast.StringPart{IsExpr: true, Expr: expr})
	}
	if len(parts) == 1 && !parts[0].IsExpr {
		return &ast.StringLit{Value: parts[0].Str}, nil
	}
	return &ast.InterpolatedString{Parts: parts}, nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func isTypeKeyword(tok lexer.Token) bool {
	switch tok.Type {
	case lexer.KW_FN:
		return true
	}
	switch tok.Literal {
	case "i8", "i16", "i32", "i64",
		"u8", "u16", "u32", "u64",
		"f32", "f64",
		"bool", "char", "string", "any",
		"void", "uint32", "size_t",
		"int", "uint":
		return true
	}
	return false
}

func isTypeToken(tok lexer.Token) bool {
	return isTypeKeyword(tok) || tok.Type == lexer.STAR || tok.Type == lexer.LBRACKET ||
		tok.Type == lexer.KW_CONST
}

// newInlineLexer creates a lexer for an inline expression string.
func newInlineLexer(src string) *lexer.Lexer {
	return lexer.New(src)
}

// ParseType parses a single Tin type expression from a source string.
// Used by the module system to deserialize type signatures.
func ParseType(src string) (ast.TypeExpr, error) {
	l := lexer.New(src)
	tokens, err := l.Tokenize()
	if err != nil {
		return nil, err
	}
	p := New(tokens)
	return p.parseTypeExpr()
}

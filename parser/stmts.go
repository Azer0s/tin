package parser

import (
	"fmt"

	"github.com/Azer0s/tin/ast"
	"github.com/Azer0s/tin/lexer"
)

// Statements

func (p *Parser) parseStatement() (ast.Node, error) {
	tags := p.parseTags()
	_ = tags

	// `{#tag} { body }` C-brace tagged blocks were removed; the tags
	// already got consumed by parseTags above, so a bare LBRACE here
	// means the user wrote the legacy form.  Surface the new syntax
	// instead of letting expression parsing emit a generic error.
	if len(tags) > 0 && p.check(lexer.LBRACE) {
		return nil, p.errorf("`{#tag} { body }` blocks were removed; use `do{#tag}: body` (indent block) or `do{#tag}: stmt` (single-statement inline) instead")
	}

	// Check for #no_parens macro invocation (same as parseTopLevel).
	// Allows macros like `loop:` to work inside function bodies.
	if p.peek().Type == lexer.IDENT {
		if expansion, ok := p.noParensMacros[p.peek().Literal]; ok {
			p.advance() // consume macro name

			expToks, err := lexer.New(expansion).Tokenize()
			if err != nil {
				return nil, fmt.Errorf("no_parens macro expansion tokenize error: %w", err)
			}

			for len(expToks) > 0 && expToks[len(expToks)-1].Type == lexer.EOF {
				expToks = expToks[:len(expToks)-1]
			}

			newToks := make([]lexer.Token, 0, len(expToks)+len(p.tokens)-p.pos)
			newToks = append(newToks, expToks...)
			newToks = append(newToks, p.tokens[p.pos:]...)
			p.tokens = append(p.tokens[:p.pos], newToks...)

			return p.parseStatement()
		}
	}

	switch p.peek().Type {
	case lexer.KW_VAR:
		// `var` is module-scope only -- it declares a global with
		// runtime-init semantics and outlives the program. A
		// block-level `var` would silently produce a TopLevelVar
		// whose binding leaks out of scope, so flag it here with a
		// targeted suggestion.
		if p.blockDepth > 0 {
			tok := p.peek()

			return nil, p.errAtTok(tok, "%q is module-scope only; use %q for a mutable local binding, or move the %q declaration to the top level",
				"var", "let", "var")
		}

		return p.parseTopLevelVar()
	case lexer.KW_SPAWN:
		return p.parseSpawnExprStmt()
	case lexer.KW_AWAIT:
		// await match [...]: is a statement; plain await expr falls through to expression statement.
		if p.peekAt(1).Type == lexer.KW_MATCH {
			return p.parseAwaitMatchStmt()
		}

		return p.parseExprStatement()
	case lexer.KW_YIELD:
		p.advance()

		return &ast.YieldStmt{}, nil
	case lexer.KW_LET, lexer.KW_CONST:
		return p.parseLetStmt()
	case lexer.KW_FN:
		return p.parseFuncDecl(tags, false)
	case lexer.KW_STRUCT:
		return p.parseStructDecl(tags)
	case lexer.KW_TYPE:
		return nil, p.errorf(
			"`type` declarations are only allowed at top level, not inside a function or block body")
	case lexer.KW_ENUM:
		return p.parseEnumDecl()
	case lexer.KW_RETURN:
		return p.parseReturnStmt()
	case lexer.KW_PASS:
		p.advance()

		return nil, nil // no-op statement; not appended to block
	case lexer.KW_BREAK:
		p.advance()

		return &ast.BreakStmt{}, nil
	case lexer.KW_DEFER:
		return p.parseDeferStmt()
	case lexer.KW_DO:
		return p.parseDoBlock()
	case lexer.KW_IF:
		return p.parseIfStmt()
	case lexer.KW_FOR:
		return p.parseForStmt()
	case lexer.KW_MATCH:
		return p.parseMatchStmt()
	case lexer.KW_ECHO:
		return p.parseEchoStmt()
	case lexer.KW_USE:
		return nil, p.errorf(
			"`use` declarations are only allowed at top level, not inside a function or block body")
	case lexer.KW_EXPORT:
		return p.parseExportDecl()
	case lexer.KW_WHERE:
		wl := &ast.WhereList{}

		for {
			wc, err := p.parseWhereClause()
			if err != nil {
				return nil, err
			}

			wl.Clauses = append(wl.Clauses, wc)

			p.skipNewlines()

			if !p.check(lexer.KW_WHERE) {
				break
			}
		}

		return wl, nil
	case lexer.LBRACE:
		// `{ #tag } { body }` is no longer accepted; use `do{#tag}: body`
		// (indent-based block) or `do{#tag}: stmt` (single-stmt inline).
		if p.peekAt(1).Type == lexer.CONTROL_TAG {
			return nil, p.errorf("`{#tag} { body }` blocks were removed; use `do{#tag}: body` (indent block) or `do{#tag}: stmt` (single-statement inline) instead")
		}

		return p.parseExprStatement()
	case lexer.NEWLINE, lexer.DEDENT, lexer.SEMI:
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

	node := &ast.VarDecl{Name: nameTok.Literal, Type: typ, Value: val, IsConst: isConst}
	node.SetPos(pos)

	return node, nil
}

func (p *Parser) parseReturnStmt() (*ast.ReturnStmt, error) {
	pos := p.curPos()
	p.advance() // consume return

	if p.match(lexer.NEWLINE, lexer.DEDENT, lexer.EOF, lexer.SEMI) {
		node := &ast.ReturnStmt{}
		node.SetPos(pos)

		return node, nil
	}

	val, err := p.parseExpr()
	if err != nil {
		return nil, err
	}

	node := &ast.ReturnStmt{Value: val}
	node.SetPos(pos)

	return node, nil
}

// parseDoBlock parses a `do[{#tag ...}]: body` statement.  Two shapes:
//
//   - `do: body`               -> plain indented (or inline) statement block.
//   - `do{#tag1 #tag2}: body`  -> TaggedBlock with the listed control tags.
//
// Replaces the legacy `{#tag} { body }` C-brace form that was removed when
// indented blocks became the only statement-block syntax in Tin.
func (p *Parser) parseDoBlock() (ast.Node, error) {
	pos := p.curPos()
	p.advance() // consume `do`

	tags := p.parseTags()

	if _, err := p.expect(lexer.COLON); err != nil {
		return nil, err
	}

	body, err := p.parseColonBody()
	if err != nil {
		return nil, err
	}

	if len(tags) == 0 {
		return body, nil
	}

	tb := &ast.TaggedBlock{Tags: tags, Body: body}
	tb.SetPos(pos)

	return tb, nil
}

// parseColonBody parses the body after a `:` -- either an indented block on
// the next line(s), or a single inline statement on the same line.  Returns
// an *ast.Block in both cases.
func (p *Parser) parseColonBody() (*ast.Block, error) {
	if p.check(lexer.NEWLINE) {
		p.advance()
		p.skipNewlines()

		if p.check(lexer.INDENT) {
			return p.parseBlock()
		}

		return &ast.Block{}, nil
	}

	stmt, err := p.parseStatement()
	if err != nil {
		return nil, err
	}

	if stmt == nil {
		return &ast.Block{}, nil
	}

	return &ast.Block{Stmts: []ast.Node{stmt}}, nil
}

func (p *Parser) parseDeferStmt() (*ast.DeferStmt, error) {
	p.advance() // consume defer

	// defer do: <body>  ->  defer (fn() void = <body>)()
	if p.check(lexer.KW_DO) {
		p.advance() // consume do

		if _, err := p.expect(lexer.COLON); err != nil {
			return nil, err
		}

		var (
			body ast.Node
			err  error
		)

		if p.check(lexer.NEWLINE) {
			p.advance()
			p.skipNewlines()

			if p.check(lexer.INDENT) {
				body, err = p.parseBlock()
				if err != nil {
					return nil, err
				}
			} else {
				body = &ast.Block{}
			}
		} else {
			stmt, err2 := p.parseStatement()
			if err2 != nil {
				return nil, err2
			}

			body = &ast.Block{Stmts: []ast.Node{stmt}}
		}

		lambda := &ast.LambdaExpr{Body: body}
		call := &ast.CallExpr{Func: lambda}

		return &ast.DeferStmt{Call: call}, nil
	}

	// defer <expr>  - must be a call expression; bare lambdas are not allowed.
	call, err := p.parseExpr()
	if err != nil {
		return nil, err
	}

	if lambda, isLambda := call.(*ast.LambdaExpr); isLambda {
		call = &ast.CallExpr{Func: lambda, Args: []ast.Node{}}
	}

	return &ast.DeferStmt{Call: call}, nil
}

func (p *Parser) parseEchoStmt() (*ast.EchoStmt, error) {
	pos := p.curPos()
	p.advance() // consume echo

	val, err := p.parseExpr()
	if err != nil {
		return nil, err
	}

	node := &ast.EchoStmt{Value: val}
	node.SetPos(pos)

	return node, nil
}

func (p *Parser) parseIfStmt() (*ast.IfStmt, error) {
	startTok := p.peek()
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
	stmt.SetPos(ast.Pos{Line: startTok.Line, Col: startTok.Col})

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

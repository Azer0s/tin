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

	// If parseTags() consumed a {#tag} block, the next token is the body opener.
	// Build the TaggedBlock using the already-parsed tags.
	if len(tags) > 0 && p.check(lexer.LBRACE) {
		return p.parseTaggedBlockWithTags(tags)
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
		return p.parseTypeDecl()
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
		// { #tag } { body }  tagged block - tags not yet parsed (legacy path)
		if p.peekAt(1).Type == lexer.CONTROL_TAG {
			return p.parseTaggedBlock()
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

	// Shorthand for-in without 'let': for c in iter: / for c T in iter:
	// Detect: IDENT [IDENT|type] KW_IN ...
	if !p.check(lexer.KW_LET) && p.check(lexer.IDENT) {
		// Look ahead: skip optional type token(s), then check for KW_IN.
		// Simple heuristic: if token[1] or token[2] is KW_IN, treat as for-in.
		t1 := p.peekAt(1)
		t2 := p.peekAt(2)

		isForIn := t1.Type == lexer.KW_IN || t2.Type == lexer.KW_IN
		if isForIn {
			nameTok := p.advance() // consume variable name
			stmt.VarName = nameTok.Literal
			// Optional type annotation between name and 'in'
			if !p.check(lexer.KW_IN) {
				var typeErr error

				stmt.VarType, typeErr = p.parseTypeExpr()
				if typeErr != nil {
					return nil, typeErr
				}
			}

			if _, err := p.expect(lexer.KW_IN); err != nil {
				return nil, err
			}

			stmt.Kind = ast.ForIn

			var iterErr error

			stmt.Iter, iterErr = p.parseExpr()
			if iterErr != nil {
				return nil, iterErr
			}

			if _, err := p.expect(lexer.COLON); err != nil {
				return nil, err
			}

			if p.check(lexer.NEWLINE) {
				p.advance()
				p.skipNewlines()

				if p.check(lexer.INDENT) {
					var bodyErr error

					stmt.Body, bodyErr = p.parseBlock()
					if bodyErr != nil {
						return nil, bodyErr
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
	}

	// Condition-only (while-style): for <bool-expr>:
	// Triggered when there is no leading 'let' keyword.
	if !p.check(lexer.KW_LET) {
		cond, err := p.parseExpr()
		if err != nil {
			return nil, err
		}

		stmt.Kind = ast.ForWhile
		stmt.Cond = cond

		if _, err := p.expect(lexer.COLON); err != nil {
			return nil, err
		}

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

	// Expect "let varName [type]"
	p.advance() // consume let

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
	// match a.(type): -> set IsType
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

// parseAwaitMatchStmt parses:
//
//	await match [e1, e2, e3]:
//	  case [x, _, _]: body
//	  case [_, y, _] if guard: body
//	  default: body
//
// The bracket list is parsed as a positional awaitable list, NOT an array literal.
// Compiler errors are emitted for non-literal array syntax, wrong pattern lengths,
// and invalid patterns (zero or multiple bindings per case).
func (p *Parser) parseAwaitMatchStmt() (*ast.AwaitMatchStmt, error) {
	awaitPos := p.peek() // position of "await" keyword
	p.advance()          // consume "await"
	p.advance()          // consume "match"

	// Require inline array literal.
	if !p.check(lexer.LBRACKET) {
		return nil, fmt.Errorf("await match requires an inline array literal [...]; variable and computed arrays are not yet supported (at %d:%d)",
			p.peek().Line, p.peek().Col)
	}

	p.advance() // consume "["

	var futures []ast.Node

	for !p.check(lexer.RBRACKET) && !p.check(lexer.EOF) {
		p.skipWhitespace()

		if p.check(lexer.RBRACKET) {
			break
		}

		expr, err := p.parseExpr()
		if err != nil {
			return nil, err
		}

		futures = append(futures, expr)

		p.skipWhitespace()

		if p.check(lexer.COMMA) {
			p.advance()
		}
	}

	if _, err := p.expect(lexer.RBRACKET); err != nil {
		return nil, err
	}

	if len(futures) == 0 {
		return nil, fmt.Errorf("await match requires at least one future in the array literal")
	}

	if _, err := p.expect(lexer.COLON); err != nil {
		return nil, err
	}

	stmt := &ast.AwaitMatchStmt{Futures: futures}

	if p.check(lexer.NEWLINE) {
		p.advance()
	}

	p.skipNewlines()

	if !p.check(lexer.INDENT) {
		return nil, fmt.Errorf("expected indented block after await match")
	}

	p.advance() // consume INDENT
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
					var err error

					stmt.Default, err = p.parseBlock()
					if err != nil {
						return nil, err
					}
				}
			} else if !p.check(lexer.EOF) && !p.check(lexer.DEDENT) {
				s, err := p.parseStatement()
				if err != nil {
					return nil, err
				}

				if s != nil {
					stmt.Default = &ast.Block{Stmts: []ast.Node{s}}
				}
			}
		} else if p.check(lexer.KW_CASE) {
			mc, err := p.parseAwaitMatchCase(len(futures))
			if err != nil {
				return nil, err
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

	// Warn only when every case arm has a guard and there is no default arm: in that
	// situation the all-exhausted path will always panic at runtime.
	if stmt.Default == nil && !p.noWarnAwaitMatchGuards {
		allGuarded := len(stmt.Cases) > 0
		for _, mc := range stmt.Cases {
			if mc.Guard == nil {
				allGuarded = false

				break
			}
		}

		if allGuarded {
			fmt.Printf("%d:%d: warning: every await match arm has a guard and there is no default arm - if all futures complete without a passing guard, the program will panic at runtime; hint: add a 'default:' arm or remove at least one guard, or suppress with -Wno-await-match-guards\n",
				awaitPos.Line, awaitPos.Col)
		}
	}

	return stmt, nil
}

// parseAwaitMatchCase parses one "case [x, _, _] if guard: body" arm.
// nFutures is the expected pattern length for validation.
func (p *Parser) parseAwaitMatchCase(nFutures int) (ast.AwaitMatchCase, error) {
	pos := p.curPos()

	if _, err := p.expect(lexer.KW_CASE); err != nil {
		return ast.AwaitMatchCase{}, err
	}

	// Must be an array pattern.
	if !p.check(lexer.LBRACKET) {
		return ast.AwaitMatchCase{}, fmt.Errorf("await match case must use an array pattern [...] (at %d:%d)",
			p.peek().Line, p.peek().Col)
	}

	p.advance() // consume "["

	type slot struct {
		name   string
		isWild bool
	}

	var slots []slot

	for !p.check(lexer.RBRACKET) && !p.check(lexer.EOF) {
		p.skipWhitespace()

		if p.check(lexer.RBRACKET) {
			break
		}

		if p.check(lexer.IDENT) && p.peek().Literal == "_" {
			p.advance()

			slots = append(slots, slot{isWild: true})
		} else if p.check(lexer.IDENT) {
			name := p.advance().Literal
			slots = append(slots, slot{name: name})
		} else {
			return ast.AwaitMatchCase{}, fmt.Errorf("unexpected token in await match pattern: %s (at %d:%d)",
				p.peek().Type, p.peek().Line, p.peek().Col)
		}

		p.skipWhitespace()

		if p.check(lexer.COMMA) {
			p.advance()
		}
	}

	if _, err := p.expect(lexer.RBRACKET); err != nil {
		return ast.AwaitMatchCase{}, err
	}

	// Validate pattern length.
	if len(slots) != nFutures {
		return ast.AwaitMatchCase{}, fmt.Errorf("await match pattern length %d does not match futures array length %d",
			len(slots), nFutures)
	}

	// Validate exactly one binding slot.
	bindIdx := -1

	for i, s := range slots {
		if !s.isWild {
			if bindIdx >= 0 {
				return ast.AwaitMatchCase{}, fmt.Errorf("await match case must have exactly one binding slot; found multiple non-wildcard slots")
			}

			bindIdx = i
		}
	}

	if bindIdx < 0 {
		return ast.AwaitMatchCase{}, fmt.Errorf("await match case has no binding slot; use 'default:' for an unconditional arm")
	}

	mc := ast.AwaitMatchCase{
		Pos:      pos,
		SlotIdx:  bindIdx,
		BindName: slots[bindIdx].name,
	}

	// Optional guard.
	if p.check(lexer.KW_IF) {
		p.advance()

		guard, err := p.parseExpr()
		if err != nil {
			return ast.AwaitMatchCase{}, err
		}

		mc.Guard = guard
	}

	if _, err := p.expect(lexer.COLON); err != nil {
		return ast.AwaitMatchCase{}, err
	}

	// Parse body.
	if p.check(lexer.NEWLINE) {
		p.advance()
		p.skipNewlines()

		if p.check(lexer.INDENT) {
			var err error

			mc.Body, err = p.parseBlock()
			if err != nil {
				return ast.AwaitMatchCase{}, err
			}
		}
	} else if !p.check(lexer.EOF) && !p.check(lexer.KW_CASE) && !p.check(lexer.KW_DEFAULT) {
		s, err := p.parseStatement()
		if err != nil {
			return ast.AwaitMatchCase{}, err
		}

		if s != nil {
			mc.Body = &ast.Block{Stmts: []ast.Node{s}}
		}
	}

	return mc, nil
}

func (p *Parser) parseMatchCase() (ast.MatchCase, error) {
	pos := p.curPos()
	if _, err := p.expect(lexer.KW_CASE); err != nil {
		return ast.MatchCase{}, err
	}

	mc := ast.MatchCase{Pos: pos}

	// Array pattern: "case [elem, ...rest]:"
	if p.check(lexer.LBRACKET) {
		ap, err := p.parseArrayPattern()
		if err != nil {
			return ast.MatchCase{}, err
		}

		mc.Pattern = ap
	} else if p.check(lexer.IDENT) && p.peekAt(1).Type == lexer.LBRACE {
		// Struct pattern: "case TypeName{...}:"
		sp, err := p.parseStructPattern()
		if err != nil {
			return ast.MatchCase{}, err
		}

		mc.Pattern = sp
	} else {
		// case varName TypeExpr: OR case expr:
		// Detect "case varName TypeName:" -- either TypeName is a built-in type token
		// OR it is a user-defined type (plain IDENT) followed immediately by ":".
		// The second heuristic handles "case _ json_null:" where json_null is a struct.
		nextIsUserType := p.check(lexer.IDENT) &&
			p.peekAt(1).Type == lexer.IDENT &&
			p.peekAt(2).Type == lexer.COLON
		if p.check(lexer.IDENT) && !isTypeToken(p.peekAt(1)) && !nextIsUserType {
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
	}

	// Optional guard: "if guard_expr"
	if p.check(lexer.KW_IF) {
		p.advance()

		guard, err := p.parseExpr()
		if err != nil {
			return ast.MatchCase{}, err
		}

		mc.Guard = guard
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
		// Inline body: case foo: return bar  OR  case foo: expr (match expression)
		stmt, err := p.parseStatement()
		if err != nil {
			return ast.MatchCase{}, err
		}

		mc.Body = &ast.Block{Stmts: []ast.Node{stmt}}
	}

	return mc, nil
}

func (p *Parser) parseStructPattern() (*ast.StructPattern, error) {
	startPos := p.curPos()
	typeName := p.advance().Literal // consume IDENT (type name)
	p.advance()                     // consume {

	sp := &ast.StructPattern{TypeName: typeName}
	sp.SetPos(startPos)

	for !p.check(lexer.RBRACE) && !p.check(lexer.EOF) {
		p.skipWhitespace()

		if p.check(lexer.RBRACE) {
			break
		}

		// "_": wildcard field - match but discard
		if p.check(lexer.IDENT) && p.peek().Literal == "_" {
			p.advance()

			sp.Fields = append(sp.Fields, ast.StructPatternField{IsWild: true})
		} else if p.check(lexer.IDENT) {
			name := p.advance().Literal

			if p.check(lexer.COLON) {
				p.advance() // consume :

				// IDENT { = nested struct pattern
				if p.check(lexer.IDENT) && p.peekAt(1).Type == lexer.LBRACE {
					nested, err := p.parseStructPattern()
					if err != nil {
						return nil, err
					}

					sp.Fields = append(sp.Fields, ast.StructPatternField{Name: name, Literal: nested})
				} else if p.isRenameBinding() {
					// Bare IDENT not followed by expression-continuation tokens -> rename.
					bindTo := p.advance().Literal
					sp.Fields = append(sp.Fields, ast.StructPatternField{Name: name, BindTo: bindTo})
				} else {
					lit, err := p.parseExpr()
					if err != nil {
						return nil, err
					}

					sp.Fields = append(sp.Fields, ast.StructPatternField{Name: name, Literal: lit})
				}
			} else {
				// bare name: bind field to this name
				sp.Fields = append(sp.Fields, ast.StructPatternField{Name: name})
			}
		} else {
			break
		}

		p.skipWhitespace()

		if p.check(lexer.COMMA) {
			p.advance()
		}
	}

	if _, err := p.expect(lexer.RBRACE); err != nil {
		return nil, err
	}

	return sp, nil
}

// parseArrayPattern parses "[elem, ...rest]" array destructuring patterns.
// Called when the current token is LBRACKET.
func (p *Parser) parseArrayPattern() (*ast.ArrayPattern, error) {
	p.advance() // consume [

	ap := &ast.ArrayPattern{}

	for !p.check(lexer.RBRACKET) && !p.check(lexer.EOF) {
		p.skipWhitespace()

		if p.check(lexer.RBRACKET) {
			break
		}

		elem := ast.ArrayPatternElement{}

		if p.check(lexer.DOTDOTDOT) {
			p.advance() // consume ...

			elem.IsRest = true

			if p.check(lexer.IDENT) {
				lit := p.peek().Literal
				if lit == "_" {
					p.advance()

					elem.IsWild = true
				} else {
					elem.Name = p.advance().Literal
				}
			}

			ap.Elems = append(ap.Elems, elem)

			break // rest must be last
		} else if p.check(lexer.IDENT) {
			lit := p.peek().Literal
			if lit == "_" {
				p.advance()

				elem.IsWild = true
			} else {
				elem.Name = p.advance().Literal
			}

			ap.Elems = append(ap.Elems, elem)
		} else {
			return nil, fmt.Errorf("unexpected token in array pattern: %s", p.peek().Type)
		}

		p.skipWhitespace()

		if p.check(lexer.COMMA) {
			p.advance()
		}
	}

	if _, err := p.expect(lexer.RBRACKET); err != nil {
		return nil, err
	}

	return ap, nil
}

// isRenameBinding returns true when the current position holds a bare IDENT
// that should be treated as a rename target (field: newName) rather than an
// expression to evaluate as a constraint. A bare IDENT is one not followed by
// any token that would continue an expression: (, [, {, ., ::, or an operator.
func (p *Parser) isRenameBinding() bool {
	if !p.check(lexer.IDENT) {
		return false
	}

	next := p.peekAt(1).Type
	switch next {
	case lexer.LPAREN, lexer.LBRACKET, lexer.LBRACE,
		lexer.DOT, lexer.DCOLON,
		lexer.PLUS, lexer.MINUS, lexer.STAR, lexer.SLASH, lexer.PERCENT,
		lexer.EQEQ, lexer.NEQ, lexer.LT, lexer.GT, lexer.LTEQ, lexer.GTEQ,
		lexer.AND, lexer.OR, lexer.PIPE, lexer.KW_AS:
		return false
	}

	return true
}

// parseTaggedBlock handles { #tag } { body } when tags haven't been pre-parsed.
func (p *Parser) parseTaggedBlock() (*ast.TaggedBlock, error) {
	tags := p.parseTags()

	return p.parseTaggedBlockWithTags(tags)
}

// parseTaggedBlockWithTags builds a TaggedBlock given already-parsed tags.
// The current token must be { (the body opening brace).
// The body can be:
//   - Inline brace form:   { ... }
//   - Indented brace form: {\n  ...\n} (INDENT/DEDENT inside braces)
func (p *Parser) parseTaggedBlockWithTags(tags []string) (*ast.TaggedBlock, error) {
	if p.check(lexer.LBRACE) {
		p.advance()
	}
	// Skip optional newline and consume INDENT if body is on the next line.
	p.skipNewlines()

	indented := false

	if p.check(lexer.INDENT) {
		p.advance()

		indented = true
	}

	var stmts []ast.Node

	for {
		p.skipNewlines()

		if p.check(lexer.EOF) {
			break
		}

		if indented && p.check(lexer.DEDENT) {
			p.advance()

			break
		}

		if !indented && p.check(lexer.RBRACE) {
			break
		}

		s, err := p.parseStatement()
		if err != nil {
			return nil, err
		}

		if s != nil {
			stmts = append(stmts, s)
		}
	}
	// Consume the closing brace (if present - it may have been preceded by DEDENT)
	if p.check(lexer.RBRACE) {
		p.advance()
	}

	return &ast.TaggedBlock{Tags: tags, Body: &ast.Block{Stmts: stmts}}, nil
}

// parseExprStatement handles assignments, augmented assignments, postfixes,
// and bare expression statements
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
	default:
		return "", false
	}
}

// parseLetStmt handles let/const statements including destructuring forms.
// Returns *ast.VarDecl, *ast.ArrayDestructDecl, or *ast.StructDestructDecl.
func (p *Parser) parseLetStmt() (ast.Node, error) {
	pos := p.curPos()
	isConst := p.peek().Type == lexer.KW_CONST
	p.advance() // consume let/const

	// Array destructuring: let [a, b] [T] = expr
	//                       let [x, ...xs] [T] = expr
	//                       let [a, b] [T1, T2] = expr
	if p.check(lexer.LBRACKET) {
		return p.parseArrayDestructDecl(isConst, pos)
	}

	// Struct destructuring: let {x, y} TypeName = expr
	if p.check(lexer.LBRACE) {
		return p.parseStructDestructDecl(isConst, pos)
	}

	// Tuple destructuring: let (x, y) = expr
	if p.check(lexer.LPAREN) {
		return p.parseTupleDestructDecl(isConst, pos)
	}

	// Normal variable declaration
	nameTok, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, err
	}

	var typ ast.TypeExpr
	if !p.match(lexer.ASSIGN, lexer.NEWLINE, lexer.EOF, lexer.SEMI) {
		typ, err = p.parseTypeExpr()
		if err != nil {
			return nil, err
		}
	}

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

// parseArrayDestructDecl parses: let [a, b] [T] = expr
//
//	let [x, ...xs] [T] = expr
//	let [a, b] [T1, T2] = expr
func (p *Parser) parseArrayDestructDecl(isConst bool, pos ast.Pos) (*ast.ArrayDestructDecl, error) {
	p.advance() // consume [

	var names []string

	for !p.check(lexer.RBRACKET) && !p.check(lexer.EOF) {
		if p.check(lexer.DOTDOTDOT) {
			p.advance() // consume ...

			nameTok, err := p.expect(lexer.IDENT)
			if err != nil {
				return nil, err
			}

			names = append(names, "..."+nameTok.Literal)
		} else {
			nameTok, err := p.expect(lexer.IDENT)
			if err != nil {
				return nil, err
			}

			names = append(names, nameTok.Literal)
		}

		if p.check(lexer.COMMA) {
			p.advance()
		}
	}

	if _, err := p.expect(lexer.RBRACKET); err != nil {
		return nil, err
	}

	// Validate: at most one rest name, must be last; if rest present, total == 2
	restCount := 0

	for i, n := range names {
		if len(n) > 3 && n[:3] == "..." {
			restCount++

			if i != len(names)-1 {
				return nil, p.errorf("rest element '...%s' must be the last in destructuring pattern", n[3:])
			}
		}
	}

	if restCount > 0 && len(names) != 2 {
		return nil, p.errorf("rest destructuring requires exactly 2 elements (one name + one ...rest)")
	}

	// Parse type annotation: [T] or [T1, T2, ...] or @[T1, T2, ...] or NamedType
	var elemTypes []ast.TypeExpr

	isAny := false

	var namedType ast.TypeExpr // non-nil when type is a named alias (resolved at codegen)

	if p.check(lexer.AT) {
		// @[T1, T2, ...] - tuple-array type alias sugar
		t, err := p.parseTypeExpr() // parseTypeSingle handles @[...]
		if err != nil {
			return nil, err
		}

		if tat, ok := t.(*ast.TupleArrayType); ok {
			elemTypes = tat.ElemTypes
			isAny = true
		} else {
			namedType = t
		}
	} else if p.check(lexer.LBRACKET) {
		p.advance() // consume [
		// Parse all types separated by commas
		for !p.check(lexer.RBRACKET) && !p.check(lexer.EOF) {
			t, err := p.parseTypeExpr()
			if err != nil {
				return nil, err
			}

			elemTypes = append(elemTypes, t)

			if p.check(lexer.COMMA) {
				p.advance()
			}
		}

		if _, err := p.expect(lexer.RBRACKET); err != nil {
			return nil, err
		}

		if len(elemTypes) == 1 {
			if st, ok := elemTypes[0].(*ast.SimpleType); ok && st.Name == "any" {
				isAny = true
			}
		} else if len(elemTypes) > 1 {
			isAny = true // multiple types -> [any] with per-slot cast
		}
	} else if !p.check(lexer.ASSIGN) {
		// Named type alias (e.g. `res` from `type res = @[i32, bool]`)
		t, err := p.parseTypeExpr()
		if err != nil {
			return nil, err
		}

		namedType = t
	}
	// Validate: per-slot types count must match non-rest names
	nonRestCount := len(names) - restCount
	if len(elemTypes) > 1 && len(elemTypes) != nonRestCount {
		return nil, p.errorf("number of per-slot types (%d) must match number of variables (%d)", len(elemTypes), nonRestCount)
	}

	if _, err := p.expect(lexer.ASSIGN); err != nil {
		return nil, err
	}

	val, err := p.parseExpr()
	if err != nil {
		return nil, err
	}

	_ = isConst

	node := &ast.ArrayDestructDecl{Names: names, ElemTypes: elemTypes, IsAny: isAny, NamedType: namedType, Value: val}
	node.SetPos(pos)

	return node, nil
}

// parseStructDestructDecl parses: let {x, y} TypeName = expr
// or the aliased form: let {x: a, y: b} TypeName = expr
func (p *Parser) parseStructDestructDecl(isConst bool, pos ast.Pos) (*ast.StructDestructDecl, error) {
	p.advance() // consume {

	var (
		names    []string
		varNames []string
	)

	hasAliases := false

	for !p.check(lexer.RBRACE) && !p.check(lexer.EOF) {
		nameTok, err := p.expect(lexer.IDENT)
		if err != nil {
			return nil, err
		}

		fieldName := nameTok.Literal
		varName := fieldName

		if p.check(lexer.COLON) {
			p.advance() // consume :

			varTok, err2 := p.expect(lexer.IDENT)
			if err2 != nil {
				return nil, err2
			}

			varName = varTok.Literal
			hasAliases = true
		}

		names = append(names, fieldName)
		varNames = append(varNames, varName)

		if p.check(lexer.COMMA) {
			p.advance()
		}
	}

	if _, err := p.expect(lexer.RBRACE); err != nil {
		return nil, err
	}

	if !hasAliases {
		varNames = nil // no aliases - use Names directly
	}

	// Parse struct type
	structType, err := p.parseTypeExpr()
	if err != nil {
		return nil, err
	}

	if _, err := p.expect(lexer.ASSIGN); err != nil {
		return nil, err
	}

	val, err := p.parseExpr()
	if err != nil {
		return nil, err
	}

	_ = isConst

	node := &ast.StructDestructDecl{Names: names, VarNames: varNames, StructType: structType, Value: val}
	node.SetPos(pos)

	return node, nil
}

// parseTupleDestructDecl parses: let (x, y) = expr
func (p *Parser) parseTupleDestructDecl(isConst bool, pos ast.Pos) (*ast.TupleDestructDecl, error) {
	p.advance() // consume (

	var names []string

	for !p.check(lexer.RPAREN) && !p.check(lexer.EOF) {
		nameTok, err := p.expect(lexer.IDENT)
		if err != nil {
			return nil, err
		}

		names = append(names, nameTok.Literal)

		if p.check(lexer.COMMA) {
			p.advance()
		}
	}

	if _, err := p.expect(lexer.RPAREN); err != nil {
		return nil, err
	}

	if _, err := p.expect(lexer.ASSIGN); err != nil {
		return nil, err
	}

	val, err := p.parseExpr()
	if err != nil {
		return nil, err
	}

	node := &ast.TupleDestructDecl{IsConst: isConst, Names: names, Value: val}
	node.SetPos(pos)

	return node, nil
}

// parseTopLevelVar parses:  var name Type [= expr]
func (p *Parser) parseTopLevelVar() (*ast.TopLevelVar, error) {
	p.advance() // consume var

	nameTok, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, err
	}

	typ, err := p.parseTypeExpr()
	if err != nil {
		return nil, err
	}

	var val ast.Node

	if p.check(lexer.ASSIGN) {
		p.advance()

		val, err = p.parseExpr()
		if err != nil {
			return nil, err
		}
	}

	return &ast.TopLevelVar{Name: nameTok.Literal, Type: typ, Value: val}, nil
}

// parseSpawnExprStmt parses a spawn statement (spawn expr or spawn do: block).
// Returns a *ast.SpawnExpr wrapped in an ExprStmt.
func (p *Parser) parseSpawnExprStmt() (ast.Node, error) {
	expr, err := p.parseSpawnExpr()
	if err != nil {
		return nil, err
	}

	return &ast.ExprStmt{Expr: expr}, nil
}

// parseSpawnExpr parses spawn as an expression.
func (p *Parser) parseSpawnExpr() (*ast.SpawnExpr, error) {
	p.advance() // consume spawn

	// spawn do: block
	if p.check(lexer.KW_DO) {
		p.advance() // consume do

		if _, err := p.expect(lexer.COLON); err != nil {
			return nil, err
		}

		var (
			block *ast.Block
			err   error
		)

		if p.check(lexer.NEWLINE) {
			p.advance()

			if p.check(lexer.INDENT) {
				// parseBlock consumes INDENT itself; do not advance here.
				block, err = p.parseBlock()
				if err != nil {
					return nil, err
				}
			}
		} else {
			stmt, err := p.parseStatement()
			if err != nil {
				return nil, err
			}

			block = &ast.Block{Stmts: []ast.Node{stmt}}
		}

		return &ast.SpawnExpr{DoBlock: block}, nil
	}

	// spawn expr
	call, err := p.parseExpr()
	if err != nil {
		return nil, err
	}

	return &ast.SpawnExpr{Call: call}, nil
}

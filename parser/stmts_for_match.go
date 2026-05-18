package parser

import (
	"github.com/Azer0s/tin/ast"
	"github.com/Azer0s/tin/lexer"
)

func (p *Parser) parseForStmt() (*ast.ForStmt, error) {
	pos := p.curPos()
	p.advance() // consume for

	stmt := &ast.ForStmt{}
	stmt.SetPos(pos)

	// for ref name in iter: -- the loop variable aliases each slot of
	// `iter` instead of taking a per-iteration copy. Assignments inside
	// the body mutate the underlying array. Only valid over a mutable,
	// directly-indexable iter; ranges and `let`/`const` arrays are
	// rejected at codegen time.
	if p.check(lexer.KW_REF) {
		p.advance() // consume ref

		nameTok, err := p.expect(lexer.IDENT)
		if err != nil {
			return nil, err
		}

		stmt.VarName = nameTok.Literal
		stmt.IsRef = true

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
	matchTok := p.advance() // consume match

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
	stmt.SetPos(ast.Pos{Line: matchTok.Line, Col: matchTok.Col})

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
//	await match (e1, e2, e3):
//	  case (x, _, _): body
//	  case (_, y, _) if guard: body
//	  default: body
//
// The paren list is a positional awaitable tuple, NOT a tuple literal.
// Compiler errors are emitted for non-literal tuple syntax, wrong pattern
// lengths, and invalid patterns (zero or multiple bindings per case).
func (p *Parser) parseAwaitMatchStmt() (*ast.AwaitMatchStmt, error) {
	awaitPos := p.peek() // position of "await" keyword
	p.advance()          // consume "await"
	p.advance()          // consume "match"

	// Require inline tuple literal.
	if !p.check(lexer.LPAREN) {
		return nil, p.errAtTok(p.peek(), "await match requires an inline tuple (...); variable and computed tuples are not yet supported")
	}

	p.advance() // consume "("

	var futures []ast.Node

	for !p.check(lexer.RPAREN) && !p.check(lexer.EOF) {
		p.skipWhitespace()

		if p.check(lexer.RPAREN) {
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

	if _, err := p.expect(lexer.RPAREN); err != nil {
		return nil, err
	}

	if len(futures) == 0 {
		return nil, p.errAtTok(awaitPos, "await match requires at least one future in the tuple")
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
		return nil, p.errAtTok(p.peek(), "expected indented block after await match")
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
			p.warnAt(awaitPos.Line, awaitPos.Col, "await-match-guards",
				"every await match arm has a guard and there is no default arm - if all futures complete without a passing guard, the program will panic at runtime; hint: add a 'default:' arm or remove at least one guard")
		}
	}

	return stmt, nil
}

// parseAwaitMatchCase parses one "case (x, _, _) if guard: body" arm.
// nFutures is the expected pattern length for validation.
func (p *Parser) parseAwaitMatchCase(nFutures int) (ast.AwaitMatchCase, error) {
	pos := p.curPos()

	if _, err := p.expect(lexer.KW_CASE); err != nil {
		return ast.AwaitMatchCase{}, err
	}

	// Must be a tuple pattern.
	if !p.check(lexer.LPAREN) {
		return ast.AwaitMatchCase{}, p.errAtTok(p.peek(), "await match case must use a tuple pattern (...)")
	}

	p.advance() // consume "("

	type slot struct {
		name   string
		isWild bool
	}

	var slots []slot

	for !p.check(lexer.RPAREN) && !p.check(lexer.EOF) {
		p.skipWhitespace()

		if p.check(lexer.RPAREN) {
			break
		}

		if p.check(lexer.IDENT) && p.peek().Literal == "_" {
			p.advance()

			slots = append(slots, slot{isWild: true})
		} else if p.check(lexer.IDENT) {
			name := p.advance().Literal
			slots = append(slots, slot{name: name})
		} else {
			return ast.AwaitMatchCase{}, p.errAtTok(p.peek(), "unexpected token in await match pattern: %s", p.peek().Type)
		}

		p.skipWhitespace()

		if p.check(lexer.COMMA) {
			p.advance()
		}
	}

	if _, err := p.expect(lexer.RPAREN); err != nil {
		return ast.AwaitMatchCase{}, err
	}

	// Validate pattern length.
	if len(slots) != nFutures {
		return ast.AwaitMatchCase{}, p.errAt(pos.Line, pos.Col, "await match pattern length %d does not match futures tuple length %d",
			len(slots), nFutures)
	}

	// Validate exactly one binding slot.
	bindIdx := -1

	for i, s := range slots {
		if !s.isWild {
			if bindIdx >= 0 {
				return ast.AwaitMatchCase{}, p.errAt(pos.Line, pos.Col, "await match case must have exactly one binding slot; found multiple non-wildcard slots")
			}

			bindIdx = i
		}
	}

	if bindIdx < 0 {
		return ast.AwaitMatchCase{}, p.errAt(pos.Line, pos.Col, "await match case has no binding slot; use 'default:' for an unconditional arm")
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
		// Detect "case varName TypeName:" -- either TypeName is a built-in type keyword
		// OR it is a user-defined type (plain IDENT) followed immediately by ":".
		// The second heuristic handles "case _ json_null:" where json_null is a struct.
		// Note: we explicitly check for type KEYWORDS (i64, string, ...) rather than
		// type TOKENS because tokens like "(" start ADT constructor patterns like
		// "case Circle(r):", which must be parsed as expressions, not as var-type.
		nextIsUserType := p.check(lexer.IDENT) &&
			p.peekAt(1).Type == lexer.IDENT &&
			p.peekAt(2).Type == lexer.COLON
		if p.check(lexer.IDENT) && !isTypeKeyword(p.peekAt(1)) && !nextIsUserType {
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

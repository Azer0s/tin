package parser

import (
	"strconv"

	"github.com/Azer0s/tin/ast"
	"github.com/Azer0s/tin/lexer"
)

// Expression precedence (lowest -> highest):
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
		// ++ is both a binary concat operator and a postfix increment
		// Only treat it as binary concat when followed by a valid expression
		// start; otherwise leave it for parseExprStatement's postfix check
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

// isExprStart returns true if tok can begin a primary expression
func isExprStart(tok lexer.Token) bool {
	switch tok.Type {
	case lexer.IDENT, lexer.INT_LIT, lexer.FLOAT_LIT, lexer.STRING_LIT, lexer.BACKTICK_LIT,
		lexer.BOOL_LIT, lexer.CHAR_LIT, lexer.NONE_LIT,
		lexer.LPAREN, lexer.LBRACKET, lexer.MINUS, lexer.NOT,
		lexer.STAR, lexer.AMP, lexer.KW_FN, lexer.KW_SIZEOF, lexer.KW_ADDR,
		lexer.KW_TYPEOF, lexer.KW_TRAITOF, lexer.KW_FIELDNAMES, lexer.KW_FIELDTYPES,
		lexer.KW_FIELDTAG, lexer.KW_GETFIELD, lexer.KW_SETFIELD:
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
			} else if idx, ok2 := expr.(*ast.IndexExpr); ok2 {
				// e.g. result[u32]::ok(42) — static method call on generic type
				if idExpr, ok3 := idx.Expr.(*ast.Identifier); ok3 {
					typeName := idExpr.Name
					if typeArgId, ok4 := idx.Index.(*ast.Identifier); ok4 {
						typeName = idExpr.Name + "[" + typeArgId.Name + "]"
					}
					sa := &ast.ScopeAccess{Path: []string{typeName, field.Literal}}
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

	case lexer.BACKTICK_LIT:
		p.advance()
		return &ast.BacktickLit{Content: tok.Literal}, nil

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

	case lexer.KW_TYPEOF:
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
		return &ast.TypeofExpr{Expr: inner}, nil

	case lexer.KW_TRAITOF:
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
		return &ast.TraitofExpr{Expr: inner}, nil

	case lexer.KW_FIELDNAMES:
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
		return &ast.FieldnamesExpr{Expr: inner}, nil

	case lexer.KW_FIELDTYPES:
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
		return &ast.FieldtypesExpr{Expr: inner}, nil

	case lexer.KW_FIELDTAG:
		p.advance()
		if _, err := p.expect(lexer.LPAREN); err != nil {
			return nil, err
		}
		expr, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(lexer.COMMA); err != nil {
			return nil, err
		}
		field, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(lexer.RPAREN); err != nil {
			return nil, err
		}
		return &ast.FieldtagExpr{Expr: expr, Field: field}, nil

	case lexer.KW_GETFIELD:
		p.advance()
		if _, err := p.expect(lexer.LPAREN); err != nil {
			return nil, err
		}
		expr, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(lexer.COMMA); err != nil {
			return nil, err
		}
		field, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(lexer.RPAREN); err != nil {
			return nil, err
		}
		return &ast.GetfieldExpr{Expr: expr, Field: field}, nil

	case lexer.KW_SETFIELD:
		p.advance()
		if _, err := p.expect(lexer.LPAREN); err != nil {
			return nil, err
		}
		expr, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(lexer.COMMA); err != nil {
			return nil, err
		}
		field, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(lexer.COMMA); err != nil {
			return nil, err
		}
		val, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(lexer.RPAREN); err != nil {
			return nil, err
		}
		return &ast.SetfieldExpr{Expr: expr, Field: field, Val: val}, nil

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
		// default(typeof(expr)) — derive zero value from expression's compile-time type
		if p.check(lexer.KW_TYPEOF) {
			inner, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if _, err := p.expect(lexer.RPAREN); err != nil {
				return nil, err
			}
			return &ast.DefaultExpr{OfExpr: inner}, nil
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
		// Block expression: (let x = ...; expr) — produced by CTFE macro splices.
		// Parsed as a sequence of statements terminated by ')'; the last statement
		// must be an expression whose value is returned.
		if p.check(lexer.KW_LET) {
			var stmts []ast.Node
			for !p.check(lexer.RPAREN) && !p.check(lexer.EOF) {
				if p.check(lexer.SEMI) || p.check(lexer.NEWLINE) {
					p.advance()
					continue
				}
				stmt, err := p.parseStatement()
				if err != nil {
					return nil, err
				}
				if stmt != nil {
					stmts = append(stmts, stmt)
				}
			}
			if _, err := p.expect(lexer.RPAREN); err != nil {
				return nil, err
			}
			return &ast.Block{Stmts: stmts}, nil
		}
		inner, err := p.parseExpr()
		if err != nil {
			return nil, err
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


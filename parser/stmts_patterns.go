package parser

import (
	"github.com/Azer0s/tin/ast"
	"github.com/Azer0s/tin/lexer"
)

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
			return nil, p.errAtTok(p.peek(), "unexpected token in array pattern: %s", p.peek().Type)
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

// parseExprStatement handles assignments, augmented assignments, postfixes,
// and bare expression statements

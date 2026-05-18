package parser

import (
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

	indentConsumed := 0

	for {
		if p.check(lexer.PIPE) {
			p.advance()
		} else if p.check(lexer.NEWLINE) {
			saved := p.pos
			p.advance() // consume NEWLINE

			consumedIndent := false

			if p.check(lexer.INDENT) {
				p.advance()

				indentConsumed++
				consumedIndent = true
			}

			if !p.check(lexer.PIPE) {
				p.pos = saved

				if consumedIndent {
					indentConsumed--
				}

				break
			}

			p.advance() // consume PIPE
		} else {
			break
		}

		right, err2 := p.parseTernary()
		if err2 != nil {
			return nil, err2
		}

		left = &ast.PipeExpr{Left: left, Right: right}
	}
	// Consume matching DEDENT(s) for any INDENT consumed during pipe continuation.
	if indentConsumed > 0 && p.check(lexer.NEWLINE) {
		p.advance()
	}

	for indentConsumed > 0 && p.check(lexer.DEDENT) {
		p.advance()

		indentConsumed--
	}

	return left, nil
}

func (p *Parser) parseRange() (ast.Node, error) {
	left, err := p.parseTernary()
	if err != nil {
		return nil, err
	}

	if p.check(lexer.RANGE) {
		opTok := p.advance()

		right, err2 := p.parseTernary()
		if err2 != nil {
			return nil, err2
		}

		be := &ast.BinExpr{Left: left, Op: "..", Right: right}
		be.SetPos(ast.Pos{Line: opTok.Line, Col: opTok.Col})

		return be, nil
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
		// Allow then-branch on next line; track consumed INDENTs to balance DEDENTs.
		indentConsumed := 0

		if p.check(lexer.NEWLINE) {
			p.advance()

			if p.check(lexer.INDENT) {
				p.advance()

				indentConsumed++
			}
		}

		then, err2 := p.parseOr()
		if err2 != nil {
			return nil, err2
		}

		p.skipWhitespace() // allow : on next (same-indent) line

		if _, err2 := p.expect(lexer.COLON); err2 != nil {
			return nil, err2
		}

		p.skipWhitespace() // allow else-branch on next line

		els, err2 := p.parseOr()
		if err2 != nil {
			return nil, err2
		}
		// Consume matching DEDENT(s) for any INDENTs consumed above.
		if indentConsumed > 0 && p.check(lexer.NEWLINE) {
			p.advance()
		}

		for indentConsumed > 0 && p.check(lexer.DEDENT) {
			p.advance()

			indentConsumed--
		}

		return &ast.TernaryExpr{Cond: cond, Then: then, Else: els}, nil
	}

	return cond, nil
}

func (p *Parser) parseOr() (ast.Node, error) {
	return p.parseBinaryAllowLeading(p.parseAnd, lexer.OR)
}

func (p *Parser) parseAnd() (ast.Node, error) {
	return p.parseBinaryAllowLeading(p.parseBitOr, lexer.AND)
}

func (p *Parser) parseBitOr() (ast.Node, error) {
	return p.parseBinaryAllowLeading(p.parseBitXor, lexer.BITOR)
}

func (p *Parser) parseBitXor() (ast.Node, error) {
	return p.parseBinaryAllowLeading(p.parseBitAnd, lexer.XOR)
}

func (p *Parser) parseBitAnd() (ast.Node, error) {
	// `&` doubles as the address-of prefix operator, so a line
	// starting with `&` is most often a unary statement (e.g.
	// `&local`).  Use the trailing-only continuation here.
	return p.parseBinary(p.parseEquality, lexer.AMP)
}

func (p *Parser) parseEquality() (ast.Node, error) {
	return p.parseBinaryAllowLeading(p.parseComparison, lexer.EQEQ, lexer.NEQ)
}

func (p *Parser) parseComparison() (ast.Node, error) {
	return p.parseBinaryAllowLeading(p.parseShift, lexer.LT, lexer.LTEQ, lexer.GT, lexer.GTEQ)
}

func (p *Parser) parseShift() (ast.Node, error) {
	return p.parseBinaryAllowLeading(p.parseAdditive, lexer.SHL, lexer.SHR)
}

func (p *Parser) parseAdditive() (ast.Node, error) {
	left, err := p.parseMultiplicative()
	if err != nil {
		return nil, err
	}

	indentConsumed := 0

	for {
		if p.check(lexer.PLUS) || p.check(lexer.MINUS) || p.check(lexer.INC) {
			// ++ is both a binary concat operator and a postfix increment.
			// Only treat it as binary concat when followed by a valid expression
			// start; otherwise leave it for parseExprStatement's postfix check.
			if p.check(lexer.INC) && !isExprStart(p.peekAt(1)) {
				break
			}
			// fall through - advance and parse right below
		} else if p.check(lexer.NEWLINE) {
			saved := p.pos
			p.advance() // consume NEWLINE

			if p.check(lexer.INDENT) {
				p.advance()

				indentConsumed++

				if !p.check(lexer.PLUS) && !p.check(lexer.MINUS) {
					p.pos = saved
					indentConsumed--

					break
				}
				// fall through - current token is PLUS/MINUS, advance and parse below
			} else if indentConsumed > 0 && (p.check(lexer.PLUS) || p.check(lexer.MINUS)) {
				// same-level continuation within an already-entered indent block
				// fall through - current token is PLUS/MINUS, advance and parse below
			} else {
				p.pos = saved

				break
			}
		} else {
			break
		}

		opTok := p.advance()
		op := opTok.Literal

		right, err2 := p.parseMultiplicative()
		if err2 != nil {
			return nil, err2
		}

		be := &ast.BinExpr{Left: left, Op: op, Right: right}
		be.SetPos(ast.Pos{Line: opTok.Line, Col: opTok.Col})
		left = be
	}
	// Consume matching DEDENT(s) for any INDENT consumed during additive continuation.
	if indentConsumed > 0 && p.check(lexer.NEWLINE) {
		p.advance()
	}

	for indentConsumed > 0 && p.check(lexer.DEDENT) {
		p.advance()

		indentConsumed--
	}

	return left, nil
}

// isExprStart returns true if tok can begin a primary expression
func isExprStart(tok lexer.Token) bool {
	switch tok.Type {
	case lexer.IDENT, lexer.INT_LIT, lexer.FLOAT_LIT, lexer.STRING_LIT, lexer.BACKTICK_LIT,
		lexer.BOOL_LIT, lexer.CHAR_LIT, lexer.AT,
		lexer.LPAREN, lexer.LBRACKET, lexer.MINUS, lexer.NOT,
		lexer.STAR, lexer.AMP, lexer.KW_FN, lexer.KW_SIZEOF, lexer.KW_ADDR,
		lexer.KW_TYPEOF, lexer.KW_TRAITOF, lexer.KW_FIELDNAMES, lexer.KW_FIELDTYPES,
		lexer.KW_FIELDTAG, lexer.KW_GETFIELD, lexer.KW_SETFIELD, lexer.KW_ISRC,
		lexer.KW_NIL:
		return true
	default:
		return isTypeKeyword(tok)
	}
}

func (p *Parser) parseMultiplicative() (ast.Node, error) {
	return p.parseBinary(p.parseUnary, lexer.STAR, lexer.SLASH, lexer.PERCENT)
}

// parseBinary parses a left-associative binary expression with
// trailing-operator continuation only.  Use parseBinaryAllowLeading
// for ops whose token is unambiguously binary (`||`, `&&`, `==`,
// ...); STAR / AMP / MINUS double as unary prefix operators so a
// line starting with them is usually a separate statement, not a
// continuation.
func (p *Parser) parseBinary(sub func() (ast.Node, error), ops ...lexer.TokenType) (ast.Node, error) {
	return p.parseBinaryImpl(sub, false, ops...)
}

// parseBinaryAllowLeading is like parseBinary but additionally
// accepts the operator at the start of a continuation line.
// Restricted to op sets that cannot also be unary prefix operators
// so a leading `||` continues the previous expression while a
// leading `*` stays a statement-level deref.
func (p *Parser) parseBinaryAllowLeading(sub func() (ast.Node, error), ops ...lexer.TokenType) (ast.Node, error) {
	return p.parseBinaryImpl(sub, true, ops...)
}

func (p *Parser) parseBinaryImpl(sub func() (ast.Node, error), allowLeading bool, ops ...lexer.TokenType) (ast.Node, error) {
	left, err := sub()
	if err != nil {
		return nil, err
	}

	for {
		// Operator on the same line: fall through to advance + parse.
		if p.match(ops...) {
			// nothing to do
		} else if allowLeading && p.check(lexer.NEWLINE) {
			// Operator at the start of the continuation line: peek
			// past NEWLINE (+ optional INDENT) and accept it as the
			// next op-token.  Consumed INDENTs flow into
			// continuationDedents so the outer skipNewlines drains
			// the matching DEDENTs later (same bookkeeping as the
			// trailing-op continuation below).
			saved := p.pos
			savedDedents := p.continuationDedents

			p.advance() // NEWLINE

			for p.check(lexer.INDENT) {
				p.advance()
				p.continuationDedents++
			}

			if !p.match(ops...) {
				p.pos = saved
				p.continuationDedents = savedDedents

				break
			}
		} else {
			break
		}

		opTok := p.advance()
		op := opTok.Literal

		// Line-continuation: operator at end of line.
		// Consume NEWLINE and any INDENT tokens so the right operand can start
		// on the next (possibly more-indented) line.  Track the consumed INDENTs
		// so skipNewlines can drain the matching DEDENTs later.
		if p.check(lexer.NEWLINE) {
			p.advance()

			for p.check(lexer.INDENT) {
				p.advance()
				p.continuationDedents++
			}
		}

		right, err2 := sub()
		if err2 != nil {
			return nil, err2
		}

		be := &ast.BinExpr{Left: left, Op: op, Right: right}
		be.SetPos(ast.Pos{Line: opTok.Line, Col: opTok.Col})
		left = be
	}

	return left, nil
}

// parseUnary handles prefix unary operators. `-x as T`, `!x as T`, and
// `~x as T` keep their long-standing meaning of `-(x as T)` / `!(x as T)`
// / `~(x as T)`: the inner `as` cast happens first, so out-of-range
// literals like `-9999999999999999999 as i128` widen before negating.
//
// Pointer-shape operators (`&`, `*`) are different. There the user almost
// always means "cast the result of the address-of" rather than "take the
// address of an AsExpr" (the latter isn't an lvalue and produced a
// codegen error). So those bind TIGHTER than `as` / `is` and any postfix
// cast applies to the wrapping expression.

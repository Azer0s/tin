package parser

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"

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

		op := p.advance().Literal

		right, err2 := p.parseMultiplicative()
		if err2 != nil {
			return nil, err2
		}

		left = &ast.BinExpr{Left: left, Op: op, Right: right}
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

func (p *Parser) parseBinary(sub func() (ast.Node, error), ops ...lexer.TokenType) (ast.Node, error) {
	left, err := sub()
	if err != nil {
		return nil, err
	}

	for p.match(ops...) {
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
func (p *Parser) parseUnary() (ast.Node, error) {
	if p.match(lexer.NOT, lexer.MINUS, lexer.TILDE) {
		opTok := p.advance()
		op := opTok.Literal

		expr, err := p.parseUnary()
		if err != nil {
			return nil, err
		}

		ue := &ast.UnaryExpr{Op: op, Expr: expr}
		ue.SetPos(ast.Pos{Line: opTok.Line, Col: opTok.Col})

		return ue, nil
	}
	// Dereference: *expr
	if p.check(lexer.STAR) {
		p.advance()

		expr, err := p.parsePointerOperand()
		if err != nil {
			return nil, err
		}

		return p.applyPostfixCasts(&ast.DerefExpr{Expr: expr})
	}
	// Address-of: &expr
	if p.check(lexer.AMP) {
		p.advance()

		expr, err := p.parsePointerOperand()
		if err != nil {
			return nil, err
		}

		return p.applyPostfixCasts(&ast.AddressOfExpr{Expr: expr})
	}

	return p.parsePostfix()
}

// parsePointerOperand parses the operand of a `&` or `*` so that
// chained pointer ops still nest (`**p`, `&&x`) but a postfix `as` /
// `is` does NOT get consumed at the inner level. The outer caller wraps
// the resulting unary node and then offers the cast a chance.
func (p *Parser) parsePointerOperand() (ast.Node, error) {
	if p.check(lexer.STAR) {
		p.advance()

		expr, err := p.parsePointerOperand()
		if err != nil {
			return nil, err
		}

		return &ast.DerefExpr{Expr: expr}, nil
	}

	if p.check(lexer.AMP) {
		p.advance()

		expr, err := p.parsePointerOperand()
		if err != nil {
			return nil, err
		}

		return &ast.AddressOfExpr{Expr: expr}, nil
	}

	return p.parsePostfixNoCast()
}

// parsePostfixNoCast is the same suffix walk parsePostfix runs (member
// access, calls, indexing, struct lit, type assert, method-chain INDENT
// continuation) but stops when it sees `as` or `is`. Used by `&` / `*`
// so the cast attaches to the enclosing unary expression instead.
//
// The suppression counter is at the parser level rather than threaded
// through arguments, so it transparently survives parsePrimary's recursive
// dispatch (BinExpr, etc.). Sub-expressions inside parens reset the counter
// (see parsePrimary's `(` handler) so legitimate inner casts like
// `*(r as *T)` still parse: the outer `*` doesn't consume `as`, but the
// paren-wrapped inner expression sees a fresh counter.
func (p *Parser) parsePostfixNoCast() (ast.Node, error) {
	p.suppressPostfixCast++

	defer func() { p.suppressPostfixCast-- }()

	return p.parsePostfix()
}

// applyPostfixCasts wraps `expr` in any chain of `as T` / `is T` postfix
// casts that immediately follow. Used by parseUnary's `&` / `*` arms so
// `&x as T` becomes `(&x) as T`.
func (p *Parser) applyPostfixCasts(expr ast.Node) (ast.Node, error) {
	for {
		var (
			next ast.Node
			err  error
		)

		switch p.peek().Type {
		case lexer.KW_AS:
			next, err = p.parseAsSuffix(expr)
		case lexer.KW_IS:
			next, err = p.parseIsSuffix(expr)
		default:
			return expr, nil
		}

		if err != nil {
			return nil, err
		}

		expr = next
	}
}

// parseAsSuffix parses `as <type>` and wraps `expr` in an AsExpr.
// Caller must have just peeked KW_AS.
func (p *Parser) parseAsSuffix(expr ast.Node) (ast.Node, error) {
	p.advance() // consume `as`

	typ, err := p.parseTypeExpr()
	if err != nil {
		return nil, err
	}

	asExpr := &ast.AsExpr{Expr: expr, Type: typ}
	asExpr.SetPos(expr.Pos())

	return asExpr, nil
}

// parseIsSuffix parses `is <type>` / `is <name> <type>` /
// `is <Variant>(args...)` and wraps `expr` in an IsExpr. Caller must
// have just peeked KW_IS.
func (p *Parser) parseIsSuffix(expr ast.Node) (ast.Node, error) {
	p.advance() // consume `is`

	isExpr := &ast.IsExpr{Expr: expr}

	if p.check(lexer.IDENT) && p.peekAt(1).Type == lexer.LPAREN {
		ctorPos := p.curPos()
		ctorName := p.advance().Literal

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

		fn := &ast.Identifier{Name: ctorName}
		fn.SetPos(ctorPos)
		call := &ast.CallExpr{Func: fn, Args: args}
		call.SetPos(ctorPos)

		isExpr.Pattern = call

		return isExpr, nil
	}

	if p.check(lexer.IDENT) && isTypeToken(p.peekAt(1)) {
		isExpr.VarName = p.advance().Literal
	}

	if !p.match(lexer.NEWLINE, lexer.COLON, lexer.EOF) {
		var err error

		isExpr.Type, err = p.parseTypeExpr()
		if err != nil {
			return nil, err
		}
	}

	return isExpr, nil
}

func (p *Parser) parsePostfix() (ast.Node, error) {
	expr, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}

	indentConsumed := 0

	for {
		// Allow NEWLINE + optional INDENT before DOT or ARROW (method chain continuation).
		if p.check(lexer.NEWLINE) {
			saved := p.pos
			p.advance() // consume NEWLINE

			consumedIndent := false

			if p.check(lexer.INDENT) {
				p.advance()

				indentConsumed++
				consumedIndent = true
			}

			if !p.check(lexer.DOT) && !p.check(lexer.ARROW) {
				p.pos = saved

				if consumedIndent {
					indentConsumed--
				}

				break
			}
			// Fall through: the loop body will consume DOT/ARROW below.
		}

		switch p.peek().Type {
		case lexer.DOT:
			p.advance()
			// Trailing dot continuation: `foo.` <newline> `  bar()`. Skip
			// NEWLINE + optional INDENT after the dot so the identifier on
			// the next line completes the field access. Mirror of the leading-
			// dot continuation handled above the switch.
			if p.check(lexer.NEWLINE) {
				p.advance()

				if p.check(lexer.INDENT) {
					p.advance()

					indentConsumed++
				}
			}
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

				fa := ast.NewFieldAccess(expr, field.Literal, false, field.Line, field.Col)
				expr = ast.NewCallExpr(fa, args, field.Line, field.Col)
			} else {
				expr = ast.NewFieldAccess(expr, field.Literal, false, field.Line, field.Col)
			}

		case lexer.ARROW:
			p.advance()

			if p.check(lexer.NEWLINE) {
				p.advance()

				if p.check(lexer.INDENT) {
					p.advance()

					indentConsumed++
				}
			}

			field, err2 := p.expect(lexer.IDENT)
			if err2 != nil {
				return nil, err2
			}

			if p.check(lexer.LPAREN) {
				args, err3 := p.parseArgList()
				if err3 != nil {
					return nil, err3
				}

				fa := ast.NewFieldAccess(expr, field.Literal, true, field.Line, field.Col)
				expr = ast.NewCallExpr(fa, args, field.Line, field.Col)
			} else {
				expr = ast.NewFieldAccess(expr, field.Literal, true, field.Line, field.Col)
			}

		case lexer.DCOLON:
			p.advance()

			if p.check(lexer.NEWLINE) {
				p.advance()

				if p.check(lexer.INDENT) {
					p.advance()

					indentConsumed++
				}
			}

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

					expr = ast.NewCallExpr(sa, args, field.Line, field.Col)
				} else if p.check(lexer.LBRACE) {
					// pkg::subpkg::Type{...} - package-qualified struct literal
					lit, err3 := p.parseStructLit(strings.Join(sa.Path, "::"))
					if err3 != nil {
						return nil, err3
					}

					expr = lit
				}
			} else if id, ok := expr.(*ast.Identifier); ok {
				sa := ast.NewScopeAccess([]string{id.Name, field.Literal}, field.Line, field.Col)

				if p.check(lexer.LPAREN) {
					args, err3 := p.parseArgList()
					if err3 != nil {
						return nil, err3
					}

					expr = ast.NewCallExpr(sa, args, field.Line, field.Col)
				} else if p.check(lexer.LBRACE) {
					// pkg::Type{...} - package-qualified struct literal
					lit, err3 := p.parseStructLit(strings.Join(sa.Path, "::"))
					if err3 != nil {
						return nil, err3
					}

					expr = lit
				} else {
					expr = sa
				}
			} else if idx, ok2 := expr.(*ast.IndexExpr); ok2 {
				// e.g. result[u32]::ok(42), pkg::Type[T,U]::method(),
				// or G[G[i64]].make(...) where the type arg is itself a
				// generic instantiation (nested IndexExpr).
				var typeName string

				switch inner := idx.Expr.(type) {
				case *ast.Identifier:
					typeName = inner.Name
				case *ast.ScopeAccess:
					typeName = strings.Join(inner.Path, "::")
				}

				if typeName != "" {
					argStr := typeNodeToString(idx.Index)
					if argStr != "" {
						typeName = typeName + "[" + argStr + "]"
					}

					sa := ast.NewScopeAccess([]string{typeName, field.Literal}, field.Line, field.Col)

					if p.check(lexer.LPAREN) {
						args, err3 := p.parseArgList()
						if err3 != nil {
							return nil, err3
						}

						expr = ast.NewCallExpr(sa, args, field.Line, field.Col)
					} else {
						expr = sa
					}
				}
			}

		case lexer.LBRACKET:
			p.advance() // consume [
			// Detect slice syntax: arr[:], arr[n:], arr[:m], arr[n:m]
			var start ast.Node

			if !p.check(lexer.COLON) && !p.check(lexer.RBRACKET) {
				var err3 error

				start, err3 = p.parseExpr()
				if err3 != nil {
					return nil, err3
				}
			}

			// Multi-type-arg: Type[T1, T2, ...] for generic instantiation.
			// A comma inside [] is not valid for array indexing (Tin has no 2D index),
			// so this is unambiguously a type argument list.
			if p.check(lexer.COMMA) {
				typeArgs := []string{}
				if startID, ok := start.(*ast.Identifier); ok {
					typeArgs = append(typeArgs, startID.Name)
				}

				for p.check(lexer.COMMA) {
					p.advance() // consume ','

					arg, err3 := p.parseExpr()
					if err3 != nil {
						return nil, err3
					}

					if argID, ok := arg.(*ast.Identifier); ok {
						typeArgs = append(typeArgs, argID.Name)
					}
				}

				if _, err2 := p.expect(lexer.RBRACKET); err2 != nil {
					return nil, err2
				}
				// Encode multiple type args as a comma-separated identifier so that
				// the DCOLON and DOT postfix handlers can reconstruct the concrete name.
				expr = &ast.IndexExpr{Expr: expr, Index: &ast.Identifier{Name: strings.Join(typeArgs, ",")}}

				// pkg::Type[K,V]{...} - generic struct literal with package qualifier
				if p.check(lexer.LBRACE) {
					if idx, ok3 := expr.(*ast.IndexExpr); ok3 {
						if baseName, ok4 := p.indexExprTypeName(idx); ok4 {
							parsedTypeArgs := p.indexExprTypeArgs(idx)

							lit, err3 := p.parseStructLit(baseName)
							if err3 != nil {
								return nil, err3
							}

							if sl, ok5 := lit.(*ast.StructLit); ok5 {
								sl.TypeArgs = parsedTypeArgs
							}

							expr = lit
						}
					}
				}
			} else if p.check(lexer.COLON) {
				p.advance() // consume :

				var end ast.Node

				if !p.check(lexer.RBRACKET) {
					var err3 error

					end, err3 = p.parseExpr()
					if err3 != nil {
						return nil, err3
					}
				}

				if _, err2 := p.expect(lexer.RBRACKET); err2 != nil {
					return nil, err2
				}

				expr = &ast.SliceExpr{Expr: expr, Start: start, End: end}
			} else {
				if _, err2 := p.expect(lexer.RBRACKET); err2 != nil {
					return nil, err2
				}

				idxExpr := &ast.IndexExpr{Expr: expr, Index: start}
				idxExpr.SetPos(expr.Pos())
				expr = idxExpr

				// pkg::Type[T]{...} - generic struct literal with package qualifier
				if p.check(lexer.LBRACE) {
					if idx, ok3 := expr.(*ast.IndexExpr); ok3 {
						if baseName, ok4 := p.indexExprTypeName(idx); ok4 {
							parsedTypeArgs := p.indexExprTypeArgs(idx)

							lit, err3 := p.parseStructLit(baseName)
							if err3 != nil {
								return nil, err3
							}

							if sl, ok5 := lit.(*ast.StructLit); ok5 {
								sl.TypeArgs = parsedTypeArgs
							}

							expr = lit
						}
					}
				}
			}

		case lexer.NOT:
			// Macro call syntax: ident!(args) - only when followed by '('
			if p.peekAt(1).Type != lexer.LPAREN {
				return expr, nil
			}

			p.advance() // consume !

			if id, ok := expr.(*ast.Identifier); ok {
				id.Name += "!"
			} else if sa, ok := expr.(*ast.ScopeAccess); ok && len(sa.Path) > 0 {
				sa.Path[len(sa.Path)-1] += "!"
			}

			args, err2 := p.parseArgList()
			if err2 != nil {
				return nil, err2
			}

			expr = &ast.CallExpr{Func: expr, Args: args}

		case lexer.LPAREN:
			// Function call - record position of the opening paren for error messages.
			callTok := p.peek()

			args, err2 := p.parseArgList()
			if err2 != nil {
				return nil, err2
			}

			expr = ast.NewCallExpr(expr, args, callTok.Line, callTok.Col)

		case lexer.KW_AS:
			if p.suppressPostfixCast > 0 {
				return expr, nil
			}

			next, err := p.parseAsSuffix(expr)
			if err != nil {
				return nil, err
			}

			expr = next

		case lexer.KW_IS:
			if p.suppressPostfixCast > 0 {
				return expr, nil
			}

			next, err := p.parseIsSuffix(expr)
			if err != nil {
				return nil, err
			}

			expr = next

		default:
			// Consume any pending DEDENTs from method-chain INDENT continuation.
			if indentConsumed > 0 && p.check(lexer.NEWLINE) {
				p.advance()
			}

			for indentConsumed > 0 && p.check(lexer.DEDENT) {
				p.advance()

				indentConsumed--
			}

			return expr, nil
		}
	}
	// Consume matching DEDENT(s) for any INDENT consumed during method-chain continuation.
	if indentConsumed > 0 && p.check(lexer.NEWLINE) {
		p.advance()
	}

	for indentConsumed > 0 && p.check(lexer.DEDENT) {
		p.advance()

		indentConsumed--
	}

	return expr, nil
}

func (p *Parser) parseArgList() ([]ast.Node, error) {
	if _, err := p.expect(lexer.LPAREN); err != nil {
		return nil, err
	}

	p.skipWhitespace()

	var args []ast.Node

	for !p.check(lexer.RPAREN) && !p.check(lexer.EOF) {
		arg, err := p.parseExpr()
		if err != nil {
			return nil, err
		}

		args = append(args, arg)

		p.skipWhitespace()

		if p.check(lexer.COMMA) {
			p.advance()
			p.skipWhitespace()
		}
	}

	if _, err := p.expect(lexer.RPAREN); err != nil {
		return nil, err
	}

	return args, nil
}

// parseIntLitToken converts the textual form of an integer literal into an
// ast.IntLit. Two encodings coexist:
//
//   - Within u64 range: stored in IntLit.Value as the i64 bit pattern. This
//     preserves existing behavior for hex constants like 0xffffffffffffffff
//     (-1 as i64, 18446744073709551615 as u64) where the variable's declared
//     type decides signedness.
//   - Above u64 range: IntLit.Big is set to the exact magnitude, and Value
//     keeps the bottom 64 bits as a fallback. Codegen reads Big to emit an
//     i128 constant (auto-upgrade); paths that ignore Big see the truncated
//     bottom 64 bits, matching the behavior of explicit truncation.
func parseIntLitToken(lit string) *ast.IntLit {
	if v, err := strconv.ParseInt(lit, 0, 64); err == nil {
		return &ast.IntLit{Value: v}
	}

	if uv, err := strconv.ParseUint(lit, 0, 64); err == nil {
		return &ast.IntLit{Value: int64(uv)}
	}

	// Exceeds u64. Parse as big.Int (handles 0x prefix) and stash both the
	// big magnitude and a bit-truncated i64 view.
	base := 10

	s := lit
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		base = 16
		s = s[2:]
	} else if strings.HasPrefix(s, "0b") || strings.HasPrefix(s, "0B") {
		base = 2
		s = s[2:]
	} else if strings.HasPrefix(s, "0o") || strings.HasPrefix(s, "0O") {
		base = 8
		s = s[2:]
	}

	bigVal, ok := new(big.Int).SetString(s, base)
	if !ok {
		// Lexer should have rejected malformed digits already; fall back to
		// zero rather than panicking in the And() below.
		return &ast.IntLit{Value: 0}
	}

	low := new(big.Int).And(bigVal, mask64).Int64()

	return &ast.IntLit{Value: low, Big: bigVal}
}

var mask64 = new(big.Int).SetUint64(^uint64(0))

func (p *Parser) parsePrimary() (ast.Node, error) {
	tok := p.peek()
	pos := p.curPos()
	_ = pos

	switch tok.Type {
	case lexer.INT_LIT:
		p.advance()

		return parseIntLitToken(tok.Literal), nil

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

	case lexer.AT:
		// @'X' -> byte value of character X  (i8 CharLit)
		// @N   -> integer N                  (i64 IntLit)
		p.advance() // consume @

		next := p.peek()
		switch next.Type {
		case lexer.CHAR_LIT:
			p.advance()

			var b byte
			if len(next.Literal) > 0 {
				b = next.Literal[0]
			}

			return &ast.CharLit{Value: b}, nil
		case lexer.INT_LIT:
			p.advance()

			return parseIntLitToken(next.Literal), nil
		default:
			return nil, fmt.Errorf("line %d: '@' must be followed by a char or integer literal, got %q",
				next.Line, next.Literal)
		}

	case lexer.BOOL_LIT:
		p.advance()

		b := &ast.BoolLit{Value: tok.Literal == "true"}
		b.SetPos(ast.Pos{Line: tok.Line, Col: tok.Col})

		return b, nil

	case lexer.ATOM_LIT:
		p.advance()

		return &ast.AtomLit{Name: tok.Literal}, nil

	case lexer.KW_MATCH:
		// match as expression: let x = match ...: case ...: value
		return p.parseMatchStmt()

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

	case lexer.KW_ISRC:
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

		return &ast.IsRCExpr{Type: typ}, nil

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
		// default(typeof(expr)) - derive zero value from expression's compile-time type
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
		// Reset the postfix-cast suppression for the inner expression: a
		// `*(r as *T)` should let the inner `r as *T` parse normally; the
		// suppression only affects the outer pointer operator.
		savedSuppress := p.suppressPostfixCast
		p.suppressPostfixCast = 0

		defer func() { p.suppressPostfixCast = savedSuppress }()
		// Block expression: (let x = ...; expr) - produced by CTFE macro splices.
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
		// Tuple literal: (e1, e2, ...) with 2+ elements
		if p.check(lexer.COMMA) {
			elems := []ast.Node{inner}

			for p.check(lexer.COMMA) {
				p.advance()

				e, err := p.parseExpr()
				if err != nil {
					return nil, err
				}

				elems = append(elems, e)
			}

			if _, err := p.expect(lexer.RPAREN); err != nil {
				return nil, err
			}

			return &ast.TupleLit{Elems: elems}, nil
		}

		if _, err := p.expect(lexer.RPAREN); err != nil {
			return nil, err
		}

		return inner, nil

	case lexer.LBRACKET:
		return p.parseArrayLit()

	case lexer.IDENT:
		tok := p.advance()
		name := tok.Literal
		// struct literal: name{...}
		if p.check(lexer.LBRACE) {
			return p.parseStructLit(name)
		}
		// generic struct literal: Name[TypeArg]{...}
		if p.check(lexer.LBRACKET) {
			// Speculatively parse [TypeArg] and check for {
			saved := p.pos

			typeArgs, err2 := p.parseTypeArgList()
			if err2 == nil && p.check(lexer.LBRACE) {
				lit, err3 := p.parseStructLit(name)
				if err3 != nil {
					return nil, err3
				}

				if sl, ok := lit.(*ast.StructLit); ok {
					sl.TypeArgs = typeArgs
				}

				return lit, nil
			}
			// Not a generic struct literal; restore position
			p.pos = saved
		}

		return ast.NewIdent(name, tok.Line, tok.Col), nil

	case lexer.KW_LET:
		// inline let (for ternary macro usage)
		return p.parseVarDecl()

	case lexer.KW_SPAWN:
		return p.parseSpawnExpr()

	case lexer.KW_AWAIT:
		p.advance()

		fut, err := p.parseExpr()
		if err != nil {
			return nil, err
		}

		return &ast.AwaitExpr{Future: fut}, nil

	case lexer.KW_NIL:
		p.advance()

		return &ast.NilLit{}, nil

	case lexer.KW_YIELD:
		p.advance()

		return &ast.YieldStmt{}, nil

	default:
		// Type keywords used as identifiers / type names in expressions
		if isTypeKeyword(tok) {
			tok = p.advance()

			return ast.NewIdent(tok.Literal, tok.Line, tok.Col), nil
		}

		return nil, p.errorf("unexpected token %s (%q)", tok.Type, tok.Literal)
	}
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

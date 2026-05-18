package parser

import (
	"strings"

	"github.com/Azer0s/tin/ast"
	"github.com/Azer0s/tin/lexer"
)

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
		pos := p.curPos()
		p.advance()

		expr, err := p.parsePointerOperand()
		if err != nil {
			return nil, err
		}

		d := &ast.DerefExpr{Expr: expr}
		d.SetPos(pos)

		return p.applyPostfixCasts(d)
	}
	// Address-of: &expr
	if p.check(lexer.AMP) {
		pos := p.curPos()
		p.advance()

		expr, err := p.parsePointerOperand()
		if err != nil {
			return nil, err
		}

		a := &ast.AddressOfExpr{Expr: expr}
		a.SetPos(pos)

		return p.applyPostfixCasts(a)
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
	return p.parsePostfixOpt(false)
}

// parsePostfixOpt is the shared postfix walker. When stopAfterCall is true,
// the loop exits after consuming a CallExpr-producing segment so the caller
// can re-enter the chain on the outer expression. Used by `await EXPR.foo()`
// to make the chain `(await EXPR.foo())` rather than `await (EXPR.foo())`.
func (p *Parser) parsePostfixOpt(stopAfterCall bool) (ast.Node, error) {
	expr, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}

	indentConsumed := 0

postfixLoop:
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

				if stopAfterCall {
					break postfixLoop
				}
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

				if stopAfterCall {
					break postfixLoop
				}
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

					if stopAfterCall {
						break postfixLoop
					}
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

					if stopAfterCall {
						break postfixLoop
					}
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

						if stopAfterCall {
							break postfixLoop
						}
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
				// `Generic[fn(...) ret]` -- the `fn(...) T` form is a
				// FuncType, not an expression, so parseExpr would mis-
				// dispatch to LambdaExpr parsing (and fail on the lack
				// of a body).  Parse a TypeExpr instead and wrap it in a
				// TypeRefNode the downstream resolver unpacks.
				if p.check(lexer.KW_FN) {
					te, err3 := p.parseFuncType()
					if err3 != nil {
						return nil, err3
					}

					trn := &ast.TypeRefNode{Type: te}
					trn.SetPos(expr.Pos())
					start = trn
				} else {
					var err3 error

					start, err3 = p.parseExpr()
					if err3 != nil {
						return nil, err3
					}
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

			if stopAfterCall {
				break postfixLoop
			}

		case lexer.LPAREN:
			// Function call - record position of the opening paren for error messages.
			callTok := p.peek()

			args, err2 := p.parseArgList()
			if err2 != nil {
				return nil, err2
			}

			expr = ast.NewCallExpr(expr, args, callTok.Line, callTok.Col)

			if stopAfterCall {
				break postfixLoop
			}

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

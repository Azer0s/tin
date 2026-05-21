package parser

import (
	"fmt"
	"strconv"

	"github.com/Azer0s/tin/ast"
	"github.com/Azer0s/tin/lexer"
)

func (p *Parser) parsePrimary() (ast.Node, error) {
	tok := p.peek()
	pos := p.curPos()

	switch tok.Type {
	case lexer.INT_LIT:
		p.advance()

		il := parseIntLitToken(tok.Literal)
		il.SetPos(pos)

		return il, nil

	case lexer.FLOAT_LIT:
		p.advance()

		v, _ := strconv.ParseFloat(tok.Literal, 64)

		fl := &ast.FloatLit{Value: v}
		fl.SetPos(pos)

		return fl, nil

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
			return nil, p.errAtTok(next, "'@' must be followed by a char or integer literal, got %q", next.Literal)
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

		// Block expression: (stmt1; stmt2; ...; last_expr)
		// Triggers on statement-only keywords that can't start an expression,
		// or on any expression followed by ';'.
		parseBlockStmts := func(first ast.Node) (ast.Node, error) {
			var stmts []ast.Node
			if first != nil {
				stmts = append(stmts, first)
			}

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

		if p.check(lexer.KW_LET) || p.check(lexer.KW_RETURN) ||
			p.check(lexer.KW_DEFER) || p.check(lexer.KW_FOR) {
			return parseBlockStmts(nil)
		}

		inner, err := p.parseExpr()
		if err != nil {
			return nil, err
		}

		// Block expression triggered by expression followed by ';'
		if p.check(lexer.SEMI) {
			p.advance()

			return parseBlockStmts(&ast.ExprStmt{Expr: inner})
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

	case lexer.IDENT, lexer.KW_FORWARD, lexer.KW_OVERRIDE:
		// `try` is a contextual keyword: at expression-prefix position
		// followed by anything other than `!` (which would indicate the
		// existing `try!` macro), it parses as a try-expression that the
		// codegen desugars against the tryable trait. Look at the token
		// stream without advancing first so the macro path is preserved.
		if p.peek().Type == lexer.IDENT && p.peek().Literal == "try" && p.peekAt(1).Type != lexer.NOT {
			tryTok := p.advance()

			inner, err := p.parseExpr()
			if err != nil {
				return nil, err
			}

			te := &ast.TryExpr{Inner: inner}
			te.SetPos(ast.Pos{Line: tryTok.Line, Col: tryTok.Col})

			return te, nil
		}

		// KW_FORWARD / KW_OVERRIDE are *contextual* keywords -- they
		// only have meaning inside struct field declarations. Accept
		// them as plain identifiers in expression position so calls
		// like `forward(p)` and types named `override` work.
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
		awaitTok := p.advance()

		// Parse the future operand as primary + chain up to (and including)
		// the first CallExpr.  This makes `await EXPR.method()` parse as
		// `(await EXPR.method()).method()` - the chain after the awaited
		// call applies to the Result/value, not the Future.  Use parens to
		// override (`await (x.y().z())` still awaits the full chain).
		fut, err := p.parsePostfixOpt(true)
		if err != nil {
			return nil, err
		}

		aw := &ast.AwaitExpr{Future: fut}
		aw.SetPos(ast.Pos{Line: awaitTok.Line, Col: awaitTok.Col})

		return aw, nil

	case lexer.KW_NIL:
		p.advance()

		return &ast.NilLit{}, nil

	case lexer.KW_MOVE:
		moveTok := p.advance()
		// Source form: `move <ident>`.  Partial moves (`move x.field`)
		// and moves of complex expressions are rejected at parse time
		// -- the source must be a bare local binding.  Codegen will
		// further reject moves of function parameters, iterator
		// bindings, and other non-owning bindings with a clear error
		// and a fix-it.
		nextTok := p.peek()
		if nextTok.Type != lexer.IDENT {
			return nil, p.errAtTok(nextTok,
				"`move` expects an identifier (the local binding being moved); got %s",
				nextTok.Type.String())
		}

		idTok := p.advance()
		// Reject partial moves and call/index chains on the moved
		// binding -- `move x.field`, `move x()`, `move x[i]` etc.
		// The user almost certainly wants to extract first or take
		// a pointer; chaining off `move` would silently transfer
		// the WHOLE binding and then operate on the value, which is
		// surprising. Force the explicit `let v = x.field; move v`
		// shape.
		switch p.peek().Type {
		case lexer.DOT, lexer.LBRACKET, lexer.LPAREN:
			return nil, p.errAtTok(p.peek(),
				"`move` expects a bare identifier; partial moves (`move %s%s...`) are not supported. Extract first: `let v = %s%s...; move v`",
				idTok.Literal, p.peek().Type.String(),
				idTok.Literal, p.peek().Type.String())
		}

		mv := &ast.MoveExpr{Name: idTok.Literal}
		mv.SetPos(ast.Pos{Line: moveTok.Line, Col: moveTok.Col})

		return mv, nil

	case lexer.KW_YIELD:
		p.advance()

		return &ast.YieldStmt{}, nil

	default:
		// Type keywords used as identifiers / type names in expressions
		if isTypeKeyword(tok) {
			tok = p.advance()

			return ast.NewIdent(tok.Literal, tok.Line, tok.Col), nil
		}

		return nil, p.errorf("unexpected token %s here; %s", describeToken(tok), suggestForToken(tok))
	}
}

// describeToken renders a token for diagnostics: keyword tokens use
// their source spelling ("var", "switch"), punctuation uses the
// literal symbol, and structural tokens get a friendly name.
func describeToken(tok lexer.Token) string {
	switch tok.Type {
	case lexer.NEWLINE:
		return "end of line"
	case lexer.INDENT:
		return "an indented block"
	case lexer.DEDENT:
		return "a dedent"
	case lexer.EOF:
		return "end of file"
	case lexer.IDENT:
		return fmt.Sprintf("identifier %q", tok.Literal)
	}

	if tok.Literal != "" {
		return fmt.Sprintf("%q", tok.Literal)
	}

	return tok.Type.String()
}

// suggestForToken produces a one-line tip pointing the user at the
// likely typo or alternative when a token shows up in an unexpected
// position. Returns "expected an expression" as a generic fallback.
func suggestForToken(tok lexer.Token) string {
	switch tok.Literal {
	case "switch":
		return "did you mean `match`?"
	case "elif", "elseif":
		return "use `else if` instead"
	case "while":
		return "use `for cond:` for a conditional loop"
	case "func", "function", "def":
		return "use `fn`"
	}

	switch tok.Type {
	case lexer.COMMA:
		return "trailing comma not allowed here"
	case lexer.INDENT:
		return "unexpected indentation; the previous line probably needs a `:` or this block is mis-aligned"
	case lexer.DEDENT:
		return "the block ended sooner than expected"
	case lexer.NEWLINE:
		return "expected the rest of an expression on this line"
	}

	return "expected an expression"
}

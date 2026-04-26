package parser

import (
	"fmt"

	"github.com/Azer0s/tin/ast"
	"github.com/Azer0s/tin/lexer"
)

// Function declaration

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

	// Optional name (lambdas are anonymous)
	// Forms:
	//   fn name(...)            - regular method
	//   fn ::name(...)          - alias-trait implementation
	//   fn trait::method(...)   - qualified trait-method implementation
	//   fn trait[T]::method(...)- generic qualified implementation
	var (
		name           string
		traitQualifier string
	)

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
			// plain name - restore and re-read cleanly
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
	// Appear BEFORE the `=` body separator.
	constraints := p.parseTypeConstraints()

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
			// Allow trailing tags after extern("sym"): e.g. extern("read") {#blocking}
			tags = append(tags, p.parseTags()...)

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

// parseFuncBody parses the body after the `=` sign
// It has already been consumed by the caller
// Handles: single-line expr, indented block, indented where list
func (p *Parser) parseFuncBody() (ast.Node, error) {
	if p.check(lexer.NEWLINE) {
		p.advance() // consume NEWLINE
		p.skipNewlines()
		// A DEDENT here is an artifact of a multi-line parameter list: parseParams
		// uses skipWhitespace to consume the INDENT between param lines, and the
		// matching DEDENT lands just before the function body's INDENT.  Consume it.
		for p.check(lexer.DEDENT) {
			p.advance()
		}

		if p.check(lexer.INDENT) {
			// Peek inside to determine block vs where-list
			if p.peekAt(1).Type == lexer.KW_WHERE {
				return p.parseWhereBlock()
			}

			return p.parseBlock()
		}
		// No INDENT after NEWLINE+DEDENTs: this can happen when a function has a
		// multi-line parameter list and the body is at a "inconsistent" indent level
		// that the lexer doesn't emit a new INDENT for.  Parse the body as a single
		// statement (covers the common case of `return expr` on one line).
		if !p.check(lexer.EOF) && !p.check(lexer.DEDENT) && !p.check(lexer.NEWLINE) {
			return p.parseStatement()
		}

		return &ast.Block{}, nil
	}
	// Single expression / statement on same line - may be SEMI-separated multi-statement
	first, err := p.parseStatement()
	if err != nil {
		return nil, err
	}

	if !p.check(lexer.SEMI) {
		return first, nil
	}
	// Multiple statements separated by semicolons on one line: wrap in a Block
	var stmts []ast.Node
	if first != nil {
		stmts = append(stmts, first)
	}

	for p.check(lexer.SEMI) {
		p.advance() // consume SEMI

		if p.check(lexer.NEWLINE) || p.check(lexer.EOF) || p.check(lexer.DEDENT) {
			break
		}

		stmt, err2 := p.parseStatement()
		if err2 != nil {
			return nil, err2
		}

		if stmt != nil {
			stmts = append(stmts, stmt)
		}
	}

	return &ast.Block{Stmts: stmts}, nil
}

// parseTypeConstraints parses zero or more trait-bound clauses that appear
// before the `=` body separator in function, struct, and type-alias
// declarations. Each clause is `<typeparam> is <bound-expr>` where
// <bound-expr> is a boolean expression with &&, ||, not, and parens over
// atoms that are type/trait expressions. Legacy `+` is still accepted as
// shorthand for `&&` and lowers to the same TBAnd tree.
//
// Multiple bounds on different type parameters can be separated with
// commas or additional `where` keywords:
//
//	where T is ord, U is comp
//	where T is ord where U is comp
//	where T is ord && not bool, U is i64 || f64
func (p *Parser) parseTypeConstraints() []ast.TypeConstraint {
	var constraints []ast.TypeConstraint

	parseOne := func() bool {
		if !p.check(lexer.IDENT) || p.peekAt(1).Type != lexer.KW_IS {
			return false
		}

		startPos := p.curPos()
		typeParam := p.advance().Literal // e.g. "t"
		p.advance()                      // consume "is"

		bound, err := p.parseTypeBound()
		if err != nil {
			return false
		}

		constraints = append(constraints, ast.TypeConstraint{
			Pos:       startPos,
			TypeParam: typeParam,
			Bound:     bound,
		})

		return true
	}

	for p.check(lexer.KW_WHERE) {
		saved := p.pos
		p.advance() // consume "where"

		if !parseOne() {
			p.pos = saved

			break
		}

		for p.check(lexer.COMMA) {
			p.advance()

			if !parseOne() {
				break
			}
		}
	}

	return constraints
}

// parseTypeBound parses the boolean expression that follows `is` in a
// type-constraint clause. Grammar:
//
//	bound := or
//	or    := and ('||' and)*
//	and   := unary ('&&' unary | '+' unary)*       // '+' is legacy AND
//	unary := 'not' atom | atom
//	atom  := '(' bound ')' | <type-expr>
//
// Atoms are parsed with parseTypeSingle, so generic trait applications
// (e.g. `iter[i64]`) work inside bounds without extra handling.
func (p *Parser) parseTypeBound() (ast.TypeBound, error) {
	return p.parseTypeBoundOr()
}

func (p *Parser) parseTypeBoundOr() (ast.TypeBound, error) {
	left, err := p.parseTypeBoundAnd()
	if err != nil {
		return nil, err
	}

	for p.check(lexer.OR) {
		pos := p.curPos()
		p.advance()

		right, err2 := p.parseTypeBoundAnd()
		if err2 != nil {
			return nil, err2
		}

		left = &ast.TBOr{NodePos: pos, Left: left, Right: right}
	}

	return left, nil
}

func (p *Parser) parseTypeBoundAnd() (ast.TypeBound, error) {
	left, err := p.parseTypeBoundUnary()
	if err != nil {
		return nil, err
	}

	for p.check(lexer.AND) || p.check(lexer.PLUS) {
		pos := p.curPos()
		p.advance()

		right, err2 := p.parseTypeBoundUnary()
		if err2 != nil {
			return nil, err2
		}

		left = &ast.TBAnd{NodePos: pos, Left: left, Right: right}
	}

	return left, nil
}

func (p *Parser) parseTypeBoundUnary() (ast.TypeBound, error) {
	if p.check(lexer.IDENT) && p.peek().Literal == "not" {
		pos := p.curPos()
		p.advance()

		inner, err := p.parseTypeBoundAtom()
		if err != nil {
			return nil, err
		}

		if atom, ok := inner.(*ast.TBAtom); ok {
			atom.Neg = !atom.Neg

			return atom, nil
		}
		// `not (expr)` -- wrap: represented as applying De Morgan lazily at
		// eval time isn't worth the complexity here. Use a synthesized atom
		// referencing a negation of the parenthesised sub-tree via a
		// special helper. For now we only allow `not` on atoms (leaves).
		return nil, fmt.Errorf("%d:%d: `not` in a type bound must apply to a trait name, not a parenthesised expression",
			pos.Line, pos.Col)
	}

	return p.parseTypeBoundAtom()
}

func (p *Parser) parseTypeBoundAtom() (ast.TypeBound, error) {
	if p.check(lexer.LPAREN) {
		p.advance() // consume (

		inner, err := p.parseTypeBound()
		if err != nil {
			return nil, err
		}

		if _, err := p.expect(lexer.RPAREN); err != nil {
			return nil, err
		}

		return inner, nil
	}

	if !isTypeToken(p.peek()) && !p.check(lexer.IDENT) {
		pos := p.curPos()

		return nil, fmt.Errorf("%d:%d: expected trait name in type bound",
			pos.Line, pos.Col)
	}

	pos := p.curPos()

	te, err := p.parseTypeSingle()
	if err != nil {
		return nil, err
	}

	return &ast.TBAtom{NodePos: pos, Trait: te}, nil
}

// parseWhereBlock consumes INDENT, parses where clauses, consumes DEDENT
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

	var (
		cond    ast.Node
		pattern ast.Node
		guard   ast.Node
	)
	// `where (pattern): ...` / `where (pat) if guard: ...` - pattern mode.
	// The leading `(` distinguishes pattern mode from bool-guard mode.
	// Users who want a parenthesised bool expression can just drop the parens.
	if p.check(lexer.LPAREN) {
		pat, err := p.parseWherePattern()
		if err != nil {
			return ast.WhereClause{}, err
		}

		pattern = pat

		if p.check(lexer.KW_IF) {
			p.advance()

			g, err2 := p.parseExpr()
			if err2 != nil {
				return ast.WhereClause{}, err2
			}

			guard = g
		}
	} else if p.check(lexer.IDENT) && p.peek().Literal == "_" {
		// Bare `_` wildcard: bool-mode catch-all; also accepted inside a
		// pattern where-list as the universal catch-all.
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

	var (
		body ast.Node
		err  error
	)

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

	return ast.WhereClause{Pos: pos, Cond: cond, Pattern: pattern, Guard: guard, Body: body}, nil
}

// parseWherePattern parses the inside of `where (...):`. The parens are
// required. Inside, one or more comma-separated pattern elements:
//
//	(0)               -> IntLit 0 (single-arg pattern)
//	(_)               -> Identifier "_" (single-arg wildcard pattern)
//	(n)               -> Identifier "n" (single-arg binder pattern)
//	(0, "hello")      -> TuplePattern for a two-arg fn
//	([x, ...xs])      -> ArrayPattern (single-arg on a list)
//	(Point{x: 0, y})  -> StructPattern (single-arg on a struct)
//
// Single-element tuples unwrap to the bare element. `where ():` is rejected
// (Tin has no zero-ary unit value).
func (p *Parser) parseWherePattern() (ast.Node, error) {
	openPos := p.curPos()
	if _, err := p.expect(lexer.LPAREN); err != nil {
		return nil, err
	}

	if p.check(lexer.RPAREN) {
		return nil, fmt.Errorf("%d:%d: empty where pattern `()` is not allowed; use `where _:` for a catch-all",
			openPos.Line, openPos.Col)
	}

	var elems []ast.Node

	for {
		e, err := p.parsePatternElem()
		if err != nil {
			return nil, err
		}

		elems = append(elems, e)

		if !p.check(lexer.COMMA) {
			break
		}

		p.advance()
	}

	if _, err := p.expect(lexer.RPAREN); err != nil {
		return nil, err
	}

	if len(elems) == 1 {
		return elems[0], nil
	}

	return &ast.TuplePattern{Elems: elems}, nil
}

// parsePatternElem parses one pattern slot inside a where (...) clause.
// Accepted shapes:
//
//	_                     wildcard (Identifier "_")
//	<ident>               binder  (Identifier)
//	<literal>             exact-match literal (IntLit/FloatLit/StringLit/BoolLit/AtomLit)
//	[elem, ...]           array pattern (reuses parseArrayPattern)
//	TypeName{f: v, ...}   struct pattern (reuses parseStructPattern)
//
// Arithmetic, comparison, and other non-pattern expressions are rejected;
// users who want boolean guards should use `where (pat) if <bool>:` or a
// plain `where <bool>:` bool-guard clause.
func (p *Parser) parsePatternElem() (ast.Node, error) {
	startPos := p.curPos()

	switch {
	case p.check(lexer.LBRACKET):
		return p.parseArrayPattern()
	case p.check(lexer.IDENT) && p.peekAt(1).Type == lexer.LBRACE:
		return p.parseStructPattern()
	case p.check(lexer.IDENT):
		tok := p.advance()
		id := &ast.Identifier{Name: tok.Literal}
		id.SetPos(startPos)

		return id, nil
	}

	node, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	// Ensure literal nodes carry the source position for diagnostics.
	if sp, ok := node.(interface{ SetPos(ast.Pos) }); ok {
		sp.SetPos(startPos)
	}

	return node, nil
}

// Block parsing

// parseBlock consumes INDENT, zero or more statements, then DEDENT
func (p *Parser) parseBlock() (*ast.Block, error) {
	pos := p.curPos()
	if _, err := p.expect(lexer.INDENT); err != nil {
		return nil, err
	}

	b := &ast.Block{}
	_ = pos

	p.skipSemisAndNewlines()

	for !p.check(lexer.DEDENT) && !p.check(lexer.EOF) {
		// A comma at block level signals we're inside a struct literal field value
		// (e.g. `fn(x) = return x,` where `,` is the struct field separator).
		// Treat it as a block terminator without consuming it.
		if p.check(lexer.COMMA) {
			break
		}

		stmt, err := p.parseStatement()
		if err != nil {
			return nil, err
		}

		if stmt != nil {
			b.Stmts = append(b.Stmts, stmt)
		}

		p.skipSemisAndNewlines()
	}

	if p.check(lexer.DEDENT) {
		p.advance()
	}

	return b, nil
}

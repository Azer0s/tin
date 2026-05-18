package parser

import (
	"github.com/Azer0s/tin/ast"
	"github.com/Azer0s/tin/lexer"
)

func (p *Parser) parseExprStatement() (ast.Node, error) {
	expr, err := p.parseExpr()
	if err != nil {
		return nil, err
	}

	// Postfix ++/--
	if p.check(lexer.INC) {
		op := p.advance().Literal

		stmt := &ast.PostfixStmt{Expr: expr, Op: op}
		stmt.SetPos(expr.Pos())

		return stmt, nil
	}

	// Assignment =
	if p.check(lexer.ASSIGN) {
		p.advance()

		val, err2 := p.parseExpr()
		if err2 != nil {
			return nil, err2
		}

		stmt := &ast.AssignStmt{Target: expr, Value: val}
		stmt.SetPos(expr.Pos())

		return stmt, nil
	}

	// Augmented assignment +=, -=, *=, /=, %=, ++=
	if aug, ok := augOp(p.peek().Type); ok {
		p.advance()

		val, err2 := p.parseExpr()
		if err2 != nil {
			return nil, err2
		}

		stmt := &ast.AugAssignStmt{Target: expr, Op: aug, Value: val}
		stmt.SetPos(expr.Pos())

		return stmt, nil
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

// parseTopLevelLetConst parses module-scoped:  let|const name [Type] [= expr]
// Producing a TopLevelVar with IsConst=true makes the binding a global
// constant rather than a statement folded into the implicit main.
func (p *Parser) parseTopLevelLetConst() (*ast.TopLevelVar, error) {
	pos := p.curPos()
	p.advance() // consume let/const

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

	tv := &ast.TopLevelVar{Name: nameTok.Literal, Type: typ, Value: val, IsConst: true}
	tv.SetPos(pos)

	return tv, nil
}

// parseSpawnExprStmt parses a spawn statement (spawn expr or spawn do: block).
// Returns a *ast.SpawnExpr wrapped in an ExprStmt.

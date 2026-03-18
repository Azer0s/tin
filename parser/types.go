package parser

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Azer0s/tin/ast"
	"github.com/Azer0s/tin/lexer"
)

// Type expressions

func (p *Parser) parseTypeExpr() (ast.TypeExpr, error) {
	return p.parseTypeUnion()
}

func (p *Parser) parseTypeUnion() (ast.TypeExpr, error) {
	first, err := p.parseTypeSingle()
	if err != nil {
		return nil, err
	}
	if !p.check(lexer.BITOR) {
		return first, nil
	}
	union := &ast.UnionTypeExpr{Types: []ast.TypeExpr{first}}
	for p.check(lexer.BITOR) {
		p.advance()
		next, err2 := p.parseTypeSingle()
		if err2 != nil {
			return nil, err2
		}
		union.Types = append(union.Types, next)
	}
	return union, nil
}

func (p *Parser) parseTypeSingle() (ast.TypeExpr, error) {
	// const *T
	isConst := false
	if p.check(lexer.KW_CONST) {
		isConst = true
		p.advance()
	}
	// *T – pointer
	if p.check(lexer.STAR) {
		p.advance()
		elem, err := p.parseTypeSingle()
		if err != nil {
			return nil, err
		}
		return &ast.PointerType{Elem: elem, IsConst: isConst}, nil
	}
	// @[T1, T2, ...] — TupleArrayType (typed per-slot destructuring annotation)
	if p.check(lexer.AT) {
		p.advance() // consume @
		if _, err := p.expect(lexer.LBRACKET); err != nil {
			return nil, err
		}
		var elems []ast.TypeExpr
		for !p.check(lexer.RBRACKET) && !p.check(lexer.EOF) {
			t, err := p.parseTypeExpr()
			if err != nil {
				return nil, err
			}
			elems = append(elems, t)
			if p.check(lexer.COMMA) {
				p.advance()
			}
		}
		if _, err := p.expect(lexer.RBRACKET); err != nil {
			return nil, err
		}
		return &ast.TupleArrayType{ElemTypes: elems}, nil
	}
	// [T] or [T; N] – array
	if p.check(lexer.LBRACKET) {
		p.advance()
		// Empty brackets = void/unknown
		if p.check(lexer.RBRACKET) {
			p.advance()
			return &ast.ArrayType{Elem: &ast.SimpleType{Name: "void"}, Size: -1}, nil
		}
		elem, err := p.parseTypeExpr()
		if err != nil {
			return nil, err
		}
		size := -1
		if p.check(lexer.SEMI) {
			p.advance()
			if p.check(lexer.INT_LIT) {
				n, _ := strconv.Atoi(p.advance().Literal)
				size = n
			}
		}
		if _, err := p.expect(lexer.RBRACKET); err != nil {
			return nil, err
		}
		return &ast.ArrayType{Elem: elem, Size: size}, nil
	}
	// fn(T...) R – function type
	if p.check(lexer.KW_FN) {
		return p.parseFuncType()
	}
	// void
	if p.check(lexer.KW_EXTERN) {
		// skip "extern" used as void-like placeholder
		p.advance()
		return &ast.SimpleType{Name: "void"}, nil
	}

	// None used as a type (e.g. in `data maybe[t] = t | None`)
	if p.check(lexer.NONE_LIT) {
		p.advance()
		return &ast.SimpleType{Name: "None"}, nil
	}

	// Named type, possibly generic: name[T, R]
	if !p.match(lexer.IDENT) && !isTypeKeyword(p.peek()) {
		return nil, p.errorf("expected type, got %s (%q)", p.peek().Type, p.peek().Literal)
	}
	name := p.advance().Literal

	// Generic type args: name[T, R]
	if p.check(lexer.LBRACKET) {
		p.advance()
		var args []ast.TypeExpr
		for !p.check(lexer.RBRACKET) && !p.check(lexer.EOF) {
			t, err := p.parseTypeExpr()
			if err != nil {
				return nil, err
			}
			args = append(args, t)
			if p.check(lexer.COMMA) {
				p.advance()
			}
		}
		if _, err := p.expect(lexer.RBRACKET); err != nil {
			return nil, err
		}
		return &ast.GenericType{Name: name, TypeParams: args}, nil
	}
	return &ast.SimpleType{Name: name}, nil
}

func (p *Parser) parseFuncType() (ast.TypeExpr, error) {
	p.advance() // consume fn
	if _, err := p.expect(lexer.LPAREN); err != nil {
		return nil, err
	}
	ft := &ast.FuncType{}
	for !p.check(lexer.RPAREN) && !p.check(lexer.EOF) {
		// optional param name (ignored in type)
		if p.check(lexer.IDENT) && isTypeToken(p.peekAt(1)) {
			p.advance() // skip name
		}
		if p.check(lexer.DOTDOTDOT) {
			ft.IsVarArgs = true
			p.advance()
			if !p.check(lexer.RPAREN) {
				t, err2 := p.parseTypeExpr()
				if err2 != nil {
					return nil, err2
				}
				ft.Params = append(ft.Params, t)
			}
			break
		}
		t, err := p.parseTypeExpr()
		if err != nil {
			return nil, err
		}
		ft.Params = append(ft.Params, t)
		if p.check(lexer.COMMA) {
			p.advance()
		}
	}
	if _, err := p.expect(lexer.RPAREN); err != nil {
		return nil, err
	}
	// return type
	if !p.match(lexer.NEWLINE, lexer.EOF, lexer.RPAREN, lexer.COMMA, lexer.RBRACKET, lexer.ASSIGN, lexer.COLON) {
		var err error
		ft.RetType, err = p.parseTypeSingle()
		if err != nil {
			return nil, err
		}
	}
	return ft, nil
}

// Parameter list

func (p *Parser) parseParams() ([]ast.Param, error) {
	if _, err := p.expect(lexer.LPAREN); err != nil {
		return nil, err
	}
	var params []ast.Param
	for !p.check(lexer.RPAREN) && !p.check(lexer.EOF) {
		param, err := p.parseParam()
		if err != nil {
			return nil, err
		}
		params = append(params, param)
		if p.check(lexer.COMMA) {
			p.advance()
		}
	}
	if _, err := p.expect(lexer.RPAREN); err != nil {
		return nil, err
	}
	return params, nil
}

func (p *Parser) parseParam() (ast.Param, error) {
	param := ast.Param{}
	if p.check(lexer.KW_CONST) {
		param.IsConst = true
		p.advance()
	}
	if p.check(lexer.DOTDOTDOT) {
		param.IsVarArgs = true
		p.advance()
		return param, nil
	}
	// name (optional for anonymous params like "const *char")
	if p.check(lexer.IDENT) && !isTypeKeyword(p.peek()) {
		// Could be name followed by type, or just a type-like name
		// Heuristic: if next token is a type token or IDENT, consume as name
		savedPos := p.pos
		candidate := p.advance().Literal
		if p.check(lexer.DOTDOTDOT) {
			// "name ..." — named variadic parameter: args ...
			param.Name = candidate
			param.IsVarArgs = true
			p.advance() // consume ...
			return param, nil
		}
		if p.match(lexer.RPAREN, lexer.COMMA) {
			// only one ident followed by ) or , — treat as type name
			p.pos = savedPos
		} else {
			param.Name = candidate
		}
	}
	if !p.match(lexer.RPAREN, lexer.COMMA, lexer.DOTDOTDOT) {
		var err error
		param.Type, err = p.parseTypeExpr()
		if err != nil {
			return param, err
		}
	}
	return param, nil
}

// parseTypeParams parses optional generic type parameters [t] or [t, r]
func (p *Parser) parseTypeParams() ([]string, error) {
	if !p.check(lexer.LBRACKET) {
		return nil, nil
	}
	p.advance()
	var params []string
	for !p.check(lexer.RBRACKET) && !p.check(lexer.EOF) {
		if p.check(lexer.IDENT) {
			params = append(params, p.advance().Literal)
		}
		if p.check(lexer.COMMA) {
			p.advance()
		}
	}
	if _, err := p.expect(lexer.RBRACKET); err != nil {
		return nil, err
	}
	return params, nil
}

// String interpolation

// parseStringInterp splits a raw string literal into an AST node
// If it contains {expr} patterns it returns an InterpolatedString,
// otherwise a plain StringLit
// ParseStringInterp is the exported form of parseStringInterp for use by codegen.
func ParseStringInterp(s string) (ast.Node, error) { return parseStringInterp(s) }

func parseStringInterp(s string) (ast.Node, error) {
	if !strings.Contains(s, "{") {
		return &ast.StringLit{Value: s}, nil
	}
	var parts []ast.StringPart
	for len(s) > 0 {
		idx := strings.Index(s, "{")
		if idx < 0 {
			parts = append(parts, ast.StringPart{Str: s})
			break
		}
		if idx > 0 {
			parts = append(parts, ast.StringPart{Str: s[:idx]})
		}
		s = s[idx+1:]
		end := strings.Index(s, "}")
		if end < 0 {
			// No closing brace – treat rest as literal
			parts = append(parts, ast.StringPart{Str: "{" + s})
			break
		}
		exprSrc := s[:end]
		s = s[end+1:]

		// Split off format specifier: {expr:fmt} → exprSrc="expr", fmtSpec="fmt"
		fmtSpec := ""
		if colonIdx := strings.Index(exprSrc, ":"); colonIdx >= 0 {
			fmtSpec = exprSrc[colonIdx+1:]
			exprSrc = exprSrc[:colonIdx]
		}

		// Re-lex and re-parse the embedded expression
		l := newInlineLexer(exprSrc)
		toks, err := l.Tokenize()
		if err != nil {
			return nil, fmt.Errorf("interpolation error in {%s}: %v", exprSrc, err)
		}
		rp := New(toks)
		expr, err := rp.parseExpr()
		if err != nil {
			return nil, fmt.Errorf("interpolation error in {%s}: %v", exprSrc, err)
		}
		parts = append(parts, ast.StringPart{IsExpr: true, Expr: expr, Format: fmtSpec})
	}
	if len(parts) == 1 && !parts[0].IsExpr {
		return &ast.StringLit{Value: parts[0].Str}, nil
	}
	return &ast.InterpolatedString{Parts: parts}, nil
}

// Helpers

func isTypeKeyword(tok lexer.Token) bool {
	switch tok.Type {
	case lexer.KW_FN:
		return true
	}
	switch tok.Literal {
	case "i8", "i16", "i32", "i64",
		"u8", "u16", "u32", "u64",
		"f32", "f64",
		"bool", "char", "string", "atom", "any",
		"void", "uint32", "size_t",
		"int", "uint":
		return true
	}
	return false
}

func isTypeToken(tok lexer.Token) bool {
	return isTypeKeyword(tok) || tok.Type == lexer.STAR || tok.Type == lexer.LBRACKET ||
		tok.Type == lexer.KW_CONST
}

// newInlineLexer creates a lexer for an inline expression string
func newInlineLexer(src string) *lexer.Lexer {
	return lexer.New(src)
}

// ParseType parses a single Tin type expression from a source string
// Used by the module system to deserialize type signatures
func ParseType(src string) (ast.TypeExpr, error) {
	l := lexer.New(src)
	tokens, err := l.Tokenize()
	if err != nil {
		return nil, err
	}
	p := New(tokens)
	return p.parseTypeExpr()
}


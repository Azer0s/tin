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
	// *T - pointer
	if p.check(lexer.STAR) {
		p.advance()

		elem, err := p.parseTypeSingle()
		if err != nil {
			return nil, err
		}

		return &ast.PointerType{Elem: elem, IsConst: isConst}, nil
	}
	// @[T1, T2, ...] - TupleArrayType (typed per-slot destructuring annotation)
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
	// [T] or [T; N] - array
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
	// fn(T...) R - function type
	if p.check(lexer.KW_FN) {
		return p.parseFuncType()
	}
	// void
	if p.check(lexer.KW_EXTERN) {
		// skip "extern" used as void-like placeholder
		p.advance()

		return &ast.SimpleType{Name: "void"}, nil
	}

	// (T1, T2, ...) - tuple type shorthand for Tuple[T1, T2, ...]
	// (T) with a single element is treated as grouping (returns T itself).
	if p.check(lexer.LPAREN) {
		p.advance() // consume (

		first, err := p.parseTypeExpr()
		if err != nil {
			return nil, err
		}

		types := []ast.TypeExpr{first}

		for p.check(lexer.COMMA) {
			p.advance()

			t, err := p.parseTypeExpr()
			if err != nil {
				return nil, err
			}

			types = append(types, t)
		}

		if _, err := p.expect(lexer.RPAREN); err != nil {
			return nil, err
		}

		if len(types) == 1 {
			return types[0], nil // grouping
		}

		return &ast.GenericType{Name: "Tuple", TypeParams: types}, nil
	}

	// Wildcard `_` in trait-bound positions, optionally named via `_: T`.
	// Parsed as a type expression unconditionally; semantic validation
	// (only legal inside trait bounds) lives in the type-checker.
	if p.check(lexer.IDENT) && p.peek().Literal == "_" {
		p.advance() // consume _

		w := &ast.WildcardType{}

		if p.check(lexer.COLON) {
			p.advance() // consume :

			tok, err := p.expect(lexer.IDENT)
			if err != nil {
				return nil, err
			}

			w.Name = tok.Literal
		}

		return w, nil
	}

	// Named type, possibly generic: name[T, R] or module::name[T, R]
	if !p.match(lexer.IDENT) && !isTypeKeyword(p.peek()) {
		return nil, p.errorf("expected type, got %s (%q)", p.peek().Type, p.peek().Literal)
	}

	name := p.advance().Literal

	// Module-qualified type: module::Type[T, R]
	for p.check(lexer.DCOLON) {
		p.advance() // consume ::

		part, err := p.expect(lexer.IDENT)
		if err != nil {
			return nil, err
		}

		name = name + "::" + part.Literal
	}

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

	ft := &ast.FuncType{}

	// Parse optional {#async} modifier on function type: fn{#async}(params) ret.
	// Reuse parseTags which handles the {#tag} grammar correctly.
	for _, tag := range p.parseTags() {
		if tag == "async" {
			ft.IsAsync = true
		}
	}

	if _, err := p.expect(lexer.LPAREN); err != nil {
		return nil, err
	}

	for !p.check(lexer.RPAREN) && !p.check(lexer.EOF) {
		// Optional param name (ignored in type). The lookahead picks
		// up two cases:
		//   1. IDENT followed by a builtin/sigil type token (`*`,
		//      `const`, `(`, or a builtin keyword like `i64`) - covered
		//      by isTypeToken minus LBRACKET.
		//   2. IDENT followed by another IDENT - the second IDENT must
		//      be the type, since two unnamed adjacent type names
		//      without a comma would be a syntax error anyway. This
		//      case matters for generic params (`fn(i t) bool` with
		//      `t` as a type parameter) and user-defined struct types
		//      (`fn(box Box)`); without it, the parser saw both as
		//      separate types and propagated a 2-arg fn-type into
		//      generic inference, which left the type variable
		//      unresolved (`@filter__t` stays generic, slice element
		//      stride defaults to i64, atom/i32 element arrays read
		//      OOB).
		//
		// LBRACKET is deliberately NOT treated as a leading sigil
		// here: `IDENT [` is ambiguous between `<name> [arr_type]` and
		// `<generic_name>[type_args...]`, and fn-type signatures never
		// expose param names, so the generic-type reading is the right
		// one (`fn(Seq[i64]) i64` is a fn taking Seq[i64], not a fn
		// taking `[i64]` with param name "Seq").
		nextTok := p.peekAt(1)
		identFollowedByNonBracketTypeStart := isTypeToken(nextTok) && nextTok.Type != lexer.LBRACKET
		identFollowedByIdent := nextTok.Type == lexer.IDENT

		if p.check(lexer.IDENT) && (identFollowedByNonBracketTypeStart || identFollowedByIdent) {
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

	p.skipWhitespace()

	var params []ast.Param

	for !p.check(lexer.RPAREN) && !p.check(lexer.EOF) {
		param, err := p.parseParam()
		if err != nil {
			return nil, err
		}

		params = append(params, param)

		if p.check(lexer.COMMA) {
			p.advance()
			p.skipWhitespace()
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
			// "name ..." - named variadic parameter: args ...
			param.Name = candidate
			param.IsVarArgs = true

			p.advance() // consume ...

			return param, nil
		}

		if p.match(lexer.RPAREN, lexer.COMMA) {
			// only one ident followed by ) or , - treat as type name
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
	params, _, err := p.parseTypeParamsWithWildcards()

	return params, err
}

// parseTypeParamsWithWildcards parses `[t, u, _: w]` and returns
// regular param names, wildcard slot names, and an error.
//
// The `_: w` form declares `w` as a call-site-supplied wildcard
// slot - the data/struct decl's body may reference w in trait
// bounds and method signatures, but the concrete type for w is
// chosen per call site rather than per instantiation. Where-guards
// on `w` are deferred to the call site.
//
// Bare `_` (no name) is rejected at this position: an unnamed
// wildcard in a type-param list has nothing else can refer to.
func (p *Parser) parseTypeParamsWithWildcards() ([]string, []string, error) {
	if !p.check(lexer.LBRACKET) {
		return nil, nil, nil
	}

	p.advance()

	var (
		params    []string
		wildcards []string
	)

	for !p.check(lexer.RBRACKET) && !p.check(lexer.EOF) {
		if p.check(lexer.IDENT) && p.peek().Literal == "_" {
			p.advance()

			if !p.check(lexer.COLON) {
				return nil, nil, p.errorf("bare `_` is not allowed in a type-parameter list; use `_: name` to introduce a named wildcard slot")
			}

			p.advance()

			tok, err := p.expect(lexer.IDENT)
			if err != nil {
				return nil, nil, err
			}

			wildcards = append(wildcards, tok.Literal)
		} else if p.check(lexer.IDENT) {
			params = append(params, p.advance().Literal)
		}

		if p.check(lexer.COMMA) {
			p.advance()
		}
	}

	if _, err := p.expect(lexer.RBRACKET); err != nil {
		return nil, nil, err
	}

	return params, wildcards, nil
}

// parseTypeArgList parses [TypeExpr] or [TypeExpr, TypeExpr, ...] (concrete type arguments).
// Used for generic struct literals like Channel[i64]{...}.
func (p *Parser) parseTypeArgList() ([]ast.TypeExpr, error) {
	if !p.check(lexer.LBRACKET) {
		return nil, nil
	}

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

	return args, nil
}

// String interpolation

// ParseStringInterp is the exported form of parseStringInterp for use by codegen.
// It splits a raw string literal into an AST node.
// If it contains {expr} patterns it returns an InterpolatedString,
// otherwise a plain StringLit.
func ParseStringInterp(s string) (ast.Node, error) { return parseStringInterp(s) }

// findFormatColon finds the index of a single ':' in s that acts as a format
// specifier separator, skipping '::' (scope-access operator).
// Returns -1 if no such colon is found.
func findFormatColon(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			// Skip '::' (scope-access operator).
			if i+1 < len(s) && s[i+1] == ':' {
				i++ // skip the second ':'

				continue
			}

			return i
		}
	}

	return -1
}

func parseStringInterp(s string) (ast.Node, error) {
	if !strings.Contains(s, "{") {
		// Still need to unescape \{ and \} that might appear even without interpolation.
		if strings.Contains(s, "\\{") || strings.Contains(s, "\\}") {
			s = strings.ReplaceAll(s, "\\{", "{")
			s = strings.ReplaceAll(s, "\\}", "}")
		}

		return &ast.StringLit{Value: s}, nil
	}

	var parts []ast.StringPart

	for len(s) > 0 {
		idx := strings.Index(s, "{")
		if idx < 0 {
			// Unescape any remaining \{ \} in the tail.
			s = strings.ReplaceAll(s, "\\{", "{")
			s = strings.ReplaceAll(s, "\\}", "}")
			parts = append(parts, ast.StringPart{Str: s})

			break
		}
		// Check for \{ escape: if the { is preceded by a backslash it is literal.
		if idx > 0 && s[idx-1] == '\\' {
			// Emit the text before \{ (without the backslash), then a literal {, and continue.
			prefix := s[:idx-1]
			prefix = strings.ReplaceAll(prefix, "\\{", "{")
			prefix = strings.ReplaceAll(prefix, "\\}", "}")
			parts = append(parts, ast.StringPart{Str: prefix + "{"})
			s = s[idx+1:]

			continue
		}

		if idx > 0 {
			prefix := s[:idx]
			prefix = strings.ReplaceAll(prefix, "\\{", "{")
			prefix = strings.ReplaceAll(prefix, "\\}", "}")
			parts = append(parts, ast.StringPart{Str: prefix})
		}

		s = s[idx+1:]

		end := strings.Index(s, "}")
		if end < 0 {
			// No closing brace - treat rest as literal
			parts = append(parts, ast.StringPart{Str: "{" + s})

			break
		}

		exprSrc := s[:end]
		s = s[end+1:]

		// Split off format specifier: {expr:fmt} -> exprSrc="expr", fmtSpec="fmt"
		// Skip "::" (scope-access operator) when searching for the format colon.
		fmtSpec := ""
		if colonIdx := findFormatColon(exprSrc); colonIdx >= 0 {
			fmtSpec = exprSrc[colonIdx+1:]
			exprSrc = exprSrc[:colonIdx]
		}

		// Re-lex and re-parse the embedded expression
		l := newInlineLexer(exprSrc)

		toks, err := l.Tokenize()
		if err != nil {
			return nil, fmt.Errorf("interpolation error in {%s}: %w", exprSrc, err)
		}

		rp := New(toks, "<interp>")

		expr, err := rp.parseExpr()
		if err != nil {
			return nil, fmt.Errorf("interpolation error in {%s}: %w", exprSrc, err)
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
	if tok.Type == lexer.KW_FN {
		return true
	}

	switch tok.Literal {
	case "i8", "i16", "i32", "i64",
		"u8", "u16", "u32", "u64",
		"f32", "f64",
		"bool", "char", "string", "atom", "any",
		"void", "uint32", "size_t":
		return true
	default:
		return false
	}
}

func isTypeToken(tok lexer.Token) bool {
	return isTypeKeyword(tok) || tok.Type == lexer.STAR || tok.Type == lexer.LBRACKET ||
		tok.Type == lexer.KW_CONST || tok.Type == lexer.LPAREN
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

	p := New(tokens, "<type>")

	return p.parseTypeExpr()
}

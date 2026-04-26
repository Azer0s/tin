package parser

import (
	"fmt"
	"strings"

	"github.com/Azer0s/tin/ast"
	"github.com/Azer0s/tin/lexer"
)

// Struct / Trait / Type / Enum / Union / Data declarations

// Struct / Trait / Type / Enum / Union / Data declarations

func (p *Parser) parseStructDecl(tags []string) (*ast.StructDecl, error) {
	p.advance() // consume struct

	// Optional {#tags} before name. Tags scoped with @fn/@method/@static_fn/@field
	// are collected separately; propagation happens in codegen.
	var scopedTags []ast.ScopedTag

	if p.check(lexer.LBRACE) {
		moreTags, moreScoped, err := p.parseStructTags()
		if err != nil {
			return nil, err
		}

		tags = append(tags, moreTags...)
		scopedTags = append(scopedTags, moreScoped...)
	}

	nameTok, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, err
	}

	typeParams, _ := p.parseTypeParams()

	// Optional trait implementations: struct Foo(TraitA, TraitB[T]) =
	// The list may span multiple lines; newlines inside parens are ignored.
	var impls []ast.TypeExpr

	if p.check(lexer.LPAREN) {
		p.advance()
		p.skipWhitespace()

		for !p.check(lexer.RPAREN) && !p.check(lexer.EOF) {
			ti, err2 := p.parseTypeExpr()
			if err2 != nil {
				return nil, err2
			}

			impls = append(impls, ti)

			p.skipWhitespace()

			if p.check(lexer.COMMA) {
				p.advance()
				p.skipWhitespace()
			}
		}

		if _, err2 := p.expect(lexer.RPAREN); err2 != nil {
			return nil, err2
		}
	}

	// Optional generic type constraints: "where t is addable"
	// Appear before `=`, same syntax as function constraints.
	constraints := p.parseTypeConstraints()

	if _, err := p.expect(lexer.ASSIGN); err != nil {
		return nil, err
	}

	decl := &ast.StructDecl{
		Name: nameTok.Literal, TypeParams: typeParams,
		Constraints: constraints, Implements: impls,
		Tags: tags, ScopedTags: scopedTags,
	}

	// Parse body (fields + methods)
	if p.check(lexer.NEWLINE) {
		p.advance()
		p.skipNewlines()

		if p.check(lexer.INDENT) {
			p.advance()
			p.skipNewlines()

			for !p.check(lexer.DEDENT) && !p.check(lexer.EOF) {
				// `pass` as the sole body element declares an empty struct.
				if p.check(lexer.KW_PASS) {
					p.advance()
					p.skipNewlines()

					continue
				}

				item, err2 := p.parseStructItem()
				if err2 != nil {
					return nil, err2
				}

				switch v := item.(type) {
				case *ast.StructField:
					decl.Fields = append(decl.Fields, *v)
				case *ast.FuncDecl:
					decl.Methods = append(decl.Methods, v)
				}

				p.skipNewlines()
			}

			if p.check(lexer.DEDENT) {
				p.advance()
			}
		}
	}

	return decl, nil
}

func (p *Parser) parseStructItem() (any, error) {
	isStatic := false
	if p.check(lexer.KW_STATIC) {
		isStatic = true

		p.advance()
	}

	if p.check(lexer.KW_FN) {
		fn, err := p.parseFuncDecl(nil, isStatic)
		if err == nil && fn != nil && !fn.IsStatic {
			// Auto-detect: a struct method with no "this" first parameter is static.
			hasThis := len(fn.Params) > 0 && fn.Params[0].Name == "this"
			if !hasThis && !fn.IsVirtual {
				fn.IsStatic = true
			}
		}

		return fn, err
	}
	// Field: [const|var] name [weak|own] type [forward]
	//
	// A leading `const` or `var` sets the field's mutability. We only
	// consume it when the next token is an identifier - that keeps a plain
	// `const` or `var` variable declaration (which can't legally appear
	// here anyway) from being silently swallowed.
	isConst := false
	isVar := false

	if p.check(lexer.KW_CONST) && p.peekAt(1).Type == lexer.IDENT {
		isConst = true

		p.advance()
	} else if p.check(lexer.KW_VAR) && p.peekAt(1).Type == lexer.IDENT {
		isVar = true

		p.advance()
	}

	nameTok, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, err
	}

	var typ ast.TypeExpr

	isWeak := false
	isOwn := false
	isForward := false

	if p.check(lexer.KW_WEAK) {
		isWeak = true

		p.advance()
	} else if p.check(lexer.KW_OWN) {
		isOwn = true

		p.advance()
	}

	if !p.match(lexer.NEWLINE, lexer.DEDENT, lexer.EOF) {
		typ, err = p.parseTypeExpr()
		if err != nil {
			return nil, err
		}
	}

	if p.check(lexer.KW_FORWARD) {
		isForward = true

		p.advance()
	}
	// Optional field tags: @"tag1" @"tag2"
	var tags []string

	for p.check(lexer.AT) {
		p.advance()

		tagTok, err2 := p.expect(lexer.STRING_LIT)
		if err2 != nil {
			return nil, err2
		}

		tags = append(tags, tagTok.Literal)
	}

	return &ast.StructField{
		Name:      nameTok.Literal,
		Type:      typ,
		IsForward: isForward,
		IsWeak:    isWeak,
		IsOwn:     isOwn,
		IsConst:   isConst,
		IsVar:     isVar,
		Tags:      tags,
	}, nil
}

func (p *Parser) parseTraitDecl() (*ast.TraitDecl, error) {
	p.advance() // consume trait
	// optional generic param [k]
	var traitTypeParams []string
	if p.check(lexer.LBRACKET) {
		traitTypeParams, _ = p.parseTypeParams()
	}

	nameTok, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, err
	}

	typeParams, _ := p.parseTypeParams()

	decl := &ast.TraitDecl{Name: nameTok.Literal, TypeParams: typeParams}
	_ = traitTypeParams

	// "trait print as fn() [char]"
	// "trait[k] implicit[t] as static fn(val t) k"
	if p.check(lexer.KW_AS) {
		p.advance()

		decl.IsAlias = true

		if p.check(lexer.KW_STATIC) {
			p.advance() // consume "static"

			decl.IsStaticAlias = true
		}

		decl.AliasType, err = p.parseTypeExpr()
		if err != nil {
			return nil, err
		}

		return decl, nil
	}

	if !p.check(lexer.ASSIGN) {
		return decl, nil
	}

	p.advance() // consume =

	if p.check(lexer.NEWLINE) {
		p.advance()
		p.skipNewlines()

		if p.check(lexer.INDENT) {
			p.advance()
			p.skipNewlines()

			for !p.check(lexer.DEDENT) && !p.check(lexer.EOF) {
				if p.check(lexer.KW_FN) {
					fn, err2 := p.parseFuncDecl(nil, false)
					if err2 != nil {
						return nil, err2
					}

					decl.Methods = append(decl.Methods, fn)
				} else if p.check(lexer.IDENT) || ((p.check(lexer.KW_CONST) || p.check(lexer.KW_VAR)) && p.peekAt(1).Type == lexer.IDENT) {
					// forward field: [const|var] name type forward
					isConst := false
					isVar := false

					if p.check(lexer.KW_CONST) {
						isConst = true

						p.advance()
					} else if p.check(lexer.KW_VAR) {
						isVar = true

						p.advance()
					}

					fname := p.advance().Literal

					ftype, err2 := p.parseTypeExpr()
					if err2 != nil {
						return nil, err2
					}

					if p.check(lexer.KW_FORWARD) {
						p.advance()
					}

					decl.ForwardFields = append(decl.ForwardFields, ast.StructField{
						Name:    fname,
						Type:    ftype,
						IsConst: isConst,
						IsVar:   isVar,
					})
				} else {
					p.advance() // skip unexpected tokens
				}

				p.skipNewlines()
			}

			if p.check(lexer.DEDENT) {
				p.advance()
			}
		}
	}

	return decl, nil
}

func (p *Parser) parseTypeDecl() (*ast.TypeDecl, error) {
	p.advance() // consume type

	nameTok, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, err
	}

	typeParams, _ := p.parseTypeParams()
	if _, err := p.expect(lexer.ASSIGN); err != nil {
		return nil, err
	}

	typ, err := p.parseTypeExpr()
	if err != nil {
		return nil, err
	}

	// Optional trait-bound clauses AFTER the RHS, matching struct/fn style:
	//   type StrPair[T] = Pair[string, T] where T is ord
	// Enforced at monomorphization time (see ensureConcreteStruct /
	// monomorphizeFunc).
	constraints := p.parseTypeConstraints()
	decl := &ast.TypeDecl{
		Name:        nameTok.Literal,
		TypeParams:  typeParams,
		Constraints: constraints,
		Type:        typ,
	}

	// optional "override = fn ..."
	if p.check(lexer.KW_OVERRIDE) {
		p.advance()

		if _, err := p.expect(lexer.ASSIGN); err != nil {
			return nil, err
		}

		if p.check(lexer.NEWLINE) {
			p.advance()
			p.skipNewlines()

			if p.check(lexer.INDENT) {
				p.advance()
				p.skipNewlines()

				for !p.check(lexer.DEDENT) && !p.check(lexer.EOF) {
					if p.check(lexer.KW_FN) {
						fn, err2 := p.parseFuncDecl(nil, false)
						if err2 != nil {
							return nil, err2
						}

						decl.Overrides = append(decl.Overrides, fn)
					}

					p.skipNewlines()
				}

				if p.check(lexer.DEDENT) {
					p.advance()
				}
			}
		}
	}

	return decl, nil
}

func (p *Parser) parseEnumDecl() (*ast.EnumDecl, error) {
	p.advance() // consume enum

	decl := &ast.EnumDecl{}

	// "enum atom status" or "enum i32 weather"
	if p.check(lexer.IDENT) && p.peek().Literal == "atom" {
		decl.IsAtom = true

		p.advance()
	} else if isTypeKeyword(p.peek()) {
		decl.BaseType, _ = p.parseTypeExpr()
	}

	nameTok, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, err
	}

	decl.Name = nameTok.Literal

	if _, err := p.expect(lexer.ASSIGN); err != nil {
		return nil, err
	}

	if p.check(lexer.NEWLINE) {
		p.advance()
		p.skipNewlines()

		if p.check(lexer.INDENT) {
			p.advance()
			p.skipNewlines()

			for !p.check(lexer.DEDENT) && !p.check(lexer.EOF) {
				mem, err2 := p.parseEnumMember(decl.IsAtom)
				if err2 != nil {
					return nil, err2
				}

				decl.Members = append(decl.Members, mem)

				p.skipNewlines()
			}

			if p.check(lexer.DEDENT) {
				p.advance()
			}
		}
	}

	return decl, nil
}

func (p *Parser) parseEnumMember(isAtom bool) (ast.EnumMember, error) {
	mem := ast.EnumMember{IsAtom: isAtom}
	if isAtom {
		if p.check(lexer.ATOM_LIT) {
			mem.Name = p.advance().Literal
		} else if p.check(lexer.IDENT) {
			mem.Name = p.advance().Literal
		}
	} else {
		nameTok, err := p.expect(lexer.IDENT)
		if err != nil {
			return mem, err
		}

		mem.Name = nameTok.Literal
	}

	if p.check(lexer.COLON) {
		p.advance()

		var err error

		mem.Value, err = p.parseExpr()
		if err != nil {
			return mem, err
		}
	}

	if p.check(lexer.COMMA) {
		p.advance()
	}

	return mem, nil
}

func (p *Parser) parseUnionDecl() (*ast.UnionDecl, error) {
	p.advance() // consume union

	nameTok, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, err
	}

	if _, err := p.expect(lexer.ASSIGN); err != nil {
		return nil, err
	}

	decl := &ast.UnionDecl{Name: nameTok.Literal}

	// "as_i8 i8 | as_string string" vs "i8 | string"
	for {
		var fieldName string

		if p.check(lexer.IDENT) && !isTypeKeyword(p.peek()) {
			// Could be "as_i8 i8" (named) or just type name matching a non-primitive
			// Check if next is a type: named field followed by type
			saved := p.pos

			candidate := p.advance().Literal
			if isTypeToken(p.peek()) || p.check(lexer.IDENT) {
				fieldName = candidate
				decl.IsNamed = true
			} else {
				p.pos = saved
			}
		}
		// Use parseTypeSingle so that '|' is NOT consumed as part of the type -
		// it is the separator between union members.
		typ, err2 := p.parseTypeSingle()
		if err2 != nil {
			return nil, err2
		}

		decl.Members = append(decl.Members, ast.UnionMember{FieldName: fieldName, Type: typ})

		if !p.check(lexer.BITOR) {
			break
		}

		p.advance()
	}

	return decl, nil
}

// parseDataDecl parses an ADT declaration:
//
//	data Name[t, u] where t is ord =
//	  Variant0
//	  Variant1(t)
//	  Variant2(name type, name type)
//
// At least one variant must carry a payload; pure-nullary ADTs are
// rejected in favor of `enum`.
func (p *Parser) parseDataDecl() (*ast.DataDecl, error) {
	// `data` is a contextual keyword: we expect IDENT("data") here.
	p.advance() // consume "data" IDENT

	nameTok, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, err
	}

	typeParams, _ := p.parseTypeParams()

	constraints := p.parseTypeConstraints()

	if _, err := p.expect(lexer.ASSIGN); err != nil {
		return nil, err
	}

	decl := &ast.DataDecl{
		Name:        nameTok.Literal,
		TypeParams:  typeParams,
		Constraints: constraints,
	}

	if !p.check(lexer.NEWLINE) {
		return nil, fmt.Errorf("%s: data %s: expected newline after '='", p.peek().String(), decl.Name)
	}

	p.advance()
	p.skipNewlines()

	if !p.check(lexer.INDENT) {
		return nil, fmt.Errorf("%s: data %s: expected indented variant list", p.peek().String(), decl.Name)
	}

	p.advance()
	p.skipNewlines()

	anyPayload := false

	for !p.check(lexer.DEDENT) && !p.check(lexer.EOF) {
		v, err2 := p.parseDataVariant()
		if err2 != nil {
			return nil, err2
		}

		if len(v.Fields) > 0 {
			anyPayload = true
		}

		decl.Variants = append(decl.Variants, v)

		p.skipNewlines()
	}

	if p.check(lexer.DEDENT) {
		p.advance()
	}

	if len(decl.Variants) == 0 {
		return nil, fmt.Errorf("data %s: at least one variant is required", decl.Name)
	}

	if !anyPayload {
		return nil, fmt.Errorf("data %s: at least one variant must carry a payload; use `enum` for pure-nullary sums", decl.Name)
	}

	return decl, nil
}

func (p *Parser) parseDataVariant() (ast.DataVariant, error) {
	pos := p.peek()

	nameTok, err := p.expect(lexer.IDENT)
	if err != nil {
		return ast.DataVariant{}, err
	}

	v := ast.DataVariant{
		Pos:  ast.Pos{Line: pos.Line, Col: pos.Col},
		Name: nameTok.Literal,
	}

	if !p.check(lexer.LPAREN) {
		return v, nil
	}

	p.advance()
	p.skipWhitespace()

	for !p.check(lexer.RPAREN) && !p.check(lexer.EOF) {
		f, err2 := p.parseDataVariantField()
		if err2 != nil {
			return v, err2
		}

		v.Fields = append(v.Fields, f)

		p.skipWhitespace()

		if p.check(lexer.COMMA) {
			p.advance()
			p.skipWhitespace()
		}
	}

	if _, err := p.expect(lexer.RPAREN); err != nil {
		return v, err
	}

	return v, nil
}

// parseDataVariantField parses one field of a data variant: either a bare
// type (positional) or `name [own|weak] type` (named).
func (p *Parser) parseDataVariantField() (ast.StructField, error) {
	if p.isNamedDataField() {
		nameTok, err := p.expect(lexer.IDENT)
		if err != nil {
			return ast.StructField{}, err
		}

		var isWeak, isOwn bool

		if p.check(lexer.KW_WEAK) {
			isWeak = true

			p.advance()
		} else if p.check(lexer.KW_OWN) {
			isOwn = true

			p.advance()
		}

		typ, err := p.parseTypeExpr()
		if err != nil {
			return ast.StructField{}, err
		}

		return ast.StructField{Name: nameTok.Literal, Type: typ, IsWeak: isWeak, IsOwn: isOwn}, nil
	}

	var isWeak, isOwn bool

	if p.check(lexer.KW_WEAK) {
		isWeak = true

		p.advance()
	} else if p.check(lexer.KW_OWN) {
		isOwn = true

		p.advance()
	}

	typ, err := p.parseTypeExpr()
	if err != nil {
		return ast.StructField{}, err
	}

	return ast.StructField{Type: typ, IsWeak: isWeak, IsOwn: isOwn}, nil
}

// isNamedDataField returns true when the next tokens look like `name type`
// rather than a bare type. Named form: IDENT followed by KW_OWN, KW_WEAK,
// another IDENT, STAR, LBRACKET, or a primitive-type keyword.
func (p *Parser) isNamedDataField() bool {
	if !p.check(lexer.IDENT) {
		return false
	}

	next := p.peekAt(1)

	switch next.Type {
	case lexer.KW_OWN, lexer.KW_WEAK, lexer.STAR, lexer.LBRACKET:
		return true
	case lexer.IDENT:
		return true
	}

	return isTypeKeyword(next)
}

func (p *Parser) parseUseDecl() (*ast.UseDecl, error) {
	p.advance() // consume use

	decl := &ast.UseDecl{}

	// `use { name1, name2! } from module` - selective import as bare names.
	if p.check(lexer.LBRACE) {
		p.advance() // consume {

		var names []string

		for !p.check(lexer.RBRACE) && !p.check(lexer.EOF) {
			if p.check(lexer.IDENT) {
				name := p.advance().Literal
				// Allow trailing ! for macro names: min!, max!
				if p.check(lexer.NOT) {
					p.advance()

					name += "!"
				}

				names = append(names, name)
			}

			if p.check(lexer.COMMA) {
				p.advance()
			}
		}

		if _, err := p.expect(lexer.RBRACE); err != nil {
			return nil, err
		}
		// Expect soft keyword "from"
		if !p.check(lexer.IDENT) || p.peek().Literal != "from" {
			return nil, fmt.Errorf("expected 'from' after import list, got %q", p.peek().Literal)
		}

		p.advance() // consume "from"
		// Parse the module path: "sync", "std::math", or "./foo.tin"
		if p.check(lexer.STRING_LIT) {
			decl.Path = p.advance().Literal
			decl.IsFile = true
		} else {
			var parts []string
			if p.check(lexer.IDENT) {
				parts = append(parts, p.advance().Literal)
			}

			for p.check(lexer.DCOLON) {
				p.advance()
				parts = append(parts, p.advance().Literal)
			}

			decl.Path = strings.Join(parts, "::")
		}

		decl.Names = names
		decl.FromSyntax = true

		return decl, nil
	}

	if p.check(lexer.KW_EXTERN) {
		decl.IsExtern = true

		p.advance()

		if _, err := p.expect(lexer.LPAREN); err != nil {
			return nil, err
		}

		p.skipWhitespace()

		for !p.check(lexer.RPAREN) && !p.check(lexer.EOF) {
			imp, err := p.parseUseImport()
			if err != nil {
				return nil, err
			}

			decl.Imports = append(decl.Imports, imp)

			if p.check(lexer.COMMA) {
				p.advance()
			}

			p.skipWhitespace()
		}

		if _, err := p.expect(lexer.RPAREN); err != nil {
			return nil, err
		}
	} else if p.check(lexer.STRING_LIT) {
		// File-path import: use "./foo.tin" or use "./bar"
		decl.Path = p.advance().Literal
		decl.IsFile = true
	} else {
		// Build path from scope-access tokens: "use io" / "use std::math"
		var parts []string
		if p.check(lexer.IDENT) {
			parts = append(parts, p.advance().Literal)
		}

		for p.check(lexer.DCOLON) {
			p.advance()
			parts = append(parts, p.advance().Literal)
		}

		decl.Path = strings.Join(parts, "::")
	}

	return decl, nil
}

func (p *Parser) parseUseImport() (ast.UseImport, error) {
	imp := ast.UseImport{}
	// Parse the local (Tin) name first
	if p.check(lexer.IDENT) {
		imp.LocalName = p.advance().Literal
		imp.ExternName = imp.LocalName // default: C name == Tin name
	}
	// Optional ("exportedName") after the local name allows renaming:
	//   malloc("mymallocname") as fn(size_t) *void
	// means LocalName="malloc", ExternName="mymallocname"
	if p.check(lexer.LPAREN) {
		p.advance()

		if p.check(lexer.STRING_LIT) {
			imp.ExternName = p.advance().Literal
		}

		if _, err := p.expect(lexer.RPAREN); err != nil {
			return imp, err
		}
	}

	if p.check(lexer.KW_AS) {
		p.advance()

		typ, err := p.parseTypeExpr()
		if err != nil {
			return imp, err
		}

		imp.Type = typ
	}

	return imp, nil
}

func (p *Parser) parseExportDecl() (*ast.ExportDecl, error) {
	p.advance() // consume export

	decl := &ast.ExportDecl{}

	if p.check(lexer.LBRACE) {
		p.advance()

		for !p.check(lexer.RBRACE) && !p.check(lexer.EOF) {
			// Skip whitespace tokens (multiline export blocks produce INDENT/DEDENT/NEWLINE)
			if p.check(lexer.NEWLINE) || p.check(lexer.INDENT) || p.check(lexer.DEDENT) {
				p.advance()

				continue
			}

			if p.check(lexer.IDENT) {
				name := p.advance().Literal
				// Allow trailing ! on macro names: "min!", "todo!", etc.
				if p.check(lexer.NOT) {
					p.advance()

					name += "!"
				}

				decl.Names = append(decl.Names, name)
			} else if p.check(lexer.NOT) {
				// Stray ! that wasn't consumed; skip to avoid infinite loop.
				p.advance()
			}

			if p.check(lexer.COMMA) {
				p.advance()
			}
		}

		if _, err := p.expect(lexer.RBRACE); err != nil {
			return nil, err
		}
	}

	if p.check(lexer.KW_AS) {
		p.advance()

		if p.check(lexer.IDENT) {
			decl.AsName = p.advance().Literal
		}
	}

	return decl, nil
}

func (p *Parser) parseTestDecl() (*ast.TestDecl, error) {
	tok := p.advance() // consume 'test'
	decl := &ast.TestDecl{}
	// Collect position from 'test' token
	_ = tok

	// Expect a string description
	if !p.check(lexer.STRING_LIT) {
		return nil, fmt.Errorf("line %d: expected string description after 'test'", p.peek().Line)
	}

	decl.Desc = p.advance().Literal

	p.skipNewlines()

	if _, err := p.expect(lexer.ASSIGN); err != nil {
		return nil, err
	}

	body, err := p.parseFuncBody()
	if err != nil {
		return nil, err
	}

	decl.Body = body

	return decl, nil
}

func (p *Parser) parseMacroDecl(tags []string) (*ast.MacroDecl, error) {
	p.advance() // consume macro

	if p.check(lexer.LBRACE) {
		moreTags := p.parseTags()
		tags = append(tags, moreTags...)
	}

	nameTok, err := p.expect(lexer.IDENT)
	if err != nil {
		return nil, err
	}
	// macro may have "!" in name: try!
	name := nameTok.Literal

	if p.check(lexer.NOT) {
		p.advance()

		name += "!"
	}
	// params - optional typed parameters: (n i64, m, s string)
	var (
		params     []string
		paramTypes []string
	)

	if p.check(lexer.LPAREN) {
		p.advance()

		for !p.check(lexer.RPAREN) && !p.check(lexer.EOF) {
			if p.check(lexer.IDENT) {
				paramName := p.advance().Literal
				params = append(params, paramName)
				// Check for optional type annotation (not a comma, closing paren, or EOF)
				if !p.check(lexer.COMMA) && !p.check(lexer.RPAREN) && !p.check(lexer.EOF) {
					te, terr := p.parseTypeExpr()
					if terr != nil {
						return nil, terr
					}

					paramTypes = append(paramTypes, te.String())
				} else {
					paramTypes = append(paramTypes, "") // untyped - infer from call site
				}
			}

			if p.check(lexer.COMMA) {
				p.advance()
			}
		}

		if _, err := p.expect(lexer.RPAREN); err != nil {
			return nil, err
		}
	}

	if _, err := p.expect(lexer.ASSIGN); err != nil {
		return nil, err
	}

	body, err := p.parseFuncBody()
	if err != nil {
		return nil, err
	}
	// CTFE macros (block body) automatically get the #computed tag
	if _, isBlock := body.(*ast.Block); isBlock {
		tags = append(tags, "computed")
	}

	return &ast.MacroDecl{Name: name, Tags: tags, Params: params, ParamTypes: paramTypes, Body: body}, nil
}

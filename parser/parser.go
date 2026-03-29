// Package parser implements a recursive-descent parser for the tin language
// It consumes the INDENT/DEDENT token stream produced by the lexer and
// builds the AST defined in the ast package
package parser

import (
	"fmt"

	"github.com/Azer0s/tin/ast"
	"github.com/Azer0s/tin/lexer"
)

// Parser holds the token stream and current position
type Parser struct {
	tokens         []lexer.Token
	pos            int
	noParensMacros map[string]string // macro name -> backtick expansion body
}

// New creates a Parser over the given token slice
func New(tokens []lexer.Token) *Parser {
	p := &Parser{tokens: tokens, noParensMacros: map[string]string{}}
	p.collectNoParensMacros()

	return p
}

// RegisterNoParensMacro adds an external #no_parens macro to the parser's
// expansion table. Call after New() and before Parse() to support imported
// #no_parens macros (e.g. `use { loop } from macros`).
func (p *Parser) RegisterNoParensMacro(name, expansion string) {
	p.noParensMacros[name] = expansion
}

// collectNoParensMacros performs a first-pass scan to find all macro declarations
// tagged with #no_parens and records their backtick expansion body.
// It does not advance p.pos - it uses a local index only.
func (p *Parser) collectNoParensMacros() {
	for i := 0; i < len(p.tokens); i++ {
		if p.tokens[i].Type != lexer.KW_MACRO {
			continue
		}
		// Expect: macro { #... no_parens ... } NAME ( ) = NEWLINE INDENT return BACKTICK_LIT
		j := i + 1
		if j >= len(p.tokens) || p.tokens[j].Type != lexer.LBRACE {
			continue
		}
		j++ // skip {
		hasNoParens := false
		for j < len(p.tokens) && p.tokens[j].Type != lexer.RBRACE {
			if p.tokens[j].Type == lexer.CONTROL_TAG && p.tokens[j].Literal == "no_parens" {
				hasNoParens = true
			}
			j++
		}
		if !hasNoParens || j >= len(p.tokens) {
			continue
		}
		j++ // skip }
		if j >= len(p.tokens) || p.tokens[j].Type != lexer.IDENT {
			continue
		}
		macroName := p.tokens[j].Literal
		j++
		// Optional ! suffix on macro name
		if j < len(p.tokens) && p.tokens[j].Type == lexer.NOT {
			j++
		}
		// () - zero-arg macro
		if j >= len(p.tokens) || p.tokens[j].Type != lexer.LPAREN {
			continue
		}
		j++
		if j >= len(p.tokens) || p.tokens[j].Type != lexer.RPAREN {
			continue
		}
		j++
		// = NEWLINE INDENT return BACKTICK_LIT
		if j >= len(p.tokens) || p.tokens[j].Type != lexer.ASSIGN {
			continue
		}
		j++
		// skip newlines/indent
		for j < len(p.tokens) && (p.tokens[j].Type == lexer.NEWLINE || p.tokens[j].Type == lexer.INDENT) {
			j++
		}
		// Expect: return BACKTICK_LIT   (simple case)
		// Also handle: just BACKTICK_LIT if body is a single expression
		if j < len(p.tokens) && p.tokens[j].Type == lexer.KW_RETURN {
			j++
		}
		if j >= len(p.tokens) || p.tokens[j].Type != lexer.BACKTICK_LIT {
			continue
		}
		expansion := p.tokens[j].Literal
		p.noParensMacros[macroName] = expansion
	}
}

// Navigation helpers

func (p *Parser) peek() lexer.Token {
	if p.pos >= len(p.tokens) {
		return lexer.Token{Type: lexer.EOF}
	}

	return p.tokens[p.pos]
}

func (p *Parser) peekAt(offset int) lexer.Token {
	i := p.pos + offset
	if i < 0 || i >= len(p.tokens) {
		return lexer.Token{Type: lexer.EOF}
	}

	return p.tokens[i]
}

func (p *Parser) advance() lexer.Token {
	tok := p.peek()
	if p.pos < len(p.tokens) {
		p.pos++
	}

	return tok
}

func (p *Parser) check(t lexer.TokenType) bool { return p.peek().Type == t }

func (p *Parser) match(ts ...lexer.TokenType) bool {
	for _, t := range ts {
		if p.check(t) {
			return true
		}
	}

	return false
}

func (p *Parser) expect(t lexer.TokenType) (lexer.Token, error) {
	tok := p.peek()
	if tok.Type != t {
		return tok, fmt.Errorf("expected %s, got %s (%q) at %d:%d",
			t, tok.Type, tok.Literal, tok.Line, tok.Col)
	}

	return p.advance(), nil
}

func (p *Parser) skipNewlines() {
	for p.check(lexer.NEWLINE) {
		p.advance()
	}
}

func (p *Parser) skipSemisAndNewlines() {
	for p.check(lexer.NEWLINE) || p.check(lexer.SEMI) {
		p.advance()
	}
}

// skipWhitespace skips NEWLINE, INDENT, and DEDENT tokens
// Use this inside parenthesised lists where indentation is not significant
func (p *Parser) skipWhitespace() {
	for p.match(lexer.NEWLINE, lexer.INDENT, lexer.DEDENT) {
		p.advance()
	}
}

func (p *Parser) curPos() ast.Pos {
	t := p.peek()

	return ast.Pos{Line: t.Line, Col: t.Col}
}

func (p *Parser) errorf(f string, a ...any) error {
	t := p.peek()

	return fmt.Errorf(f+" (at %d:%d)", append(a, t.Line, t.Col)...)
}

// Entry point

// Parse builds and returns the complete AST for the token stream
func (p *Parser) Parse() (*ast.Program, error) {
	prog := &ast.Program{}
	p.skipSemisAndNewlines()
	for !p.check(lexer.EOF) {
		node, err := p.parseTopLevel()
		if err != nil {
			return nil, err
		}
		if node != nil {
			prog.Stmts = append(prog.Stmts, node)
		}
		p.skipSemisAndNewlines()
	}

	return prog, nil
}

// ParseExpr parses a single expression from the token stream.
// Used by the CTFE macro system to re-parse macro execution output.
func (p *Parser) ParseExpr() (ast.Node, error) {
	p.skipNewlines()

	return p.parseExpr()
}

// Top-level declarations

func (p *Parser) parseTopLevel() (ast.Node, error) {
	// Collect leading control tags: fn{#pure #recurse} ...
	tags := p.parseTags()

	// If parseTags() consumed a {#tag} block, the next token must be a body brace
	// for a tagged block statement.
	if len(tags) > 0 && p.check(lexer.LBRACE) {
		return p.parseTaggedBlockWithTags(tags)
	}

	switch p.peek().Type {
	case lexer.KW_FN:

		return p.parseFuncDecl(tags, false)
	case lexer.KW_MACRO:

		return p.parseMacroDecl(tags)
	case lexer.KW_STRUCT:

		return p.parseStructDecl(tags)
	case lexer.KW_TRAIT:

		return p.parseTraitDecl()
	case lexer.KW_TYPE:

		return p.parseTypeDecl()
	case lexer.KW_ENUM:

		return p.parseEnumDecl()
	case lexer.KW_UNION:

		return p.parseUnionDecl()
	case lexer.KW_USE:

		return p.parseUseDecl()
	case lexer.KW_EXPORT:

		return p.parseExportDecl()
	case lexer.KW_TEST:

		return p.parseTestDecl()
	case lexer.KW_STATIC:
		p.advance()

		return p.parseFuncDecl(tags, true)
	case lexer.DEDENT:
		p.advance() // consume stray DEDENT at top level (from multiline function bodies)

		return nil, nil
	case lexer.IDENT:
		// Check if this identifier is a #no_parens macro name.
		// If so, expand the macro by injecting its backtick expansion as prefix tokens
		// before the rest of the declaration.
		if expansion, ok := p.noParensMacros[p.peek().Literal]; ok {
			p.advance() // consume macro name
			expToks, err := lexer.New(expansion).Tokenize()
			if err != nil {
				return nil, fmt.Errorf("no_parens macro expansion tokenize error: %w", err)
			}
			// Remove trailing EOF from expansion tokens
			for len(expToks) > 0 && expToks[len(expToks)-1].Type == lexer.EOF {
				expToks = expToks[:len(expToks)-1]
			}
			// Inject expansion tokens before current position
			newToks := make([]lexer.Token, 0, len(expToks)+len(p.tokens)-p.pos)
			newToks = append(newToks, expToks...)
			newToks = append(newToks, p.tokens[p.pos:]...)
			p.tokens = append(p.tokens[:p.pos], newToks...)

			return p.parseTopLevel()
		}

		return p.parseStatement()
	default:

		return p.parseStatement()
	}
}

// parseTags consumes optional {#tag ...} before a declaration keyword
func (p *Parser) parseTags() []string {
	var tags []string
	// Two forms: fn{#pure} or just leading control tags on the fn line
	if p.check(lexer.LBRACE) {
		// Peek ahead to see if this is a {#tag} block
		saved := p.pos
		p.advance() // consume {
		if p.check(lexer.CONTROL_TAG) {
			for p.check(lexer.CONTROL_TAG) {
				tags = append(tags, p.advance().Literal)
				// optional @fn / @field qualifier
				if p.check(lexer.AT) {
					p.advance()
					p.advance() // qualifier ident
				}
			}
			if p.check(lexer.RBRACE) {
				p.advance()
			}
		} else {
			p.pos = saved // not a tag block - restore
		}
	}

	return tags
}

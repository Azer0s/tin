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
	tokens []lexer.Token
	pos    int
}

// New creates a Parser over the given token slice
func New(tokens []lexer.Token) *Parser {
	return &Parser{tokens: tokens}
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
	p.skipNewlines()
	for !p.check(lexer.EOF) {
		node, err := p.parseTopLevel()
		if err != nil {
			return nil, err
		}
		if node != nil {
			prog.Stmts = append(prog.Stmts, node)
		}
		p.skipNewlines()
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
	// Collect leading control tags: fn{#pure #recurse} …
	tags := p.parseTags()

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
	case lexer.KW_DATA:
		return p.parseDataDecl()
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
	default:
		return p.parseStatement()
	}
}

// parseTags consumes optional {#tag …} before a declaration keyword
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
			p.pos = saved // not a tag block – restore
		}
	}
	return tags
}


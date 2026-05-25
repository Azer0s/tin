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
	tokens                 []lexer.Token
	pos                    int
	file                   string            // source file path; prepended to every error so the snippet renderer can locate the line
	warnings               []string          // raw `file:L:C: warning: msg [-Wname]` lines, drained by the caller through Warnings()
	noParensMacros         map[string]string // macro name -> backtick expansion body
	noWarnAwaitMatchGuards bool
	// continuationDedents tracks INDENT tokens consumed inside parseBinary for
	// multi-line operator continuations (e.g. `a && \n  b`).  skipNewlines
	// drains these so the body INDENT of the enclosing if/for/... is visible.
	continuationDedents int
	// suppressPostfixCast > 0 means parsePostfix should NOT consume `as` /
	// `is` postfix casts. Used by `&` / `*` operand parsing so the cast
	// attaches to the enclosing pointer expression (`(&x) as T`) instead
	// of the inner operand (`&(x as T)`).
	suppressPostfixCast int
	// pendingLambdaDedents counts trailing DEDENT tokens that belong to
	// a lambda body that exited early via the RPAREN-break in parseBlock
	// (the `f(fn(p) = stmt; stmt)` shape with `)` on the last body line).
	// The lambda's INDENT was consumed but the matching DEDENT arrives
	// later (after the call's `)` is consumed by the outer arg parser);
	// without this counter the DEDENT pops the OUTER scope instead.
	// skipNewlines / parseBlock loop check + decrement when DEDENT shows
	// up.
	pendingLambdaDedents int
	// blockDepth counts how many block scopes are currently open.
	// Bumped by parseBlock at entry, decremented at exit. Lets
	// parseStatement reject `var` (which is module-scope only) with
	// a clear diagnostic instead of silently producing a TopLevelVar
	// whose binding leaks out of scope.
	blockDepth int
}

// New creates a Parser over the given token slice. The file path is
// prepended to every error message so the snippet renderer can locate
// the offending source line; pass "" for unknown / synthetic input.
func New(tokens []lexer.Token, file string) *Parser {
	p := &Parser{tokens: tokens, file: file, noParensMacros: map[string]string{}}
	p.collectNoParensMacros()

	return p
}

// errAt returns an error tagged with the parser's source file plus the
// given line/column, in the canonical `file:L:C: msg` shape that
// codegen.RenderDiagnostic recognizes.
func (p *Parser) errAt(line, col int, format string, args ...any) error {
	return fmt.Errorf("%s:%d:%d: "+format, append([]any{p.file, line, col}, args...)...)
}

// errAtTok is errAt sourced from a token's position. Saves callers
// from repeating `t.Line, t.Col` everywhere.
func (p *Parser) errAtTok(t lexer.Token, format string, args ...any) error {
	return p.errAt(t.Line, t.Col, format, args...)
}

// warnAt records a warning at line/col. The string is stored in the
// canonical `file:L:C: warning: msg [-Wname]` shape so the caller can
// hand it straight to a Rust-style snippet renderer (see codegen.
// RenderDiagnostic) without further normalisation.
func (p *Parser) warnAt(line, col int, name, format string, args ...any) {
	body := fmt.Sprintf(format, args...)
	p.warnings = append(p.warnings,
		fmt.Sprintf("%s:%d:%d: warning: %s [-W%s]", p.file, line, col, body, name))
}

// Warnings returns the list of parser-emitted warnings recorded during
// the last Parse() call. Each entry is a `file:L:C: warning: msg
// [-Wname]` string ready for codegen.RenderDiagnostic.
func (p *Parser) Warnings() []string { return p.warnings }

// SetNoWarnAwaitMatchGuards suppresses the "all await match arms have guards" warning.
func (p *Parser) SetNoWarnAwaitMatchGuards(v bool) { p.noWarnAwaitMatchGuards = v }

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
		return tok, p.errAtTok(tok, "expected %s, got %s (%q)",
			t, tok.Type, tok.Literal)
	}

	return p.advance(), nil
}

func (p *Parser) skipNewlines() {
	for p.check(lexer.NEWLINE) {
		p.advance()
	}
	// Drain any continuation DEDENTs accumulated by parseBinary so that the
	// body INDENT of the enclosing if/for/... statement is visible next.
	for p.continuationDedents > 0 && p.check(lexer.DEDENT) {
		p.advance()
		p.continuationDedents--
	}
	// Drain owed lambda-DEDENTs: when parseBlock broke early on RPAREN
	// (multi-line lambda body that ends with `)` on the last stmt), the
	// matching DEDENT arrives after the call's `)` is consumed. Without
	// this drain, the outer block's parseBlock loop sees the DEDENT and
	// pops its own scope prematurely.
	for p.pendingLambdaDedents > 0 && p.check(lexer.DEDENT) {
		p.advance()
		p.pendingLambdaDedents--
	}
}

func (p *Parser) skipSemisAndNewlines() {
	for p.check(lexer.NEWLINE) || p.check(lexer.SEMI) {
		p.advance()
	}
	// Drain owed continuation DEDENTs from multi-line binary
	// operator expressions (e.g. `let c = a\n      || b`).
	// parseBinary records each consumed INDENT in
	// continuationDedents; parseBlock's loop would otherwise see
	// the matching DEDENT as the block terminator and close the
	// enclosing scope early.
	for p.continuationDedents > 0 && p.check(lexer.DEDENT) {
		p.advance()
		p.continuationDedents--
	}
	// Same lambda-DEDENT drain as in skipNewlines - parseBlock's main
	// loop calls this after each stmt and would otherwise see the owed
	// DEDENT and exit the wrong scope.
	for p.pendingLambdaDedents > 0 && p.check(lexer.DEDENT) {
		p.advance()
		p.pendingLambdaDedents--
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
	return p.errAtTok(p.peek(), f, a...)
}

// Entry point

// Parse builds and returns the complete AST for the token stream
func (p *Parser) Parse() (*ast.Program, error) {
	prog := &ast.Program{}

	p.skipSemisAndNewlines()

	for !p.check(lexer.EOF) {
		startPos := p.curPos()

		node, err := p.parseTopLevel()
		if err != nil {
			return nil, err
		}

		if node != nil {
			// Ensure every top-level node carries a position. Individual
			// parse* helpers don't always SetPos on the returned node, which
			// makes the REPL's source-extraction (extractSrc in repl/) fall
			// back to "the whole file" when boundaries aren't computable.
			if sp, ok := node.(interface{ SetPos(ast.Pos) }); ok {
				if node.Pos().Line == 0 {
					sp.SetPos(startPos)
				}
			}

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

	// `{#tag} { body }` C-brace tagged blocks were removed; use
	// `do{#tag}: body` at statement scope instead.  Top-level tags
	// still attach to the following fn/struct/macro/etc decl below.
	if len(tags) > 0 && p.check(lexer.LBRACE) {
		return nil, p.errorf("`{#tag} { body }` blocks were removed; use `do{#tag}: body` (indent block) or `do{#tag}: stmt` (single-statement inline) instead")
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
	case lexer.KW_CONST:
		// Module-scope `const` is a TU-level binding (a global, like
		// `var`), NOT a statement folded into the implicit main.
		// Routing through TopLevelVar makes the binding visible to
		// every function in the file -- including a user-defined
		// `fn main()` -- and stops it from suppressing the user main
		// via the implicit-main path.
		// Module-scope `let`, by contrast, IS part of the implicit
		// main and falls through to the statement parser below.
		return p.parseTopLevelLetConst()
	case lexer.KW_STATIC:
		p.advance()

		return p.parseFuncDecl(tags, true)
	case lexer.DEDENT:
		p.advance() // consume stray DEDENT at top level (from multiline function bodies)

		return nil, nil
	case lexer.IDENT:
		// Contextual keyword: `data Name = V0 | V1(...)` at the top level.
		// We accept `data` as a bare IDENT in all other positions (field name,
		// variable name, ...) to keep the surface surface area small.
		if p.peek().Literal == "data" && p.peekAt(1).Type == lexer.IDENT {
			return p.parseDataDecl()
		}
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

// parseStructTags is the struct-decl variant of parseTags: it preserves
// the optional `@scope` qualifier after each tag. Unscoped tags go into
// the first return value (for compatibility with hasTag consumers);
// scoped tags go into the second return value for codegen propagation.
// Unknown scope names produce a parse error at the struct decl site.
func (p *Parser) parseStructTags() ([]string, []ast.ScopedTag, error) {
	var (
		tags   []string
		scoped []ast.ScopedTag
	)

	if !p.check(lexer.LBRACE) {
		return nil, nil, nil
	}

	saved := p.pos
	p.advance() // consume {

	if !p.check(lexer.CONTROL_TAG) {
		p.pos = saved

		return nil, nil, nil
	}

	for p.check(lexer.CONTROL_TAG) {
		tagTok := p.advance()
		name := tagTok.Literal

		if p.check(lexer.AT) {
			p.advance()
			// Scope name may collide with a reserved keyword (notably `fn`),
			// so accept any non-whitespace token and dispatch on its literal.
			scopeTok := p.advance()
			scope := scopeTok.Literal

			if !isValidStructTagScope(scope) {
				return nil, nil, p.errAtTok(scopeTok, "unknown struct tag scope @%s (valid: @fn, @method, @static_fn, @field)", scope)
			}

			scoped = append(scoped, ast.ScopedTag{Name: name, Scope: scope})
		} else {
			tags = append(tags, name)
		}
	}

	if p.check(lexer.RBRACE) {
		p.advance()
	}

	return tags, scoped, nil
}

// isValidStructTagScope reports whether s names one of the member
// scopes recognized on a struct's {#tag@scope} header. Kept in sync
// with the propagation table in codegen.
func isValidStructTagScope(s string) bool {
	switch s {
	case "fn", "method", "static_fn", "field":
		return true
	}

	return false
}

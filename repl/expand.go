package repl

import (
	"fmt"
	"strings"

	"github.com/Azer0s/tin/ast"
	"github.com/Azer0s/tin/codegen"
	"github.com/Azer0s/tin/lexer"
	"github.com/Azer0s/tin/parser"
)

// tabGuard is an AutoCompleter that does nothing but prevents the terminal
// bell when Tab is pressed (readline rings the bell if AutoComplete is nil).
type tabGuard struct{}

func (t *tabGuard) Do([]rune, int) ([][]rune, int) { return nil, 0 }

// tryExpandMacro attempts to find and expand the outermost (or last) macro
// call in src. Returns the fully expanded line and true on success.
func tryExpandMacro(src string, reg *macroRegistry) (string, bool) {
	// Lex to find the macro call span.
	l := lexer.New(src)
	toks, err := l.Tokenize()
	if err != nil {
		return "", false
	}

	// Find the last macro call: IDENT (optionally + !) (optionally + (...)).
	// exprStart: byte where the full "pkg::macro!(args)" expression begins (for substitution).
	// textStart: byte where the bare "macro!(args)" begins (for expandEntry).
	exprStart, textStart, callEnd, bare, hasBang := findLastMacroCall(toks, src, reg)
	if exprStart < 0 {
		return "", false
	}

	entry, ok := reg.lookup(bare)
	if !ok {
		return "", false
	}

	// callText is just the bare macro call without scope prefix, so expandEntry
	// can parse it as a standalone macro invocation.
	callText := src[textStart:callEnd]
	expandedBody, err := expandEntry(entry, callText, hasBang)
	if err != nil {
		return "", false
	}

	// Replace from exprStart so the "pkg::" prefix is also removed.
	return src[:exprStart] + expandedBody, true
}

// findLastMacroCall scans tokens to find the last macro invocation.
// Returns (exprStart, textStart, end, bareName, hasBang) or (-1,...) if none found.
// exprStart is the byte offset of the full expression (including any scope prefix).
// textStart is the byte offset of the bare macro identifier (excluding scope prefix).
func findLastMacroCall(toks []lexer.Token, src string, reg *macroRegistry) (exprStart, textStart, end int, bare string, hasBang bool) {
	exprStart = -1
	runes := []rune(src)

	for i, tok := range toks {
		if tok.Type != lexer.IDENT {
			continue
		}
		name := tok.Literal
		bang := false

		// Check for adjacent ! token.
		if i+1 < len(toks) && toks[i+1].Type == lexer.NOT && toks[i+1].Col == tok.Col+len(tok.Literal) {
			bang = true
			name = tok.Literal // bare name without !
		}

		// Is this a known macro?
		if _, ok := reg.lookup(name); !ok {
			if !bang {
				continue
			}
			continue
		}

		// Find the byte offset of this token in the source.
		// tok.Col is 1-based column in the line.
		tokByteStart := colToByteOffset(runes, tok.Col-1)
		if tokByteStart < 0 {
			continue
		}

		// Walk back over any scope prefix (IDENT :: IDENT :: ...) so that the
		// full "pkg::macro" expression is replaced, not just the bare name.
		callByteStart := tokByteStart
		for j := i - 1; j >= 1; j -= 2 {
			if toks[j].Type == lexer.DCOLON && toks[j-1].Type == lexer.IDENT {
				callByteStart = colToByteOffset(runes, toks[j-1].Col-1)
			} else {
				break
			}
		}

		// Determine the end of the call.
		tokEnd := tokByteStart + len(tok.Literal)
		if bang {
			// skip the ! token
			tokEnd++ // '!' is 1 byte
		}

		// Check for opening paren.
		nextIdx := i + 1
		if bang {
			nextIdx = i + 2
		}
		if nextIdx < len(toks) && toks[nextIdx].Type == lexer.LPAREN {
			// Find matching close paren.
			depth := 1
			j := nextIdx + 1
			for j < len(toks) && depth > 0 {
				switch toks[j].Type {
				case lexer.LPAREN:
					depth++
				case lexer.RPAREN:
					depth--
				}
				j++
			}
			// j is now one past the closing paren token.
			if j <= len(toks) {
				closeCol := toks[j-1].Col - 1 // 0-based col of ')'
				tokEnd = colToByteOffset(runes, closeCol) + 1
			}
		}

		exprStart = callByteStart
		textStart = tokByteStart
		end = tokEnd
		bare = tok.Literal
		hasBang = bang
	}
	return
}

// colToByteOffset converts a 0-based rune column to a byte offset in src.
func colToByteOffset(runes []rune, col int) int {
	if col < 0 || col >= len(runes) {
		return col // approximation for ASCII
	}
	return len(string(runes[:col]))
}

// expandEntry expands a macro entry given the full call text (e.g. "min!(3, 5)").
func expandEntry(entry macroEntry, callText string, hasBang bool) (string, error) {
	if entry.Decl == nil {
		return "", fmt.Errorf("no macro declaration")
	}

	// No-parens macros (#no_parens #no_excl): expand the backtick body directly.
	// These are parsed via a special token-injection path and can't go through
	// ExpandProgramMacros without pre-registering them with the parser.
	if entry.NoParens {
		body := entry.Decl.Body
		if es, ok := body.(*ast.ExprStmt); ok {
			body = es.Expr
		}
		if btl, ok := body.(*ast.BacktickLit); ok {
			return btl.Content, nil
		}
		return ast.PrintStmt(body, 0), nil
	}

	// For regular macros: build a minimal source with the declaration + call,
	// then use ExpandProgramMacros to do the expansion (including CTFE).
	declSrc := macroSrc(entry)
	src := declSrc + "\n" + callText

	l := lexer.New(src)
	toks, err := l.Tokenize()
	if err != nil {
		return "", err
	}
	p := parser.New(toks)
	prog, err := p.Parse()
	if err != nil {
		return "", err
	}
	if prog == nil {
		return "", fmt.Errorf("parse returned nil")
	}

	cg := codegen.New("<expand>")
	expanded, err := cg.ExpandProgramMacros(prog)
	if err != nil {
		return "", err
	}

	var parts []string
	for _, stmt := range expanded.Stmts {
		if _, isMacro := stmt.(*ast.MacroDecl); isMacro {
			continue
		}
		// expandNodeMacros wraps expansions in ExprStmt, so unwrap ExprStmt{ReturnStmt{Value}}.
		if es, ok := stmt.(*ast.ExprStmt); ok {
			if ret, ok2 := es.Expr.(*ast.ReturnStmt); ok2 && ret.Value != nil {
				parts = append(parts, ast.PrintStmt(ret.Value, 0))
				continue
			}
		}
		if ret, ok := stmt.(*ast.ReturnStmt); ok && ret.Value != nil {
			parts = append(parts, ast.PrintStmt(ret.Value, 0))
			continue
		}
		parts = append(parts, ast.PrintStmt(stmt, 0))
	}
	return strings.Join(parts, "\n"), nil
}

// macroSrc reconstructs the source declaration for a macro entry.
func macroSrc(entry macroEntry) string {
	if entry.Decl == nil {
		return ""
	}
	m := entry.Decl

	var sb strings.Builder
	sb.WriteString("macro")
	if len(m.Tags) > 0 {
		sb.WriteString("{")
		for i, tag := range m.Tags {
			if i > 0 {
				sb.WriteString(" ")
			}
			sb.WriteString("#")
			sb.WriteString(tag)
		}
		sb.WriteString("}")
	}
	sb.WriteString(" ")
	sb.WriteString(m.Name)
	sb.WriteString("(")
	sb.WriteString(strings.Join(m.Params, ", "))
	sb.WriteString(") = ")
	sb.WriteString(ast.PrintStmt(m.Body, 0))
	return sb.String()
}

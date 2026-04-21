package repl

import (
	"github.com/Azer0s/tin/lexer"
)

const (
	ansiReset      = "\033[0m"
	ansiBoldCyan   = "\033[1;36m"
	ansiGreen      = "\033[32m"
	ansiYellow     = "\033[33m"
	ansiBoldYellow = "\033[1;33m"
	ansiMagenta    = "\033[35m"
	ansiCyan       = "\033[36m"
)

var builtinTypes = map[string]bool{
	"i8": true, "i16": true, "i32": true, "i64": true, "i128": true,
	"u8": true, "u16": true, "u32": true, "u64": true, "u128": true,
	"f32": true, "f64": true, "f128": true,
	"bool": true, "string": true, "char": true, "any": true, "void": true,
}

type highlighter struct {
	macros *macroRegistry
}

func (h *highlighter) Paint(line []rune, _ int) []rune {
	src := string(line)
	l := lexer.New(src)
	tokens, err := l.Tokenize()
	if err != nil || len(tokens) == 0 {
		return line
	}

	// Pre-compute which tokens are part of a macro invocation:
	// An IDENT immediately followed by NOT (!) at adjacent columns is a
	// bang-macro. The NOT token that immediately follows is also part of it.
	// Any IDENT :: prefix (e.g. "macros::") before the macro name is also highlighted.
	isMacroPart := make([]bool, len(tokens))
	for i, tok := range tokens {
		if tok.Type != lexer.IDENT {
			continue
		}
		isMacro := false
		// Check adjacency with next ! token.
		if i+1 < len(tokens) {
			next := tokens[i+1]
			if next.Type == lexer.NOT && next.Col == tok.Col+len(tok.Literal) {
				isMacroPart[i] = true   // the IDENT
				isMacroPart[i+1] = true // the !
				isMacro = true
			}
		}
		// No-excl / no-parens macro: known name used as bare identifier.
		if !isMacro && h.macros != nil && h.macros.isMacroIdent(tok.Literal) {
			isMacroPart[i] = true
			isMacro = true
		}
		// Mark any scope prefix (IDENT :: IDENT ::) preceding the macro name.
		if isMacro {
			for j := i - 1; j >= 1; j -= 2 {
				if tokens[j].Type == lexer.DCOLON && tokens[j-1].Type == lexer.IDENT {
					isMacroPart[j] = true
					isMacroPart[j-1] = true
				} else {
					break
				}
			}
		}
	}

	runes := []rune(src)
	var out []rune
	pos := 0

	for i, tok := range tokens {
		if tok.Type == lexer.EOF || tok.Type == lexer.NEWLINE || tok.Type == lexer.INDENT || tok.Type == lexer.DEDENT {
			continue
		}

		lit := []rune(tok.Literal)
		if len(lit) == 0 {
			continue
		}

		// Find the token's rune offset in the source.
		start := -1
		for j := pos; j <= len(runes)-len(lit); j++ {
			match := true
			for k, r := range lit {
				if runes[j+k] != r {
					match = false
					break
				}
			}
			if match {
				start = j
				break
			}
		}
		if start == -1 {
			continue
		}

		out = append(out, runes[pos:start]...)

		var color string
		if isMacroPart[i] {
			color = ansiYellow
		} else {
			color = colorFor(tok.Type, tok.Literal)
		}

		if color != "" {
			out = append(out, []rune(color)...)
		}
		out = append(out, lit...)
		if color != "" {
			out = append(out, []rune(ansiReset)...)
		}

		pos = start + len(lit)
	}

	out = append(out, runes[pos:]...)
	return out
}

func colorFor(t lexer.TokenType, lit string) string {
	switch {
	case t >= lexer.KW_LET && t <= lexer.KW_OWN:
		return ansiBoldCyan
	case t == lexer.STRING_LIT || t == lexer.CHAR_LIT || t == lexer.BACKTICK_LIT:
		return ansiGreen
	case t == lexer.INT_LIT || t == lexer.FLOAT_LIT:
		return ansiYellow
	case t == lexer.ATOM_LIT:
		return ansiMagenta
	case t == lexer.BOOL_LIT:
		return ansiCyan
	case t == lexer.IDENT && builtinTypes[lit]:
		return ansiBoldYellow
	}
	return ""
}

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
	ansiBoldBlue   = "\033[1;34m"
)

var builtinTypes = map[string]bool{
	"i8": true, "i16": true, "i32": true, "i64": true, "i128": true,
	"u8": true, "u16": true, "u32": true, "u64": true, "u128": true,
	"f32": true, "f64": true, "f128": true,
	"bool": true, "string": true, "char": true, "any": true, "void": true,
}

type highlighter struct {
	macros   *macroRegistry
	opTraits *opTraitRegistry
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

		switch {
		case isMacroPart[i]:
			color = ansiYellow
		case h.isOverloadedOp(tok.Type):
			color = ansiBoldBlue
		default:
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

// isOverloadedOp reports whether tokenType is an operator whose corresponding
// built-in trait has been implemented in this REPL session. The mapping
// matches binOpTraitName / unaryOpTraitName in codegen — keep them in sync.
//
// `[`/`]` are left alone here: they appear in type-arg lists and array
// literals far more often than in indexing, so painting them based on
// `index`/`index_set` overload state would be visually noisy.
func (h *highlighter) isOverloadedOp(tokenType lexer.TokenType) bool {
	if h.opTraits == nil {
		return false
	}

	traits := opTraitsForToken(tokenType)
	for _, name := range traits {
		if h.opTraits.isOverloaded(name) {
			return true
		}
	}

	return false
}

// opTraitsForToken returns the trait names whose impl makes a given operator
// token "overloaded". Some tokens can dispatch through multiple traits
// (e.g. `==` -> comp; `<` -> ord; binary `+` -> add; unary `+` -> pos).
func opTraitsForToken(t lexer.TokenType) []string {
	switch t {
	case lexer.PLUS, lexer.PLUSEQ:
		return []string{"add", "pos"}
	case lexer.MINUS, lexer.MINUSEQ:
		return []string{"sub", "neg"}
	case lexer.STAR, lexer.STAREQ:
		return []string{"mul"}
	case lexer.SLASH, lexer.SLASHEQ:
		return []string{"div"}
	case lexer.PERCENT, lexer.PERCENTEQ:
		return []string{"mod"}
	case lexer.NOT:
		return []string{"not"}
	case lexer.EQEQ, lexer.NEQ:
		return []string{"comp"}
	case lexer.LT, lexer.LTEQ, lexer.GT, lexer.GTEQ:
		return []string{"ord"}
	case lexer.INC:
		return []string{"concat"}
	}

	return nil
}

func colorFor(t lexer.TokenType, lit string) string {
	switch {
	case t >= lexer.KW_LET && t <= lexer.KW_OWN:
		return ansiBoldCyan
	case t == lexer.IDENT && lit == "data":
		// `data` is a contextual keyword (ADT declarations); colorise like any
		// other declaration keyword so top-level `data Name = ...` reads as one.
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

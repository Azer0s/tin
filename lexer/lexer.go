// Package lexer implements the tin language lexer
// It produces INDENT/DEDENT tokens for indentation-sensitive parsing
// and handles all tin-specific syntax including atoms, control tags,
// string interpolation, and the full operator set
package lexer

import (
	"fmt"
	"strings"
	"unicode"
)

// TokenType identifies the kind of a token
type TokenType int

const (
	// Literals
	INT_LIT      TokenType = iota // 42, 0xFF
	FLOAT_LIT                     // 3.14
	STRING_LIT                    // "hello"
	BACKTICK_LIT                  // `expr`  - code splice literal
	CHAR_LIT                      // 'a'
	BOOL_LIT                      // true / false
	ATOM_LIT                      // 'ok  'err
	NONE_LIT                      // None

	// Identifier
	IDENT

	// Keywords
	KW_LET
	KW_CONST
	KW_FN
	KW_TYPE
	KW_STRUCT
	KW_TRAIT
	KW_ENUM
	KW_UNION
	KW_DATA
	KW_USE
	KW_EXPORT
	KW_EXTERN
	KW_RETURN
	KW_IF
	KW_ELSE
	KW_FOR
	KW_IN
	KW_MATCH
	KW_CASE
	KW_DEFAULT
	KW_DEFER
	KW_WHERE
	KW_MACRO
	KW_STATIC
	KW_VIRTUAL
	KW_AS
	KW_IS
	KW_FORWARD
	KW_OVERRIDE
	KW_SIZEOF
	KW_ADDR
	KW_BREAK
	KW_ECHO
	KW_TEST
	KW_TYPEOF
	KW_TRAITOF
	KW_FIELDNAMES
	KW_FIELDTYPES
	KW_FIELDTAG
	KW_GETFIELD
	KW_SETFIELD
	KW_PASS

	// Operators
	PLUS      // +
	MINUS     // -
	STAR      // *
	SLASH     // /
	PERCENT   // %
	ASSIGN    // =
	EQEQ      // ==
	NEQ       // !=
	LT        // <
	LTEQ      // <=
	GT        // >
	GTEQ      // >=
	AND       // &&
	OR        // ||
	NOT       // !
	AMP       // &
	BITOR     // |
	XOR       // ^
	SHL       // <<
	SHR       // >>
	INC       // ++
	APPENDEQ  // ++=
	PLUSEQ    // +=
	MINUSEQ   // -=
	STAREQ    // *=
	SLASHEQ   // /=
	PERCENTEQ // %=
	RANGE     // ..
	PIPE      // |>
	ARROW     // ->
	DCOLON    // ::
	QUESTION  // ?
	AT        // @
	TILDE     // ~
	DOTDOTDOT // ... (varargs)

	// Delimiters
	LPAREN   // (
	RPAREN   // )
	LBRACE   // {
	RBRACE   // }
	LBRACKET // [
	RBRACKET // ]
	COLON    // :
	SEMI     // ;
	COMMA    // ,
	DOT      // .
	HASH     // # (standalone, for tags parsed specially)

	// Tag (e.g. #pure, #no_recurse, #sideffect)
	CONTROL_TAG // #ident

	// Indentation
	NEWLINE
	INDENT
	DEDENT

	EOF
	ILLEGAL
)

var tokenNames = map[TokenType]string{
	INT_LIT: "INT", FLOAT_LIT: "FLOAT", STRING_LIT: "STRING", BACKTICK_LIT: "BACKTICK",
	CHAR_LIT: "CHAR", BOOL_LIT: "BOOL", ATOM_LIT: "ATOM", NONE_LIT: "NONE",
	IDENT:  "IDENT",
	KW_LET: "let", KW_CONST: "const", KW_FN: "fn", KW_TYPE: "type",
	KW_STRUCT: "struct", KW_TRAIT: "trait", KW_ENUM: "enum", KW_UNION: "union",
	KW_DATA: "data", KW_USE: "use", KW_EXPORT: "export", KW_EXTERN: "extern",
	KW_RETURN: "return", KW_IF: "if", KW_ELSE: "else", KW_FOR: "for",
	KW_IN: "in", KW_MATCH: "match", KW_CASE: "case", KW_DEFAULT: "default",
	KW_DEFER: "defer", KW_WHERE: "where", KW_MACRO: "macro",
	KW_STATIC: "static", KW_VIRTUAL: "virtual", KW_AS: "as", KW_IS: "is",
	KW_FORWARD: "forward", KW_OVERRIDE: "override", KW_SIZEOF: "sizeof",
	KW_ADDR: "addr", KW_BREAK: "break", KW_ECHO: "echo", KW_TEST: "test",
	KW_TYPEOF: "typeof", KW_TRAITOF: "traitof", KW_FIELDNAMES: "fieldnames",
	KW_FIELDTYPES: "fieldtypes", KW_FIELDTAG: "fieldtag",
	KW_GETFIELD: "getfield", KW_SETFIELD: "setfield", KW_PASS: "pass",
	PLUS: "+", MINUS: "-", STAR: "*", SLASH: "/", PERCENT: "%",
	ASSIGN: "=", EQEQ: "==", NEQ: "!=", LT: "<", LTEQ: "<=", GT: ">", GTEQ: ">=",
	AND: "&&", OR: "||", NOT: "!", AMP: "&", BITOR: "|", XOR: "^",
	SHL: "<<", SHR: ">>", INC: "++", APPENDEQ: "++=", PLUSEQ: "+=",
	MINUSEQ: "-=", STAREQ: "*=", SLASHEQ: "/=", PERCENTEQ: "%=",
	RANGE: "..", PIPE: "|>", ARROW: "->", DCOLON: "::", QUESTION: "?",
	AT: "@", TILDE: "~", DOTDOTDOT: "...",
	LPAREN: "(", RPAREN: ")", LBRACE: "{", RBRACE: "}", LBRACKET: "[", RBRACKET: "]",
	COLON: ":", SEMI: ";", COMMA: ",", DOT: ".", HASH: "#",
	CONTROL_TAG: "TAG",
	NEWLINE:     "NEWLINE", INDENT: "INDENT", DEDENT: "DEDENT",
	EOF: "EOF", ILLEGAL: "ILLEGAL",
}

func (t TokenType) String() string {
	if s, ok := tokenNames[t]; ok {
		return s
	}
	return fmt.Sprintf("TOKEN(%d)", int(t))
}

var keywords = map[string]TokenType{
	"let": KW_LET, "const": KW_CONST, "fn": KW_FN, "type": KW_TYPE,
	"struct": KW_STRUCT, "trait": KW_TRAIT, "enum": KW_ENUM, "union": KW_UNION,
	"data": KW_DATA, "use": KW_USE, "export": KW_EXPORT, "extern": KW_EXTERN,
	"return": KW_RETURN, "if": KW_IF, "else": KW_ELSE, "for": KW_FOR,
	"in": KW_IN, "match": KW_MATCH, "case": KW_CASE, "default": KW_DEFAULT,
	"defer": KW_DEFER, "where": KW_WHERE, "macro": KW_MACRO,
	"static": KW_STATIC, "virtual": KW_VIRTUAL, "as": KW_AS, "is": KW_IS,
	"forward": KW_FORWARD, "override": KW_OVERRIDE, "sizeof": KW_SIZEOF,
	"addr": KW_ADDR, "break": KW_BREAK, "echo": KW_ECHO, "test": KW_TEST,
	"typeof": KW_TYPEOF, "traitof": KW_TRAITOF, "fieldnames": KW_FIELDNAMES,
	"fieldtypes": KW_FIELDTYPES, "fieldtag": KW_FIELDTAG,
	"getfield": KW_GETFIELD, "setfield": KW_SETFIELD, "pass": KW_PASS,
	"true": BOOL_LIT, "false": BOOL_LIT, "None": NONE_LIT,
}

// Token is a single lexical unit
type Token struct {
	Type    TokenType
	Literal string
	Line    int
	Col     int
}

func (t Token) String() string {
	return fmt.Sprintf("Token(%s, %q, %d:%d)", t.Type, t.Literal, t.Line, t.Col)
}

// Lexer tokenizes tin source code
type Lexer struct {
	src         []rune
	pos         int
	line        int
	col         int
	indentStack []int
	tokens      []Token
	atLineStart bool
	lineIndent  int
	// dedenting is true when we are re-entering handleLineStart to emit
	// additional DEDENT tokens for the same line.  In that case the position
	// has already been advanced past the leading whitespace, so we must use
	// lineIndent instead of recounting
	dedenting bool
}

// New creates a new Lexer for the given source string
func New(src string) *Lexer {
	return &Lexer{
		src:         []rune(src),
		pos:         0,
		line:        1,
		col:         1,
		indentStack: []int{0},
		atLineStart: true,
		lineIndent:  0,
	}
}

// Tokenize returns all tokens from the source
func (l *Lexer) Tokenize() ([]Token, error) {
	for {
		tok, err := l.nextToken()
		if err != nil {
			return nil, err
		}
		l.tokens = append(l.tokens, tok)
		if tok.Type == EOF {
			break
		}
	}
	return l.tokens, nil
}

func (l *Lexer) peek() rune {
	if l.pos >= len(l.src) {
		return 0
	}
	return l.src[l.pos]
}

func (l *Lexer) peekAt(offset int) rune {
	i := l.pos + offset
	if i >= len(l.src) {
		return 0
	}
	return l.src[i]
}

func (l *Lexer) advance() rune {
	if l.pos >= len(l.src) {
		return 0
	}
	ch := l.src[l.pos]
	l.pos++
	if ch == '\n' {
		l.line++
		l.col = 1
	} else {
		l.col++
	}
	return ch
}

func (l *Lexer) nextToken() (Token, error) {
	// Handle indentation at line start
	if l.atLineStart {
		return l.handleLineStart()
	}

	// Skip inline whitespace
	for l.pos < len(l.src) && (l.src[l.pos] == ' ' || l.src[l.pos] == '\t') {
		l.advance()
	}

	if l.pos >= len(l.src) {
		// Emit remaining DEDENTs
		return l.handleEOF()
	}

	ch := l.peek()

	// Newline
	if ch == '\n' {
		l.advance()
		l.atLineStart = true
		l.lineIndent = 0
		return Token{Type: NEWLINE, Literal: "\n", Line: l.line - 1, Col: l.col}, nil
	}

	// Comments
	if ch == '/' && l.peekAt(1) == '/' {
		for l.pos < len(l.src) && l.peek() != '\n' {
			l.advance()
		}
		return l.nextToken()
	}
	if ch == '/' && l.peekAt(1) == '*' {
		l.advance() // consume /
		l.advance() // consume *
		for l.pos < len(l.src) {
			if l.peek() == '*' && l.peekAt(1) == '/' {
				l.advance() // consume *
				l.advance() // consume /
				break
			}
			if l.peek() == '\n' {
				l.line++
				l.col = 0
			}
			l.advance()
		}
		return l.nextToken()
	}

	line, col := l.line, l.col

	switch ch {
	case '"':
		return l.readString(line, col)
	case '\'':
		return l.readSingleQuote(line, col)
	case '#':
		return l.readControlTag(line, col)
	case '`':
		return l.readBacktick(line, col)
	}

	if ch == '.' && l.peekAt(1) == '.' && l.peekAt(2) == '.' {
		l.advance()
		l.advance()
		l.advance()
		return Token{Type: DOTDOTDOT, Literal: "...", Line: line, Col: col}, nil
	}
	if ch == '.' && l.peekAt(1) == '.' {
		l.advance()
		l.advance()
		return Token{Type: RANGE, Literal: "..", Line: line, Col: col}, nil
	}

	if unicode.IsDigit(ch) || (ch == '0' && (l.peekAt(1) == 'x' || l.peekAt(1) == 'X')) {
		return l.readNumber(line, col)
	}

	if unicode.IsLetter(ch) || ch == '_' {
		return l.readIdentOrKeyword(line, col)
	}

	return l.readOperatorOrDelim(line, col)
}

func (l *Lexer) handleLineStart() (Token, error) {
	// Count leading whitespace (skip recount when emitting additional DEDENTs)
	var indent int
	var startPos int
	if l.dedenting {
		// We already counted the indent for this line; reuse it
		indent = l.lineIndent
		startPos = l.pos
	} else {
		startPos = l.pos
		for l.pos < len(l.src) && (l.src[l.pos] == ' ' || l.src[l.pos] == '\t') {
			if l.src[l.pos] == '\t' {
				indent += 4 // treat tab as 4 spaces
			} else {
				indent++
			}
			l.pos++
			l.col++
		}
	}

	// Check for blank line or comment
	if l.pos >= len(l.src) {
		return l.handleEOF()
	}
	if l.src[l.pos] == '\n' {
		// Blank line - skip
		l.advance()
		// Reset
		l.pos = startPos + (l.pos - startPos)
		_ = startPos
		return l.nextToken()
	}
	if l.src[l.pos] == '/' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '/' {
		// Comment line - skip to end
		for l.pos < len(l.src) && l.src[l.pos] != '\n' {
			l.advance()
		}
		if l.pos < len(l.src) {
			l.advance() // consume \n
		}
		return l.nextToken()
	}
	if l.src[l.pos] == '/' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '*' {
		// Block comment - set atLineStart=false so we don't re-enter here
		l.atLineStart = false
		return l.nextToken()
	}

	wasDedenting := l.dedenting
	l.atLineStart = false
	l.dedenting = false
	if !wasDedenting {
		l.lineIndent = indent
	}
	top := l.indentStack[len(l.indentStack)-1]

	if indent > top {
		l.indentStack = append(l.indentStack, indent)
		return Token{Type: INDENT, Literal: "", Line: l.line, Col: 1}, nil
	} else if indent < top {
		// Pop stack until we match
		l.indentStack = l.indentStack[:len(l.indentStack)-1]
		// If still not matching, we'll emit more DEDENTs on subsequent calls
		if len(l.indentStack) == 0 || indent > l.indentStack[len(l.indentStack)-1] {
			// Inconsistent indentation
			l.indentStack = append(l.indentStack, indent)
		}
		// Re-enter handleLineStart to possibly emit more DEDENTs, but reuse
		// the already-computed indent so we don't recount past whitespace
		if indent < l.indentStack[len(l.indentStack)-1] {
			l.atLineStart = true
			l.dedenting = true
		}
		return Token{Type: DEDENT, Literal: "", Line: l.line, Col: 1}, nil
	}
	// indent == top: just continue tokenizing
	return l.nextToken()
}

func (l *Lexer) handleEOF() (Token, error) {
	// Emit remaining DEDENTs before EOF
	if len(l.indentStack) > 1 {
		l.indentStack = l.indentStack[:len(l.indentStack)-1]
		return Token{Type: DEDENT, Literal: "", Line: l.line, Col: l.col}, nil
	}
	return Token{Type: EOF, Literal: "", Line: l.line, Col: l.col}, nil
}

func (l *Lexer) readString(line, col int) (Token, error) {
	l.advance() // consume opening "
	var sb strings.Builder
	for l.pos < len(l.src) {
		ch := l.peek()
		if ch == '"' {
			l.advance()
			break
		}
		if ch == '\\' {
			l.advance()
			esc := l.advance()
			switch esc {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case 'r':
				sb.WriteByte('\r')
			case '"':
				sb.WriteByte('"')
			case '\\':
				sb.WriteByte('\\')
			case '0':
				sb.WriteByte(0)
			default:
				sb.WriteRune('\\')
				sb.WriteRune(esc)
			}
			continue
		}
		sb.WriteRune(l.advance())
	}
	return Token{Type: STRING_LIT, Literal: sb.String(), Line: line, Col: col}, nil
}

func (l *Lexer) readSingleQuote(line, col int) (Token, error) {
	l.advance() // consume '
	if l.pos >= len(l.src) {
		return Token{Type: ILLEGAL, Literal: "'", Line: line, Col: col}, nil
	}

	ch := l.peek()

	// '"..."  ->  quoted atom literal (for non-standard / user-defined names)
	// If the content is a plain identifier (letters, digits, underscores only),
	// the quotes are stripped and the atom is equivalent to the unquoted form:
	// '"hello"' == 'hello.  Complex contents (e.g. '"fn(i64)bool"') keep the
	// surrounding double-quotes in the literal so they remain distinct
	if ch == '"' {
		l.advance() // consume opening "
		var sb strings.Builder
		isSimple := true
		for l.pos < len(l.src) && l.peek() != '"' && l.peek() != '\n' {
			r := l.advance()
			sb.WriteRune(r)
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
				isSimple = false
			}
		}
		if l.pos < len(l.src) && l.peek() == '"' {
			l.advance() // consume closing "
		}
		name := sb.String()
		if !isSimple {
			// Keep surrounding double-quotes for complex atoms like '"fn(i64)bool"'
			name = "\"" + name + "\""
		}
		return Token{Type: ATOM_LIT, Literal: name, Line: line, Col: col}, nil
	}

	// If it's a letter or underscore - it's an atom
	// Simple atoms only allow letters, digits, and underscores (no special characters)
	// Use '"..."' quoted form for atoms containing special characters
	if unicode.IsLetter(ch) || ch == '_' {
		// Check: is it a single char followed by ' ? That would be a char literal
		// We need to look ahead: if next-next is ' it might be a 1-char string
		// But atoms like 'ok don't have a closing quote
		// Rule: peek ahead - if we find a non-ident char before finding ', it's ambiguous
		// Simpler: if after the char there's immediately a closing quote, it's a char literal
		nextCh := l.peekAt(1)
		if nextCh == '\'' {
			// char literal: 'x'
			c := l.advance() // consume the char
			l.advance()      // consume closing '
			return Token{Type: CHAR_LIT, Literal: string(c), Line: line, Col: col}, nil
		}
		// It's an atom - only letters, digits, and underscores allowed
		var sb strings.Builder
		for l.pos < len(l.src) && (unicode.IsLetter(l.peek()) || unicode.IsDigit(l.peek()) || l.peek() == '_') {
			sb.WriteRune(l.advance())
		}
		return Token{Type: ATOM_LIT, Literal: sb.String(), Line: line, Col: col}, nil
	}

	// Non-letter after ' - must be a char literal like ','
	c := l.advance()
	if l.pos < len(l.src) && l.peek() == '\'' {
		l.advance() // consume closing '
	}
	return Token{Type: CHAR_LIT, Literal: string(c), Line: line, Col: col}, nil
}

func (l *Lexer) readControlTag(line, col int) (Token, error) {
	l.advance() // consume #
	var sb strings.Builder
	for l.pos < len(l.src) && (unicode.IsLetter(l.peek()) || unicode.IsDigit(l.peek()) || l.peek() == '_') {
		sb.WriteRune(l.advance())
	}
	if sb.Len() == 0 {
		return Token{Type: HASH, Literal: "#", Line: line, Col: col}, nil
	}
	return Token{Type: CONTROL_TAG, Literal: sb.String(), Line: line, Col: col}, nil
}

func (l *Lexer) readBacktick(line, col int) (Token, error) {
	l.advance() // consume `
	var sb strings.Builder
	depth := 1
	for l.pos < len(l.src) {
		ch := l.advance()
		if ch == '`' {
			depth--
			if depth == 0 {
				break
			}
		}
		sb.WriteRune(ch)
	}
	return Token{Type: BACKTICK_LIT, Literal: sb.String(), Line: line, Col: col}, nil
}

func (l *Lexer) readNumber(line, col int) (Token, error) {
	var sb strings.Builder
	isFloat := false

	switch {
	case l.peek() == '0' && (l.peekAt(1) == 'x' || l.peekAt(1) == 'X'):
		sb.WriteRune(l.advance()) // 0
		sb.WriteRune(l.advance()) // x/X
		for l.pos < len(l.src) && isHexDigit(l.peek()) {
			sb.WriteRune(l.advance())
		}
		return Token{Type: INT_LIT, Literal: sb.String(), Line: line, Col: col}, nil

	case l.peek() == '0' && (l.peekAt(1) == 'b' || l.peekAt(1) == 'B'):
		sb.WriteRune(l.advance()) // 0
		sb.WriteRune(l.advance()) // b/B
		for l.pos < len(l.src) && (l.peek() == '0' || l.peek() == '1') {
			sb.WriteRune(l.advance())
		}
		return Token{Type: INT_LIT, Literal: sb.String(), Line: line, Col: col}, nil

	case l.peek() == '0' && (l.peekAt(1) == 'o' || l.peekAt(1) == 'O'):
		sb.WriteRune(l.advance()) // 0
		sb.WriteRune(l.advance()) // o/O
		for l.pos < len(l.src) && l.peek() >= '0' && l.peek() <= '7' {
			sb.WriteRune(l.advance())
		}
		return Token{Type: INT_LIT, Literal: sb.String(), Line: line, Col: col}, nil

	default:
		for l.pos < len(l.src) && unicode.IsDigit(l.peek()) {
			sb.WriteRune(l.advance())
		}
		if l.pos < len(l.src) && l.peek() == '.' && l.peekAt(1) != '.' {
			isFloat = true
			sb.WriteRune(l.advance()) // .
			for l.pos < len(l.src) && unicode.IsDigit(l.peek()) {
				sb.WriteRune(l.advance())
			}
		}
		if l.pos < len(l.src) && (l.peek() == 'e' || l.peek() == 'E') {
			isFloat = true
			sb.WriteRune(l.advance())
			if l.pos < len(l.src) && (l.peek() == '+' || l.peek() == '-') {
				sb.WriteRune(l.advance())
			}
			for l.pos < len(l.src) && unicode.IsDigit(l.peek()) {
				sb.WriteRune(l.advance())
			}
		}
	}
	if isFloat {
		return Token{Type: FLOAT_LIT, Literal: sb.String(), Line: line, Col: col}, nil
	}
	return Token{Type: INT_LIT, Literal: sb.String(), Line: line, Col: col}, nil
}

func isHexDigit(ch rune) bool {
	return unicode.IsDigit(ch) || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')
}

func (l *Lexer) readIdentOrKeyword(line, col int) (Token, error) {
	var sb strings.Builder
	for l.pos < len(l.src) && (unicode.IsLetter(l.peek()) || unicode.IsDigit(l.peek()) || l.peek() == '_') {
		sb.WriteRune(l.advance())
	}
	word := sb.String()
	if tt, ok := keywords[word]; ok {
		lit := word
		if tt == BOOL_LIT {
			lit = word // "true" or "false"
		}
		return Token{Type: tt, Literal: lit, Line: line, Col: col}, nil
	}
	return Token{Type: IDENT, Literal: word, Line: line, Col: col}, nil
}

func (l *Lexer) readOperatorOrDelim(line, col int) (Token, error) {
	ch := l.advance()
	switch ch {
	case '+':
		if l.peek() == '+' {
			l.advance()
			if l.peek() == '=' {
				l.advance()
				return Token{Type: APPENDEQ, Literal: "++=", Line: line, Col: col}, nil
			}
			return Token{Type: INC, Literal: "++", Line: line, Col: col}, nil
		}
		if l.peek() == '=' {
			l.advance()
			return Token{Type: PLUSEQ, Literal: "+=", Line: line, Col: col}, nil
		}
		return Token{Type: PLUS, Literal: "+", Line: line, Col: col}, nil
	case '-':
		if l.peek() == '>' {
			l.advance()
			return Token{Type: ARROW, Literal: "->", Line: line, Col: col}, nil
		}
		if l.peek() == '-' {
			l.advance()
			return Token{Type: INC, Literal: "--", Line: line, Col: col}, nil
		}
		if l.peek() == '=' {
			l.advance()
			return Token{Type: MINUSEQ, Literal: "-=", Line: line, Col: col}, nil
		}
		return Token{Type: MINUS, Literal: "-", Line: line, Col: col}, nil
	case '*':
		if l.peek() == '=' {
			l.advance()
			return Token{Type: STAREQ, Literal: "*=", Line: line, Col: col}, nil
		}
		return Token{Type: STAR, Literal: "*", Line: line, Col: col}, nil
	case '/':
		if l.peek() == '=' {
			l.advance()
			return Token{Type: SLASHEQ, Literal: "/=", Line: line, Col: col}, nil
		}
		return Token{Type: SLASH, Literal: "/", Line: line, Col: col}, nil
	case '%':
		if l.peek() == '=' {
			l.advance()
			return Token{Type: PERCENTEQ, Literal: "%=", Line: line, Col: col}, nil
		}
		return Token{Type: PERCENT, Literal: "%", Line: line, Col: col}, nil
	case '=':
		if l.peek() == '=' {
			l.advance()
			return Token{Type: EQEQ, Literal: "==", Line: line, Col: col}, nil
		}
		return Token{Type: ASSIGN, Literal: "=", Line: line, Col: col}, nil
	case '!':
		if l.peek() == '=' {
			l.advance()
			return Token{Type: NEQ, Literal: "!=", Line: line, Col: col}, nil
		}
		return Token{Type: NOT, Literal: "!", Line: line, Col: col}, nil
	case '<':
		if l.peek() == '=' {
			l.advance()
			return Token{Type: LTEQ, Literal: "<=", Line: line, Col: col}, nil
		}
		if l.peek() == '<' {
			l.advance()
			return Token{Type: SHL, Literal: "<<", Line: line, Col: col}, nil
		}
		return Token{Type: LT, Literal: "<", Line: line, Col: col}, nil
	case '>':
		if l.peek() == '=' {
			l.advance()
			return Token{Type: GTEQ, Literal: ">=", Line: line, Col: col}, nil
		}
		if l.peek() == '>' {
			l.advance()
			return Token{Type: SHR, Literal: ">>", Line: line, Col: col}, nil
		}
		return Token{Type: GT, Literal: ">", Line: line, Col: col}, nil
	case '&':
		if l.peek() == '&' {
			l.advance()
			return Token{Type: AND, Literal: "&&", Line: line, Col: col}, nil
		}
		return Token{Type: AMP, Literal: "&", Line: line, Col: col}, nil
	case '|':
		if l.peek() == '|' {
			l.advance()
			return Token{Type: OR, Literal: "||", Line: line, Col: col}, nil
		}
		if l.peek() == '>' {
			l.advance()
			return Token{Type: PIPE, Literal: "|>", Line: line, Col: col}, nil
		}
		return Token{Type: BITOR, Literal: "|", Line: line, Col: col}, nil
	case '^':
		return Token{Type: XOR, Literal: "^", Line: line, Col: col}, nil
	case '~':
		return Token{Type: TILDE, Literal: "~", Line: line, Col: col}, nil
	case '?':
		return Token{Type: QUESTION, Literal: "?", Line: line, Col: col}, nil
	case '@':
		return Token{Type: AT, Literal: "@", Line: line, Col: col}, nil
	case '.':
		if l.peek() == '.' {
			l.advance()
			if l.peek() == '.' {
				l.advance()
				return Token{Type: DOTDOTDOT, Literal: "...", Line: line, Col: col}, nil
			}
			return Token{Type: RANGE, Literal: "..", Line: line, Col: col}, nil
		}
		return Token{Type: DOT, Literal: ".", Line: line, Col: col}, nil
	case ':':
		if l.peek() == ':' {
			l.advance()
			return Token{Type: DCOLON, Literal: "::", Line: line, Col: col}, nil
		}
		return Token{Type: COLON, Literal: ":", Line: line, Col: col}, nil
	case '(':
		return Token{Type: LPAREN, Literal: "(", Line: line, Col: col}, nil
	case ')':
		return Token{Type: RPAREN, Literal: ")", Line: line, Col: col}, nil
	case '{':
		return Token{Type: LBRACE, Literal: "{", Line: line, Col: col}, nil
	case '}':
		return Token{Type: RBRACE, Literal: "}", Line: line, Col: col}, nil
	case '[':
		return Token{Type: LBRACKET, Literal: "[", Line: line, Col: col}, nil
	case ']':
		return Token{Type: RBRACKET, Literal: "]", Line: line, Col: col}, nil
	case ';':
		return Token{Type: SEMI, Literal: ";", Line: line, Col: col}, nil
	case ',':
		return Token{Type: COMMA, Literal: ",", Line: line, Col: col}, nil
	}

	return Token{Type: ILLEGAL, Literal: string(ch), Line: line, Col: col}, fmt.Errorf("unexpected character %q at %d:%d", ch, line, col)
}

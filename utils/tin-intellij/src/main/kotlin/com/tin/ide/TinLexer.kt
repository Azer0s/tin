package com.tin.ide

import com.intellij.lexer.LexerBase
import com.intellij.psi.TokenType
import com.intellij.psi.tree.IElementType

/**
 * Hand-written stateful lexer for Tin.
 *
 * Incremental state encoding (returned by getState / restored by start):
 *
 *   bits 0-3  : string nesting depth  (how many `"` have been opened without a closing `"`)
 *   bits 4-11 : brace depth inside the current interpolation level
 *               > 0 means we are currently scanning code inside `{...}` interpolation
 *   bit  12   : 1 = currently inside a block comment  /* ... */
 *
 * Context hints (HINT_*) drive identifier colouring but are NOT part of the
 * incremental state - they reset to HINT_NONE whenever a new segment starts.
 * The worst-case consequence is one mis-coloured token at the start of an
 * incremental re-lex segment, which is acceptable for a highlighter-only lexer.
 */
class TinLexer : LexerBase() {

    companion object {
        private fun encodeState(strDepth: Int, interpDepth: Int, inBlockComment: Boolean) =
            (strDepth and 0xF) or ((interpDepth and 0xFF) shl 4) or (if (inBlockComment) (1 shl 12) else 0)
        private fun strDepth(state: Int)        = state and 0xF
        private fun interpDepth(state: Int)     = (state ushr 4) and 0xFF
        private fun inBlockComment(state: Int)  = (state ushr 12) and 1 != 0

        private const val HINT_NONE           = 0
        private const val HINT_AFTER_FN       = 1
        private const val HINT_AFTER_TYPE_KW  = 2
        private const val HINT_AFTER_COLONCOLON = 3
    }

    private var buffer: CharSequence = ""
    private var bufferEnd  = 0
    private var tokenStart = 0
    private var tokenEnd   = 0
    private var tokenType: IElementType? = null

    private var inBlockComment = false
    /** How many `"` have been opened without a matching `"`. */
    private var strDepth = 0

    /**
     * When > 0 we are scanning normal code inside a string interpolation.
     * The value is the net brace depth: it starts at 1 when we enter `{`,
     * increments on `{`, and decrements on `}`. Reaching 0 closes the
     * interpolation and we return to string-content scanning.
     */
    private var interpDepth = 0

    // Context hints for identifier classification (not encoded in state)
    private var contextHint  = HINT_NONE
    private var contextDepth = 0

    //  Lexer infrastructure

    override fun start(buffer: CharSequence, startOffset: Int, endOffset: Int, initialState: Int) {
        this.buffer     = buffer
        this.bufferEnd  = endOffset
        this.tokenStart = startOffset
        this.tokenEnd   = startOffset
        this.tokenType  = null
        this.inBlockComment = inBlockComment(initialState)
        this.strDepth    = strDepth(initialState)
        this.interpDepth = interpDepth(initialState)
        this.contextHint  = HINT_NONE
        this.contextDepth = 0
        advance()
    }

    override fun getState(): Int = encodeState(strDepth, interpDepth, inBlockComment)
    override fun getTokenType(): IElementType? = tokenType
    override fun getTokenStart(): Int = tokenStart
    override fun getTokenEnd(): Int   = tokenEnd
    override fun getBufferSequence(): CharSequence = buffer
    override fun getBufferEnd(): Int  = bufferEnd

    override fun advance() {
        tokenStart = tokenEnd
        if (tokenStart >= bufferEnd) { tokenType = null; return }
        tokenType = when {
            inBlockComment                   -> scanBlockComment()
            strDepth > 0 && interpDepth == 0 -> scanStringContent()
            else                             -> scanNormal()
        }
    }

    private fun char(pos: Int): Char = if (pos < bufferEnd) buffer[pos] else '\u0000'

    //  Block comment scanning

    private fun scanBlockComment(): IElementType {
        // tokenStart is already inside the comment (past the opening `/*`)
        while (tokenEnd < bufferEnd) {
            if (char(tokenEnd) == '*' && char(tokenEnd + 1) == '/') {
                tokenEnd += 2
                inBlockComment = false
                break
            }
            tokenEnd++
        }
        return TinTokenTypes.BLOCK_COMMENT
    }

    //  String-content scanning

    private fun scanStringContent(): IElementType {
        tokenEnd = tokenStart + 1
        val c = char(tokenStart)

        // Closing quote
        if (c == '"') {
            strDepth--
            return TinTokenTypes.STRING_END
        }

        // Interpolation start: `{`
        // Only treat { as interpolation if there is a matching } before the next unescaped ".
        // This mirrors the parser behaviour: an unclosed { is treated as literal text.
        if (c == '{' && hasInterpolationClose(tokenStart + 1)) {
            interpDepth = 1   // enter interpolation; strDepth stays until STRING_INTERP_END
            return TinTokenTypes.STRING_INTERP_START
        }

        // Escape sequence
        if (c == '\\') {
            val next = char(tokenStart + 1)
            if (next == 'n' || next == 'r' || next == 't' || next == '\\' ||
                next == '"' || next == '\'' || next == '0' ||
                next == '{' || next == '}') {  // \{ and \} are literal braces, not interpolation
                tokenEnd = tokenStart + 2
            }
            return TinTokenTypes.STRING_ESCAPE
        }

        // Plain string content - consume until a special char
        while (tokenEnd < bufferEnd) {
            val ch = char(tokenEnd)
            if (ch == '"' || ch == '{' || ch == '\\') break
            tokenEnd++
        }
        return TinTokenTypes.STRING_CONTENT
    }

    //  Normal / interpolation scanning

    private fun scanNormal(): IElementType {
        val c = char(tokenStart)
        tokenEnd = tokenStart + 1

        // Whitespace - never touches contextHint
        if (c.isWhitespace()) {
            while (tokenEnd < bufferEnd && char(tokenEnd).isWhitespace()) tokenEnd++
            return TinTokenTypes.WHITESPACE
        }

        // Line comment
        if (c == '/' && char(tokenStart + 1) == '/') {
            tokenEnd = tokenStart + 2
            while (tokenEnd < bufferEnd && char(tokenEnd) != '\n') tokenEnd++
            return TinTokenTypes.LINE_COMMENT
        }

        // Block comment
        if (c == '/' && char(tokenStart + 1) == '*') {
            tokenEnd = tokenStart + 2
            inBlockComment = true
            return scanBlockComment()
        }

        // String start (also valid inside interpolation)
        if (c == '"') {
            strDepth++
            contextHint = HINT_NONE
            return TinTokenTypes.STRING_START
        }

        // Backtick literal (no interpolation)
        if (c == '`') {
            while (tokenEnd < bufferEnd && char(tokenEnd) != '`') tokenEnd++
            if (tokenEnd < bufferEnd) tokenEnd++
            contextHint = HINT_NONE
            return TinTokenTypes.BACKTICK
        }

        // Atom or char literal
        if (c == '\'') {
            contextHint = HINT_NONE
            val c1 = char(tokenStart + 1)
            // Quoted atom: '"content"  (content may contain special chars)
            if (c1 == '"') {
                tokenEnd = tokenStart + 2
                while (tokenEnd < bufferEnd && char(tokenEnd) != '"' && char(tokenEnd) != '\n') tokenEnd++
                if (tokenEnd < bufferEnd && char(tokenEnd) == '"') tokenEnd++ // consume closing "
                return TinTokenTypes.ATOM
            }
            // Escaped char literal: '\n', '\t', '\r', '\\', '\'', '\0', '"'
            if (c1 == '\\' && char(tokenStart + 3) == '\'') {
                tokenEnd = tokenStart + 4; return TinTokenTypes.CHAR_LITERAL
            }
            // Plain char literal: 'x'
            if (c1 != '\'' && char(tokenStart + 2) == '\'') {
                tokenEnd = tokenStart + 3; return TinTokenTypes.CHAR_LITERAL
            }
            // Simple atom: 'identifier
            if (c1.isLetter() || c1 == '_') {
                tokenEnd = tokenStart + 2
                while (tokenEnd < bufferEnd && (char(tokenEnd).isLetterOrDigit() || char(tokenEnd) == '_')) tokenEnd++
                return TinTokenTypes.ATOM
            }
            return TokenType.BAD_CHARACTER
        }

        // Control tag - intentionally does NOT clear contextHint
        // so `fn{#inline} name` keeps HINT_AFTER_FN alive.
        if (c == '#') {
            val c1 = char(tokenStart + 1)
            if (c1.isLetter() || c1 == '_') {
                tokenEnd = tokenStart + 2
                while (tokenEnd < bufferEnd && (char(tokenEnd).isLetterOrDigit() || char(tokenEnd) == '_')) tokenEnd++
                return TinTokenTypes.CONTROL_TAG
            }
            contextHint = HINT_NONE
            return TokenType.BAD_CHARACTER
        }

        // Numbers
        if (c.isDigit()) {
            contextHint = HINT_NONE
            return scanNumber(c)
        }

        // Identifiers and keywords
        if (c.isLetter() || c == '_') {
            while (tokenEnd < bufferEnd && (char(tokenEnd).isLetterOrDigit() || char(tokenEnd) == '_')) tokenEnd++
            return classifyWord(buffer.substring(tokenStart, tokenEnd))
        }

        return scanSymbol(c)
    }

    private fun scanNumber(c: Char): IElementType {
        val c1 = char(tokenStart + 1)
        if (c == '0') when {
            c1 == 'x' || c1 == 'X' -> {
                tokenEnd = tokenStart + 2
                while (tokenEnd < bufferEnd && (char(tokenEnd).isHexDigit() || char(tokenEnd) == '_')) tokenEnd++
                return TinTokenTypes.NUMBER
            }
            c1 == 'b' || c1 == 'B' -> {
                tokenEnd = tokenStart + 2
                while (tokenEnd < bufferEnd && (char(tokenEnd) == '0' || char(tokenEnd) == '1' || char(tokenEnd) == '_')) tokenEnd++
                return TinTokenTypes.NUMBER
            }
            c1 == 'o' || c1 == 'O' -> {
                tokenEnd = tokenStart + 2
                while (tokenEnd < bufferEnd && (char(tokenEnd) in '0'..'7' || char(tokenEnd) == '_')) tokenEnd++
                return TinTokenTypes.NUMBER
            }
        }
        while (tokenEnd < bufferEnd && (char(tokenEnd).isDigit() || char(tokenEnd) == '_')) tokenEnd++
        if (char(tokenEnd) == '.' && char(tokenEnd + 1).isDigit()) {
            tokenEnd++
            while (tokenEnd < bufferEnd && (char(tokenEnd).isDigit() || char(tokenEnd) == '_')) tokenEnd++
            val e = char(tokenEnd)
            if (e == 'e' || e == 'E') {
                tokenEnd++
                if (char(tokenEnd) == '+' || char(tokenEnd) == '-') tokenEnd++
                while (tokenEnd < bufferEnd && char(tokenEnd).isDigit()) tokenEnd++
            }
        }
        return TinTokenTypes.NUMBER
    }

    //  Identifier / keyword classification

    private fun classifyWord(word: String): IElementType {
        val kwToken = keywordToken(word)
        if (kwToken != null) {
            contextHint = when (word) {
                "fn"                                         -> { contextDepth = 0; HINT_AFTER_FN }
                "struct", "trait", "enum", "union", "type"  -> HINT_AFTER_TYPE_KW
                "atom" -> if (contextHint == HINT_AFTER_TYPE_KW) HINT_AFTER_TYPE_KW else HINT_NONE
                else   -> HINT_NONE
            }
            return kwToken
        }

        val hint = contextHint
        contextHint = HINT_NONE

        if (hint == HINT_AFTER_FN && contextDepth == 0)  return TinTokenTypes.FUNCTION_DECL
        if (hint == HINT_AFTER_TYPE_KW)                   return TinTokenTypes.TYPE_NAME
        if (hint == HINT_AFTER_COLONCOLON) {
            val (n) = peek2(tokenEnd)
            return if (n == '(') TinTokenTypes.FUNCTION_CALL else TinTokenTypes.NAMESPACE_ITEM
        }

        val (n1, n2) = peek2(tokenEnd)
        return when {
            n1 == '('               -> TinTokenTypes.FUNCTION_CALL
            n1 == '!'               -> TinTokenTypes.FUNCTION_CALL  // macro!()
            n1 == ':' && n2 == ':'  -> TinTokenTypes.NAMESPACE
            else                    -> TinTokenTypes.IDENTIFIER
        }
    }

    private fun keywordToken(word: String): IElementType? = when (word) {
        "if", "else", "for", "in", "match", "case", "default",
        "break", "return", "do", "defer", "where", "yield", "await", "spawn"
            -> TinTokenTypes.KEYWORD_CONTROL
        "fn" -> TinTokenTypes.KEYWORD_FN
        "let", "const", "var", "struct", "trait", "enum", "union", "macro",
        "extern", "export", "use", "from", "forward", "override", "static", "virtual", "type", "weak"
            -> TinTokenTypes.KEYWORD_DECL
        "as", "is", "sizeof", "addr", "typeof", "traitof",
        "fieldnames", "fieldtypes", "fieldtag", "getfield", "setfield", "isrc"
            -> TinTokenTypes.KEYWORD_EXPR
        "echo", "test", "pass" -> TinTokenTypes.KEYWORD_OTHER
        "i8", "i16", "i32", "i64", "u8", "u16", "u32", "u64",
        "f32", "f64", "bool", "string", "atom", "void", "any"
            -> TinTokenTypes.BUILTIN_TYPE
        "true", "false" -> TinTokenTypes.BOOLEAN
        "nil"           -> TinTokenTypes.NIL
        else            -> null
    }

    //  Symbol scanning

    private fun scanSymbol(c: Char): IElementType {
        val n1 = char(tokenStart + 1)
        val n2 = char(tokenStart + 2)
        return when (c) {
            '{' -> {
                if (interpDepth > 0) interpDepth++           // nested brace inside interpolation
                if (contextHint == HINT_AFTER_FN) contextDepth++
                TinTokenTypes.LBRACE
            }
            '}' -> {
                if (interpDepth > 0) {
                    interpDepth--
                    if (interpDepth == 0) {
                        // Close string interpolation - return to string-content mode
                        return TinTokenTypes.STRING_INTERP_END
                    }
                }
                if (contextHint == HINT_AFTER_FN && contextDepth > 0) contextDepth--
                TinTokenTypes.RBRACE
            }
            '[' -> {
                if (contextHint == HINT_AFTER_FN) contextDepth++
                TinTokenTypes.LBRACKET
            }
            ']' -> {
                if (contextHint == HINT_AFTER_FN && contextDepth > 0) contextDepth--
                TinTokenTypes.RBRACKET
            }
            '(' -> TinTokenTypes.LPAREN
            ')' -> TinTokenTypes.RPAREN
            ',', ';' -> TinTokenTypes.PUNCTUATION
            ':' -> {
                if (n1 == ':') {
                    tokenEnd++; contextHint = HINT_AFTER_COLONCOLON; TinTokenTypes.PUNCTUATION
                } else {
                    contextHint = HINT_NONE; TinTokenTypes.PUNCTUATION
                }
            }
            '.' -> {
                contextHint = HINT_NONE
                when {
                    n1 == '.' && n2 == '.' -> { tokenEnd += 2; TinTokenTypes.OPERATOR }
                    n1 == '.'              -> { tokenEnd++;    TinTokenTypes.OPERATOR }
                    else                   ->                  TinTokenTypes.PUNCTUATION
                }
            }
            '+' -> {
                contextHint = HINT_NONE
                when {
                    n1 == '+' && n2 == '=' -> { tokenEnd += 2; TinTokenTypes.OPERATOR }
                    n1 == '+' || n1 == '=' -> { tokenEnd++;    TinTokenTypes.OPERATOR }
                    else                   ->                  TinTokenTypes.OPERATOR
                }
            }
            '-' -> { contextHint = HINT_NONE; if (n1 == '>' || n1 == '=') tokenEnd++; TinTokenTypes.OPERATOR }
            '*' -> { contextHint = HINT_NONE; if (n1 == '=') tokenEnd++; TinTokenTypes.OPERATOR }
            '/' -> { contextHint = HINT_NONE; if (n1 == '=') tokenEnd++; TinTokenTypes.OPERATOR }
            '%' -> { contextHint = HINT_NONE; if (n1 == '=') tokenEnd++; TinTokenTypes.OPERATOR }
            '=' -> { contextHint = HINT_NONE; if (n1 == '=') tokenEnd++; TinTokenTypes.OPERATOR }
            '!' -> { contextHint = HINT_NONE; if (n1 == '=') tokenEnd++; TinTokenTypes.OPERATOR }
            '<' -> { contextHint = HINT_NONE; if (n1 == '=' || n1 == '<') tokenEnd++; TinTokenTypes.OPERATOR }
            '>' -> { contextHint = HINT_NONE; if (n1 == '=' || n1 == '>') tokenEnd++; TinTokenTypes.OPERATOR }
            '&' -> { contextHint = HINT_NONE; if (n1 == '&') tokenEnd++; TinTokenTypes.OPERATOR }
            '|' -> { contextHint = HINT_NONE; if (n1 == '|' || n1 == '>') tokenEnd++; TinTokenTypes.OPERATOR }
            '^', '~', '@', '?' -> { contextHint = HINT_NONE; TinTokenTypes.OPERATOR }
            else -> { contextHint = HINT_NONE; TokenType.BAD_CHARACTER }
        }
    }

    //  Helpers

    /**
     * Returns true if there is a matching `}` before the next `"` or newline.
     * Used to distinguish a real interpolation `{expr}` from a bare `{` that the
     * parser treats as literal text (e.g. the string `"{"` in json.tin).
     */
    private fun hasInterpolationClose(from: Int): Boolean {
        var i = from
        var depth = 1
        while (i < bufferEnd) {
            when (char(i)) {
                '\\' -> i++              // skip escaped char
                '{'  -> depth++
                '}'  -> { depth--; if (depth == 0) return true }
                '"', '\n' -> return false
            }
            i++
        }
        return false
    }

    /** First two non-horizontal-whitespace chars starting at [from]. */
    private fun peek2(from: Int): Pair<Char, Char> {
        var i = from
        while (i < bufferEnd && (buffer[i] == ' ' || buffer[i] == '\t')) i++
        return Pair(if (i < bufferEnd) buffer[i] else '\u0000',
                    if (i + 1 < bufferEnd) buffer[i + 1] else '\u0000')
    }

    private fun Char.isHexDigit() = this in '0'..'9' || this in 'a'..'f' || this in 'A'..'F'
}

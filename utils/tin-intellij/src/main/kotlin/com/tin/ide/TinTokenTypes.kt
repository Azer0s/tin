package com.tin.ide

import com.intellij.psi.TokenType
import com.intellij.psi.tree.IElementType

object TinTokenTypes {
    @JvmField val LINE_COMMENT        = IElementType("LINE_COMMENT",        TinLanguage)
    @JvmField val BLOCK_COMMENT       = IElementType("BLOCK_COMMENT",       TinLanguage)
    @JvmField val WHITESPACE          = TokenType.WHITE_SPACE
    @JvmField val BAD_CHARACTER       = TokenType.BAD_CHARACTER

    // String tokens (split so interpolation/escapes can be coloured separately)
    @JvmField val STRING_START        = IElementType("STRING_START",        TinLanguage) // opening "
    @JvmField val STRING_CONTENT      = IElementType("STRING_CONTENT",      TinLanguage) // plain chars
    @JvmField val STRING_ESCAPE       = IElementType("STRING_ESCAPE",       TinLanguage) // \n \t \\ etc.
    @JvmField val STRING_INTERP_START = IElementType("STRING_INTERP_START", TinLanguage) // { inside "
    @JvmField val STRING_INTERP_END   = IElementType("STRING_INTERP_END",   TinLanguage) // } closes interp
    @JvmField val STRING_END          = IElementType("STRING_END",          TinLanguage) // closing "

    @JvmField val BACKTICK            = IElementType("BACKTICK",            TinLanguage)
    @JvmField val ATOM                = IElementType("ATOM",                TinLanguage)
    @JvmField val CHAR_LITERAL        = IElementType("CHAR_LITERAL",        TinLanguage)
    @JvmField val NUMBER              = IElementType("NUMBER",              TinLanguage)
    @JvmField val CONTROL_TAG         = IElementType("CONTROL_TAG",         TinLanguage)

    // Keywords
    @JvmField val KEYWORD_CONTROL     = IElementType("KEYWORD_CONTROL",     TinLanguage)
    @JvmField val KEYWORD_FN          = IElementType("KEYWORD_FN",          TinLanguage)
    @JvmField val KEYWORD_DECL        = IElementType("KEYWORD_DECL",        TinLanguage)
    @JvmField val KEYWORD_EXPR        = IElementType("KEYWORD_EXPR",        TinLanguage)
    @JvmField val KEYWORD_OTHER       = IElementType("KEYWORD_OTHER",       TinLanguage)

    // Types and constants
    @JvmField val BUILTIN_TYPE        = IElementType("BUILTIN_TYPE",        TinLanguage)
    @JvmField val BOOLEAN             = IElementType("BOOLEAN",             TinLanguage)
    @JvmField val NIL                 = IElementType("NIL",                 TinLanguage)

    // Identifiers — context-sensitive
    @JvmField val IDENTIFIER          = IElementType("IDENTIFIER",          TinLanguage)
    @JvmField val FUNCTION_DECL       = IElementType("FUNCTION_DECL",       TinLanguage)
    @JvmField val FUNCTION_CALL       = IElementType("FUNCTION_CALL",       TinLanguage)
    @JvmField val TYPE_NAME           = IElementType("TYPE_NAME",           TinLanguage)
    @JvmField val NAMESPACE           = IElementType("NAMESPACE",           TinLanguage)
    @JvmField val NAMESPACE_ITEM      = IElementType("NAMESPACE_ITEM",      TinLanguage)

    // Operators / punctuation
    @JvmField val OPERATOR            = IElementType("OPERATOR",            TinLanguage)
    @JvmField val LBRACE              = IElementType("LBRACE",              TinLanguage)
    @JvmField val RBRACE              = IElementType("RBRACE",              TinLanguage)
    @JvmField val LBRACKET            = IElementType("LBRACKET",            TinLanguage)
    @JvmField val RBRACKET            = IElementType("RBRACKET",            TinLanguage)
    @JvmField val LPAREN              = IElementType("LPAREN",              TinLanguage)
    @JvmField val RPAREN              = IElementType("RPAREN",              TinLanguage)
    @JvmField val PUNCTUATION         = IElementType("PUNCTUATION",         TinLanguage)
}

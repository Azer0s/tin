package com.tin.ide

import com.intellij.lexer.Lexer
import com.intellij.openapi.editor.DefaultLanguageHighlighterColors
import com.intellij.openapi.editor.HighlighterColors
import com.intellij.openapi.editor.colors.TextAttributesKey
import com.intellij.openapi.fileTypes.SyntaxHighlighterBase
import com.intellij.psi.TokenType
import com.intellij.psi.tree.IElementType

class TinSyntaxHighlighter : SyntaxHighlighterBase() {

    companion object {
        val COMMENT          = key("TIN_COMMENT",          DefaultLanguageHighlighterColors.LINE_COMMENT)
        val BLOCK_COMMENT    = key("TIN_BLOCK_COMMENT",    DefaultLanguageHighlighterColors.BLOCK_COMMENT)

        // String parts
        val STRING           = key("TIN_STRING",           DefaultLanguageHighlighterColors.STRING)
        val STRING_ESCAPE    = key("TIN_STRING_ESCAPE",    DefaultLanguageHighlighterColors.VALID_STRING_ESCAPE)
        val STRING_INTERP    = key("TIN_STRING_INTERP",    DefaultLanguageHighlighterColors.TEMPLATE_LANGUAGE_COLOR)

        val NUMBER           = key("TIN_NUMBER",           DefaultLanguageHighlighterColors.NUMBER)
        val ATOM             = key("TIN_ATOM",             DefaultLanguageHighlighterColors.CONSTANT)
        val CONTROL_TAG      = key("TIN_CONTROL_TAG",      DefaultLanguageHighlighterColors.METADATA)

        val KEYWORD          = key("TIN_KEYWORD",          DefaultLanguageHighlighterColors.KEYWORD)
        val KEYWORD_FN       = key("TIN_KEYWORD_FN",       DefaultLanguageHighlighterColors.KEYWORD)
        val KEYWORD_DECL     = key("TIN_KEYWORD_DECL",     DefaultLanguageHighlighterColors.KEYWORD)
        val KEYWORD_EXPR     = key("TIN_KEYWORD_EXPR",     DefaultLanguageHighlighterColors.KEYWORD)
        val KEYWORD_OTHER    = key("TIN_KEYWORD_OTHER",    DefaultLanguageHighlighterColors.KEYWORD)

        val BUILTIN_TYPE     = key("TIN_BUILTIN_TYPE",     DefaultLanguageHighlighterColors.KEYWORD)
        val BOOLEAN          = key("TIN_BOOLEAN",          DefaultLanguageHighlighterColors.KEYWORD)
        val NIL              = key("TIN_NIL",              DefaultLanguageHighlighterColors.KEYWORD)

        val IDENTIFIER       = key("TIN_IDENTIFIER",       DefaultLanguageHighlighterColors.IDENTIFIER)
        val FUNCTION_DECL    = key("TIN_FUNCTION_DECL",    DefaultLanguageHighlighterColors.FUNCTION_DECLARATION)
        val FUNCTION_CALL    = key("TIN_FUNCTION_CALL",    DefaultLanguageHighlighterColors.FUNCTION_CALL)
        val MACRO_CALL       = key("TIN_MACRO_CALL",       DefaultLanguageHighlighterColors.METADATA)
        val TYPE_NAME        = key("TIN_TYPE_NAME",        DefaultLanguageHighlighterColors.CLASS_NAME)
        val NAMESPACE        = key("TIN_NAMESPACE",        DefaultLanguageHighlighterColors.CLASS_REFERENCE)
        val NAMESPACE_ITEM   = key("TIN_NAMESPACE_ITEM",   DefaultLanguageHighlighterColors.CLASS_REFERENCE)

        val OPERATOR         = key("TIN_OPERATOR",         DefaultLanguageHighlighterColors.OPERATION_SIGN)
        val BRACES           = key("TIN_BRACES",           DefaultLanguageHighlighterColors.BRACES)
        val BRACKETS         = key("TIN_BRACKETS",         DefaultLanguageHighlighterColors.BRACKETS)
        val PARENS           = key("TIN_PARENS",           DefaultLanguageHighlighterColors.PARENTHESES)
        val PUNCTUATION      = key("TIN_PUNCTUATION",      DefaultLanguageHighlighterColors.DOT)
        val BAD_CHAR         = key("TIN_BAD_CHAR",         HighlighterColors.BAD_CHARACTER)

        private fun key(name: String, fallback: TextAttributesKey) =
            TextAttributesKey.createTextAttributesKey(name, fallback)

        private val COMMENT_KEYS         = arrayOf(COMMENT)
        private val BLOCK_COMMENT_KEYS   = arrayOf(BLOCK_COMMENT)
        private val STRING_KEYS          = arrayOf(STRING)
        private val STRING_ESCAPE_KEYS   = arrayOf(STRING_ESCAPE)
        private val STRING_INTERP_KEYS   = arrayOf(STRING_INTERP)
        private val NUMBER_KEYS          = arrayOf(NUMBER)
        private val ATOM_KEYS            = arrayOf(ATOM)
        private val CONTROL_TAG_KEYS     = arrayOf(CONTROL_TAG)
        private val KEYWORD_KEYS         = arrayOf(KEYWORD)
        private val KEYWORD_FN_KEYS      = arrayOf(KEYWORD_FN)
        private val KEYWORD_DECL_KEYS    = arrayOf(KEYWORD_DECL)
        private val KEYWORD_EXPR_KEYS    = arrayOf(KEYWORD_EXPR)
        private val KEYWORD_OTHER_KEYS   = arrayOf(KEYWORD_OTHER)
        private val BUILTIN_TYPE_KEYS    = arrayOf(BUILTIN_TYPE)
        private val BOOLEAN_KEYS         = arrayOf(BOOLEAN)
        private val NIL_KEYS             = arrayOf(NIL)
        private val IDENTIFIER_KEYS      = emptyArray<TextAttributesKey>()
        private val FUNCTION_DECL_KEYS   = arrayOf(FUNCTION_DECL)
        private val FUNCTION_CALL_KEYS   = arrayOf(FUNCTION_CALL)
        private val MACRO_CALL_KEYS      = arrayOf(MACRO_CALL)
        private val TYPE_NAME_KEYS       = arrayOf(TYPE_NAME)
        private val NAMESPACE_KEYS       = arrayOf(NAMESPACE)
        private val NAMESPACE_ITEM_KEYS  = arrayOf(NAMESPACE_ITEM)
        private val OPERATOR_KEYS        = arrayOf(OPERATOR)
        private val BRACES_KEYS          = arrayOf(BRACES)
        private val BRACKETS_KEYS        = arrayOf(BRACKETS)
        private val PARENS_KEYS          = arrayOf(PARENS)
        private val PUNCTUATION_KEYS     = arrayOf(PUNCTUATION)
        private val BAD_CHAR_KEYS        = arrayOf(BAD_CHAR)
        private val EMPTY_KEYS           = emptyArray<TextAttributesKey>()
    }

    override fun getHighlightingLexer(): Lexer = TinLexer()

    override fun getTokenHighlights(type: IElementType): Array<TextAttributesKey> = when (type) {
        TinTokenTypes.LINE_COMMENT                       -> COMMENT_KEYS
        TinTokenTypes.BLOCK_COMMENT                      -> BLOCK_COMMENT_KEYS

        TinTokenTypes.STRING_START,
        TinTokenTypes.STRING_CONTENT,
        TinTokenTypes.STRING_END,
        TinTokenTypes.BACKTICK,
        TinTokenTypes.CHAR_LITERAL                       -> STRING_KEYS

        TinTokenTypes.STRING_ESCAPE                      -> STRING_ESCAPE_KEYS

        TinTokenTypes.STRING_INTERP_START,
        TinTokenTypes.STRING_INTERP_END                  -> STRING_INTERP_KEYS

        TinTokenTypes.NUMBER                             -> NUMBER_KEYS
        TinTokenTypes.ATOM                               -> ATOM_KEYS
        TinTokenTypes.CONTROL_TAG                        -> CONTROL_TAG_KEYS

        TinTokenTypes.KEYWORD_CONTROL                    -> KEYWORD_KEYS
        TinTokenTypes.KEYWORD_FN                         -> KEYWORD_FN_KEYS
        TinTokenTypes.KEYWORD_DECL                       -> KEYWORD_DECL_KEYS
        TinTokenTypes.KEYWORD_EXPR                       -> KEYWORD_EXPR_KEYS
        TinTokenTypes.KEYWORD_OTHER                      -> KEYWORD_OTHER_KEYS

        TinTokenTypes.BUILTIN_TYPE                       -> BUILTIN_TYPE_KEYS
        TinTokenTypes.BOOLEAN                            -> BOOLEAN_KEYS
        TinTokenTypes.NIL                                -> NIL_KEYS

        TinTokenTypes.IDENTIFIER                         -> IDENTIFIER_KEYS
        TinTokenTypes.FUNCTION_DECL                      -> FUNCTION_DECL_KEYS
        TinTokenTypes.FUNCTION_CALL                      -> FUNCTION_CALL_KEYS
        TinTokenTypes.TYPE_NAME                          -> TYPE_NAME_KEYS
        TinTokenTypes.NAMESPACE                          -> NAMESPACE_KEYS
        TinTokenTypes.NAMESPACE_ITEM                     -> NAMESPACE_ITEM_KEYS

        TinTokenTypes.OPERATOR                           -> OPERATOR_KEYS
        TinTokenTypes.LBRACE, TinTokenTypes.RBRACE       -> BRACES_KEYS
        TinTokenTypes.LBRACKET, TinTokenTypes.RBRACKET   -> BRACKETS_KEYS
        TinTokenTypes.LPAREN, TinTokenTypes.RPAREN       -> PARENS_KEYS
        TinTokenTypes.PUNCTUATION                        -> PUNCTUATION_KEYS
        TokenType.BAD_CHARACTER                          -> BAD_CHAR_KEYS
        else                                             -> EMPTY_KEYS
    }
}

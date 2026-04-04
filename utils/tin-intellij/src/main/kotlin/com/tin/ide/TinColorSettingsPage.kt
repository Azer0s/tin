package com.tin.ide

import com.intellij.openapi.editor.colors.TextAttributesKey
import com.intellij.openapi.fileTypes.SyntaxHighlighter
import com.intellij.openapi.options.colors.AttributesDescriptor
import com.intellij.openapi.options.colors.ColorDescriptor
import com.intellij.openapi.options.colors.ColorSettingsPage
import javax.swing.Icon

class TinColorSettingsPage : ColorSettingsPage {
    private val attributes = arrayOf(
        AttributesDescriptor("Comment//Line comment",                TinSyntaxHighlighter.COMMENT),
        AttributesDescriptor("Comment//Block comment",               TinSyntaxHighlighter.BLOCK_COMMENT),

        AttributesDescriptor("String//Text",                        TinSyntaxHighlighter.STRING),
        AttributesDescriptor("String//Escape sequence",             TinSyntaxHighlighter.STRING_ESCAPE),
        AttributesDescriptor("String//Interpolation braces",        TinSyntaxHighlighter.STRING_INTERP),

        AttributesDescriptor("Number",                              TinSyntaxHighlighter.NUMBER),
        AttributesDescriptor("Atom literal",                        TinSyntaxHighlighter.ATOM),
        AttributesDescriptor("Control tag",                         TinSyntaxHighlighter.CONTROL_TAG),

        AttributesDescriptor("Keyword//Control flow",               TinSyntaxHighlighter.KEYWORD),
        AttributesDescriptor("Keyword//fn",                         TinSyntaxHighlighter.KEYWORD_FN),
        AttributesDescriptor("Keyword//Declaration",                TinSyntaxHighlighter.KEYWORD_DECL),
        AttributesDescriptor("Keyword//Expression",                 TinSyntaxHighlighter.KEYWORD_EXPR),
        AttributesDescriptor("Keyword//Statement",                  TinSyntaxHighlighter.KEYWORD_OTHER),

        AttributesDescriptor("Type//Built-in type",                 TinSyntaxHighlighter.BUILTIN_TYPE),
        AttributesDescriptor("Type//User-defined type name",        TinSyntaxHighlighter.TYPE_NAME),
        AttributesDescriptor("Type//Boolean",                       TinSyntaxHighlighter.BOOLEAN),
        AttributesDescriptor("Type//Nil",                           TinSyntaxHighlighter.NIL),

        AttributesDescriptor("Identifier//Variable (plain)",        TinSyntaxHighlighter.IDENTIFIER),
        AttributesDescriptor("Identifier//Function declaration",    TinSyntaxHighlighter.FUNCTION_DECL),
        AttributesDescriptor("Identifier//Function call",           TinSyntaxHighlighter.FUNCTION_CALL),
        AttributesDescriptor("Identifier//Macro call",              TinSyntaxHighlighter.MACRO_CALL),
        AttributesDescriptor("Identifier//Namespace",               TinSyntaxHighlighter.NAMESPACE),
        AttributesDescriptor("Identifier//Namespace item",          TinSyntaxHighlighter.NAMESPACE_ITEM),

        AttributesDescriptor("Operator",                            TinSyntaxHighlighter.OPERATOR),
        AttributesDescriptor("Braces and operators//Brace",         TinSyntaxHighlighter.BRACES),
        AttributesDescriptor("Braces and operators//Bracket",       TinSyntaxHighlighter.BRACKETS),
        AttributesDescriptor("Braces and operators//Parenthesis",   TinSyntaxHighlighter.PARENS),
        AttributesDescriptor("Braces and operators//Dot and comma", TinSyntaxHighlighter.PUNCTUATION),
        AttributesDescriptor("Bad character",                       TinSyntaxHighlighter.BAD_CHAR),
    )

    override fun getIcon(): Icon = TinIcons.FILE
    override fun getHighlighter(): SyntaxHighlighter = TinSyntaxHighlighter()
    override fun getAttributeDescriptors(): Array<AttributesDescriptor> = attributes
    override fun getColorDescriptors(): Array<ColorDescriptor> = ColorDescriptor.EMPTY_ARRAY
    override fun getDisplayName(): String = "Tin"
    override fun getAdditionalHighlightingTagToDescriptorMap(): Map<String, TextAttributesKey>? = null

    override fun getDemoText(): String = """
        // line comment
        /* block comment */
        use stdlib::io

        struct point =
          x f64
          y f64

        fn{#inline} distance(a point, b point) f64 =
          let dx = a.x - b.x
          let dy = a.y - b.y
          return sqrt(dx * dx + dy * dy)

        fn main() void =
          let p = point{x: 1.0, y: 2.0}
          let msg = "coords: ({p.x}, {p.y})\n"
          let raw = "tab:\there\nnewline"
          echo msg
          let ok = 'ok
          let n i64 = 0xFF + 0b1010 + 42
          if n > 0 =
            echo stdlib::io::stderr
          let total = fold!(nums, 0, fn(a i32, b i32) i32 = a + b)
    """.trimIndent()
}

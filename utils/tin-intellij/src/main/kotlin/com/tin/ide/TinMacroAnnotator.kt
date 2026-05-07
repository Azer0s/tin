package com.tin.ide

import com.intellij.lang.annotation.AnnotationHolder
import com.intellij.lang.annotation.Annotator
import com.intellij.lang.annotation.HighlightSeverity
import com.intellij.openapi.util.TextRange
import com.intellij.psi.PsiElement
import com.intellij.psi.PsiFile
import com.intellij.psi.util.CachedValueProvider
import com.intellij.psi.util.CachedValuesManager

class TinMacroAnnotator : Annotator {

    override fun annotate(element: PsiElement, holder: AnnotationHolder) {
        val type = element.node?.elementType ?: return
        if (type != TinTokenTypes.IDENTIFIER && type != TinTokenTypes.FUNCTION_CALL) return

        val file     = element.containingFile ?: return
        val fileText = file.text
        val end      = element.textRange.endOffset

        // Skip horizontal whitespace - matches the lexer's peek2() behaviour
        var i = end
        while (i < fileText.length && (fileText[i] == ' ' || fileText[i] == '\t')) i++
        val next = if (i < fileText.length) fileText[i] else '\u0000'

        when {
            // name!(...) - macro invocation; colour the name. The trailing `!`
            // is its own PsiElement and lives outside this element's textRange,
            // so we cannot legally annotate it from here (IntelliJ's annotator
            // contract requires annotation ranges to be inside the element).
            next == '!' -> macro(element.textRange, holder)

            // name() - plain function call; lexer already gives it FUNCTION_CALL colour, nothing to do
            next == '(' -> return

            // name used standalone and it is a known import - colour as potential macro
            element.text in getImportedMacros(file) -> macro(element.textRange, holder)
        }
    }

    private fun macro(range: TextRange, holder: AnnotationHolder) {
        holder.newSilentAnnotation(HighlightSeverity.INFORMATION)
            .range(range)
            .textAttributes(TinSyntaxHighlighter.MACRO_CALL)
            .create()
    }

    companion object {
        private val useFromPattern = Regex("""use\s*\{([^}]*)\}\s*from""")

        fun getImportedMacros(file: PsiFile): Set<String> =
            CachedValuesManager.getCachedValue(file) {
                val result = mutableSetOf<String>()
                for (match in useFromPattern.findAll(file.text)) {
                    match.groupValues[1].split(",").forEach { raw ->
                        val name = raw.trim().trimEnd('!')
                        if (name.isNotEmpty()) result.add(name)
                    }
                }
                CachedValueProvider.Result.create(result as Set<String>, file)
            }
    }
}

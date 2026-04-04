package com.tin.ide

import com.intellij.extapi.psi.ASTWrapperPsiElement
import com.intellij.extapi.psi.PsiFileBase
import com.intellij.lang.ASTNode
import com.intellij.lang.ParserDefinition
import com.intellij.lang.PsiParser
import com.intellij.lexer.Lexer
import com.intellij.openapi.project.Project
import com.intellij.psi.FileViewProvider
import com.intellij.psi.PsiElement
import com.intellij.psi.PsiFile
import com.intellij.psi.tree.IFileElementType
import com.intellij.psi.tree.TokenSet

class TinParserDefinition : ParserDefinition {

    companion object {
        val FILE     = IFileElementType(TinLanguage)
        val COMMENTS = TokenSet.create(TinTokenTypes.LINE_COMMENT, TinTokenTypes.BLOCK_COMMENT)
        val STRINGS  = TokenSet.create(
            TinTokenTypes.STRING_START,
            TinTokenTypes.STRING_CONTENT,
            TinTokenTypes.STRING_END
        )
    }

    override fun createLexer(project: Project?): Lexer = TinLexer()

    /** Flat parser: all tokens become direct children of the file root. */
    override fun createParser(project: Project?): PsiParser = PsiParser { root, builder ->
        val marker = builder.mark()
        while (!builder.eof()) builder.advanceLexer()
        marker.done(root)
        builder.treeBuilt
    }

    override fun getFileNodeType(): IFileElementType = FILE

    override fun getCommentTokens(): TokenSet = COMMENTS

    override fun getStringLiteralElements(): TokenSet = STRINGS

    override fun createElement(node: ASTNode): PsiElement = ASTWrapperPsiElement(node)

    override fun createFile(viewProvider: FileViewProvider): PsiFile =
        object : PsiFileBase(viewProvider, TinLanguage) {
            override fun getFileType() = TinFileType
        }
}

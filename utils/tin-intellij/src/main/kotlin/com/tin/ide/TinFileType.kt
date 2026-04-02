package com.tin.ide

import com.intellij.openapi.fileTypes.LanguageFileType
import javax.swing.Icon

object TinFileType : LanguageFileType(TinLanguage) {
    override fun getName(): String = "Tin"
    override fun getDescription(): String = "Tin language file"
    override fun getDefaultExtension(): String = "tin"
    override fun getIcon(): Icon = TinIcons.FILE
}

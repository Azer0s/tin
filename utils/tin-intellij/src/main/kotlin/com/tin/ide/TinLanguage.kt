package com.tin.ide

import com.intellij.lang.Language

object TinLanguage : Language("Tin") {
    private fun readResolve(): Any = TinLanguage
}

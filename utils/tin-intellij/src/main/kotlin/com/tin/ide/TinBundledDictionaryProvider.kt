package com.tin.ide

import com.intellij.spellchecker.BundledDictionaryProvider

class TinBundledDictionaryProvider : BundledDictionaryProvider {
    override fun getBundledDictionaries(): Array<String> =
        arrayOf("/com/tin/ide/tin.dic")
}

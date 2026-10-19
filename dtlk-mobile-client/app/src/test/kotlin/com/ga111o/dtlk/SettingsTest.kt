package com.ga111o.dtlk

import org.junit.Assert.assertEquals
import org.junit.Assert.fail
import org.junit.Test

// Same cases as TestNormalizeConfig / TestEffectiveConfig in dtlk-package/config_test.go
class SettingsTest {
    @Test
    fun normalizes() {
        val ok = mapOf(
            ("server" to "files.example.com/") to "https://files.example.com",
            ("server" to "http://x:8780") to "http://x:8780",
            ("server" to " http://1.2.3.4:8780// ") to "http://1.2.3.4:8780",
            ("hours" to "48") to "48",
            ("hours" to " 7 ") to "7",
            ("tag" to "MyProj") to "myproj",
            ("tag" to "") to "",
            ("hours" to "") to "",
            ("server" to "  ") to "",
        )
        for ((input, want) in ok) {
            assertEquals("$input", want, normalizeSetting(input.first, input.second))
        }
    }

    @Test
    fun rejects() {
        val bad = listOf("hours" to "0", "hours" to "169", "hours" to "x", "hours" to "1.5", "tag" to "a.b",
            "tag" to "x".repeat(65), "name" to "x")
        for ((k, v) in bad) {
            try {
                normalizeSetting(k, v)
                fail("$k=$v: expected an error")
            } catch (_: SettingsError) {
            }
        }
    }

    @Test
    fun effectiveDefaults() {
        assertEquals(Settings(DEFAULT_SERVER, 24, ""), Settings.effective(emptyMap()))
        assertEquals(Settings(DEFAULT_SERVER, 24, ""), Settings.effective(mapOf("server" to "", "hours" to "", "tag" to "")))
        assertEquals(
            Settings("https://a.example", 48, "asdf"),
            Settings.effective(mapOf("server" to "https://a.example", "hours" to "48", "tag" to "asdf")),
        )
    }
}

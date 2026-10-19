package com.ga111o.dtlk

import android.content.Context

// Same keys, defaults and rules as `dtlk config` (dtlk-package/config.go).

const val DEFAULT_SERVER = "https://dtlk.ga111o.com"
const val DEFAULT_HOURS = 24
const val MAX_HOURS = 168

val SETTING_KEYS = listOf("server", "hours", "tag")

private val TAG_RE = Regex("[a-z0-9_-]{1,64}")

class SettingsError(message: String) : Exception(message)

/** The values uploads use, defaults filled in. */
data class Settings(val server: String, val hours: Int, val tag: String) {
    companion object {
        /** stored: key -> value as saved, "" meaning unset. */
        fun effective(stored: Map<String, String>): Settings = Settings(
            server = stored["server"].orEmpty().ifEmpty { DEFAULT_SERVER },
            hours = stored["hours"]?.toIntOrNull() ?: DEFAULT_HOURS,
            tag = stored["tag"].orEmpty(),
        )
    }
}

/** Adds https:// when no scheme is given and drops trailing slashes. */
fun normalizeServer(s: String): String {
    val v = s.trim().trimEnd('/')
    return if (v.startsWith("http://") || v.startsWith("https://")) v else "https://$v"
}

fun parseHours(s: String): Int {
    val h = s.toIntOrNull()
    if (h == null || h < 1 || h > MAX_HOURS) {
        throw SettingsError("hours must be an integer between 1 and $MAX_HOURS: $s")
    }
    return h
}

/** Lowercases a list tag; the server stores it the same way. "" means no tag. */
fun parseTag(s: String): String {
    val t = s.lowercase()
    if (t.isNotEmpty() && !TAG_RE.matches(t)) {
        throw SettingsError("tag must be 1-64 letters, digits, '-' or '_': \"$s\"")
    }
    return t
}

/** Validates value for key and returns it as stored; "" stays "" (unset). */
fun normalizeSetting(key: String, value: String): String {
    val v = value.trim()
    if (v.isEmpty()) return ""
    return when (key) {
        "server" -> normalizeServer(v)
        "hours" -> parseHours(v).toString()
        "tag" -> parseTag(v)
        else -> throw SettingsError("unknown setting: $key")
    }
}

object Prefs {
    private fun prefs(ctx: Context) = ctx.getSharedPreferences("settings", Context.MODE_PRIVATE)

    fun stored(ctx: Context): Map<String, String> {
        val p = prefs(ctx)
        return SETTING_KEYS.associateWith { p.getString(it, null).orEmpty() }
    }

    fun load(ctx: Context): Settings = Settings.effective(stored(ctx))

    /** values must already be normalized; "" removes the key. */
    fun save(ctx: Context, values: Map<String, String>) {
        val e = prefs(ctx).edit()
        for ((k, v) in values) {
            if (v.isEmpty()) e.remove(k) else e.putString(k, v)
        }
        e.apply()
    }
}

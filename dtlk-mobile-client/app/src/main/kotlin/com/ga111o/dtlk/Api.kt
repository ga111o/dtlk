package com.ga111o.dtlk

import org.json.JSONArray
import org.json.JSONException
import org.json.JSONObject
import java.io.IOException
import java.net.HttpURLConnection
import java.net.URL
import java.util.Collections
import java.util.concurrent.Executors
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicBoolean

// The /api/uploads calls of dtlk-package/api.go over HttpURLConnection.

/** A non-2xx response; detail is the server's message. */
class ApiError(val status: Int, val detail: String, val retryAfterSec: Long) : IOException(
    detail.ifEmpty { "server returned $status" } +
        if (retryAfterSec > 0) " (retry after ${goDuration(retryAfterSec)})" else ""
)

/** The upload was stopped on purpose (Cancel, or Android ending the service). */
class CanceledException(message: String) : IOException(message)

/** Like Go's time.Duration.String for whole seconds: 30m0s, 1h0m0s, 45s. */
fun goDuration(sec: Long): String {
    val h = sec / 3600
    val m = sec % 3600 / 60
    val s = sec % 60
    return when {
        h > 0 -> "${h}h${m}m${s}s"
        m > 0 -> "${m}m${s}s"
        else -> "${s}s"
    }
}

/** Whether a failed request is worth repeating: network errors, 408 and 5xx except 507. */
fun retryable(e: Throwable): Boolean = when (e) {
    is CanceledException -> false
    is ApiError -> e.status == 408 || (e.status >= 500 && e.status != 507)
    is IOException -> true
    else -> false
}

class CreatedUpload(val id: String, val chunkSize: Int, val maxSize: Long)

class Completed(val code: String, val expiresAt: String, val size: Long)

class Api(val base: String, private val userAgent: String) {
    private val active: MutableSet<HttpURLConnection> = Collections.synchronizedSet(HashSet())

    @Volatile
    private var canceled: String? = null

    /**
     * items: the file names in a bundle (kind "bundle"), empty otherwise.
     * text: the file is shared text, which dtlk down copies instead of saving.
     */
    fun createUpload(
        name: String,
        kind: String,
        estimatedSize: Long,
        hours: Int,
        tag: String,
        items: List<String> = emptyList(),
        text: Boolean = false,
    ): CreatedUpload {
        val body = JSONObject()
            .put("name", name)
            .put("kind", kind)
            .put("estimated_size", estimatedSize)
            .put("hours", hours)
        if (tag.isNotEmpty()) body.put("tag", tag)
        if (items.isNotEmpty()) {
            body.put("items", JSONArray(items.map { JSONObject().put("name", it).put("kind", "file") }))
        }
        if (text) body.put("text", true)
        val res = json(request("POST", "/api/uploads", body.toString().toByteArray(), "application/json", CALL_TIMEOUT_MS))
        return try {
            CreatedUpload(res.getString("upload_id"), res.getInt("chunk_size"), res.getLong("max_size"))
                .also { if (it.id.isEmpty() || it.chunkSize <= 0) throw IOException("invalid response from server") }
        } catch (e: JSONException) {
            throw IOException("invalid response from server", e)
        }
    }

    fun putPart(id: String, n: Int, data: ByteArray, len: Int) {
        request("PUT", "/api/uploads/$id/parts/$n", data, "application/octet-stream", PART_TIMEOUT_MS, len)
    }

    fun complete(id: String): Completed {
        val res = json(request("POST", "/api/uploads/$id/complete", ByteArray(0), "application/json", CALL_TIMEOUT_MS))
        return try {
            Completed(res.getString("code"), res.getString("expires_at"), res.getLong("size"))
        } catch (e: JSONException) {
            throw IOException("invalid response from server", e)
        }
    }

    /** Tells the server to drop an unfinished upload; best effort, also after cancel(). */
    fun abort(id: String) {
        try {
            request("DELETE", "/api/uploads/$id", null, null, ABORT_TIMEOUT_MS, track = false)
        } catch (_: IOException) {
        }
    }

    /** Fails every request in flight and every later one (except abort) with CanceledException(reason). */
    fun cancel(reason: String) {
        canceled = reason
        synchronized(active) { active.toList() }.forEach { it.disconnect() }
    }

    private fun json(text: String): JSONObject = try {
        JSONObject(text)
    } catch (e: JSONException) {
        throw IOException("failed to decode server response", e)
    }

    private fun request(
        method: String,
        path: String,
        body: ByteArray?,
        contentType: String?,
        timeoutMs: Long,
        len: Int = body?.size ?: 0,
        track: Boolean = true,
    ): String {
        val c = URL(base + path).openConnection() as HttpURLConnection
        c.requestMethod = method
        c.connectTimeout = CONNECT_TIMEOUT_MS
        c.readTimeout = READ_TIMEOUT_MS
        c.useCaches = false
        c.setRequestProperty("User-Agent", userAgent)
        if (track) {
            active += c // before the check, so cancel() cannot slip in between
            canceled?.let { active -= c; throw CanceledException(it) }
        }
        // HttpURLConnection has no overall deadline (Go: partTimeout); drop the connection instead.
        val timedOut = AtomicBoolean(false)
        val watchdog = TIMER.schedule({ timedOut.set(true); c.disconnect() }, timeoutMs, TimeUnit.MILLISECONDS)
        try {
            if (body != null) {
                c.doOutput = true
                c.setFixedLengthStreamingMode(len)
                c.setRequestProperty("Content-Type", contentType)
                c.outputStream.use { it.write(body, 0, len) }
            }
            val status = c.responseCode
            val text = (if (status >= 400) c.errorStream else c.inputStream)
                ?.use { it.readBytes().decodeToString() }.orEmpty()
            if (status >= 300) throw apiError(status, text, c.getHeaderField("Retry-After"))
            return text
        } catch (e: ApiError) {
            throw e
        } catch (e: IOException) {
            canceled?.let { if (track) throw CanceledException(it) }
            if (timedOut.get()) throw IOException("request timed out (${base}$path)", e)
            throw IOException("cannot reach server ($base): ${e.message}", e)
        } finally {
            watchdog.cancel(false)
            if (track) active -= c
        }
    }

    private fun apiError(status: Int, text: String, retryAfter: String?): ApiError {
        val detail = try {
            JSONObject(text).opt("detail") as? String
        } catch (_: JSONException) {
            null
        }
        return ApiError(status, detail.orEmpty(), retryAfter?.trim()?.toLongOrNull()?.takeIf { it > 0 } ?: 0)
    }

    companion object {
        const val CONNECT_TIMEOUT_MS = 30_000
        const val READ_TIMEOUT_MS = 120_000 // Go: ResponseHeaderTimeout
        const val CALL_TIMEOUT_MS = 5 * 60_000L
        const val PART_TIMEOUT_MS = 10 * 60_000L // Go: partTimeout
        const val ABORT_TIMEOUT_MS = 10_000L

        private val TIMER = Executors.newSingleThreadScheduledExecutor { r ->
            Thread(r, "dtlk-timeouts").apply { isDaemon = true }
        }
    }
}

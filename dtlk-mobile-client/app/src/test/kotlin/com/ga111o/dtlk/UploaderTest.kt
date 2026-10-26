package com.ga111o.dtlk

import com.github.luben.zstd.ZstdInputStream
import com.sun.net.httpserver.HttpExchange
import com.sun.net.httpserver.HttpServer
import org.apache.commons.compress.archivers.tar.TarArchiveInputStream
import org.json.JSONObject
import org.junit.After
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Test
import java.io.ByteArrayInputStream
import java.io.IOException
import java.net.InetSocketAddress
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.Executors
import java.util.concurrent.atomic.AtomicBoolean
import java.util.concurrent.atomic.AtomicInteger
import kotlin.random.Random

/** Just enough of dtlk-server's /api/uploads to watch what the uploader sends. */
private class FakeServer : AutoCloseable {
    private val http = HttpServer.create(InetSocketAddress("127.0.0.1", 0), 0)
    val parts = ConcurrentHashMap<Int, ByteArray>()
    val attempts = ConcurrentHashMap<Int, AtomicInteger>()
    val aborted = AtomicBoolean()

    @Volatile var created: JSONObject? = null
    @Volatile var chunkSize = 1000
    @Volatile var maxSize = 10L shl 30
    @Volatile var createError: Triple<Int, String, String?>? = null // status, detail, Retry-After
    @Volatile var failPart: (n: Int, attempt: Int) -> Int? = { _, _ -> null }

    val base get() = "http://127.0.0.1:${http.address.port}"

    init {
        http.executor = Executors.newCachedThreadPool()
        http.createContext("/") { handle(it) }
        http.start()
    }

    private fun reply(ex: HttpExchange, status: Int, body: String) {
        val b = body.toByteArray()
        ex.responseHeaders.add("Content-Type", "application/json")
        ex.sendResponseHeaders(status, b.size.toLong())
        ex.responseBody.use { it.write(b) }
    }

    private fun handle(ex: HttpExchange) {
        val path = ex.requestURI.path
        val body = ex.requestBody.readBytes()
        val part = Regex("/api/uploads/u1/parts/(\\d+)").matchEntire(path)
        when {
            ex.requestMethod == "POST" && path == "/api/uploads" -> {
                created = JSONObject(String(body))
                createError?.let { (status, detail, retry) ->
                    retry?.let { ex.responseHeaders.add("Retry-After", it) }
                    return reply(ex, status, JSONObject().put("detail", detail).toString())
                }
                reply(ex, 200, """{"upload_id":"u1","chunk_size":$chunkSize,"max_size":$maxSize}""")
            }
            ex.requestMethod == "PUT" && part != null -> {
                val n = part.groupValues[1].toInt()
                val attempt = attempts.getOrPut(n) { AtomicInteger() }.incrementAndGet()
                failPart(n, attempt)?.let { return reply(ex, it, """{"detail":"part $n failed"}""") }
                parts[n] = body
                reply(ex, 200, """{"ok":true}""")
            }
            ex.requestMethod == "POST" && path == "/api/uploads/u1/complete" ->
                reply(ex, 200, """{"code":"482913","expires_at":"2026-10-19T05:20:00+00:00","size":${parts.values.sumOf { it.size }}}""")
            ex.requestMethod == "DELETE" && path == "/api/uploads/u1" -> {
                aborted.set(true)
                ex.sendResponseHeaders(204, -1)
                ex.close()
            }
            else -> reply(ex, 404, """{"detail":"Not Found"}""")
        }
    }

    /** The parts in order, unpacked: archive name -> content (null for a directory). */
    fun unpack(): Map<String, ByteArray?> {
        val ns = parts.keys.sorted()
        assertEquals((1..ns.size).toList(), ns)
        val zst = ns.map { parts.getValue(it) }.reduce { a, b -> a + b }
        val out = LinkedHashMap<String, ByteArray?>()
        TarArchiveInputStream(ZstdInputStream(ByteArrayInputStream(zst))).use { t ->
            while (true) {
                val e = t.nextEntry ?: break
                out[e.name] = if (e.isDirectory) null else t.readBytes()
            }
        }
        return out
    }

    override fun close() = http.stop(0)
}

class UploaderTest {
    private val server = FakeServer()
    private val now = 1_800_000_000L
    private val stamp = "dtlk-261019-120000"

    @After
    fun stop() = server.close()

    private fun src(name: String, data: ByteArray) =
        Source(name, data.size.toLong(), now - 60) { ByteArrayInputStream(data) }

    private fun uploader() = Uploader(Api(server.base, "dtlk-android/test"), backoff = { 1L })

    private inline fun <reified T : Throwable> fails(block: () -> Unit): T {
        try {
            block()
        } catch (e: Throwable) {
            if (e is T) return e
            throw e
        }
        fail("expected ${T::class.simpleName}")
        throw AssertionError()
    }

    @Test
    fun uploadsSeveralItemsAsOneFolder() {
        val a = Random(1).nextBytes(5000)
        val photo = Random(2).nextBytes(3000)
        val text = "https://example.com/\n한글".toByteArray()
        val archive = Archive.of(listOf(src("a.bin", a), src("사진 1.jpg", photo), src("$stamp.txt", text)), stamp, now)
        assertEquals("$stamp/", archive.label)

        var last = 0L to 0L
        val res = uploader().run(archive, 48, "proj", now) { read, total -> last = read to total }

        assertEquals("482913", res.code)
        assertEquals((a.size + photo.size + text.size).toLong().let { it to it }, last)
        val c = server.created!!
        assertEquals(stamp, c.getString("name"))
        assertEquals("bundle", c.getString("kind"))
        val items = c.getJSONArray("items")
        assertEquals(listOf("a.bin", "사진 1.jpg", "$stamp.txt"), (0 until items.length()).map { items.getJSONObject(it).getString("name") })
        assertEquals(setOf("file"), (0 until items.length()).map { items.getJSONObject(it).getString("kind") }.toSet())
        assertEquals(48, c.getInt("hours"))
        assertEquals("proj", c.getString("tag"))
        assertFalse(c.has("text"))
        assertTrue(c.getLong("estimated_size") >= Tar.size(archive.entries))

        val got = server.unpack()
        assertEquals(listOf("$stamp/", "$stamp/a.bin", "$stamp/사진 1.jpg", "$stamp/$stamp.txt"), got.keys.toList())
        assertArrayEquals(a, got["$stamp/a.bin"])
        assertArrayEquals(photo, got["$stamp/사진 1.jpg"])
        assertArrayEquals(text, got["$stamp/$stamp.txt"])
        assertTrue(server.parts.size > 3) // several parts, sent PART_WORKERS at a time
        assertFalse(server.aborted.get())
    }

    @Test
    fun oneFileKeepsItsName() {
        val data = "hello".toByteArray()
        uploader().run(Archive.of(listOf(src("a.txt", data)), stamp, now), 24, "", now) { _, _ -> }
        val c = server.created!!
        assertEquals("a.txt", c.getString("name"))
        assertEquals("file", c.getString("kind"))
        assertFalse(c.has("tag"))
        assertFalse(c.has("items"))
        assertFalse(c.has("text"))
        assertEquals(listOf("a.txt"), server.unpack().keys.toList())
    }

    @Test
    fun sharedTextIsMarked() {
        val data = "https://example.com/\n한글".toByteArray()
        uploader().run(Archive.of(listOf(src("$stamp.txt", data)), stamp, now, text = true), 24, "", now) { _, _ -> }
        val c = server.created!!
        assertEquals("$stamp.txt", c.getString("name"))
        assertEquals("file", c.getString("kind"))
        assertTrue(c.getBoolean("text"))
        assertArrayEquals(data, server.unpack()["$stamp.txt"])
    }

    @Test
    fun retriesServerErrors() {
        server.failPart = { n, attempt -> if (n == 2 && attempt <= 2) 503 else null }
        uploader().run(Archive.of(listOf(src("a", Random(3).nextBytes(4000))), stamp, now), 24, "", now) { _, _ -> }
        assertEquals(3, server.attempts.getValue(2).get())
        assertFalse(server.aborted.get())
    }

    @Test
    fun clientErrorFailsAtOnceAndAborts() {
        server.failPart = { n, _ -> if (n == 1) 400 else null }
        val e = fails<ApiError> {
            uploader().run(Archive.of(listOf(src("a", Random(4).nextBytes(4000))), stamp, now), 24, "", now) { _, _ -> }
        }
        assertEquals("part 1 failed", e.message)
        assertEquals(1, server.attempts.getValue(1).get())
        assertTrue(server.aborted.get())
    }

    @Test
    fun givesUpAfterMaxRetries() {
        server.failPart = { n, _ -> if (n == 1) 502 else null }
        fails<ApiError> {
            uploader().run(Archive.of(listOf(src("a", Random(5).nextBytes(500))), stamp, now), 24, "", now) { _, _ -> }
        }
        assertEquals(MAX_RETRIES, server.attempts.getValue(1).get())
        assertTrue(server.aborted.get())
    }

    @Test
    fun stopsAtTheSizeLimit() {
        server.maxSize = 1500
        val e = fails<UploadError> {
            uploader().run(Archive.of(listOf(src("a", Random(6).nextBytes(10_000))), stamp, now), 24, "", now) { _, _ -> }
        }
        assertEquals("compressed size exceeds 1.5 KB limit; upload aborted", e.message)
        assertTrue(server.aborted.get())
    }

    @Test
    fun cancelAbortsTheUpload() {
        val up = uploader()
        val e = fails<CanceledException> {
            up.run(Archive.of(listOf(src("a", Random(7).nextBytes(100_000))), stamp, now), 24, "", now) { read, _ ->
                if (read > 50_000) up.cancel("Canceled")
            }
        }
        assertEquals("Canceled", e.message)
        assertTrue(server.aborted.get())
    }

    @Test
    fun fileThatChangesSizeFails() {
        val liar = Source("a", 10, now) { ByteArrayInputStream(ByteArray(5)) }
        val e = fails<IOException> { uploader().run(Archive.of(listOf(liar), stamp, now), 24, "", now) { _, _ -> } }
        assertEquals("file size changed during upload: a", e.message)
        assertTrue(server.aborted.get())
    }

    @Test
    fun showsTheServersMessage() {
        server.createError = Triple(429, "Hourly upload limit reached (max 30 per hour)", "1800")
        val e = fails<ApiError> { uploader().run(Archive.of(listOf(src("a", ByteArray(1))), stamp, now), 24, "", now) { _, _ -> } }
        assertEquals("Hourly upload limit reached (max 30 per hour) (retry after 30m0s)", e.message)
        assertFalse(retryable(e))
    }

    @Test
    fun cannotReachServer() {
        val e = fails<IOException> {
            Uploader(Api("http://127.0.0.1:1", "t")).run(Archive.of(listOf(src("a", ByteArray(1))), stamp, now), 24, "", now) { _, _ -> }
        }
        assertTrue(e.message!!, e.message!!.startsWith("cannot reach server (http://127.0.0.1:1)"))
    }

    @Test
    fun archiveRules() {
        val x = "x".toByteArray()
        assertEquals("Duplicate file name: a.txt",
            fails<UploadError> { Archive.of(listOf(src("a.txt", x), src("a.txt", x)), stamp, now) }.message)
        assertEquals("Invalid file name: a/b", fails<UploadError> { Archive.of(listOf(src("a/b", x)), stamp, now) }.message)
        assertEquals("Invalid file name: ..", fails<UploadError> { Archive.of(listOf(src("..", x)), stamp, now) }.message)
        assertEquals("Nothing to upload", fails<UploadError> { Archive.of(emptyList(), stamp, now) }.message)
        assertTrue(validName("가".repeat(85))) // 255 bytes
        assertFalse(validName("가".repeat(86)))
    }

    @Test
    fun formats() {
        assertEquals("30m0s", goDuration(1800))
        assertEquals("1h0m5s", goDuration(3605))
        assertEquals("45s", goDuration(45))
        assertEquals("512 B", human(512))
        assertEquals("1.5 KB", human(1536))
        assertEquals("10.0 GB", human(10L shl 30))
        assertTrue(retryable(ApiError(503, "", 0)))
        assertFalse(retryable(ApiError(507, "", 0)))
        assertTrue(retryable(ApiError(408, "", 0)))
        assertFalse(retryable(CanceledException("x")))
    }
}

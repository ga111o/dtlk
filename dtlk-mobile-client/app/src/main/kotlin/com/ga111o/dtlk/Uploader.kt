package com.ga111o.dtlk

import com.github.luben.zstd.ZstdOutputStream
import java.io.InputStream
import java.io.OutputStream
import java.util.concurrent.CountDownLatch
import java.util.concurrent.Executors
import java.util.concurrent.Semaphore
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicReference

// cmdUp and uploadStream of dtlk-package/up.go: tar, zstd and send in parts,
// PART_WORKERS at a time. Plain Kotlin so the JVM tests can run it.

const val PART_WORKERS = 3
const val MAX_RETRIES = 5
const val ZSTD_LEVEL = 3

/** A problem with what was shared, found before anything is sent. */
class UploadError(message: String) : Exception(message)

/** One shared item; open() must yield exactly size bytes. */
class Source(val name: String, val size: Long, val mtime: Long, val open: () -> InputStream)

/** What goes to the server: name and kind for /api/uploads, and the archive members. */
class Archive(val name: String, val kind: String, val entries: List<Entry>, val bytes: Long) {
    val label: String get() = name + if (kind == "dir") "/" else ""

    companion object {
        /**
         * One item stays as it is; several go into a <stamp> folder (stamp is
         * dtlk-yymmdd-hhmmss), like the web upload page. Names must differ.
         */
        fun of(items: List<Source>, stamp: String, now: Long): Archive {
            if (items.isEmpty()) throw UploadError("Nothing to upload")
            val seen = HashSet<String>()
            for (s in items) {
                if (!validName(s.name)) throw UploadError("Invalid file name: ${s.name}")
                if (!seen.add(s.name)) throw UploadError("Duplicate file name: ${s.name}")
            }
            val bytes = items.sumOf { it.size }
            if (items.size == 1) {
                val s = items[0]
                return Archive(s.name, "file", listOf(Entry(s.name, false, s.size, s.mtime, s.open)), bytes)
            }
            val entries = listOf(Entry(stamp, true, mtime = now)) +
                items.map { Entry("$stamp/${it.name}", false, it.size, it.mtime, it.open) }
            return Archive(stamp, "dir", entries, bytes)
        }
    }
}

/** The server's rule for a top-level name (valid_name in dtlk-server/app/service.py). */
fun validName(name: String): Boolean {
    val n = name.toByteArray(Charsets.UTF_8).size
    return n in 1..255 && name != "." && name != ".." && '/' !in name && '\u0000' !in name
}

fun backoffMs(attempt: Int): Long = minOf(1000L shl attempt, 15_000L)

class Uploader(private val api: Api, private val backoff: (Int) -> Long = ::backoffMs) {
    private val stop = CountDownLatch(1)

    /** The reason given to cancel(), once it has been called. */
    @Volatile
    var canceled: String? = null
        private set

    /** Stops the upload from any thread; run() then fails with CanceledException(reason). */
    fun cancel(reason: String) {
        canceled = reason
        halt(reason)
    }

    // Wakes retry waits and drops requests in flight.
    private fun halt(reason: String) {
        stop.countDown()
        api.cancel(reason)
    }

    /** progress(fileBytesRead, totalFileBytes) is called from the calling thread. */
    fun run(b: Archive, hours: Int, tag: String, now: Long, progress: (Long, Long) -> Unit): Completed {
        val tarSize = Tar.size(b.entries)
        val up = api.createUpload(b.name, b.kind, tarSize + tarSize / 100 + (1 shl 20), hours, tag)
        try {
            send(up, b, now, progress)
            return api.complete(up.id)
        } catch (e: Throwable) {
            api.abort(up.id)
            canceled?.let { throw CanceledException(it) }
            throw e
        }
    }

    private fun send(up: CreatedUpload, b: Archive, now: Long, progress: (Long, Long) -> Unit) {
        val pool = Executors.newFixedThreadPool(PART_WORKERS)
        val slots = Semaphore(PART_WORKERS)
        val failure = AtomicReference<Throwable?>(null)
        fun fail(e: Throwable) {
            if (failure.compareAndSet(null, e)) halt(e.message ?: e.toString())
        }

        val parts = object : OutputStream() {
            var buf = ByteArray(up.chunkSize)
            var len = 0
            var n = 0
            var total = 0L
            var dead = false // the tar failed: send nothing more

            override fun write(b: Int) = write(byteArrayOf(b.toByte()), 0, 1)

            override fun write(b: ByteArray, off: Int, l: Int) {
                if (dead) return
                var o = off
                var left = l
                while (left > 0) {
                    val k = minOf(left, buf.size - len)
                    b.copyInto(buf, len, o, o + k)
                    len += k
                    o += k
                    left -= k
                    if (len == buf.size) flushPart()
                }
            }

            override fun close() = flushPart()

            fun flushPart() {
                if (len == 0 || dead) return
                total += len
                if (total > up.maxSize) {
                    throw UploadError("compressed size exceeds ${human(up.maxSize)} limit; upload aborted")
                }
                slots.acquire()
                failure.get()?.let { slots.release(); throw it }
                val (data, size, num) = Triple(buf, len, ++n)
                pool.execute {
                    try {
                        putPart(up.id, num, data, size)
                    } catch (e: Throwable) {
                        fail(e)
                    } finally {
                        slots.release()
                    }
                }
                buf = ByteArray(up.chunkSize)
                len = 0
            }
        }

        var read = 0L
        val zw = ZstdOutputStream(parts, ZSTD_LEVEL)
        try {
            Tar.write(zw, b.entries, now) { k ->
                canceled?.let { throw CanceledException(it) }
                failure.get()?.let { throw it }
                read += k
                progress(read, b.bytes)
            }
            zw.close() // flushes the last part
        } catch (e: Throwable) {
            parts.dead = true
            runCatching { zw.close() }
            fail(e)
        } finally {
            slots.acquireUninterruptibly(PART_WORKERS) // every part has finished
            pool.shutdown()
        }
        failure.get()?.let { throw it }
    }

    private fun putPart(id: String, n: Int, data: ByteArray, len: Int) {
        var attempt = 1
        while (true) {
            try {
                api.putPart(id, n, data, len)
                return
            } catch (e: Throwable) {
                if (stop.count == 0L || !retryable(e) || attempt >= MAX_RETRIES) throw e
                if (stop.await(backoff(attempt), TimeUnit.MILLISECONDS)) throw e
                attempt++
            }
        }
    }
}

fun human(n: Long): String {
    var v = n.toDouble()
    for (unit in listOf("B", "KB", "MB", "GB")) {
        if (v < 1024 || unit == "GB") return if (unit == "B") "$n B" else String.format(java.util.Locale.ROOT, "%.1f %s", v, unit)
        v /= 1024
    }
    return "$n B"
}

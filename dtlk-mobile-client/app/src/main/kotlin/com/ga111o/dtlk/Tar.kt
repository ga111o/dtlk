package com.ga111o.dtlk

import java.io.IOException
import java.io.InputStream
import java.io.OutputStream

// The archive format the CLI and web upload page produce (see header() and
// paxRecord() in dtlk-server/app/upload.js): ustar headers, uid/gid 0, PAX
// records for long or non-ASCII names and for sizes of 8 GiB and up.

/** One archive member; a file's open() must yield exactly size bytes. */
class Entry(
    val path: String,
    val dir: Boolean,
    val size: Long = 0,
    val mtime: Long,
    val open: (() -> InputStream)? = null,
)

object Tar {
    private const val BIG = 8L shl 30 // the octal size field holds less than this

    private fun octal(n: Long, width: Int): ByteArray =
        n.toString(8).padStart(width - 1, '0').toByteArray(Charsets.US_ASCII)

    fun header(name: ByteArray, type: Char, size: Long, mode: Int, mtime: Long): ByteArray {
        val h = ByteArray(512)
        fun put(off: Int, b: ByteArray) = b.copyInto(h, off)
        put(0, name.copyOf(minOf(name.size, 100)))
        put(100, octal(mode.toLong(), 8))
        put(108, octal(0, 8))
        put(116, octal(0, 8))
        put(124, octal(size, 12))
        put(136, octal(maxOf(mtime, 0), 12))
        put(148, "        ".toByteArray(Charsets.US_ASCII))
        h[156] = type.code.toByte()
        put(257, "ustar\u000000".toByteArray(Charsets.US_ASCII))
        val sum = h.sumOf { it.toInt() and 0xff }
        put(148, octal(sum.toLong(), 7) + byteArrayOf(0, ' '.code.toByte()))
        return h
    }

    /** "<len> key=value\n" where len counts the whole record, its own digits included. */
    fun paxRecord(key: String, value: ByteArray): ByteArray {
        val body = " $key=".toByteArray(Charsets.UTF_8) + value + '\n'.code.toByte()
        var len = body.size + body.size.toString().length
        if (len.toString().length + body.size != len) len++
        return len.toString().toByteArray(Charsets.US_ASCII) + body
    }

    fun padding(n: Long): Int = ((512 - n % 512) % 512).toInt()

    private fun name(e: Entry): ByteArray = (e.path + if (e.dir) "/" else "").toByteArray(Charsets.UTF_8)

    private fun pax(name: ByteArray, size: Long): ByteArray {
        var pax = ByteArray(0)
        if (name.size > 100 || name.any { it < 0 }) pax += paxRecord("path", name)
        if (size >= BIG) pax += paxRecord("size", size.toString().toByteArray(Charsets.US_ASCII))
        return pax
    }

    /** Exact archive size of entries. */
    fun size(entries: List<Entry>): Long = entries.sumOf { e ->
        val pax = pax(name(e), e.size).size.toLong()
        (if (pax > 0) 512 + pax + padding(pax) else 0) + 512 + e.size + padding(e.size)
    } + 1024

    /**
     * Writes entries as one tar stream. onRead(n) reports file bytes consumed and may
     * throw to stop.
     */
    fun write(out: OutputStream, entries: List<Entry>, now: Long, onRead: (Int) -> Unit) {
        val buf = ByteArray(256 * 1024)
        for (e in entries) {
            val name = name(e)
            val size = if (e.dir) 0 else e.size
            val pax = pax(name, size)
            if (pax.isNotEmpty()) {
                out.write(header("PaxHeader".toByteArray(Charsets.US_ASCII), 'x', pax.size.toLong(), 420, now))
                out.write(pax)
                out.write(ByteArray(padding(pax.size.toLong())))
            }
            out.write(header(name, if (e.dir) '5' else '0', if (size >= BIG) 0 else size, if (e.dir) 493 else 420, e.mtime))
            if (e.dir) continue
            val input = e.open?.invoke() ?: throw IOException("cannot open ${e.path}")
            input.use {
                var left = size
                while (left > 0) {
                    val n = it.read(buf, 0, minOf(buf.size.toLong(), left).toInt())
                    if (n < 0) throw IOException("file size changed during upload: ${e.path}")
                    out.write(buf, 0, n)
                    left -= n
                    onRead(n)
                }
                if (it.read() >= 0) throw IOException("file size changed during upload: ${e.path}")
            }
            out.write(ByteArray(padding(size)))
        }
        out.write(ByteArray(1024))
    }
}

package com.ga111o.dtlk

import org.apache.commons.compress.archivers.tar.TarArchiveInputStream
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Test
import java.io.ByteArrayInputStream
import java.io.ByteArrayOutputStream
import java.io.IOException

class TarTest {
    private fun file(path: String, data: ByteArray, mtime: Long = 1_700_000_000) =
        Entry(path, false, data.size.toLong(), mtime) { ByteArrayInputStream(data) }

    private fun write(entries: List<Entry>): ByteArray =
        ByteArrayOutputStream().also { Tar.write(it, entries, 1_800_000_000) {} }.toByteArray()

    @Test
    fun readsBackWithAnotherTarReader() {
        val long = "top/" + "d".repeat(150) + ".txt"
        val korean = "top/사진 2026-10-19.jpg"
        val entries = listOf(
            Entry("top", true, mtime = 1_800_000_000),
            file("top/a.txt", "hello".toByteArray()),
            file(long, ByteArray(1000) { it.toByte() }),
            file(korean, ByteArray(513) { 7 }),
            file("top/empty", ByteArray(0)),
        )
        val tar = write(entries)
        assertEquals(Tar.size(entries), tar.size.toLong())
        assertEquals(0, tar.size % 512)

        val seen = mutableListOf<String>()
        TarArchiveInputStream(ByteArrayInputStream(tar)).use { t ->
            while (true) {
                val e = t.nextEntry ?: break
                seen += e.name
                val want = entries.first { it.path + (if (it.dir) "/" else "") == e.name }
                assertEquals(e.name, want.dir, e.isDirectory)
                assertEquals(e.name, if (want.dir) 493 else 420, e.mode)
                assertEquals(e.name, want.mtime, e.modTime.time / 1000)
                assertEquals(e.name, 0L, e.longUserId)
                if (!want.dir) assertArrayEquals(e.name, want.open!!().readBytes(), t.readBytes())
            }
        }
        assertEquals(listOf("top/", "top/a.txt", long, korean, "top/empty"), seen)
    }

    @Test
    fun failsWhenAFileChangesSize() {
        for (actual in listOf(3, 7)) {
            val e = Entry("a", false, 5, 0) { ByteArrayInputStream(ByteArray(actual)) }
            try {
                write(listOf(e))
                fail("$actual bytes for 5: expected an error")
            } catch (ex: IOException) {
                assertEquals("file size changed during upload: a", ex.message)
            }
        }
    }

    @Test
    fun paxRecordCountsItsOwnLength() {
        for (n in 0..1200) {
            val r = Tar.paxRecord("path", "x".repeat(n).toByteArray())
            val len = String(r).substringBefore(' ').toInt()
            assertEquals(r.size, len)
            assertTrue(String(r).endsWith("\n"))
        }
    }
}

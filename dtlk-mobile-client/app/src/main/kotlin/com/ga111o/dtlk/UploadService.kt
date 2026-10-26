package com.ga111o.dtlk

import android.app.NotificationManager
import android.app.Service
import android.content.Intent
import android.content.pm.ServiceInfo
import android.net.Uri
import android.os.Build
import android.os.Handler
import android.os.IBinder
import android.os.Looper
import android.os.PowerManager
import android.os.SystemClock
import android.provider.DocumentsContract
import android.provider.MediaStore
import android.provider.OpenableColumns
import java.io.ByteArrayInputStream
import java.io.File
import java.io.IOException
import java.util.concurrent.Executors
import java.util.concurrent.atomic.AtomicInteger

/**
 * Uploads what ShareActivity hands over, one share at a time, as a foreground
 * service so it keeps going with no screen of ours open. The URIs arrive in the
 * intent's ClipData with FLAG_GRANT_READ_URI_PERMISSION; Android keeps that read
 * grant until the service is stopped past that start, i.e. after the queue empties.
 */
class UploadService : Service() {
    private class Job(val uris: List<Uri>, val text: String?, val stamp: String, val now: Long)

    private lateinit var notifier: Notifier
    private lateinit var nm: NotificationManager
    private lateinit var wakeLock: PowerManager.WakeLock
    private val worker = Executors.newSingleThreadExecutor()
    private val main = Handler(Looper.getMainLooper())
    private val pending = AtomicInteger() // changed on the main thread only

    @Volatile
    private var shown: android.app.Notification? = null

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onCreate() {
        super.onCreate()
        notifier = Notifier(this)
        nm = getSystemService(NotificationManager::class.java)
        wakeLock = getSystemService(PowerManager::class.java)
            .newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "dtlk:upload")
            .apply { setReferenceCounted(false); acquire(6 * 3600 * 1000L) }
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        try {
            val n = shown ?: notifier.progress(null, 0, -1, pending.get())
            if (Build.VERSION.SDK_INT >= 29) {
                startForeground(Notifier.PROGRESS_ID, n, ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC)
            } else {
                startForeground(Notifier.PROGRESS_ID, n)
            }
        } catch (e: Exception) { // e.g. ForegroundServiceStartNotAllowedException
            notifier.failed(null, "Android did not allow the upload to start: ${e.message}", false)
            if (pending.get() == 0) stopSelf(startId)
            return START_NOT_STICKY
        }
        if (intent == null) {
            if (pending.get() == 0) stopSelf(startId)
            return START_NOT_STICKY
        }
        val clip = intent.clipData
        val job = Job(
            uris = (0 until (clip?.itemCount ?: 0)).mapNotNull { clip!!.getItemAt(it).uri },
            text = intent.getStringExtra(EXTRA_TEXT),
            stamp = intent.getStringExtra(EXTRA_STAMP) ?: stamp(System.currentTimeMillis() / 1000),
            now = intent.getLongExtra(EXTRA_NOW, System.currentTimeMillis() / 1000),
        )
        pending.incrementAndGet()
        worker.execute { process(job, startId) }
        return START_NOT_STICKY
    }

    private fun waiting() = (pending.get() - 1).coerceAtLeast(0)

    private fun show(n: android.app.Notification) {
        shown = n
        nm.notify(Notifier.PROGRESS_ID, n)
    }

    private fun process(job: Job, startId: Int) {
        var label: String? = null
        val temps = mutableListOf<File>()
        try {
            val settings = Prefs.load(this)
            val up = Uploader(Api(settings.server, "dtlk-android/${BuildConfig.VERSION_NAME}"))
            current = up
            show(notifier.progress(null, 0, -1, waiting()))

            val sources = job.uris.map { resolve(it, job.now, temps, up) }.toMutableList()
            job.text?.let {
                val bytes = it.toByteArray(Charsets.UTF_8)
                sources += Source("${job.stamp}.txt", bytes.size.toLong(), job.now) { ByteArrayInputStream(bytes) }
            }
            val bundle = Archive.of(sources, job.stamp, job.now, text = job.uris.isEmpty() && job.text != null)
            label = bundle.label
            up.canceled?.let { throw CanceledException(it) }

            var last = 0L
            show(notifier.progress(label, 0, bundle.bytes, waiting()))
            val res = up.run(bundle, settings.hours, settings.tag, job.now) { read, total ->
                val t = SystemClock.elapsedRealtime()
                if (t - last >= 1000) {
                    last = t
                    show(notifier.progress(label, read, total, waiting()))
                }
            }
            val link = "${settings.server}/${res.code}"
            notifier.done(bundle.label, link, res.code, localTime(res.expiresAt), copyLink(this, link))
        } catch (e: CanceledException) {
            notifier.failed(label, e.message ?: "Canceled", true)
        } catch (e: Exception) {
            notifier.failed(label, e.message ?: e.toString(), false)
        } finally {
            current = null
            temps.forEach { it.delete() }
            main.post {
                if (pending.decrementAndGet() == 0) {
                    shown = null
                    stopSelf(startId)
                }
            }
        }
    }

    /** Name, size and mtime of a shared URI. Streams of unknown length are copied to the cache first. */
    private fun resolve(uri: Uri, now: Long, temps: MutableList<File>, up: Uploader): Source {
        val cr = contentResolver
        var name: String? = null
        var size = -1L
        var mtime = now
        try {
            cr.query(uri, null, null, null, null)?.use { c ->
                if (c.moveToFirst()) {
                    fun col(n: String) = c.getColumnIndex(n).takeIf { it >= 0 && !c.isNull(it) }
                    col(OpenableColumns.DISPLAY_NAME)?.let { name = c.getString(it) }
                    col(OpenableColumns.SIZE)?.let { size = c.getLong(it) }
                    val docMs = col(DocumentsContract.Document.COLUMN_LAST_MODIFIED)?.let { c.getLong(it) } ?: 0
                    val mediaSec = col(MediaStore.MediaColumns.DATE_MODIFIED)?.let { c.getLong(it) } ?: 0
                    if (docMs > 0) mtime = docMs / 1000 else if (mediaSec > 0) mtime = mediaSec
                }
            }
        } catch (_: Exception) {
            // some providers refuse queries; opening the stream below reports real problems
        }
        val n = name?.takeIf { it.isNotEmpty() } ?: uri.lastPathSegment?.substringAfterLast('/') ?: "file"
        if (size < 0) {
            size = try {
                cr.openAssetFileDescriptor(uri, "r")?.use { it.length } ?: -1
            } catch (_: Exception) {
                -1
            }
        }
        val open = { cr.openInputStream(uri) ?: throw IOException("cannot open $n") }
        if (size >= 0) return Source(n, size, mtime, open)

        // a tar header needs the size before the data
        val f = File.createTempFile("share", null, cacheDir).also { temps += it }
        open().use { i ->
            f.outputStream().use { o ->
                val buf = ByteArray(256 * 1024)
                while (true) {
                    up.canceled?.let { throw CanceledException(it) }
                    val k = i.read(buf)
                    if (k < 0) break
                    o.write(buf, 0, k)
                }
            }
        }
        return Source(n, f.length(), mtime) { f.inputStream() }
    }

    // Android 15+: dataSync services get about 6 hours a day.
    override fun onTimeout(startId: Int, fgsType: Int) {
        current?.cancel("Android stopped the upload: the background time limit was reached")
        worker.shutdownNow()
        stopSelf()
    }

    override fun onDestroy() {
        current?.cancel("Canceled")
        worker.shutdownNow()
        if (wakeLock.isHeld) wakeLock.release()
        super.onDestroy()
    }

    companion object {
        const val EXTRA_TEXT = "text"
        const val EXTRA_STAMP = "stamp"
        const val EXTRA_NOW = "now"

        @Volatile
        private var current: Uploader? = null

        fun cancelCurrent() {
            current?.cancel("Canceled")
        }
    }
}

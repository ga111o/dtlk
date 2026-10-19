package com.ga111o.dtlk

import android.app.Activity
import android.app.NotificationManager
import android.content.ClipData
import android.content.Intent
import android.net.Uri
import android.os.Bundle
import android.widget.Toast
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

/** dtlk-yymmdd-hhmmss in local time, like the web upload page. */
fun stamp(epochSec: Long): String = SimpleDateFormat("'dtlk-'yyMMdd-HHmmss", Locale.US).format(Date(epochSec * 1000))

/**
 * The share sheet target. It has no screen: it passes the shared files and text
 * to UploadService (allowed to start in the foreground because this activity is
 * on top for a moment) and closes.
 */
class ShareActivity : Activity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        try {
            forward()
        } finally {
            finish()
        }
    }

    private fun forward() {
        val uris = streams(intent)
        val text = text(intent)
        if (uris.isEmpty() && text.isNullOrEmpty()) {
            toast("Nothing to upload")
            return
        }
        val now = System.currentTimeMillis() / 1000
        val svc = Intent(this, UploadService::class.java)
            .putExtra(UploadService.EXTRA_TEXT, text?.takeIf { it.isNotEmpty() })
            .putExtra(UploadService.EXTRA_STAMP, stamp(now))
            .putExtra(UploadService.EXTRA_NOW, now)
        if (uris.isNotEmpty()) {
            val clip = ClipData.newRawUri("", uris[0])
            uris.drop(1).forEach { clip.addItem(ClipData.Item(it)) }
            svc.clipData = clip
            svc.addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
        }
        if (!getSystemService(NotificationManager::class.java).areNotificationsEnabled()) {
            toast("Notifications are off; open dtlk to enable")
        }
        try {
            startForegroundService(svc)
        } catch (_: SecurityException) {
            // The sharing app did not grant us its URIs, so they cannot be passed on;
            // the service still runs and reports what it cannot read.
            svc.removeFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
            startForegroundService(svc)
        }
    }

    private fun toast(msg: String) = Toast.makeText(applicationContext, msg, Toast.LENGTH_LONG).show()

    @Suppress("DEPRECATION") // the typed getters are broken on API 33
    private fun streams(i: Intent): List<Uri> {
        val extra: List<Uri> = when (i.action) {
            Intent.ACTION_SEND -> listOfNotNull(i.getParcelableExtra(Intent.EXTRA_STREAM) as? Uri)
            Intent.ACTION_SEND_MULTIPLE -> i.getParcelableArrayListExtra<Uri>(Intent.EXTRA_STREAM).orEmpty()
            else -> emptyList()
        }
        if (extra.isNotEmpty()) return extra
        val clip = i.clipData ?: return emptyList()
        return (0 until clip.itemCount).mapNotNull { clip.getItemAt(it).uri }
    }

    private fun text(i: Intent): String? =
        i.getCharSequenceExtra(Intent.EXTRA_TEXT)?.toString()
            ?: i.getCharSequenceArrayListExtra(Intent.EXTRA_TEXT)?.joinToString("\n")
}

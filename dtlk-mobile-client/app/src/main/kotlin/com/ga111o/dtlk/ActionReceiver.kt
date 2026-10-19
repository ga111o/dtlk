package com.ga111o.dtlk

import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.ClipData
import android.content.ClipboardManager
import android.content.Context
import android.content.Intent
import android.os.Build
import android.widget.Toast

/** Notification actions: tapping a finished upload copies its link; Cancel stops the current upload. */
class ActionReceiver : BroadcastReceiver() {
    override fun onReceive(ctx: Context, intent: Intent) {
        when (intent.action) {
            ACTION_COPY -> intent.getStringExtra(EXTRA_LINK)?.let {
                if (copyLink(ctx, it) && Build.VERSION.SDK_INT < 33) {
                    Toast.makeText(ctx, "Link copied", Toast.LENGTH_SHORT).show() // 33+ shows its own
                }
            }
            ACTION_CANCEL -> UploadService.cancelCurrent()
        }
    }

    companion object {
        private const val ACTION_COPY = "com.ga111o.dtlk.COPY"
        private const val ACTION_CANCEL = "com.ga111o.dtlk.CANCEL"
        private const val EXTRA_LINK = "link"

        fun copyIntent(ctx: Context, id: Int, link: String): PendingIntent = PendingIntent.getBroadcast(
            ctx, id,
            Intent(ctx, ActionReceiver::class.java).setAction(ACTION_COPY).putExtra(EXTRA_LINK, link),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )

        fun cancelIntent(ctx: Context): PendingIntent = PendingIntent.getBroadcast(
            ctx, 0,
            Intent(ctx, ActionReceiver::class.java).setAction(ACTION_CANCEL),
            PendingIntent.FLAG_IMMUTABLE,
        )
    }
}

/** Writing the clipboard is allowed from the background; only reading is restricted (Android 10+). */
fun copyLink(ctx: Context, link: String): Boolean = try {
    ctx.getSystemService(ClipboardManager::class.java).setPrimaryClip(ClipData.newPlainText("dtlk", link))
    true
} catch (_: Exception) {
    false
}

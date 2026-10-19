package com.ga111o.dtlk

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.content.Context
import android.graphics.drawable.Icon
import android.os.Build
import android.os.Bundle
import java.time.OffsetDateTime
import java.time.ZoneId
import java.time.format.DateTimeFormatter
import java.util.concurrent.atomic.AtomicInteger

private const val CH_PROGRESS = "progress"
private const val CH_RESULTS = "results"

// Notification.EXTRA_REQUEST_PROMOTED_ONGOING: Android 16 (API 36.0) honours it;
// setRequestPromotedOngoing() only exists from 36.1.
private const val EXTRA_REQUEST_PROMOTED_ONGOING = "android.requestPromotedOngoing"

/** expires_at from the server (ISO 8601) as local "MM-dd HH:mm". */
fun localTime(iso: String): String = try {
    OffsetDateTime.parse(iso).atZoneSameInstant(ZoneId.systemDefault())
        .format(DateTimeFormatter.ofPattern("MM-dd HH:mm"))
} catch (_: Exception) {
    iso
}

class Notifier(private val ctx: Context) {
    private val nm = ctx.getSystemService(NotificationManager::class.java)

    init {
        nm.createNotificationChannels(
            listOf(
                // not IMPORTANCE_MIN: that channel could not be promoted to a Live Update
                NotificationChannel(CH_PROGRESS, "Upload progress", NotificationManager.IMPORTANCE_LOW),
                NotificationChannel(CH_RESULTS, "Upload results", NotificationManager.IMPORTANCE_DEFAULT),
            )
        )
    }

    /**
     * The foreground service's notification. label is null and total < 0 while the
     * shared items are being looked at. On Android 16+ it asks to be a Live Update
     * (Now Bar, status bar chip); the rules are no custom views, no colorizing, ongoing.
     */
    fun progress(label: String?, read: Long, total: Long, waiting: Int): Notification {
        val preparing = label == null || total < 0
        val pct = if (preparing || total <= 0) 0 else (read * 100 / total).toInt().coerceIn(0, 100)
        val text = buildString {
            append(if (preparing) "Preparing" else "$pct% · ${human(read)} / ${human(total)}")
            if (waiting > 0) append(" · $waiting waiting")
        }
        val b = Notification.Builder(ctx, CH_PROGRESS)
            .setSmallIcon(R.drawable.ic_upload)
            .setContentTitle(if (label == null) "Uploading" else "Uploading $label")
            .setContentText(text)
            .setOngoing(true)
            .setOnlyAlertOnce(true)
            .setShowWhen(false)
            .setCategory(Notification.CATEGORY_PROGRESS)
            .addAction(Notification.Action.Builder(null as Icon?, "Cancel", ActionReceiver.cancelIntent(ctx)).build())
        if (Build.VERSION.SDK_INT >= 36) {
            b.addExtras(Bundle().apply { putBoolean(EXTRA_REQUEST_PROMOTED_ONGOING, true) })
            if (!preparing) b.setShortCriticalText("$pct%")
            b.setStyle(
                Notification.ProgressStyle()
                    .setProgressSegments(listOf(Notification.ProgressStyle.Segment(100)))
                    .setProgress(pct)
                    .setProgressIndeterminate(preparing)
            )
        } else {
            b.setProgress(100, pct, preparing)
        }
        return b.build()
    }

    fun done(label: String, link: String, code: String, expires: String, copied: Boolean) {
        val id = ids.incrementAndGet()
        val state = if (copied) "copied" else "not copied"
        nm.notify(
            id,
            Notification.Builder(ctx, CH_RESULTS)
                .setSmallIcon(R.drawable.ic_upload)
                .setContentTitle("Uploaded $label")
                .setContentText("$link ($state) · expires $expires")
                .setStyle(
                    Notification.BigTextStyle()
                        .bigText("$link ($state)\nCode $code · expires $expires\nTap to copy the link again")
                )
                .setContentIntent(ActionReceiver.copyIntent(ctx, id, link))
                .build(),
        )
    }

    fun failed(label: String?, message: String, canceled: Boolean) {
        val title = (if (canceled) "Upload canceled" else "Upload failed") + (label?.let { ": $it" } ?: "")
        nm.notify(
            ids.incrementAndGet(),
            Notification.Builder(ctx, CH_RESULTS)
                .setSmallIcon(R.drawable.ic_upload)
                .setContentTitle(title)
                .setContentText(message)
                .setStyle(Notification.BigTextStyle().bigText(message))
                .setAutoCancel(true)
                .build(),
        )
    }

    companion object {
        const val PROGRESS_ID = 1

        // result notifications must not replace each other, also across restarts
        private val ids = AtomicInteger((System.currentTimeMillis() / 1000 % 1_000_000_000).toInt())
    }
}

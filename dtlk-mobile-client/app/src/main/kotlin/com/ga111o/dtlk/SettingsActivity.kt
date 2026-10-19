package com.ga111o.dtlk

import android.Manifest
import android.app.Activity
import android.app.NotificationManager
import android.content.ActivityNotFoundException
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import android.provider.Settings as SystemSettings
import android.view.View
import android.view.WindowInsets
import android.widget.Button
import android.widget.EditText
import android.widget.TextView

// Settings.ACTION_APP_NOTIFICATION_PROMOTION_SETTINGS, added in API 36.1
private const val ACTION_PROMOTION_SETTINGS = "android.settings.APP_NOTIFICATION_PROMOTION_SETTINGS"

/** The launcher screen: the defaults every share uploads with. */
class SettingsActivity : Activity() {
    private lateinit var fields: Map<String, EditText>
    private lateinit var message: TextView
    private lateinit var status: TextView

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.settings)
        val root = findViewById<View>(R.id.root)
        if (Build.VERSION.SDK_INT >= 35) { // edge-to-edge is enforced from here on
            root.setOnApplyWindowInsetsListener { v, insets ->
                val bars = insets.getInsets(WindowInsets.Type.systemBars() or WindowInsets.Type.ime())
                v.setPadding(bars.left, bars.top, bars.right, bars.bottom)
                insets
            }
        }
        fields = mapOf("server" to R.id.server, "hours" to R.id.hours, "tag" to R.id.tag)
            .mapValues { findViewById(it.value) }
        message = findViewById(R.id.message)
        status = findViewById(R.id.status)
        findViewById<TextView>(R.id.version).text = "dtlk ${BuildConfig.VERSION_NAME}"

        val stored = Prefs.stored(this)
        fields.forEach { (k, f) -> f.setText(stored[k]) }
        findViewById<Button>(R.id.save).setOnClickListener { save() }
        findViewById<Button>(R.id.notifications).setOnClickListener {
            open(Intent(SystemSettings.ACTION_APP_NOTIFICATION_SETTINGS))
        }
        findViewById<Button>(R.id.live_updates).apply {
            visibility = if (Build.VERSION.SDK_INT >= 36) View.VISIBLE else View.GONE
            setOnClickListener {
                open(Intent(ACTION_PROMOTION_SETTINGS), Intent(SystemSettings.ACTION_APP_NOTIFICATION_SETTINGS))
            }
        }

        if (Build.VERSION.SDK_INT >= 33 &&
            checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED
        ) {
            requestPermissions(arrayOf(Manifest.permission.POST_NOTIFICATIONS), 1)
        }
    }

    override fun onResume() {
        super.onResume()
        showStatus()
    }

    override fun onRequestPermissionsResult(requestCode: Int, permissions: Array<out String>, grantResults: IntArray) {
        showStatus()
    }

    private fun save() {
        val values = try {
            fields.mapValues { (k, f) -> normalizeSetting(k, f.text.toString()) }
        } catch (e: SettingsError) {
            message.text = e.message
            return
        }
        Prefs.save(this, values)
        values.forEach { (k, v) -> fields.getValue(k).setText(v) }
        val s = Prefs.load(this)
        message.text = "Saved.\nserver=${s.server}\nhours=${s.hours}\ntag=${s.tag}"
    }

    private fun showStatus() {
        val nm = getSystemService(NotificationManager::class.java)
        val lines = mutableListOf("Notifications: " + if (nm.areNotificationsEnabled()) "on" else "off")
        lines += if (Build.VERSION.SDK_INT >= 36) {
            "Live Updates (Now Bar, status bar chip): " + if (nm.canPostPromotedNotifications()) "on" else "off"
        } else {
            "Live Updates need Android 16; progress shows as a normal notification"
        }
        status.text = lines.joinToString("\n")
    }

    /** Opens the first settings screen this device has. */
    private fun open(vararg intents: Intent) {
        for (i in intents) {
            try {
                startActivity(i.putExtra(SystemSettings.EXTRA_APP_PACKAGE, packageName))
                return
            } catch (_: ActivityNotFoundException) {
            }
        }
    }
}

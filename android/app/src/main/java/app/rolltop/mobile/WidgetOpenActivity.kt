package app.rolltop.mobile

import android.app.Activity
import android.content.Intent
import android.os.Bundle

/**
 * WidgetOpenActivity is where a tap on the agenda widget lands. It draws
 * nothing: it opens the page in the server's installed PWA when there is one
 * ([PwaHandoff]) and in this app otherwise, and finishes. A tap that has to be
 * answered by signing in always opens this app, because the widget reads the
 * calendar with this app's session and the PWA cannot give it one.
 */
class WidgetOpenActivity : Activity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val path = intent.getStringExtra(AgendaWidget.EXTRA_PATH).orEmpty()
        val inApp = intent.getBooleanExtra(EXTRA_IN_APP, false)
        val target = AgendaWidgetModel.calendarTarget(RolltopPrefs.serverUrl(this), path)
        if (inApp || target == null || !PwaHandoff.open(this, target)) {
            val open = Intent(this, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
            if (target != null) open.putExtra(AgendaWidget.EXTRA_PATH, path)
            startActivity(open)
        }
        finish()
    }

    companion object {
        /** EXTRA_IN_APP asks for this app even when a PWA is installed. */
        const val EXTRA_IN_APP = "app.rolltop.mobile.extra.WIDGET_IN_APP"
    }
}

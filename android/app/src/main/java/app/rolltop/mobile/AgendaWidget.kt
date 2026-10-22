package app.rolltop.mobile

import android.app.AlarmManager
import android.app.PendingIntent
import android.appwidget.AppWidgetManager
import android.appwidget.AppWidgetProvider
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.text.SpannableStringBuilder
import android.text.Spanned
import android.text.format.DateFormat
import android.text.style.ForegroundColorSpan
import android.text.style.StrikethroughSpan
import android.widget.RemoteViews
import androidx.core.widget.RemoteViewsCompat
import java.time.Instant
import java.time.LocalDate
import java.time.ZoneId
import java.time.ZonedDateTime
import java.time.format.DateTimeFormatter
import java.time.format.FormatStyle
import java.util.Locale
import java.util.concurrent.Executors

/**
 * The agenda widget: the coming fortnight on the home screen, dark, in the
 * shape of the app's phone agenda. [AgendaWidgetModel] decides what a day
 * holds; this file draws it and keeps it current.
 *
 * Three things move it. New data, from [AgendaWidgetWorker]. Time passing,
 * which needs no data: an alarm at the next moment the widget looks different
 * -- an appointment beginning or ending moves the now line and greys what is
 * over, midnight moves the days -- redraws it from the stored copy. And the
 * reader: the refresh button, and leaving the app, where they may have just
 * entered something ([requestRefresh] from MainActivity).
 *
 * A tap opens the calendar on that day, with the event's dialog up
 * (`/calendar/<day>?event=<id>`); the plus opens a new event there. It opens in
 * the server's installed PWA when there is one and in this app otherwise
 * ([WidgetOpenActivity]); a widget that has no calendar to show because nobody
 * is signed in opens this app, where signing in is what it needs.
 */
class AgendaWidgetProvider : AppWidgetProvider() {
    override fun onUpdate(context: Context, manager: AppWidgetManager, appWidgetIds: IntArray) {
        inBackground {
            AgendaWidget.render(context, appWidgetIds)
            AgendaWidgetWorker.schedule(context)
            AgendaWidget.requestRefresh(context)
        }
    }

    override fun onAppWidgetOptionsChanged(
        context: Context,
        manager: AppWidgetManager,
        appWidgetId: Int,
        newOptions: android.os.Bundle
    ) {
        inBackground { AgendaWidget.render(context, intArrayOf(appWidgetId)) }
    }

    override fun onEnabled(context: Context) {
        AgendaWidgetWorker.schedule(context)
    }

    override fun onDisabled(context: Context) {
        inBackground {
            AgendaWidgetWorker.cancel(context)
            AgendaWidget.cancelTick(context)
            AgendaWidgetStore.clear(context)
        }
    }

    override fun onReceive(context: Context, intent: Intent) {
        when (intent.action) {
            AgendaWidget.ACTION_REFRESH -> inBackground { AgendaWidget.requestRefresh(context) }
            AgendaWidget.ACTION_TICK,
            Intent.ACTION_TIME_CHANGED,
            Intent.ACTION_TIMEZONE_CHANGED,
            Intent.ACTION_LOCALE_CHANGED -> inBackground { AgendaWidget.render(context) }
            else -> super.onReceive(context, intent)
        }
    }

    /** inBackground keeps the broadcast alive while the stored copy is read and
     * drawn off the main thread, where a receiver's own work would block it. */
    private fun inBackground(block: () -> Unit) {
        val pending = goAsync()
        AgendaWidget.post {
            try {
                block()
            } finally {
                pending.finish()
            }
        }
    }
}

internal object AgendaWidget {
    const val ACTION_REFRESH = "app.rolltop.mobile.action.AGENDA_WIDGET_REFRESH"
    const val ACTION_TICK = "app.rolltop.mobile.action.AGENDA_WIDGET_TICK"

    /** EXTRA_PATH carries the calendar route a tap opens. It names a page of
     * the configured server and nothing about a user, so MainActivity resolves
     * it against that server alone. */
    const val EXTRA_PATH = "app.rolltop.mobile.extra.WIDGET_PATH"

    private const val REQUEST_OPEN = 300
    private const val REQUEST_NEW = 301
    private const val REQUEST_REFRESH = 302
    private const val REQUEST_ROW = 303
    private const val REQUEST_TICK = 304
    private const val REQUEST_EMPTY = 305

    /** The layouts the list can hold; the count has to cover all of them. */
    private const val VIEW_TYPES = 5

    private const val COLOR_TEXT = 0xFFE3E3E3.toInt()
    private const val COLOR_MUTED = 0xFFA2A9B0.toInt()
    private const val COLOR_PAST = 0xFF80868B.toInt()
    private const val COLOR_CLASH = 0xFFF2B8B5.toInt()

    fun hasWidgets(context: Context): Boolean = widgetIds(context).isNotEmpty()

    fun requestRefresh(context: Context) {
        if (hasWidgets(context)) AgendaWidgetWorker.refreshNow(context)
    }

    /** signedOut forgets the calendar of the user who just signed out, so the
     * home screen does not keep showing it to whoever holds the phone next. */
    fun signedOut(context: Context) {
        val server = RolltopPrefs.serverUrl(context)
        // Dropping the copy stays on the caller's thread: it has to happen
        // before the sign-out request goes out, or a refresh in flight could
        // save the old session's answer after it (see AgendaWidgetStore).
        try {
            if (server.isBlank()) AgendaWidgetStore.clear(context) else AgendaWidgetStore.markSignedOut(context, server)
        } catch (_: java.io.IOException) {
            // The marker could not be written; deleting the copy still takes
            // the previous user's calendar off the home screen.
            AgendaWidgetStore.clear(context)
        }
        post { if (hasWidgets(context)) render(context) }
    }

    /** One thread draws the widget, so two redraws never interleave and none of
     * them runs on the main thread. */
    private val drawing = Executors.newSingleThreadExecutor()

    fun post(task: () -> Unit) {
        drawing.execute(task)
    }

    fun render(context: Context, ids: IntArray = widgetIds(context)) {
        if (ids.isEmpty()) return
        val manager = AppWidgetManager.getInstance(context)
        val server = RolltopPrefs.serverUrl(context)
        val snapshot = AgendaWidgetStore.load(context)?.takeIf { it.serverUrl == server && server.isNotBlank() }
        val now = ZonedDateTime.now()
        val calendars = AgendaWidgetModel.parseCalendars(snapshot?.calendars)
        val plan = AgendaWidgetModel.plan(AgendaWidgetModel.parseEvents(snapshot?.events), now)
        val locale = currentLocale(context)
        val formats = Formats(context, locale)

        for (id in ids) {
            val views = RemoteViews(context.packageName, R.layout.widget_agenda)
            views.setTextViewText(R.id.agenda_title, formats.title.format(now))
            views.setTextViewText(R.id.agenda_subtitle, subtitle(context, plan, snapshot, formats))
            // A widget still loading its first copy opens the PWA like any
            // other; only one that is signed out needs this app.
            val needsSignIn = server.isBlank() || snapshot?.signedOut == true
            val today = calendarPath(plan.today, null)
            views.setOnClickPendingIntent(R.id.agenda_header, openPending(context, REQUEST_OPEN, today, inApp = needsSignIn))
            views.setOnClickPendingIntent(R.id.agenda_empty, openPending(context, REQUEST_EMPTY, today, inApp = needsSignIn))
            views.setOnClickPendingIntent(R.id.agenda_new, openPending(context, REQUEST_NEW, "$today?new=1", inApp = needsSignIn))
            views.setOnClickPendingIntent(R.id.agenda_refresh, refreshPending(context))
            views.setPendingIntentTemplate(R.id.agenda_list, rowTemplate(context))
            views.setEmptyView(R.id.agenda_list, R.id.agenda_empty)
            views.setTextViewText(R.id.agenda_empty, emptyText(context, server, snapshot))

            val items = RemoteViewsCompat.RemoteCollectionItems.Builder()
                .setHasStableIds(true)
                .setViewTypeCount(VIEW_TYPES)
            if (snapshot != null && !snapshot.signedOut) {
                for (row in plan.rows) {
                    items.addItem(stableId(row.key), rowViews(context, row, calendars, formats, now.zone))
                }
            }
            RemoteViewsCompat.setRemoteAdapter(context, views, id, R.id.agenda_list, items.build())
            manager.updateAppWidget(id, views)
        }
        scheduleTick(context, plan.nextChange)
    }

    fun cancelTick(context: Context) {
        context.getSystemService(AlarmManager::class.java)?.cancel(tickPending(context))
    }

    private fun widgetIds(context: Context): IntArray =
        AppWidgetManager.getInstance(context)
            .getAppWidgetIds(ComponentName(context, AgendaWidgetProvider::class.java))

    private fun subtitle(context: Context, plan: AgendaPlan, snapshot: AgendaSnapshot?, formats: Formats): String {
        if (snapshot == null || snapshot.signedOut) return ""
        val resources = context.resources
        val parts = ArrayList<String>()
        parts += if (plan.todayEvents == 0) {
            resources.getString(R.string.widget_agenda_today_none)
        } else {
            resources.getQuantityString(R.plurals.widget_agenda_today_events, plan.todayEvents, plan.todayEvents)
        }
        if (snapshot.failedAt > 0 && snapshot.fetchedAt > 0) {
            val fetched = Instant.ofEpochMilli(snapshot.fetchedAt).atZone(ZoneId.systemDefault())
            // A copy from an earlier day names the day too: "as of 14:00" on a
            // copy three days old reads as this afternoon's.
            val at = formats.time.format(fetched)
            val day = fetched.toLocalDate()
            parts += resources.getString(
                R.string.widget_agenda_stale,
                if (day == plan.today) at else "${formats.dayName(day)} ${day.dayOfMonth} $at"
            )
        }
        return parts.joinToString(" · ")
    }

    private fun emptyText(context: Context, server: String, snapshot: AgendaSnapshot?): String = when {
        server.isBlank() -> context.getString(R.string.widget_agenda_no_server)
        snapshot?.signedOut == true -> context.getString(R.string.widget_agenda_signed_out)
        snapshot == null -> context.getString(R.string.widget_agenda_loading)
        else -> context.getString(R.string.widget_agenda_empty)
    }

    private fun rowViews(
        context: Context,
        row: AgendaRow,
        calendars: Map<Long, AgendaCalendar>,
        formats: Formats,
        zone: ZoneId
    ): RemoteViews = when (row) {
        is AgendaRow.Heading -> RemoteViews(context.packageName, R.layout.widget_agenda_heading).also { views ->
            views.setTextViewText(R.id.agenda_heading, formats.heading(row.day))
            views.setOnClickFillInIntent(R.id.agenda_row, openFill(calendarPath(row.day, null)))
        }
        is AgendaRow.AllDay -> RemoteViews(context.packageName, R.layout.widget_agenda_allday).also { views ->
            // One name and a count: the line is read at a glance, and a list of
            // every holiday and bin day was the part nobody could read.
            val first = row.events.first()
            val text = SpannableStringBuilder()
            appendSwatch(text, colorOf(first, calendars))
            text.append(title(context, first))
            if (row.events.size > 1) {
                val more = row.events.size - 1
                text.append(" · ")
                text.append(context.resources.getQuantityString(R.plurals.widget_agenda_more_all_day, more, more))
            }
            views.setTextViewText(R.id.agenda_allday_text, text)
            views.setOnClickFillInIntent(R.id.agenda_row, openFill(calendarPath(row.day, null)))
        }
        is AgendaRow.Span -> RemoteViews(context.packageName, R.layout.widget_agenda_span).also { views ->
            val first = formats.dayName(row.first) + " " + row.first.dayOfMonth
            views.setTextViewText(
                R.id.agenda_span_dates,
                if (row.last == row.first) first else "$first – ${formats.dayName(row.last)} ${row.last.dayOfMonth}"
            )
            val text = SpannableStringBuilder()
            appendSwatch(text, colorOf(row.event, calendars))
            text.append(title(context, row.event))
            views.setTextViewText(R.id.agenda_span_title, text)
            views.setOnClickFillInIntent(R.id.agenda_row, openFill(calendarPath(row.first, row.event.id)))
        }
        is AgendaRow.Now -> RemoteViews(context.packageName, R.layout.widget_agenda_now)
        is AgendaRow.Event -> RemoteViews(context.packageName, R.layout.widget_agenda_event).also { views ->
            bindEvent(context, views, row, calendars, formats, zone)
            views.setOnClickFillInIntent(R.id.agenda_row, openFill(calendarPath(row.day, row.event.id)))
        }
    }

    /** longEventMinutes is how long an appointment runs before its line also
     * says when it ends: "MSP-Global until 17" tells a day apart from an hour. */
    private const val LONG_EVENT_MINUTES = 120L

    private fun bindEvent(
        context: Context,
        views: RemoteViews,
        row: AgendaRow.Event,
        calendars: Map<Long, AgendaCalendar>,
        formats: Formats,
        zone: ZoneId
    ) {
        val event = row.event
        val dayStart = AgendaWidgetModel.dayStart(row.day, zone)
        val startsBefore = event.start.isBefore(dayStart)
        // One time per line: when it starts, or for an appointment carried over
        // from yesterday, when it ends.
        views.setTextViewText(
            R.id.agenda_event_time,
            if (startsBefore) "–" + formats.time.format(event.end.atZone(zone)) else formats.time.format(event.start.atZone(zone))
        )

        val title = SpannableStringBuilder(title(context, event))
        if (event.declined) title.setSpan(StrikethroughSpan(), 0, title.length, Spanned.SPAN_EXCLUSIVE_EXCLUSIVE)
        val minutes = (event.end.toEpochMilli() - event.start.toEpochMilli()) / 60_000
        // An appointment running past midnight says so whatever its length:
        // "until 10" on a line that began at 20:00 would read as the morning
        // before it, so the end names its day.
        val endsLater = event.end.isAfter(AgendaWidgetModel.dayStart(row.day.plusDays(1), zone))
        if (!startsBefore && (minutes >= LONG_EVENT_MINUTES || endsLater)) {
            val end = event.end.atZone(zone)
            val until = if (endsLater) "${formats.dayName(end.toLocalDate())} ${formats.until(end)}" else formats.until(end)
            val from = title.length
            title.append("  ")
            title.append(context.getString(R.string.widget_agenda_until, until))
            title.setSpan(ForegroundColorSpan(if (row.past) COLOR_PAST else COLOR_MUTED), from, title.length, Spanned.SPAN_EXCLUSIVE_EXCLUSIVE)
        }
        views.setTextViewText(R.id.agenda_event_title, title)

        views.setInt(R.id.agenda_event_bar, "setColorFilter", if (row.past) COLOR_PAST else colorOf(event, calendars))
        views.setTextColor(R.id.agenda_event_title, if (row.past) COLOR_PAST else COLOR_TEXT)
        views.setTextColor(
            R.id.agenda_event_time,
            when {
                row.past -> COLOR_PAST
                row.clash -> COLOR_CLASH
                else -> COLOR_TEXT
            }
        )
    }

    private fun title(context: Context, event: AgendaEvent): String = when {
        event.summary.isNotBlank() -> event.summary.trim()
        event.travel.isNotBlank() -> context.getString(R.string.widget_agenda_travel)
        else -> context.getString(R.string.widget_agenda_no_title)
    }

    private fun colorOf(event: AgendaEvent, calendars: Map<Long, AgendaCalendar>): Int =
        calendars[event.calendarId]?.color ?: AgendaWidgetModel.FALLBACK_COLOR

    private fun appendSwatch(text: SpannableStringBuilder, color: Int) {
        val start = text.length
        text.append("■ ")
        text.setSpan(ForegroundColorSpan(color), start, start + 1, Spanned.SPAN_EXCLUSIVE_EXCLUSIVE)
    }

    private fun calendarPath(day: LocalDate, eventId: Long?): String {
        val path = "/calendar/$day"
        return if (eventId != null && eventId > 0) "$path?event=$eventId" else path
    }

    private fun openIntent(context: Context): Intent =
        Intent(context, WidgetOpenActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)

    private fun openPending(context: Context, requestCode: Int, path: String, inApp: Boolean): PendingIntent =
        PendingIntent.getActivity(
            context,
            requestCode,
            openIntent(context)
                .putExtra(EXTRA_PATH, path)
                .putExtra(WidgetOpenActivity.EXTRA_IN_APP, inApp),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE
        )

    /** The rows share one template and add their path to it, which is why it
     * has to stay mutable. */
    private fun rowTemplate(context: Context): PendingIntent =
        PendingIntent.getActivity(
            context,
            REQUEST_ROW,
            openIntent(context),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_MUTABLE
        )

    private fun openFill(path: String): Intent = Intent().putExtra(EXTRA_PATH, path)

    private fun refreshPending(context: Context): PendingIntent =
        PendingIntent.getBroadcast(
            context,
            REQUEST_REFRESH,
            Intent(context, AgendaWidgetProvider::class.java).setAction(ACTION_REFRESH),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE
        )

    private fun tickPending(context: Context): PendingIntent =
        PendingIntent.getBroadcast(
            context,
            REQUEST_TICK,
            Intent(context, AgendaWidgetProvider::class.java).setAction(ACTION_TICK),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE
        )

    /** scheduleTick redraws the widget when it next changes on its own. The
     * alarm is inexact and does not wake the device: a home screen nobody is
     * looking at need not be redrawn, and the launcher asks again when it is. */
    private fun scheduleTick(context: Context, at: Instant) {
        val alarms = context.getSystemService(AlarmManager::class.java) ?: return
        alarms.set(AlarmManager.RTC, at.toEpochMilli() + 1_000, tickPending(context))
    }

    private fun stableId(key: String): Long {
        // FNV-1a over the key: stable across processes, unlike hashCode of a
        // data class, and wide enough that a fortnight of rows does not collide.
        var hash = -0x340d631b7bdddcdbL
        for (char in key) {
            hash = hash xor char.code.toLong()
            hash *= 0x100000001b3L
        }
        return hash
    }

    private fun currentLocale(context: Context): Locale =
        context.resources.configuration.locales.get(0) ?: Locale.getDefault()

    private class Formats(context: Context, private val locale: Locale) {
        private val twentyFourHour = DateFormat.is24HourFormat(context)
        val time: DateTimeFormatter = bestPattern(
            if (twentyFourHour) "Hm" else "hm",
            DateTimeFormatter.ofLocalizedTime(FormatStyle.SHORT)
        )
        val title: DateTimeFormatter = bestPattern(
            "EEEdMMMM",
            DateTimeFormatter.ofLocalizedDate(FormatStyle.FULL)
        )
        private val dayHeading: DateTimeFormatter = bestPattern(
            "EEEEd",
            DateTimeFormatter.ofLocalizedDate(FormatStyle.FULL)
        )
        private val weekday: DateTimeFormatter = DateTimeFormatter.ofPattern("EEE", locale)

        /** heading names a day above its appointments: "DONNERSTAG, 22.". */
        fun heading(date: LocalDate): String = dayHeading.format(date).uppercase(locale)

        /** until is an end time as short as it reads: "17" for 17:00 on a
         * 24-hour clock, the full time otherwise -- midnight included, which
         * shortened would read "until 0". */
        fun until(at: ZonedDateTime): String =
            if (twentyFourHour && at.minute == 0 && at.hour != 0) at.hour.toString() else time.format(at)

        /** bestPattern asks ICU for the locale's pattern and falls back when
         * java.time cannot read it: ICU answers some locales with letters
         * (the day period "B") that older java.time rejects, and a widget that
         * throws while it is drawn is a widget that is never drawn again. */
        private fun bestPattern(skeleton: String, fallback: DateTimeFormatter): DateTimeFormatter = try {
            DateTimeFormatter.ofPattern(DateFormat.getBestDateTimePattern(locale, skeleton), locale)
        } catch (_: IllegalArgumentException) {
            fallback.withLocale(locale)
        }

        /** dayName is the short weekday in capitals, without the full stop
         * German puts after it: "DI", "MON". */
        fun dayName(date: LocalDate): String = weekday.format(date).trimEnd('.').uppercase(locale)
    }
}

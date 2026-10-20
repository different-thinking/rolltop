package app.rolltop.mobile

import android.app.AlarmManager
import android.app.PendingIntent
import android.appwidget.AppWidgetManager
import android.appwidget.AppWidgetProvider
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.graphics.Bitmap
import android.graphics.Canvas
import android.graphics.Paint
import android.graphics.RectF
import android.text.SpannableStringBuilder
import android.text.Spanned
import android.text.format.DateFormat
import android.text.style.ForegroundColorSpan
import android.text.style.StrikethroughSpan
import android.view.View
import android.widget.RemoteViews
import androidx.core.widget.RemoteViewsCompat
import java.time.Instant
import java.time.LocalDate
import java.time.ZoneId
import java.time.ZonedDateTime
import java.time.format.DateTimeFormatter
import java.util.Locale

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
 * A tap opens the calendar in the app on that day, with the event's dialog up
 * (`/calendar/<day>?event=<id>`); the plus opens a new event there.
 */
class AgendaWidgetProvider : AppWidgetProvider() {
    override fun onUpdate(context: Context, manager: AppWidgetManager, appWidgetIds: IntArray) {
        AgendaWidget.render(context, appWidgetIds)
        AgendaWidgetWorker.schedule(context)
        AgendaWidget.requestRefresh(context)
    }

    override fun onAppWidgetOptionsChanged(
        context: Context,
        manager: AppWidgetManager,
        appWidgetId: Int,
        newOptions: android.os.Bundle
    ) {
        AgendaWidget.render(context, intArrayOf(appWidgetId))
    }

    override fun onEnabled(context: Context) {
        AgendaWidgetWorker.schedule(context)
    }

    override fun onDisabled(context: Context) {
        AgendaWidgetWorker.cancel(context)
        AgendaWidget.cancelTick(context)
        AgendaWidgetStore.clear(context)
    }

    override fun onReceive(context: Context, intent: Intent) {
        when (intent.action) {
            AgendaWidget.ACTION_REFRESH -> AgendaWidget.requestRefresh(context)
            AgendaWidget.ACTION_TICK,
            Intent.ACTION_TIME_CHANGED,
            Intent.ACTION_TIMEZONE_CHANGED,
            Intent.ACTION_LOCALE_CHANGED -> AgendaWidget.render(context)
            else -> super.onReceive(context, intent)
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

    /** The layouts the list can hold; the count has to cover all of them. */
    private const val VIEW_TYPES = 7

    private const val COLOR_TEXT = 0xFFE3E3E3.toInt()
    private const val COLOR_MUTED = 0xFFA2A9B0.toInt()
    private const val COLOR_PAST = 0xFF80868B.toInt()
    private const val COLOR_ACCENT = 0xFFA8C7FA.toInt()
    private const val COLOR_TODAY_TEXT = 0xFF062E6F.toInt()
    private const val COLOR_CLASH = 0xFFF2B8B5.toInt()
    private const val COLOR_TRACK = 0xFF333537.toInt()

    fun hasWidgets(context: Context): Boolean = widgetIds(context).isNotEmpty()

    fun requestRefresh(context: Context) {
        if (hasWidgets(context)) AgendaWidgetWorker.refreshNow(context)
    }

    /** signedOut forgets the calendar of the user who just signed out, so the
     * home screen does not keep showing it to whoever holds the phone next. */
    fun signedOut(context: Context) {
        val server = RolltopPrefs.serverUrl(context)
        if (server.isBlank()) AgendaWidgetStore.clear(context) else AgendaWidgetStore.markSignedOut(context, server)
        if (hasWidgets(context)) render(context)
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
            views.setOnClickPendingIntent(R.id.agenda_header, openPending(context, REQUEST_OPEN, calendarPath(plan.today, null)))
            views.setOnClickPendingIntent(R.id.agenda_new, openPending(context, REQUEST_NEW, "${calendarPath(plan.today, null)}?new=1"))
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
        if (plan.todayOverlaps > 0) {
            parts += resources.getQuantityString(R.plurals.widget_agenda_overlaps, plan.todayOverlaps, plan.todayOverlaps)
        }
        if (snapshot.failedAt > 0 && snapshot.fetchedAt > 0) {
            val fetched = Instant.ofEpochMilli(snapshot.fetchedAt).atZone(ZoneId.systemDefault())
            parts += resources.getString(R.string.widget_agenda_stale, formats.time.format(fetched))
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
        is AgendaRow.Divider -> RemoteViews(context.packageName, R.layout.widget_agenda_divider)
        is AgendaRow.AllDay -> RemoteViews(context.packageName, R.layout.widget_agenda_allday).also { views ->
            bindDay(views, row.label, formats)
            val text = SpannableStringBuilder()
            row.events.forEachIndexed { index, event ->
                if (index > 0) text.append("   ")
                appendSwatch(text, colorOf(event, calendars))
                text.append(title(context, event))
            }
            views.setTextViewText(R.id.agenda_allday_text, text)
            views.setOnClickFillInIntent(R.id.agenda_row, openFill(calendarPath(row.day, null)))
        }
        is AgendaRow.Span -> RemoteViews(context.packageName, R.layout.widget_agenda_span).also { views ->
            val first = formats.dayName(row.first) + " " + row.first.dayOfMonth
            views.setTextViewText(
                R.id.agenda_span_dates,
                if (row.last == row.first) first else "$first\n– ${formats.dayName(row.last)} ${row.last.dayOfMonth}"
            )
            views.setTextColor(R.id.agenda_span_dates, if (row.isToday) COLOR_ACCENT else COLOR_MUTED)
            val text = SpannableStringBuilder()
            appendSwatch(text, colorOf(row.event, calendars))
            text.append(title(context, row.event))
            views.setTextViewText(R.id.agenda_span_title, text)
            views.setOnClickFillInIntent(R.id.agenda_row, openFill(calendarPath(row.first, row.event.id)))
        }
        is AgendaRow.Now -> RemoteViews(context.packageName, R.layout.widget_agenda_now).also { views ->
            bindDay(views, row.label, formats)
        }
        is AgendaRow.Single -> RemoteViews(context.packageName, R.layout.widget_agenda_event).also { views ->
            bindDay(views, row.label, formats)
            bindEvent(context, views, row.event, row.day, row.past, calendars, formats, zone)
            views.setOnClickFillInIntent(R.id.agenda_row, openFill(calendarPath(row.day, row.event.id)))
        }
        is AgendaRow.GroupHead -> RemoteViews(context.packageName, R.layout.widget_agenda_group_head).also { views ->
            bindDay(views, row.label, formats)
            val resources = context.resources
            val count = resources.getQuantityString(R.plurals.widget_agenda_together, row.count, row.count)
            val text = if (row.calendarCount > 1) {
                count + " · " + resources.getQuantityString(
                    R.plurals.widget_agenda_calendars,
                    row.calendarCount,
                    row.calendarCount
                )
            } else {
                count
            }
            views.setTextViewText(R.id.agenda_group_text, text)
            views.setTextViewText(
                R.id.agenda_group_time,
                formats.time.format(row.start.atZone(zone)) + "–" + formats.time.format(row.end.atZone(zone))
            )
            views.setOnClickFillInIntent(R.id.agenda_row, openFill(calendarPath(row.day, null)))
        }
        is AgendaRow.GroupMember -> RemoteViews(context.packageName, R.layout.widget_agenda_group_event).also { views ->
            bindEvent(context, views, row.event, row.day, row.past, calendars, formats, zone)
            views.setInt(
                R.id.agenda_group_box,
                "setBackgroundResource",
                if (row.last) R.drawable.widget_agenda_group_bottom else R.drawable.widget_agenda_group_middle
            )
            views.setImageViewBitmap(
                R.id.agenda_event_span,
                spanBar(row.from, row.to, if (row.past) COLOR_PAST else colorOf(row.event, calendars))
            )
            views.setOnClickFillInIntent(R.id.agenda_row, openFill(calendarPath(row.day, row.event.id)))
        }
    }

    private fun bindDay(views: RemoteViews, label: AgendaDayLabel?, formats: Formats) {
        if (label == null) {
            views.setViewVisibility(R.id.agenda_day_name, View.GONE)
            views.setViewVisibility(R.id.agenda_day_number, View.GONE)
            return
        }
        views.setViewVisibility(R.id.agenda_day_name, View.VISIBLE)
        views.setViewVisibility(R.id.agenda_day_number, View.VISIBLE)
        views.setTextViewText(R.id.agenda_day_name, formats.dayName(label.date))
        views.setTextViewText(R.id.agenda_day_number, label.date.dayOfMonth.toString())
        views.setTextColor(R.id.agenda_day_name, if (label.isToday) COLOR_ACCENT else COLOR_MUTED)
        views.setTextColor(R.id.agenda_day_number, if (label.isToday) COLOR_TODAY_TEXT else COLOR_TEXT)
        views.setInt(
            R.id.agenda_day_number,
            "setBackgroundResource",
            if (label.isToday) R.drawable.widget_agenda_today else 0
        )
    }

    private fun bindEvent(
        context: Context,
        views: RemoteViews,
        event: AgendaEvent,
        day: LocalDate,
        past: Boolean,
        calendars: Map<Long, AgendaCalendar>,
        formats: Formats,
        zone: ZoneId
    ) {
        val dayStart = AgendaWidgetModel.dayStart(day, zone)
        val dayEnd = AgendaWidgetModel.dayStart(day.plusDays(1), zone)
        val startsBefore = event.start.isBefore(dayStart)
        val endsAfter = event.end.isAfter(dayEnd)
        val first = if (startsBefore) {
            context.getString(R.string.widget_agenda_until, formats.time.format(event.end.atZone(zone)))
        } else {
            formats.time.format(event.start.atZone(zone))
        }
        val second = when {
            startsBefore -> ""
            endsAfter -> context.getString(R.string.widget_agenda_past_midnight)
            else -> formats.time.format(event.end.atZone(zone))
        }
        views.setTextViewText(R.id.agenda_event_start, first)
        views.setTextViewText(R.id.agenda_event_end, second)
        views.setViewVisibility(R.id.agenda_event_end, if (second.isEmpty()) View.GONE else View.VISIBLE)

        val title = SpannableStringBuilder(title(context, event))
        if (event.declined) title.setSpan(StrikethroughSpan(), 0, title.length, Spanned.SPAN_EXCLUSIVE_EXCLUSIVE)
        views.setTextViewText(R.id.agenda_event_title, title)

        val meta = ArrayList<String>()
        if (event.travelBeforeMinutes > 0 && !startsBefore) {
            val leave = event.start.minusSeconds(event.travelBeforeMinutes * 60L).atZone(zone)
            meta += context.getString(R.string.widget_agenda_leave_at, formats.time.format(leave))
        }
        if (event.location.isNotBlank()) {
            meta += event.location.trim()
        } else if (event.onlineMeeting) {
            meta += event.onlineMeetingProvider.takeIf { it.isNotBlank() }?.let(::meetingName)
                ?: context.getString(R.string.widget_agenda_online)
        }
        views.setTextViewText(R.id.agenda_event_meta, meta.joinToString(" · "))
        views.setViewVisibility(R.id.agenda_event_meta, if (meta.isEmpty()) View.GONE else View.VISIBLE)

        val color = colorOf(event, calendars)
        views.setInt(R.id.agenda_event_bar, "setColorFilter", if (past) COLOR_PAST else color)
        views.setTextColor(R.id.agenda_event_title, if (past) COLOR_PAST else COLOR_TEXT)
        views.setTextColor(R.id.agenda_event_start, if (past) COLOR_PAST else COLOR_TEXT)
        views.setTextColor(R.id.agenda_event_end, if (past) COLOR_PAST else COLOR_MUTED)
        views.setTextColor(R.id.agenda_event_meta, if (past) COLOR_PAST else COLOR_MUTED)
    }

    private fun title(context: Context, event: AgendaEvent): String = when {
        event.summary.isNotBlank() -> event.summary.trim()
        event.travel.isNotBlank() -> context.getString(R.string.widget_agenda_travel)
        else -> context.getString(R.string.widget_agenda_no_title)
    }

    private fun meetingName(provider: String): String = when (provider.lowercase(Locale.ROOT)) {
        "teams", "teamsforbusiness" -> "Teams"
        "meet", "hangoutsmeet", "google_meet" -> "Google Meet"
        else -> provider
    }

    private fun colorOf(event: AgendaEvent, calendars: Map<Long, AgendaCalendar>): Int =
        calendars[event.calendarId]?.color ?: AgendaWidgetModel.FALLBACK_COLOR

    private fun appendSwatch(text: SpannableStringBuilder, color: Int) {
        val start = text.length
        text.append("■ ")
        text.setSpan(ForegroundColorSpan(color), start, start + 1, Spanned.SPAN_EXCLUSIVE_EXCLUSIVE)
    }

    /** spanBar draws where one appointment sits inside its box's span. It is a
     * bitmap because a layout cannot be given a weight from a RemoteViews. */
    private fun spanBar(from: Float, to: Float, color: Int): Bitmap {
        val width = 240
        val height = 8
        val bitmap = Bitmap.createBitmap(width, height, Bitmap.Config.ARGB_8888)
        val canvas = Canvas(bitmap)
        val paint = Paint(Paint.ANTI_ALIAS_FLAG)
        val radius = height / 2f
        paint.color = COLOR_TRACK
        canvas.drawRoundRect(RectF(0f, 0f, width.toFloat(), height.toFloat()), radius, radius, paint)
        val left = (from * width).coerceIn(0f, width.toFloat())
        val right = (to * width).coerceIn(left + height, width.toFloat())
        paint.color = color
        canvas.drawRoundRect(RectF(left.coerceAtMost(right - height), 0f, right, height.toFloat()), radius, radius, paint)
        return bitmap
    }

    private fun calendarPath(day: LocalDate, eventId: Long?): String {
        val path = "/calendar/$day"
        return if (eventId != null && eventId > 0) "$path?event=$eventId" else path
    }

    private fun openIntent(context: Context): Intent =
        Intent(context, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)

    private fun openPending(context: Context, requestCode: Int, path: String): PendingIntent =
        PendingIntent.getActivity(
            context,
            requestCode,
            openIntent(context).putExtra(EXTRA_PATH, path),
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
        val time: DateTimeFormatter = DateTimeFormatter.ofPattern(
            DateFormat.getBestDateTimePattern(locale, if (DateFormat.is24HourFormat(context)) "Hm" else "hm"),
            locale
        )
        val title: DateTimeFormatter = DateTimeFormatter.ofPattern(
            DateFormat.getBestDateTimePattern(locale, "EEEEdMMMM"),
            locale
        )
        private val weekday: DateTimeFormatter = DateTimeFormatter.ofPattern("EEE", locale)

        /** dayName is the short weekday in capitals, without the full stop
         * German puts after it: "DI", "MON". */
        fun dayName(date: LocalDate): String = weekday.format(date).trimEnd('.').uppercase(locale)
    }
}

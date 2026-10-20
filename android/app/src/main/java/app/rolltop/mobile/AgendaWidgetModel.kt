package app.rolltop.mobile

import org.json.JSONArray
import org.json.JSONObject
import java.time.Instant
import java.time.LocalDate
import java.time.OffsetDateTime
import java.time.ZoneId
import java.time.ZoneOffset
import java.time.ZonedDateTime

/**
 * The agenda on the home screen, as arithmetic. The web calendar's phone agenda
 * (`frontend/src/features/calendar/mobileModel.ts`) decides what a day holds,
 * and this file answers the same questions the same way, so the widget and the
 * app cannot disagree about a day: an all-day event counts in the dates Google
 * named, read in UTC; a timed event that fills twelve hours of a day is drawn
 * with that day's all-day events; and appointments whose time overlaps --
 * counting the journey around them -- are kept together as one entry.
 *
 * What the widget adds is that it shows only what is there. A day with nothing
 * on it is left out rather than drawn empty, and an all-day event is named once,
 * on the first day the widget shows it, instead of on every day it covers: a
 * week's holiday is one line, not seven.
 *
 * Everything here is pure, so it is tested on the JVM without a device.
 */
internal data class AgendaCalendar(val id: Long, val name: String, val color: Int)

internal data class AgendaEvent(
    val id: Long,
    val calendarId: Long,
    val summary: String,
    val location: String,
    val start: Instant,
    val end: Instant,
    val allDay: Boolean,
    /** declined is an invitation the reader turned down or an event the
     * organizer cancelled: still listed, as the app lists it, but struck out. */
    val declined: Boolean,
    val onlineMeeting: Boolean,
    val onlineMeetingProvider: String,
    val travelBeforeMinutes: Int,
    val travelAfterMinutes: Int,
    /** travel is "before" or "after" for a travel block the server could not
     * fold into an event; it is drawn on its own rather than hidden. */
    val travel: String
)

/** AgendaDayLabel heads the first row of a day in the widget's date column. */
internal data class AgendaDayLabel(val date: LocalDate, val isToday: Boolean)

internal sealed class AgendaRow {
    /** key identifies a row across renders, so the launcher keeps its place. */
    abstract val key: String

    /** Divider separates two days. */
    data class Divider(override val key: String) : AgendaRow()

    /** AllDay is one line naming a day's all-day events above its appointments. */
    data class AllDay(val label: AgendaDayLabel?, val day: LocalDate, val events: List<AgendaEvent>) : AgendaRow() {
        override val key = "allday-$day"
    }

    /** Span is an all-day event on a day that has no appointments, with the
     * dates it covers as its label: a holiday reads "Sat 24 - Sat 31" once. */
    data class Span(val first: LocalDate, val last: LocalDate, val isToday: Boolean, val event: AgendaEvent) : AgendaRow() {
        override val key = "span-$first-${event.id}"
    }

    /** Now is the line drawn before the first appointment that has not begun. */
    data class Now(val label: AgendaDayLabel?, val at: Instant) : AgendaRow() {
        override val key = "now"
    }

    /** Single is an appointment that overlaps nothing. */
    data class Single(
        val label: AgendaDayLabel?,
        val day: LocalDate,
        val event: AgendaEvent,
        val past: Boolean
    ) : AgendaRow() {
        override val key = "event-$day-${event.id}"
    }

    /** GroupHead opens a box of appointments that run at the same time. */
    data class GroupHead(
        val label: AgendaDayLabel?,
        val day: LocalDate,
        val count: Int,
        val calendarCount: Int,
        val start: Instant,
        val end: Instant
    ) : AgendaRow() {
        override val key = "group-$day-${start.epochSecond}"
    }

    /** GroupMember is one appointment in that box. from and to are where its
     * own time sits inside the box's span, 0 to 1, which is what the bar under
     * it draws: three appointments at once read as a clash, and the bars say
     * how much of one. */
    data class GroupMember(
        val day: LocalDate,
        val event: AgendaEvent,
        val past: Boolean,
        val last: Boolean,
        val from: Float,
        val to: Float
    ) : AgendaRow() {
        override val key = "member-$day-${event.id}"
    }
}

internal data class AgendaPlan(
    val today: LocalDate,
    /** todayEvents counts the appointments of today, all-day events aside. */
    val todayEvents: Int,
    /** todayOverlaps counts today's boxes of appointments at the same time. */
    val todayOverlaps: Int,
    val rows: List<AgendaRow>,
    /** nextChange is the next moment the widget looks different without new
     * data: an appointment beginning or ending, or midnight. */
    val nextChange: Instant
)

internal object AgendaWidgetModel {
    /** days is how far ahead the widget reads, today included. */
    const val DAYS = 14L

    /** wholeDayTimedMinutes matches the app's phone views: an absence entered
     * as 00:00 to 00:00 is a timed event, and drawn as one it takes the room of
     * a whole day's appointments. */
    const val WHOLE_DAY_TIMED_MINUTES = 12 * 60L

    /** maxRows bounds what one update hands the launcher; a collection update
     * travels in one parcel. */
    const val MAX_ROWS = 160

    /** fallbackColor is the dark theme's accent, for a calendar with no colour. */
    const val FALLBACK_COLOR = 0xFFA8C7FA.toInt()

    fun parseCalendars(array: JSONArray?): Map<Long, AgendaCalendar> {
        if (array == null) return emptyMap()
        val calendars = LinkedHashMap<Long, AgendaCalendar>()
        for (index in 0 until array.length()) {
            val item = array.optJSONObject(index) ?: continue
            val id = item.optLong("id", 0)
            if (id <= 0) continue
            calendars[id] = AgendaCalendar(
                id = id,
                name = item.optString("name", ""),
                color = parseColor(item.optString("color", "")) ?: FALLBACK_COLOR
            )
        }
        return calendars
    }

    fun parseEvents(array: JSONArray?): List<AgendaEvent> {
        if (array == null) return emptyList()
        val events = ArrayList<AgendaEvent>(array.length())
        for (index in 0 until array.length()) {
            val item = array.optJSONObject(index) ?: continue
            val start = parseInstant(item.optString("start_at", "")) ?: continue
            val end = parseInstant(item.optString("end_at", "")) ?: continue
            events += AgendaEvent(
                id = item.optLong("id", 0),
                calendarId = item.optLong("calendar_id", 0),
                summary = item.optString("summary", ""),
                location = item.optString("location", ""),
                start = start,
                end = if (end.isBefore(start)) start else end,
                allDay = item.optBoolean("all_day", false),
                declined = item.optString("my_response", "") == "declined" ||
                    item.optString("status", "") == "cancelled",
                onlineMeeting = item.optBoolean("online_meeting", false),
                onlineMeetingProvider = item.optString("online_meeting_provider", ""),
                travelBeforeMinutes = item.optInt("travel_before_minutes", 0).coerceAtLeast(0),
                travelAfterMinutes = item.optInt("travel_after_minutes", 0).coerceAtLeast(0),
                travel = item.optString("travel", "")
            )
        }
        return events
    }

    /** trimEvent keeps the fields the widget reads, so the copy kept on the
     * device holds no notes, guest list or meeting link it never shows. */
    fun trimEvent(item: JSONObject): JSONObject = JSONObject().apply {
        for (name in EVENT_FIELDS) {
            if (item.has(name)) put(name, item.get(name))
        }
    }

    /** trimCalendar does the same for a calendar. */
    fun trimCalendar(item: JSONObject): JSONObject = JSONObject().apply {
        for (name in CALENDAR_FIELDS) {
            if (item.has(name)) put(name, item.get(name))
        }
    }

    private val EVENT_FIELDS = listOf(
        "id", "calendar_id", "summary", "location", "status", "start_at", "end_at", "all_day",
        "my_response", "online_meeting", "online_meeting_provider",
        "travel_before_minutes", "travel_after_minutes", "travel"
    )
    private val CALENDAR_FIELDS = listOf("id", "name", "color")

    fun plan(events: List<AgendaEvent>, now: ZonedDateTime): AgendaPlan {
        val zone = now.zone
        val today = now.toLocalDate()
        val nowInstant = now.toInstant()
        val rows = ArrayList<AgendaRow>()
        val named = HashSet<Long>()
        var todayEvents = 0
        var todayOverlaps = 0
        var nextChange = dayStart(today.plusDays(1), zone)

        for (offset in 0 until DAYS) {
            if (rows.size >= MAX_ROWS) break
            val day = today.plusDays(offset)
            val isToday = offset == 0L
            val start = dayStart(day, zone)
            val end = dayStart(day.plusDays(1), zone)

            val fresh = allDayEvents(events, day, zone).filter { named.add(it.id) }
            val timed = events.filter { !it.allDay && touches(it, start, end) && !fillsDay(it, start, end) }
            if (fresh.isEmpty() && timed.isEmpty()) continue

            if (rows.isNotEmpty()) rows += AgendaRow.Divider("divider-$day")
            if (timed.isEmpty()) {
                fresh.forEachIndexed { index, event ->
                    rows += AgendaRow.Span(day, lastDay(event, zone).coerceAtLeast(day), isToday && index == 0, event)
                }
                continue
            }

            var label: AgendaDayLabel? = AgendaDayLabel(day, isToday)
            fun takeLabel(): AgendaDayLabel? = label.also { label = null }

            if (fresh.isNotEmpty()) rows += AgendaRow.AllDay(takeLabel(), day, fresh)
            val groups = groupOverlapping(timed, start, end)
            if (isToday) {
                todayEvents = timed.size
                todayOverlaps = groups.count { it.events.size > 1 }
                timed.flatMap { listOf(it.start, it.end) }
                    .filter { it.isAfter(nowInstant) && it.isBefore(nextChange) }
                    .minOrNull()
                    ?.let { nextChange = it }
            }
            var nowDrawn = !isToday
            for (group in groups) {
                if (!nowDrawn && group.eventStart.isAfter(nowInstant)) {
                    rows += AgendaRow.Now(takeLabel(), nowInstant)
                    nowDrawn = true
                }
                if (group.events.size == 1) {
                    val event = group.events[0]
                    rows += AgendaRow.Single(takeLabel(), day, event, past = !event.end.isAfter(nowInstant))
                    continue
                }
                val boxStart = maxOf(group.eventStart, start)
                val boxEnd = minOf(group.eventEnd, end)
                rows += AgendaRow.GroupHead(
                    takeLabel(),
                    day,
                    group.events.size,
                    group.events.map { it.calendarId }.distinct().size,
                    boxStart,
                    boxEnd
                )
                val span = (boxEnd.toEpochMilli() - boxStart.toEpochMilli()).coerceAtLeast(1).toFloat()
                group.events.forEachIndexed { index, event ->
                    val from = (maxOf(event.start, boxStart).toEpochMilli() - boxStart.toEpochMilli()) / span
                    val to = (minOf(event.end, boxEnd).toEpochMilli() - boxStart.toEpochMilli()) / span
                    rows += AgendaRow.GroupMember(
                        day,
                        event,
                        past = !event.end.isAfter(nowInstant),
                        last = index == group.events.size - 1,
                        from = from.coerceIn(0f, 1f),
                        to = to.coerceIn(0f, 1f)
                    )
                }
            }
        }
        return AgendaPlan(today, todayEvents, todayOverlaps, rows.take(MAX_ROWS), nextChange)
    }

    /** calendarTarget resolves what a tap on the widget opens. Only calendar
     * pages of the configured server qualify: the path rides in an intent, and
     * an intent is not where the app takes a destination from on trust. */
    fun calendarTarget(baseUrl: String, path: String): String? {
        if (!path.startsWith("/calendar/") && path != "/calendar") return null
        val resolved = RolltopPrefs.resolveInternalUrl(baseUrl, path) ?: return null
        // Asked again of the normalized address: a browser removes dot
        // segments, so "/calendar/../settings" passes the prefix and lands
        // elsewhere.
        val normalized = try {
            java.net.URI(resolved).normalize()
        } catch (_: Exception) {
            return null
        }
        val normalizedPath = normalized.path.orEmpty()
        if (!normalizedPath.startsWith("/calendar/") && normalizedPath != "/calendar") return null
        return normalized.toASCIIString()
    }

    fun dayStart(day: LocalDate, zone: ZoneId): Instant = day.atStartOfDay(zone).toInstant()

    /** allDayEvents is what the app draws above a day: its all-day events,
     * longest first, then the timed events that fill it. */
    fun allDayEvents(events: List<AgendaEvent>, day: LocalDate, zone: ZoneId): List<AgendaEvent> {
        val start = dayStart(day, zone)
        val end = dayStart(day.plusDays(1), zone)
        val allDay = events
            .filter { it.allDay && allDayCovers(it, day) }
            .sortedWith(compareByDescending<AgendaEvent> { allDayLength(it) }.thenBy { it.summary })
        val filling = events
            .filter { !it.allDay && touches(it, start, end) && fillsDay(it, start, end) }
            .sortedWith(compareBy<AgendaEvent> { it.start }.thenBy { it.summary })
        return allDay + filling
    }

    /** allDayCovers compares plain dates: the event's in UTC, where Google's
     * dates were anchored, against the reader's day. End dates are exclusive,
     * and a pair whose end is not after its start is still one day. */
    fun allDayCovers(event: AgendaEvent, day: LocalDate): Boolean {
        val first = utcDate(event.start)
        val end = utcDate(event.end)
        if (!end.isAfter(first)) return day == first
        return !day.isBefore(first) && day.isBefore(end)
    }

    /** lastDay is the last day an event is drawn on. */
    fun lastDay(event: AgendaEvent, zone: ZoneId): LocalDate {
        if (event.allDay) {
            val first = utcDate(event.start)
            val end = utcDate(event.end)
            return if (end.isAfter(first)) end.minusDays(1) else first
        }
        val last = if (event.end.isAfter(event.start)) event.end.minusNanos(1) else event.start
        return last.atZone(zone).toLocalDate()
    }

    fun touches(event: AgendaEvent, dayStart: Instant, dayEnd: Instant): Boolean =
        event.start.isBefore(dayEnd) && maxOf(event.end, event.start).isAfter(dayStart)

    /** fillsDay is measured per day, against the day's own length: the day the
     * clocks change has 23 or 25 hours. */
    fun fillsDay(event: AgendaEvent, dayStart: Instant, dayEnd: Instant): Boolean {
        if (event.allDay) return false
        val covered = minOf(event.end, dayEnd).toEpochMilli() - maxOf(event.start, dayStart).toEpochMilli()
        val dayMinutes = (dayEnd.toEpochMilli() - dayStart.toEpochMilli()) / 60_000
        return covered / 60_000 >= minOf(WHOLE_DAY_TIMED_MINUTES, dayMinutes)
    }

    internal data class Group(val eventStart: Instant, val eventEnd: Instant, val events: List<AgendaEvent>)

    /**
     * groupOverlapping joins appointments whose busy time overlaps, the journey
     * there and back included, the way the app's agenda does. What a group says
     * on screen is the appointments' own time; the journeys only decide what is
     * grouped.
     */
    fun groupOverlapping(events: List<AgendaEvent>, dayStart: Instant, dayEnd: Instant): List<Group> {
        data class Busy(val event: AgendaEvent, val start: Instant, val end: Instant)
        val spans = events.map { event ->
            Busy(
                event,
                maxOf(event.start.minusSeconds(event.travelBeforeMinutes * 60L), dayStart),
                minOf(maxOf(event.end, event.start).plusSeconds(event.travelAfterMinutes * 60L), dayEnd)
            )
        }.sortedWith(
            compareBy<Busy> { it.start }.thenByDescending { it.end }.thenBy { it.event.summary }
        )
        val groups = ArrayList<Group>()
        var busyEnd = Instant.MIN
        var members = ArrayList<AgendaEvent>()
        var eventStart = Instant.MIN
        var eventEnd = Instant.MIN
        for (span in spans) {
            if (members.isNotEmpty() && span.start.isBefore(busyEnd)) {
                members += span.event
                busyEnd = maxOf(busyEnd, span.end)
                eventStart = minOf(eventStart, span.event.start)
                eventEnd = maxOf(eventEnd, span.event.end)
                continue
            }
            if (members.isNotEmpty()) groups += Group(eventStart, eventEnd, members)
            members = arrayListOf(span.event)
            busyEnd = span.end
            eventStart = span.event.start
            eventEnd = span.event.end
        }
        if (members.isNotEmpty()) groups += Group(eventStart, eventEnd, members)
        return groups
    }

    /** parseColor reads the CSS hex colours calendars carry: #rgb, #rrggbb and
     * #rrggbbaa. Anything else answers null and the caller falls back. */
    fun parseColor(value: String): Int? {
        val hex = value.trim().removePrefix("#")
        if (hex.isEmpty() || hex.length !in setOf(3, 6, 8) || !hex.all { it.isDigit() || it.lowercaseChar() in 'a'..'f' }) {
            return null
        }
        return when (hex.length) {
            3 -> (0xFF000000.toInt() or hex.map { "$it$it" }.joinToString("").toInt(16))
            6 -> (0xFF000000.toInt() or hex.toInt(16))
            else -> {
                val rgb = hex.substring(0, 6).toInt(16)
                val alpha = hex.substring(6, 8).toInt(16)
                (alpha shl 24) or rgb
            }
        }
    }

    private fun allDayLength(event: AgendaEvent): Long {
        val days = (utcDate(event.end).toEpochDay() - utcDate(event.start).toEpochDay())
        return days.coerceAtLeast(1)
    }

    private fun utcDate(instant: Instant): LocalDate = instant.atOffset(ZoneOffset.UTC).toLocalDate()

    private fun parseInstant(value: String): Instant? = try {
        OffsetDateTime.parse(value).toInstant()
    } catch (_: Exception) {
        null
    }
}

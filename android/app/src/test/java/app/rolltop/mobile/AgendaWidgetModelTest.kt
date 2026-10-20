package app.rolltop.mobile

import org.json.JSONArray
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import java.time.Instant
import java.time.LocalDate
import java.time.ZoneId
import java.time.ZonedDateTime

class AgendaWidgetModelTest {
    private val berlin = ZoneId.of("Europe/Berlin")

    // Tuesday 20 October 2026, 08:40 in Berlin (UTC+2).
    private val now = ZonedDateTime.of(2026, 10, 20, 8, 40, 0, 0, berlin)

    private var nextId = 1L

    private fun timed(
        summary: String,
        start: String,
        end: String,
        calendarId: Long = 1,
        travelBefore: Int = 0,
        travelAfter: Int = 0
    ) = AgendaEvent(
        id = nextId++,
        calendarId = calendarId,
        summary = summary,
        location = "",
        start = ZonedDateTime.parse("${start}+02:00[Europe/Berlin]").toInstant(),
        end = ZonedDateTime.parse("${end}+02:00[Europe/Berlin]").toInstant(),
        allDay = false,
        declined = false,
        onlineMeeting = false,
        onlineMeetingProvider = "",
        travelBeforeMinutes = travelBefore,
        travelAfterMinutes = travelAfter,
        travel = ""
    )

    private fun allDay(summary: String, first: String, endExclusive: String, calendarId: Long = 2) = AgendaEvent(
        id = nextId++,
        calendarId = calendarId,
        summary = summary,
        location = "",
        start = Instant.parse("${first}T00:00:00Z"),
        end = Instant.parse("${endExclusive}T00:00:00Z"),
        allDay = true,
        declined = false,
        onlineMeeting = false,
        onlineMeetingProvider = "",
        travelBeforeMinutes = 0,
        travelAfterMinutes = 0,
        travel = ""
    )

    @Test
    fun daysWithNothingOnThemAreLeftOut() {
        val plan = AgendaWidgetModel.plan(
            listOf(
                timed("Standup", "2026-10-20T09:00", "2026-10-20T09:15"),
                timed("Review", "2026-10-23T14:00", "2026-10-23T15:00")
            ),
            now
        )

        val days = plan.rows.filterIsInstance<AgendaRow.Single>().map { it.day }
        assertEquals(listOf(LocalDate.of(2026, 10, 20), LocalDate.of(2026, 10, 23)), days)
        assertEquals(1, plan.rows.count { it is AgendaRow.Divider })
    }

    @Test
    fun aMultiDayAllDayEventIsNamedOnceOnTheFirstDayShown() {
        val holiday = allDay("Herbstferien", "2026-10-12", "2026-10-24")
        val plan = AgendaWidgetModel.plan(
            listOf(
                holiday,
                timed("Standup", "2026-10-20T09:00", "2026-10-20T09:15"),
                timed("Laufen", "2026-10-21T07:00", "2026-10-21T08:00")
            ),
            now
        )

        val allDayRows = plan.rows.filterIsInstance<AgendaRow.AllDay>()
        assertEquals(1, allDayRows.size)
        assertEquals(LocalDate.of(2026, 10, 20), allDayRows[0].day)
        assertEquals(listOf(holiday), allDayRows[0].events)
    }

    @Test
    fun aDayWithOnlyAnAllDayEventShowsItsDatesOnce() {
        val trip = allDay("Urlaub Gardasee", "2026-10-24", "2026-11-01")
        val plan = AgendaWidgetModel.plan(listOf(trip), now)

        val spans = plan.rows.filterIsInstance<AgendaRow.Span>()
        assertEquals(1, spans.size)
        assertEquals(LocalDate.of(2026, 10, 24), spans[0].first)
        assertEquals(LocalDate.of(2026, 10, 31), spans[0].last)
        assertEquals(1, plan.rows.size)
    }

    @Test
    fun anAllDayEventKeepsItsDateWestOfUtc() {
        val pacific = ZonedDateTime.of(2026, 10, 20, 8, 40, 0, 0, ZoneId.of("America/Los_Angeles"))
        val holiday = allDay("Holiday", "2026-10-21", "2026-10-22")

        val plan = AgendaWidgetModel.plan(listOf(holiday), pacific)

        val span = plan.rows.single() as AgendaRow.Span
        assertEquals(LocalDate.of(2026, 10, 21), span.first)
        assertEquals(LocalDate.of(2026, 10, 21), span.last)
    }

    @Test
    fun aTimedEventFillingTheDayIsDrawnWithTheAllDayEvents() {
        val absence = timed("Abwesend", "2026-10-20T00:00", "2026-10-21T00:00")
        val plan = AgendaWidgetModel.plan(
            listOf(absence, timed("Standup", "2026-10-20T09:00", "2026-10-20T09:15")),
            now
        )

        assertEquals(listOf(absence), plan.rows.filterIsInstance<AgendaRow.AllDay>().single().events)
        assertEquals(1, plan.todayEvents)
    }

    @Test
    fun appointmentsAtTheSameTimeAreOneBoxWithTheirShareOfIt() {
        val review = timed("Sprint-Review", "2026-10-20T10:00", "2026-10-20T11:00", calendarId = 1)
        val kita = timed("Elterngespräch Kita", "2026-10-20T10:30", "2026-10-20T11:30", calendarId = 2)
        val call = timed("Rückruf Steuerberater", "2026-10-20T11:00", "2026-10-20T11:15", calendarId = 3)
        val dentist = timed("Zahnarzt", "2026-10-20T16:30", "2026-10-20T17:15", calendarId = 3)

        val plan = AgendaWidgetModel.plan(listOf(dentist, call, kita, review), now)

        val head = plan.rows.filterIsInstance<AgendaRow.GroupHead>().single()
        assertEquals(3, head.count)
        assertEquals(3, head.calendarCount)
        assertEquals(review.start, head.start)
        assertEquals(kita.end, head.end)
        val members = plan.rows.filterIsInstance<AgendaRow.GroupMember>()
        assertEquals(listOf(review, kita, call), members.map { it.event })
        assertEquals(listOf(false, false, true), members.map { it.last })
        assertEquals(0f, members[0].from, 0.001f)
        assertEquals(2f / 3f, members[0].to, 0.001f)
        assertEquals(1f / 3f, members[1].from, 0.001f)
        assertEquals(1f, members[1].to, 0.001f)
        assertEquals(2f / 3f, members[2].from, 0.001f)
        assertEquals(5f / 6f, members[2].to, 0.001f)
        assertEquals(4, plan.todayEvents)
        assertEquals(1, plan.todayOverlaps)
        assertEquals(dentist, plan.rows.filterIsInstance<AgendaRow.Single>().single().event)
    }

    @Test
    fun theJourneyCountsWhenDecidingWhatOverlaps() {
        val lunch = timed("Mittag", "2026-10-20T12:00", "2026-10-20T13:00")
        val client = timed("Kunde", "2026-10-20T13:30", "2026-10-20T14:30", travelBefore = 45)

        val plan = AgendaWidgetModel.plan(listOf(lunch, client), now)

        assertEquals(2, plan.rows.filterIsInstance<AgendaRow.GroupMember>().size)
    }

    @Test
    fun theNowLineComesBeforeTheFirstAppointmentNotYetBegun() {
        val early = timed("Laufen", "2026-10-20T07:00", "2026-10-20T08:00")
        val standup = timed("Standup", "2026-10-20T09:00", "2026-10-20T09:15")

        val plan = AgendaWidgetModel.plan(listOf(early, standup), now)

        val kinds = plan.rows.map { it::class.simpleName }
        assertEquals(listOf("Single", "Now", "Single"), kinds)
        val first = plan.rows[0] as AgendaRow.Single
        assertTrue(first.past)
        assertTrue(first.label?.isToday == true)
        assertNull((plan.rows[2] as AgendaRow.Single).label)
        assertFalse((plan.rows[2] as AgendaRow.Single).past)
        assertEquals(standup.start, plan.nextChange)
    }

    @Test
    fun withNothingLeftTodayTheNextChangeIsMidnight() {
        val plan = AgendaWidgetModel.plan(listOf(timed("Laufen", "2026-10-20T07:00", "2026-10-20T08:00")), now)

        assertEquals(ZonedDateTime.of(2026, 10, 21, 0, 0, 0, 0, berlin).toInstant(), plan.nextChange)
        assertTrue(plan.rows.none { it is AgendaRow.Now })
    }

    @Test
    fun eventsParseFromTheCalendarRouteAndKeepOnlyWhatTheWidgetShows() {
        val raw = JSONObject()
            .put("id", 7)
            .put("calendar_id", 3)
            .put("summary", "Termin")
            .put("description", "private notes")
            .put("attendees", JSONArray().put(JSONObject().put("email", "guest@example.com")))
            .put("start_at", "2026-10-20T09:00:00+02:00")
            .put("end_at", "2026-10-20T10:00:00Z")
            .put("all_day", false)
            .put("my_response", "declined")
            .put("travel_before_minutes", 30)

        val trimmed = AgendaWidgetModel.trimEvent(raw)
        assertFalse(trimmed.has("description"))
        assertFalse(trimmed.has("attendees"))

        val event = AgendaWidgetModel.parseEvents(JSONArray().put(trimmed)).single()
        assertEquals(7L, event.id)
        assertEquals(Instant.parse("2026-10-20T07:00:00Z"), event.start)
        assertEquals(Instant.parse("2026-10-20T10:00:00Z"), event.end)
        assertTrue(event.declined)
        assertEquals(30, event.travelBeforeMinutes)
    }

    @Test
    fun anEventWithUnreadableTimesIsSkipped() {
        val raw = JSONArray().put(JSONObject().put("id", 1).put("start_at", "soon").put("end_at", ""))

        assertTrue(AgendaWidgetModel.parseEvents(raw).isEmpty())
    }

    @Test
    fun calendarColoursParseAndFallBack() {
        assertEquals(0xFF9FE1E7.toInt(), AgendaWidgetModel.parseColor("#9fe1e7"))
        assertEquals(0xFFAABBCC.toInt(), AgendaWidgetModel.parseColor("#abc"))
        assertEquals(0x809FE1E7.toInt(), AgendaWidgetModel.parseColor("#9fe1e780"))
        assertNull(AgendaWidgetModel.parseColor("blue"))
        assertNull(AgendaWidgetModel.parseColor("#12345"))

        val calendars = AgendaWidgetModel.parseCalendars(
            JSONArray().put(JSONObject().put("id", 4).put("name", "Arbeit").put("color", ""))
        )
        assertEquals(AgendaWidgetModel.FALLBACK_COLOR, calendars.getValue(4).color)
    }

    @Test
    fun aTapOnlyOpensCalendarPagesOfTheConfiguredServer() {
        val base = "https://mail.example.com"

        assertEquals(
            "https://mail.example.com/calendar/2026-10-20?event=7",
            AgendaWidgetModel.calendarTarget(base, "/calendar/2026-10-20?event=7")
        )
        assertNull(AgendaWidgetModel.calendarTarget(base, "/mail/relevant"))
        assertNull(AgendaWidgetModel.calendarTarget(base, "/calendar/../settings"))
        assertNull(AgendaWidgetModel.calendarTarget(base, "//evil.example/calendar/x"))
        assertNull(AgendaWidgetModel.calendarTarget(base, "https://evil.example/calendar/x"))
        assertNull(AgendaWidgetModel.calendarTarget("", "/calendar/2026-10-20"))
    }
}

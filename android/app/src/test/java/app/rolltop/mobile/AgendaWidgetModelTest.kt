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

        val days = plan.rows.filterIsInstance<AgendaRow.Event>().map { it.day }
        assertEquals(listOf(LocalDate.of(2026, 10, 20), LocalDate.of(2026, 10, 23)), days)
        // Today is the widget's own title; only the days after it are headed.
        assertEquals(listOf(LocalDate.of(2026, 10, 23)), plan.rows.filterIsInstance<AgendaRow.Heading>().map { it.day })
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
    fun appointmentsThatBeginWhileAnotherRunsAreMarkedNotBoxed() {
        val block = timed("Buchhaltung", "2026-10-21T13:00", "2026-10-21T16:00", calendarId = 1)
        val call = timed("comTeam", "2026-10-21T13:30", "2026-10-21T15:15", calendarId = 2)
        val talk = timed("HaloPSA", "2026-10-21T14:35", "2026-10-21T15:05", calendarId = 2)
        val climbing = timed("Klettern", "2026-10-21T16:00", "2026-10-21T19:00", calendarId = 3)

        val plan = AgendaWidgetModel.plan(listOf(climbing, talk, call, block), now)

        val rows = plan.rows.filterIsInstance<AgendaRow.Event>()
        assertEquals(listOf(block, call, talk, climbing), rows.map { it.event })
        // The block itself is drawn plainly; what begins inside it is marked,
        // and an appointment that starts as it ends overlaps nothing.
        assertEquals(listOf(false, true, true, false), rows.map { it.clash })
    }

    @Test
    fun theJourneyCountsWhenDecidingWhatOverlaps() {
        val lunch = timed("Mittag", "2026-10-20T12:00", "2026-10-20T13:00")
        val client = timed("Kunde", "2026-10-20T13:30", "2026-10-20T14:30", travelBefore = 45)

        val plan = AgendaWidgetModel.plan(listOf(lunch, client), now)

        assertEquals(listOf(false, true), plan.rows.filterIsInstance<AgendaRow.Event>().map { it.clash })
    }

    @Test
    fun theNowLineComesBeforeTheFirstAppointmentNotYetBegun() {
        val early = timed("Laufen", "2026-10-20T07:00", "2026-10-20T08:00")
        val standup = timed("Standup", "2026-10-20T09:00", "2026-10-20T09:15")

        val plan = AgendaWidgetModel.plan(listOf(early, standup), now)

        val kinds = plan.rows.map { it::class.simpleName }
        assertEquals(listOf("Event", "Now", "Event"), kinds)
        assertTrue((plan.rows[0] as AgendaRow.Event).past)
        assertFalse((plan.rows[2] as AgendaRow.Event).past)
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
        // Digits outside ASCII are digits to Char.isDigit, not hex.
        assertNull(AgendaWidgetModel.parseColor("#\u0663\u0663\u0663"))

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

package app.rolltop.mobile

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class PwaHandoffTest {
    private val own = "app.rolltop.mobile"
    private val browser = WebAppFilter("com.android.chrome", hosts = emptyList(), pathPrefixes = emptyList())
    private val mailApp = WebAppFilter("org.chromium.webapk.mail", listOf("mail.example.com"), listOf("/"))
    private val calendarApp = WebAppFilter("org.chromium.webapk.calendar", listOf("mail.example.com"), listOf("/calendar"))

    @Test
    fun aBrowserIsNotTheServersWebApp() {
        assertNull(PwaHandoff.pick(listOf(browser), "https://mail.example.com/calendar/2026-10-21", own))
    }

    @Test
    fun theCalendarAppOpensCalendarPagesAndTheMailAppTheRest() {
        val installed = listOf(browser, mailApp, calendarApp)

        assertEquals(
            calendarApp.packageName,
            PwaHandoff.pick(installed, "https://mail.example.com/calendar/2026-10-21?event=7", own)
        )
        assertEquals(mailApp.packageName, PwaHandoff.pick(installed, "https://mail.example.com/mail/relevant", own))
    }

    @Test
    fun withOnlyTheMailAppInstalledItOpensCalendarPagesToo() {
        assertEquals(
            mailApp.packageName,
            PwaHandoff.pick(listOf(browser, mailApp), "https://mail.example.com/calendar/2026-10-21", own)
        )
    }

    @Test
    fun anotherServersWebAppAndThisAppAreNeverPicked() {
        val other = WebAppFilter("org.chromium.webapk.other", listOf("other.example.com"), listOf("/"))
        val self = WebAppFilter(own, listOf("mail.example.com"), listOf("/"))

        assertNull(PwaHandoff.pick(listOf(other, self), "https://mail.example.com/calendar/2026-10-21", own))
    }

    @Test
    fun aScopeThatDoesNotCoverThePageDoesNotOpenIt() {
        assertNull(PwaHandoff.pick(listOf(calendarApp), "https://mail.example.com/mail/relevant", own))
    }

    @Test
    fun hostsCompareWithoutCase() {
        val upper = WebAppFilter("org.chromium.webapk.mail", listOf("Mail.Example.com"), emptyList())

        assertEquals(upper.packageName, PwaHandoff.pick(listOf(upper), "https://mail.example.com/calendar/x", own))
    }
}

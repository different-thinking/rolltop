package app.rolltop.mobile

import android.content.Context
import androidx.work.Constraints
import androidx.work.ExistingPeriodicWorkPolicy
import androidx.work.ExistingWorkPolicy
import androidx.work.NetworkType
import androidx.work.OneTimeWorkRequest
import androidx.work.OutOfQuotaPolicy
import androidx.work.PeriodicWorkRequest
import androidx.work.WorkManager
import androidx.work.Worker
import androidx.work.WorkerParameters
import org.json.JSONArray
import java.net.URLEncoder
import java.nio.charset.StandardCharsets
import java.time.ZonedDateTime
import java.util.concurrent.TimeUnit

/**
 * AgendaWidgetWorker renews the agenda widget's copy of the calendar with the
 * WebView's session cookie, the way the mail poll reads its feed. It reads the
 * same two routes the calendar does -- the calendars for their colours, and the
 * events of the coming fortnight, which the server has already limited to the
 * calendars switched on and folded so a linked pair is one entry -- and then
 * draws every widget from the new copy.
 */
class AgendaWidgetWorker(context: Context, params: WorkerParameters) : Worker(context, params) {
    override fun doWork(): Result {
        val context = applicationContext
        if (!AgendaWidget.hasWidgets(context)) return Result.success()
        val outcome = synchronized(REFRESH_LOCK) { refresh(context) }
        AgendaWidget.render(context)
        return if (outcome == Outcome.RETRY && runAttemptCount < MAX_RETRY_ATTEMPTS) Result.retry() else Result.success()
    }

    private enum class Outcome { DONE, RETRY }

    private fun refresh(context: Context): Outcome {
        val base = RolltopPrefs.serverUrl(context)
        if (base.isBlank()) {
            AgendaWidgetStore.clear(context)
            return Outcome.DONE
        }
        val generation = AgendaWidgetStore.generation()
        val calendars = fetchArray(context, "$base/api/calendar/calendars", "calendars")
        val events = when (calendars) {
            is Fetch.Ok -> fetchArray(context, eventsUrl(base), "events")
            else -> calendars
        }
        val now = System.currentTimeMillis()
        return when {
            calendars is Fetch.Ok && events is Fetch.Ok -> {
                val keptCalendars = JSONArray()
                for (index in 0 until calendars.items.length()) {
                    calendars.items.optJSONObject(index)?.let { keptCalendars.put(AgendaWidgetModel.trimCalendar(it)) }
                }
                val keptEvents = JSONArray()
                for (index in 0 until events.items.length()) {
                    events.items.optJSONObject(index)?.let { keptEvents.put(AgendaWidgetModel.trimEvent(it)) }
                }
                // The server may have been changed while the requests were out;
                // a copy read from the old one is not this server's calendar.
                // So may the user have signed out: the answer then belongs to
                // the session that ended, and saveFetched refuses it.
                if (RolltopPrefs.serverUrl(context) == base) {
                    AgendaWidgetStore.saveFetched(context, base, keptCalendars, keptEvents, now, generation)
                }
                Outcome.DONE
            }
            events == Fetch.SignedOut -> {
                AgendaWidgetStore.markSignedOut(context, base)
                Outcome.DONE
            }
            events == Fetch.Unreachable -> {
                AgendaWidgetStore.markFailed(context, base, now)
                Outcome.RETRY
            }
            else -> {
                AgendaWidgetStore.markFailed(context, base, now)
                Outcome.DONE
            }
        }
    }

    private sealed class Fetch {
        data class Ok(val items: JSONArray) : Fetch()
        data object SignedOut : Fetch()
        data object Unreachable : Fetch()
        data object Refused : Fetch()
    }

    private fun fetchArray(context: Context, url: String, field: String): Fetch {
        val response = HttpJson.getResponse(context, url, connectTimeoutMillis = 10_000, readTimeoutMillis = 20_000)
            ?: return Fetch.Unreachable
        if (response.statusCode == 401 || response.statusCode == 403) return Fetch.SignedOut
        if (response.statusCode in 500..599 || response.statusCode == 408 || response.statusCode == 429) {
            return Fetch.Unreachable
        }
        if (response.statusCode !in 200..299) return Fetch.Refused
        val items = response.body?.optJSONArray(field) ?: return Fetch.Refused
        return Fetch.Ok(items)
    }

    private fun eventsUrl(base: String): String {
        val today = ZonedDateTime.now().toLocalDate()
        val zone = ZonedDateTime.now().zone
        val from = AgendaWidgetModel.dayStart(today, zone).toString()
        val to = AgendaWidgetModel.dayStart(today.plusDays(AgendaWidgetModel.DAYS), zone).toString()
        return "$base/api/calendar/events?from=${encode(from)}&to=${encode(to)}"
    }

    private fun encode(value: String): String = URLEncoder.encode(value, StandardCharsets.UTF_8.name())

    companion object {
        private const val PERIODIC_WORK_NAME = "rolltop-agenda-widget"
        private const val IMMEDIATE_WORK_NAME = "rolltop-agenda-widget-now"
        private const val MAX_RETRY_ATTEMPTS = 3
        private val REFRESH_LOCK = Any()

        /** schedule keeps the copy fresh while a widget is on the home screen.
         * Thirty minutes is often enough for a calendar that the app itself
         * refreshes the moment it is left ([AgendaWidget.requestRefresh]). */
        fun schedule(context: Context) {
            val request = PeriodicWorkRequest.Builder(AgendaWidgetWorker::class.java, 30, TimeUnit.MINUTES)
                .setConstraints(networkConstraint())
                .build()
            WorkManager.getInstance(context).enqueueUniquePeriodicWork(
                PERIODIC_WORK_NAME,
                ExistingPeriodicWorkPolicy.UPDATE,
                request
            )
        }

        fun refreshNow(context: Context) {
            val request = OneTimeWorkRequest.Builder(AgendaWidgetWorker::class.java)
                .setConstraints(networkConstraint())
                .setExpedited(OutOfQuotaPolicy.RUN_AS_NON_EXPEDITED_WORK_REQUEST)
                .build()
            WorkManager.getInstance(context).enqueueUniqueWork(
                IMMEDIATE_WORK_NAME,
                ExistingWorkPolicy.REPLACE,
                request
            )
        }

        fun cancel(context: Context) {
            WorkManager.getInstance(context).cancelUniqueWork(PERIODIC_WORK_NAME)
            WorkManager.getInstance(context).cancelUniqueWork(IMMEDIATE_WORK_NAME)
        }

        private fun networkConstraint(): Constraints = Constraints.Builder()
            .setRequiredNetworkType(NetworkType.CONNECTED)
            .build()
    }
}

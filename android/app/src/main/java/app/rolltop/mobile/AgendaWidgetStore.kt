package app.rolltop.mobile

import android.content.Context
import org.json.JSONArray
import org.json.JSONObject
import java.io.File

/**
 * The agenda widget's copy of the calendar: what the last refresh read, for the
 * server it read it from. The launcher draws the widget whenever it likes, and
 * mostly without a network, so the widget is drawn from this file and the
 * refresh only replaces it.
 *
 * It holds one user's calendar and is dropped when that user signs out
 * ([AgendaWidget.signedOut], called where the push endpoint is) and whenever
 * the server answers that nobody is signed in. A copy written for another
 * server is never drawn.
 */
internal data class AgendaSnapshot(
    val serverUrl: String,
    val fetchedAt: Long,
    /** failedAt is when the last refresh could not reach the server, 0 when the
     * last one succeeded: the widget then says how old its copy is. */
    val failedAt: Long,
    val signedOut: Boolean,
    val calendars: JSONArray,
    val events: JSONArray
)

internal object AgendaWidgetStore {
    private const val FILE_NAME = "agenda_widget.json"
    private val lock = Any()

    fun load(context: Context): AgendaSnapshot? = synchronized(lock) {
        val file = file(context)
        if (!file.exists()) return null
        try {
            val json = JSONObject(file.readText())
            AgendaSnapshot(
                serverUrl = json.optString("server", ""),
                fetchedAt = json.optLong("fetched_at", 0),
                failedAt = json.optLong("failed_at", 0),
                signedOut = json.optBoolean("signed_out", false),
                calendars = json.optJSONArray("calendars") ?: JSONArray(),
                events = json.optJSONArray("events") ?: JSONArray()
            )
        } catch (_: Exception) {
            null
        }
    }

    fun saveFetched(context: Context, serverUrl: String, calendars: JSONArray, events: JSONArray, at: Long) {
        write(context, AgendaSnapshot(serverUrl, at, 0, false, calendars, events))
    }

    /** markFailed keeps the copy and notes that it could not be renewed. */
    fun markFailed(context: Context, serverUrl: String, at: Long) = synchronized(lock) {
        val current = load(context)?.takeIf { it.serverUrl == serverUrl } ?: return
        write(context, current.copy(failedAt = at))
    }

    /** markSignedOut drops every event and remembers why the widget is empty. */
    fun markSignedOut(context: Context, serverUrl: String) {
        write(context, AgendaSnapshot(serverUrl, 0, 0, true, JSONArray(), JSONArray()))
    }

    fun clear(context: Context) = synchronized(lock) {
        file(context).delete()
    }

    private fun write(context: Context, snapshot: AgendaSnapshot) = synchronized(lock) {
        val json = JSONObject().apply {
            put("server", snapshot.serverUrl)
            put("fetched_at", snapshot.fetchedAt)
            put("failed_at", snapshot.failedAt)
            put("signed_out", snapshot.signedOut)
            put("calendars", snapshot.calendars)
            put("events", snapshot.events)
        }
        val target = file(context)
        val temporary = File(target.parentFile, "$FILE_NAME.tmp")
        temporary.writeText(json.toString())
        if (!temporary.renameTo(target)) {
            target.writeText(json.toString())
            temporary.delete()
        }
    }

    private fun file(context: Context): File = File(context.noBackupFilesDir, FILE_NAME)
}

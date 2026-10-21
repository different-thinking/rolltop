package app.rolltop.mobile

import android.content.ActivityNotFoundException
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.os.PatternMatcher
import java.net.URI

/** WebAppFilter is what one installed app says it opens: the hosts it names
 * and, when it names any, the path prefixes under them. */
internal data class WebAppFilter(val packageName: String, val hosts: List<String>, val pathPrefixes: List<String>)

/**
 * PwaHandoff hands Rolltop pages to the installed web app (PWA) of the
 * configured server, for a reader who uses the PWA and keeps this app only for
 * the agenda widget.
 *
 * Chrome installs a PWA on Android as a WebAPK, which declares the server's
 * host and the app's scope as links it opens. A browser declares no host at
 * all, so "an app that names this host" is the PWA. The calendar installs as an
 * app of its own (scope `/calendar`) beside the mail app (scope `/`), so the
 * longest scope that covers a page is the app that opens it.
 *
 * The same answer decides whether this app is only the widget's host: with the
 * server's PWA installed, the PWA is the reader's app and has its own
 * notifications, so this one asks for none and polls for none
 * ([isWidgetHost]); notifying from both would announce every message twice.
 */
internal object PwaHandoff {
    /** pick answers the package of the installed web app that opens url, or
     * null when only browsers (or this app) would. */
    fun pick(filters: List<WebAppFilter>, url: String, ownPackage: String): String? {
        val uri = try {
            URI(url)
        } catch (_: Exception) {
            return null
        }
        val host = uri.host ?: return null
        val path = uri.rawPath.orEmpty().ifEmpty { "/" }
        var best: String? = null
        var bestScore = -1
        for (filter in filters) {
            if (filter.packageName == ownPackage) continue
            if (filter.hosts.none { it.equals(host, ignoreCase = true) }) continue
            val score = if (filter.pathPrefixes.isEmpty()) {
                0
            } else {
                filter.pathPrefixes.filter { path.startsWith(it) }.maxOfOrNull { it.length } ?: continue
            }
            if (score > bestScore) {
                best = filter.packageName
                bestScore = score
            }
        }
        return best
    }

    fun installedWebApp(context: Context, url: String): String? =
        pick(webAppFilters(context, url), url, context.packageName)

    /** isWidgetHost reports whether the configured server's PWA is installed,
     * which makes this app the widget's host and nothing else. */
    fun isWidgetHost(context: Context): Boolean {
        val server = RolltopPrefs.serverUrl(context)
        if (server.isBlank()) return false
        return installedWebApp(context, RolltopPrefs.buildUrl(context, "/")) != null ||
            installedWebApp(context, RolltopPrefs.buildUrl(context, "/calendar/")) != null
    }

    /** open shows url in the installed web app; false when there is none. */
    fun open(context: Context, url: String): Boolean {
        val target = installedWebApp(context, url) ?: return false
        return try {
            context.startActivity(
                Intent(Intent.ACTION_VIEW, Uri.parse(url))
                    .addCategory(Intent.CATEGORY_BROWSABLE)
                    .setPackage(target)
                    .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
            )
            true
        } catch (_: ActivityNotFoundException) {
            false
        }
    }

    private fun webAppFilters(context: Context, url: String): List<WebAppFilter> {
        val intent = Intent(Intent.ACTION_VIEW, Uri.parse(url)).addCategory(Intent.CATEGORY_BROWSABLE)
        val flags = PackageManager.GET_RESOLVED_FILTER or PackageManager.MATCH_ALL
        val handlers = try {
            if (Build.VERSION.SDK_INT >= 33) {
                context.packageManager.queryIntentActivities(intent, PackageManager.ResolveInfoFlags.of(flags.toLong()))
            } else {
                @Suppress("DEPRECATION")
                context.packageManager.queryIntentActivities(intent, flags)
            }
        } catch (_: Exception) {
            return emptyList()
        }
        return handlers.mapNotNull { info ->
            val filter = info.filter ?: return@mapNotNull null
            val packageName = info.activityInfo?.packageName ?: return@mapNotNull null
            WebAppFilter(packageName, hosts(filter), pathPrefixes(filter))
        }
    }

    private fun hosts(filter: IntentFilter): List<String> =
        (0 until filter.countDataAuthorities()).mapNotNull { filter.getDataAuthority(it)?.host }

    /** A scope is a path prefix, which is how a WebAPK declares it; a filter
     * naming paths of other kinds only is read as naming none of this page's. */
    private fun pathPrefixes(filter: IntentFilter): List<String> {
        val paths = (0 until filter.countDataPaths()).mapNotNull { filter.getDataPath(it) }
        if (paths.isEmpty()) return emptyList()
        val prefixes = paths
            .filter { it.type == PatternMatcher.PATTERN_PREFIX }
            .map { it.path }
        return prefixes.ifEmpty { listOf(UNMATCHABLE_PATH) }
    }

    private const val UNMATCHABLE_PATH = "\u0000"
}

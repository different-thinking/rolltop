package app.rolltop.mobile

import android.webkit.WebViewClient

/** What the shell shows when the first page of a session could not be loaded. */
internal enum class ConnectionFailureAction {
    /** The device or the server is unreachable right now: say so, offer Retry,
     * and keep the configured server. */
    OFFLINE_RETRY,

    /** The address or the server behind it is wrong: back to the server form. */
    SETUP
}

/**
 * Decides between the two screens behind a failed initial load.
 *
 * The web app's own service worker answers an offline navigation with the
 * shell it kept, so the WebView usually never gets this far without a network.
 * It does on the first launch after an update, before that worker is
 * installed, and whenever the worker had nothing stored - and the wrapper used
 * to answer both with the server form and "Check the address and try again".
 * A reader on a train, whose server worked yesterday, was being told their URL
 * was wrong, and the form's Connect button validated it against the network
 * they did not have. A failure to reach a server that has been reached before
 * is a connection problem until proven otherwise; only a server that has never
 * answered is a question about the address.
 */
internal object ConnectionFailurePolicy {
    private val connectivityErrors = setOf(
        WebViewClient.ERROR_HOST_LOOKUP,
        WebViewClient.ERROR_CONNECT,
        WebViewClient.ERROR_TIMEOUT,
        WebViewClient.ERROR_IO,
        WebViewClient.ERROR_UNKNOWN
    )

    fun forLoadError(errorCode: Int, serverReachedBefore: Boolean): ConnectionFailureAction =
        if (serverReachedBefore && errorCode in connectivityErrors) {
            ConnectionFailureAction.OFFLINE_RETRY
        } else {
            ConnectionFailureAction.SETUP
        }

    /** A 5xx from a server that has served the app before is the server having
     * a bad moment - a proxy answering for a backend that is restarting, or the
     * backend itself failing - never a wrong address; a 4xx says the address
     * reached something that is not this Rolltop, or no longer is. */
    fun forHttpError(statusCode: Int, serverReachedBefore: Boolean): ConnectionFailureAction =
        if (serverReachedBefore && statusCode in 500..599) {
            ConnectionFailureAction.OFFLINE_RETRY
        } else {
            ConnectionFailureAction.SETUP
        }
}

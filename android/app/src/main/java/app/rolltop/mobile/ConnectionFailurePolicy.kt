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

    /** The gateway answers a reverse proxy gives while the backend behind it is
     * down or restarting; the address is right and the server will be back. */
    private val gatewayStatuses = setOf(502, 503, 504)

    fun forLoadError(errorCode: Int, serverReachedBefore: Boolean): ConnectionFailureAction =
        if (serverReachedBefore && errorCode in connectivityErrors) {
            ConnectionFailureAction.OFFLINE_RETRY
        } else {
            ConnectionFailureAction.SETUP
        }

    fun forHttpError(statusCode: Int, serverReachedBefore: Boolean): ConnectionFailureAction =
        if (serverReachedBefore && statusCode in gatewayStatuses) {
            ConnectionFailureAction.OFFLINE_RETRY
        } else {
            ConnectionFailureAction.SETUP
        }
}

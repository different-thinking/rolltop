package app.rolltop.mobile

import android.webkit.WebViewClient
import org.junit.Assert.assertEquals
import org.junit.Test

class ConnectionFailurePolicyTest {
    @Test
    fun aKnownServerThatCannotBeReachedIsOffline() {
        listOf(
            WebViewClient.ERROR_HOST_LOOKUP,
            WebViewClient.ERROR_CONNECT,
            WebViewClient.ERROR_TIMEOUT,
            WebViewClient.ERROR_IO,
            WebViewClient.ERROR_UNKNOWN
        ).forEach { code ->
            assertEquals(
                "Expected error $code on a known server to offer Retry",
                ConnectionFailureAction.OFFLINE_RETRY,
                ConnectionFailurePolicy.forLoadError(code, serverReachedBefore = true)
            )
        }
    }

    @Test
    fun aServerNeverReachedGoesBackToTheForm() {
        assertEquals(
            ConnectionFailureAction.SETUP,
            ConnectionFailurePolicy.forLoadError(WebViewClient.ERROR_HOST_LOOKUP, serverReachedBefore = false)
        )
        assertEquals(
            ConnectionFailureAction.SETUP,
            ConnectionFailurePolicy.forHttpError(503, serverReachedBefore = false)
        )
    }

    @Test
    fun errorsAboutTheAddressItselfGoBackToTheFormEvenOnAKnownServer() {
        listOf(
            WebViewClient.ERROR_BAD_URL,
            WebViewClient.ERROR_UNSUPPORTED_SCHEME,
            WebViewClient.ERROR_REDIRECT_LOOP,
            WebViewClient.ERROR_FAILED_SSL_HANDSHAKE
        ).forEach { code ->
            assertEquals(
                "Expected error $code to go back to the server form",
                ConnectionFailureAction.SETUP,
                ConnectionFailurePolicy.forLoadError(code, serverReachedBefore = true)
            )
        }
    }

    @Test
    fun aGatewayAnsweringForADownBackendIsOfflineOnlyOnAKnownServer() {
        listOf(502, 503, 504).forEach { status ->
            assertEquals(
                "Expected $status on a known server to offer Retry",
                ConnectionFailureAction.OFFLINE_RETRY,
                ConnectionFailurePolicy.forHttpError(status, serverReachedBefore = true)
            )
        }
        listOf(400, 401, 404, 500).forEach { status ->
            assertEquals(
                "Expected $status to go back to the server form",
                ConnectionFailureAction.SETUP,
                ConnectionFailurePolicy.forHttpError(status, serverReachedBefore = true)
            )
        }
    }
}

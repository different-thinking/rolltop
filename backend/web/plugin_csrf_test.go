// File overview: The host's CSRF gate on backend-plugin API routes. A plugin
// that never calls VerifyCSRF must still not be reachable cross-site, and a
// route that deliberately serves non-browser clients must still be able to opt
// out of the token check.

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"rolltop/backend/plugins"
	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
)

// forgetfulPluginRoute is the handler a third-party plugin author writes when
// they have not read the note on ProtectedAPIRoute: it mutates, and it checks
// nothing itself.
func forgetfulPluginRoute(called *bool) plugins.ProtectedAPIHandler {
	return func(plugins.APIHost, string, http.ResponseWriter, *http.Request) {
		*called = true
	}
}

// pluginCSRFServer is a server carrying one registered plugin route, enabled,
// so dispatch reaches the CSRF gate rather than stopping at the enabled check.
func pluginCSRFServer(t *testing.T, route plugins.ProtectedAPIRoute) *Server {
	t.Helper()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	// SetPluginEnabled only knows plugins that exist, so the route is hung off
	// a real bundled one. Which plugin owns it is irrelevant here: the gate is
	// the host's, and it does not read the plugin.
	if err := db.SetPluginEnabled(context.Background(), plugins.ClientSidePGP, true); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: db, protectedAPIRoutes: newProtectedAPIRouteRegistry()}
	if _, err := server.protectedAPIRouteRegistry().register(plugins.ClientSidePGP, route); err != nil {
		t.Fatal(err)
	}
	return server
}

func pluginRouteRequest(t *testing.T, server *Server, method string, withToken bool) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, "/api/plugins/client_side_pgp/csrf-probe", nil)
	request = request.WithContext(context.WithValue(request.Context(), userContextKey,
		currentUser{User: store.User{ID: 1, Email: "reader@example.test"}}))
	const csrfBase = "plugin-route-csrf"
	request.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrfBase})
	if withToken {
		request.Header.Set("X-CSRF-Token", server.csrfForBase(csrfBase))
	}
	return request
}

func TestPluginAPIMutationWithoutCSRFTokenIsRefused(t *testing.T) {
	called := false
	server := pluginCSRFServer(t, plugins.ProtectedAPIRoute{
		Path:   "plugins/client_side_pgp/csrf-probe",
		Handle: forgetfulPluginRoute(&called),
	})

	recorder := httptest.NewRecorder()
	if !server.dispatchProtectedAPIPath(recorder, pluginRouteRequest(t, server, http.MethodPost, false), "plugins/client_side_pgp/csrf-probe") {
		t.Fatal("the route did not match")
	}
	if called {
		t.Fatal("a plugin mutation ran without a CSRF token")
	}
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusForbidden)
	}
}

func TestPluginAPIMutationWithCSRFTokenRuns(t *testing.T) {
	called := false
	server := pluginCSRFServer(t, plugins.ProtectedAPIRoute{
		Path:   "plugins/client_side_pgp/csrf-probe",
		Handle: forgetfulPluginRoute(&called),
	})

	recorder := httptest.NewRecorder()
	if !server.dispatchProtectedAPIPath(recorder, pluginRouteRequest(t, server, http.MethodPost, true), "plugins/client_side_pgp/csrf-probe") {
		t.Fatal("the route did not match")
	}
	if !called {
		t.Fatalf("the plugin route did not run with a valid token: status %d", recorder.Code)
	}
}

// A read is not a mutation, and the gate must not turn every plugin settings
// page into one that needs a token to display.
func TestPluginAPIReadNeedsNoCSRFToken(t *testing.T) {
	called := false
	server := pluginCSRFServer(t, plugins.ProtectedAPIRoute{
		Path:   "plugins/client_side_pgp/csrf-probe",
		Handle: forgetfulPluginRoute(&called),
	})

	recorder := httptest.NewRecorder()
	if !server.dispatchProtectedAPIPath(recorder, pluginRouteRequest(t, server, http.MethodGet, false), "plugins/client_side_pgp/csrf-probe") {
		t.Fatal("the route did not match")
	}
	if !called {
		t.Fatalf("a plugin read was refused without a token: status %d", recorder.Code)
	}
}

// The opt-out exists for routes whose callers are not browsers and therefore
// have no token to send. It has to actually opt out, or those routes break.
func TestPluginAPIRouteCanOptOutOfCSRF(t *testing.T) {
	called := false
	server := pluginCSRFServer(t, plugins.ProtectedAPIRoute{
		Path:          "plugins/client_side_pgp/csrf-probe",
		Handle:        forgetfulPluginRoute(&called),
		SkipCSRFCheck: true,
	})

	recorder := httptest.NewRecorder()
	if !server.dispatchProtectedAPIPath(recorder, pluginRouteRequest(t, server, http.MethodPost, false), "plugins/client_side_pgp/csrf-probe") {
		t.Fatal("the route did not match")
	}
	if !called {
		t.Fatalf("an opted-out plugin route was refused: status %d", recorder.Code)
	}
}

// Authentication is checked before the token, so an unauthenticated caller is
// told to log in rather than being handed a CSRF complaint that would suggest
// the route is otherwise reachable.
func TestPluginAPIMutationWithoutSessionIsUnauthorized(t *testing.T) {
	called := false
	server := pluginCSRFServer(t, plugins.ProtectedAPIRoute{
		Path:   "plugins/client_side_pgp/csrf-probe",
		Handle: forgetfulPluginRoute(&called),
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/plugins/client_side_pgp/csrf-probe", nil)
	if !server.dispatchProtectedAPIPath(recorder, request, "plugins/client_side_pgp/csrf-probe") {
		t.Fatal("the route did not match")
	}
	if called {
		t.Fatal("a plugin mutation ran without a session")
	}
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
}

// File overview: Tests that every SPA path the server links users to is
// actually served, rather than falling through to a 404.

package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"rolltop/backend/store"
)

func TestPublicAuthRoutesAreServedAsAppRoutes(t *testing.T) {
	// isPublicAuthRoute promises these paths stay reachable without a session,
	// but handleApp rejects anything isAppRoute does not know, so a path listed
	// in only one of the two 404s before the session handling ever runs.
	for _, path := range []string{"/login", "/setup", "/reset-password"} {
		if !isPublicAuthRoute(path) {
			t.Fatalf("%s is not a public auth route", path)
		}
		if !isAppRoute(path) {
			t.Fatalf("%s is a public auth route but not an app route, so it 404s", path)
		}
	}
}

// TestEverySPARouteIsActuallyServed closes the gap the route table exists for:
// a declaration that no mux pattern answers is a page that 404s in production
// while every predicate insists it is a real route.
func TestEverySPARouteIsActuallyServed(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server, err := New(Options{
		Store:      db,
		MasterKey:  []byte("12345678901234567890123456789012"),
		SessionTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	for _, route := range spaRoutes {
		targets := []string{}
		if route.exact {
			targets = append(targets, route.path)
		}
		if route.prefix {
			targets = append(targets, route.path+"/child")
		}
		for _, target := range targets {
			if !isAppRoute(target) {
				t.Fatalf("%s is declared but isAppRoute rejects it", target)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
			// Without a built frontend the shell answers 503; the point is that
			// the path reaches a handler at all rather than falling through.
			if rec.Code == http.StatusNotFound {
				t.Fatalf("declared SPA route %s is not registered on the mux", target)
			}
		}
	}
}

func TestPasswordResetLinkTargetIsAnAppRoute(t *testing.T) {
	// Password reset emails link to this path; if it is not served, every
	// reset link a user receives is dead.
	if !isAppRoute("/reset-password") {
		t.Fatal("/reset-password is not served, so password reset emails link nowhere")
	}
}

func TestGoogleSettingsCallbackTargetIsAnAppRoute(t *testing.T) {
	// The OAuth callback redirects the browser here.
	if !isAppRoute(googleSettingsPath) {
		t.Fatalf("%s is not served, so the Google callback lands on a 404", googleSettingsPath)
	}
}

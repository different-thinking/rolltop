// File overview: Tests that every SPA path the server links users to is
// actually served, rather than falling through to a 404.

package web

import "testing"

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

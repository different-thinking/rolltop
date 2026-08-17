// File overview: Regression tests for auth behavior when the system store
// fails transiently (busy/locked SQLite under heavy sync, disk pressure). A
// store failure must never be reported as "no users" (which sends signed-in
// browsers to the first-run admin setup screen) or as "signed out" (which
// answers 401 to a valid session), and recovery routes such as logout must
// keep working during the outage.

package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"rolltop/backend/auth"
	mmcrypto "rolltop/backend/crypto"
	"rolltop/backend/store"
)

func newStoreFailureTestServer(t *testing.T) (*store.Store, *Server, http.Handler, *http.Cookie) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	user, err := db.CreateUser(ctx, "admin@example.test", "Admin", "hash", true)
	if err != nil {
		t.Fatal(err)
	}
	token, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateSession(ctx, user.ID, mmcrypto.TokenHash(token), time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	server, err := New(Options{
		Store:      db,
		MasterKey:  []byte("12345678901234567890123456789012"),
		SessionTTL: time.Hour,
		// Route and cookie behavior needs no background workers, and workers that
		// outlive this test read its torn-down database, which logs a failure into
		// an unrelated test's captured output.
		DisableBackgroundWorkers: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return db, server, server.Handler(), &http.Cookie{Name: sessionCookie, Value: token}
}

func TestBootstrapReportsErrorNotSetupWhenUserCountFails(t *testing.T) {
	db, _, handler, _ := newStoreFailureTestServer(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("GET /api/bootstrap with failing store status = %d body=%s, want 500 rather than a users_exist:false payload", rec.Code, rec.Body.String())
	}
}

func TestSessionLookupFailureIsNotTreatedAsSignedOut(t *testing.T) {
	db, _, handler, cookie := newStoreFailureTestServer(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/bootstrap", "/api/profile"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("GET %s with valid session but failing store status = %d body=%s, want 503 rather than an anonymous response", path, rec.Code, rec.Body.String())
		}
	}
}

func TestLogoutStillClearsCookieWhenStoreFails(t *testing.T) {
	db, server, handler, cookie := newStoreFailureTestServer(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/logout", nil)
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", server.csrfForBase(cookie.Value))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/logout with failing store status = %d body=%s, want 200 so the cookie can still be cleared", rec.Code, rec.Body.String())
	}
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("logout did not clear the session cookie")
	}
}

func TestSetupRefusesWhenUserCountFails(t *testing.T) {
	db, _, handler, _ := newStoreFailureTestServer(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/setup", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("POST /api/setup with failing store status = %d body=%s, want 500 so no second admin can be created", rec.Code, rec.Body.String())
	}
}

func TestUnknownSessionCookieStillMeansSignedOut(t *testing.T) {
	db, _, handler, cookie := newStoreFailureTestServer(t)
	defer db.Close()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/profile", nil)
	req.AddCookie(&http.Cookie{Name: cookie.Name, Value: "unknown-token"})
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/profile with unknown session status = %d body=%s, want 401", rec.Code, rec.Body.String())
	}
}

func TestLoginStoreFailureIsNotACredentialVerdict(t *testing.T) {
	db, server, handler, _ := newStoreFailureTestServer(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/login", nil)
	req.AddCookie(&http.Cookie{Name: csrfCookie, Value: "csrf-base"})
	req.Header.Set("X-CSRF-Token", server.csrfForBase("csrf-base"))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("POST /api/login with failing store status = %d body=%s, want 500 rather than an invalid-credentials response", rec.Code, rec.Body.String())
	}
}

func TestBootstrapPayloadSessionFailureKeepsPublicRoutesAnonymous(t *testing.T) {
	db, server, _, _ := newStoreFailureTestServer(t)
	defer db.Close()

	failed := httptest.NewRequest(http.MethodGet, "/login", nil)
	failed = failed.WithContext(context.WithValue(failed.Context(), sessionErrorContextKey, context.DeadlineExceeded))
	if _, err := server.bootstrapPayload(httptest.NewRecorder(), failed); !errors.Is(err, errSessionUnavailable) {
		t.Fatalf("bootstrapPayload with failed session lookup err = %v, want errSessionUnavailable", err)
	}

	// handleApp falls back to an anonymous render for /login, /setup, and
	// /reset-password by clearing the recorded session error; that render must
	// succeed while the rest of the store still works.
	cleared := failed.WithContext(context.WithValue(failed.Context(), sessionErrorContextKey, nil))
	payload, err := server.bootstrapPayload(httptest.NewRecorder(), cleared)
	if err != nil {
		t.Fatalf("anonymous fallback bootstrap failed: %v", err)
	}
	if payload["user"] != nil {
		t.Fatalf("anonymous fallback payload user = %v, want nil", payload["user"])
	}
	if usersExist, _ := payload["users_exist"].(bool); !usersExist {
		t.Fatal("anonymous fallback payload must still report existing users")
	}
	if !isPublicAuthRoute("/login") || !isPublicAuthRoute("/setup") || isPublicAuthRoute("/mail") {
		t.Fatal("public auth route classification is wrong")
	}
}

func TestStoreSideDeadlineIsNotTreatedAsAnAbandonedRequest(t *testing.T) {
	// A deadline the store or SQLite driver applies internally under lock
	// contention arrives as context.DeadlineExceeded while the browser is
	// still waiting. Classifying that by the error alone would demote a valid
	// session to anonymous and force the spurious logout this file guards.
	if clientAbandonedRequest(context.Background()) {
		t.Fatal("a live request context was classified as abandoned")
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if !clientAbandonedRequest(canceled) {
		t.Fatal("a canceled request context was not classified as abandoned")
	}

	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	if !clientAbandonedRequest(expired) {
		t.Fatal("an expired request context was not classified as abandoned")
	}
}

func TestIconRoutesAnswerUnavailableRatherThanNotFoundDuringStoreFailure(t *testing.T) {
	db, _, handler, cookie := newStoreFailureTestServer(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// A 404 here is cached by the browser as "this icon does not exist" and
	// survives the outage, so these routes must report unavailability instead.
	for _, target := range []string{"/contacts/1/icon", "/brand-icons/example.test"} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		request.AddCookie(cookie)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, request)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("GET %s during store failure status=%d, want 503", target, rec.Code)
		}
	}
}

func TestIconRoutesStillHideThemselvesFromAnonymousCallers(t *testing.T) {
	db, _, handler, _ := newStoreFailureTestServer(t)
	// The server's background workers keep writing into the temp directory, so
	// the store has to be closed before the test's cleanup removes it.
	defer db.Close()
	for _, target := range []string{"/contacts/1/icon", "/brand-icons/example.test"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("anonymous GET %s status=%d, want 404", target, rec.Code)
		}
	}
}

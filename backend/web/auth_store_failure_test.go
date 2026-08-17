// File overview: Regression tests for auth behavior when the system store
// fails transiently (busy/locked SQLite under heavy sync, disk pressure). A
// store failure must never be reported as "no users" (which sends signed-in
// browsers to the first-run admin setup screen) or as "signed out" (which
// answers 401 to a valid session).

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"rolltop/backend/auth"
	mmcrypto "rolltop/backend/crypto"
	"rolltop/backend/store"
)

func newStoreFailureTestServer(t *testing.T) (*store.Store, http.Handler, *http.Cookie) {
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
	})
	if err != nil {
		t.Fatal(err)
	}
	return db, server.Handler(), &http.Cookie{Name: sessionCookie, Value: token}
}

func TestBootstrapReportsErrorNotSetupWhenUserCountFails(t *testing.T) {
	db, handler, _ := newStoreFailureTestServer(t)
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
	db, handler, cookie := newStoreFailureTestServer(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	req.AddCookie(cookie)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("request with valid session but failing store status = %d body=%s, want 503 rather than an anonymous response", rec.Code, rec.Body.String())
	}
}

func TestSetupRefusesWhenUserCountFails(t *testing.T) {
	db, handler, _ := newStoreFailureTestServer(t)
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
	db, handler, cookie := newStoreFailureTestServer(t)
	defer db.Close()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/profile", nil)
	req.AddCookie(&http.Cookie{Name: cookie.Name, Value: "unknown-token"})
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/profile with unknown session status = %d body=%s, want 401", rec.Code, rec.Body.String())
	}
}

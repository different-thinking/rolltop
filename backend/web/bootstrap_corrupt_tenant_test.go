// File overview: Regression tests for the app shell when a signed-in user's own
// database is unreadable. The account lives in the system database, so the
// session stays valid; failing the bootstrap instead served a 500 for /login
// and /api/bootstrap alike, which locked the operator out of the admin database
// page that repairs the tenant at fault.

package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"rolltop/backend/auth"
	mmcrypto "rolltop/backend/crypto"
	"rolltop/backend/store"
)

func newCorruptTenantTestServer(t *testing.T) (http.Handler, *http.Cookie) {
	t.Helper()
	ctx := context.Background()
	dataDir := filepath.Join(t.TempDir(), "data")
	db, err := store.OpenServer(filepath.Join(dataDir, "rolltop.db"), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
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
	if err := db.MarkCorrupt(user.ID, "database disk image is malformed"); err == nil {
		t.Fatal("MarkCorrupt did not report the tenant as corrupt")
	}
	server, err := New(Options{
		Store:                    db,
		MasterKey:                []byte("12345678901234567890123456789012"),
		SessionTTL:               time.Hour,
		DisableBackgroundWorkers: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	return server.Handler(), &http.Cookie{Name: sessionCookie, Value: token}
}

func TestBootstrapServesTheShellWhenTheTenantDatabaseIsUnavailable(t *testing.T) {
	handler, cookie := newCorruptTenantTestServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	req.AddCookie(cookie)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/bootstrap with a damaged tenant status = %d body=%s, want 200 so the browser can reach the repair page", rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	// Dropping the session here would send a signed-in operator back to the
	// login screen, which cannot repair anything.
	if payload["user"] == nil {
		t.Fatalf("payload signed the user out: %v", rec.Body.String())
	}
	if unavailable, _ := payload["database_unavailable"].(bool); !unavailable {
		t.Fatal("payload does not flag the unavailable database, so the UI would present it as an account with no mail")
	}
	mailboxes, ok := payload["mailboxes"].([]any)
	if !ok || len(mailboxes) != 0 {
		t.Fatalf("mailboxes = %v, want an empty list", payload["mailboxes"])
	}
}

func TestAdminDatabasePageIsADirectlyReachableAppRoute(t *testing.T) {
	// The SPA routes /admin/database, but a browser that opens or reloads that
	// URL asks the server first; without this the repair page 404s on refresh.
	if !isAppRoute("/admin/database") {
		t.Fatal("/admin/database is not served as an app route")
	}
}

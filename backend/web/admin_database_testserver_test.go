package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
)

// newDatabaseAdminServer builds a server with an admin user, for the tests that
// drive the admin database and PostgreSQL console routes.
func newDatabaseAdminServer(t *testing.T) (*Server, store.User, string) {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := db.CreateUser(ctx, "admin@example.test", "Admin", "hash", true)
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(Options{
		Store: db, DataDir: dataDir, PluginDir: t.TempDir(),
		DatabaseTarget: "test@localhost/rolltop_test", DatabaseMaxConns: 4,
		DisableBackgroundWorkers: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return server, admin, dataDir
}

func adminDatabaseRequest(t *testing.T, server *Server, admin store.User, method, target string, body any) *http.Request {
	t.Helper()
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		payload = encoded
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(context.WithValue(request.Context(), userContextKey, currentUser{User: admin}))
	const csrfBase = "database-admin-csrf"
	request.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrfBase})
	request.Header.Set("X-CSRF-Token", server.csrfForBase(csrfBase))
	return request
}

// TestAdminDatabaseOverviewReportsTheConnectionAndTheVolume covers what the
// page still answers now that it reports storage rather than driving repairs.
func TestAdminDatabaseOverviewReportsTheConnectionAndTheVolume(t *testing.T) {
	server, admin, dataDir := newDatabaseAdminServer(t)

	recorder := httptest.NewRecorder()
	server.apiAdminDatabase(recorder, adminDatabaseRequest(t, server, admin, http.MethodGet, "/api/admin/database", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var overview databaseOverview
	if err := json.Unmarshal(recorder.Body.Bytes(), &overview); err != nil {
		t.Fatal(err)
	}
	if !overview.Database.Reachable {
		t.Fatalf("database reported unreachable: %s", overview.Database.Error)
	}
	if overview.Database.ServerVersion == "" {
		t.Error("no server version reported")
	}
	if overview.Database.Bytes <= 0 {
		t.Errorf("database size = %d, want a positive size", overview.Database.Bytes)
	}
	if overview.Database.PoolMaxConns != 4 {
		t.Errorf("pool max conns = %d, want the configured 4", overview.Database.PoolMaxConns)
	}
	if overview.Volume.DataDir != dataDir {
		t.Errorf("data dir = %q, want %q", overview.Volume.DataDir, dataDir)
	}
	if overview.Volume.TotalBytes <= 0 {
		t.Errorf("volume total = %d, want the volume's capacity", overview.Volume.TotalBytes)
	}
}

// TestAdminDatabaseRejectsNonAdmins pins that the storage figures stay behind
// the admin check: they name the server and its size.
func TestAdminDatabaseRejectsNonAdmins(t *testing.T) {
	server, _, _ := newDatabaseAdminServer(t)
	ctx := context.Background()
	member, err := server.store.CreateUser(ctx, "member@example.test", "Member", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.apiAdminDatabase(recorder, adminDatabaseRequest(t, server, member, http.MethodGet, "/api/admin/database", nil))
	if recorder.Code == http.StatusOK {
		t.Fatalf("a non-admin read the database overview: %s", recorder.Body.String())
	}
}

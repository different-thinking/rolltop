package web

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"rolltop/backend/logging"
	"rolltop/backend/store"
)

func TestAdminLogReturnsTheCapturedTail(t *testing.T) {
	server, admin, _ := newDatabaseAdminServer(t)
	captureLogLine(t, "error server error GET /api/mail: database disk image is malformed")

	rec := httptest.NewRecorder()
	server.apiAdminLog(rec, adminLogRequest(admin, "/api/admin/log"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Lines []apiLogLine `json:"lines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Lines) == 0 {
		t.Fatal("no log lines returned")
	}
	last := payload.Lines[len(payload.Lines)-1]
	if !strings.Contains(last.Message, "database disk image is malformed") {
		t.Fatalf("newest line = %q", last.Message)
	}
	if !last.Error {
		t.Fatalf("line %q was not marked as an error", last.Message)
	}
	if last.Time == "" {
		t.Fatal("line carries no timestamp")
	}
}

// The tail carries request paths and backend error text, so it must never
// answer a signed-in non-admin.
func TestAdminLogRefusesNonAdmins(t *testing.T) {
	server, _, _ := newDatabaseAdminServer(t)
	member, err := server.store.CreateUser(context.Background(), "member@example.test", "Member", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	captureLogLine(t, "error server error GET /api/mail: secret detail")

	rec := httptest.NewRecorder()
	server.apiAdminLog(rec, adminLogRequest(member, "/api/admin/log"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret detail") {
		t.Fatalf("non-admin response leaked the log tail: %s", rec.Body.String())
	}
}

func TestAdminLogLimitIsBoundedAndForgiving(t *testing.T) {
	server, admin, _ := newDatabaseAdminServer(t)
	for range 6 {
		captureLogLine(t, "error server error GET /api/mail: boom")
	}
	for _, target := range []string{"/api/admin/log?limit=2", "/api/admin/log?limit=nonsense", "/api/admin/log?limit=-1"} {
		rec := httptest.NewRecorder()
		server.apiAdminLog(rec, adminLogRequest(admin, target))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d", target, rec.Code)
		}
		var payload struct {
			Lines []apiLogLine `json:"lines"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		want := defaultLogTailLines
		if strings.HasSuffix(target, "limit=2") {
			want = 2
		}
		if len(payload.Lines) > want {
			t.Fatalf("GET %s returned %d lines, want at most %d", target, len(payload.Lines), want)
		}
		if len(payload.Lines) == 0 {
			t.Fatalf("GET %s returned nothing", target)
		}
	}
}

// captureLogLine routes one line through the same writer the binary installs,
// so the test exercises the real capture path rather than the ring directly.
func captureLogLine(t *testing.T, message string) {
	t.Helper()
	previous := log.Writer()
	log.SetOutput(logging.Recorder())
	log.Print(message)
	log.SetOutput(previous)
}

func adminLogRequest(user store.User, target string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	return req.WithContext(context.WithValue(req.Context(), userContextKey, currentUser{User: user}))
}

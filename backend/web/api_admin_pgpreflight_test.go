package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestAdminPostgresPreflightRequiresAdmin(t *testing.T) {
	server, admin, _ := newDatabaseAdminServer(t)
	user := admin
	user.IsAdmin = false

	recorder := httptest.NewRecorder()
	request := adminDatabaseRequest(t, server, user, http.MethodPost, "/api/admin/postgres-preflight", map[string]string{"dsn": "postgres://x"})
	server.apiAdminPostgresPreflight(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("non-admin status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestAdminPostgresPreflightRejectsGETAndEmptyDSN(t *testing.T) {
	server, admin, _ := newDatabaseAdminServer(t)

	recorder := httptest.NewRecorder()
	server.apiAdminPostgresPreflight(recorder, adminDatabaseRequest(t, server, admin, http.MethodGet, "/api/admin/postgres-preflight", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	server.apiAdminPostgresPreflight(recorder, adminDatabaseRequest(t, server, admin, http.MethodPost, "/api/admin/postgres-preflight", map[string]string{"dsn": "   "}))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("empty DSN status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

// TestAdminPostgresPreflightUnreachableTarget exercises the full handler path
// without needing a Postgres server: an unreachable target must produce a
// well-formed failed report, and the response must not echo the DSN's
// password.
func TestAdminPostgresPreflightUnreachableTarget(t *testing.T) {
	server, admin, _ := newDatabaseAdminServer(t)

	recorder := httptest.NewRecorder()
	request := adminDatabaseRequest(t, server, admin, http.MethodPost, "/api/admin/postgres-preflight",
		map[string]string{"dsn": "postgres://rolltop:supersecret@127.0.0.1:1/rolltop?connect_timeout=1"})
	server.apiAdminPostgresPreflight(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var report struct {
		OK     bool `json:"ok"`
		Checks []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.OK || len(report.Checks) != 1 || report.Checks[0].ID != "connect" || report.Checks[0].Status != "fail" {
		t.Fatalf("report = %s", recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "supersecret") {
		t.Fatal("response echoes the DSN password")
	}
}

// TestAdminPostgresPreflightAgainstRealPostgres runs the handler end to end
// when TEST_DATABASE_URL provides a live server; otherwise it is skipped.
func TestAdminPostgresPreflightAgainstRealPostgres(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	server, admin, _ := newDatabaseAdminServer(t)

	recorder := httptest.NewRecorder()
	request := adminDatabaseRequest(t, server, admin, http.MethodPost, "/api/admin/postgres-preflight", map[string]string{"dsn": dsn})
	server.apiAdminPostgresPreflight(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var report struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if !report.OK {
		t.Fatalf("preflight failed: %s", recorder.Body.String())
	}
}

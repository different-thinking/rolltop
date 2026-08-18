package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"rolltop/backend/store"
	"rolltop/backend/store/pgtestdb"
)

const pgSchemaPath = "/api/admin/postgres-schema"

func TestAdminPostgresSchemaRequiresAdmin(t *testing.T) {
	server, admin, _ := newDatabaseAdminServer(t)
	user := admin
	user.IsAdmin = false

	recorder := httptest.NewRecorder()
	server.apiAdminPostgresSchema(recorder, adminDatabaseRequest(t, server, user, http.MethodPost, pgSchemaPath,
		map[string]string{"dsn": "postgres://x", "action": "inspect"}))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("non-admin status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestAdminPostgresSchemaRejectsBadRequests(t *testing.T) {
	server, admin, _ := newDatabaseAdminServer(t)

	recorder := httptest.NewRecorder()
	server.apiAdminPostgresSchema(recorder, adminDatabaseRequest(t, server, admin, http.MethodGet, pgSchemaPath, nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	server.apiAdminPostgresSchema(recorder, adminDatabaseRequest(t, server, admin, http.MethodPost, pgSchemaPath,
		map[string]string{"dsn": "   ", "action": "inspect"}))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("empty DSN status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	// An unknown action must be refused before anything connects. Accepting one
	// and falling through to a default would make the destructive actions
	// reachable by a typo.
	recorder = httptest.NewRecorder()
	server.apiAdminPostgresSchema(recorder, adminDatabaseRequest(t, server, admin, http.MethodPost, pgSchemaPath,
		map[string]string{"dsn": "postgres://x/y", "action": "truncate"}))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown action status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

// TestAdminPostgresSchemaNeverEchoesCredentials keeps the console under the
// same rule as the preflight: the response carries no password, in any DSN
// spelling, for any action.
func TestAdminPostgresSchemaNeverEchoesCredentials(t *testing.T) {
	server, admin, _ := newDatabaseAdminServer(t)
	dsns := []string{
		"postgres://rolltop:supersecret@127.0.0.1:1/rolltop?connect_timeout=1",
		"host=127.0.0.1 port=notaport password = supersecret",
		"host=127.0.0.1 password = supersecret sslmode=bogus",
	}
	for _, dsn := range dsns {
		for _, action := range []string{"inspect", "create", "drop"} {
			recorder := httptest.NewRecorder()
			server.apiAdminPostgresSchema(recorder, adminDatabaseRequest(t, server, admin, http.MethodPost, pgSchemaPath,
				map[string]string{"dsn": dsn, "action": action}))
			if strings.Contains(recorder.Body.String(), "supersecret") {
				t.Errorf("%s of %q leaked the password: %s", action, dsn, recorder.Body.String())
			}
		}
	}
}

func TestAdminPostgresSchemaCapsBodySize(t *testing.T) {
	server, admin, _ := newDatabaseAdminServer(t)

	recorder := httptest.NewRecorder()
	server.apiAdminPostgresSchema(recorder, adminDatabaseRequest(t, server, admin, http.MethodPost, pgSchemaPath,
		map[string]string{"dsn": strings.Repeat("x", pgSchemaMaxBody+1), "action": "inspect"}))
	if recorder.Code == http.StatusOK {
		t.Fatalf("oversized body was accepted: %s", recorder.Body.String())
	}
}

func TestAdminPostgresSchemaRejectsConcurrentActions(t *testing.T) {
	server, admin, _ := newDatabaseAdminServer(t)

	pgSchemaLock <- struct{}{}
	defer func() { <-pgSchemaLock }()

	recorder := httptest.NewRecorder()
	server.apiAdminPostgresSchema(recorder, adminDatabaseRequest(t, server, admin, http.MethodPost, pgSchemaPath,
		map[string]string{"dsn": "postgres://x/y", "action": "inspect"}))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("second concurrent action status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

// TestAdminPostgresSchemaAgainstRealPostgres drives the whole staged loop
// through the HTTP layer, which is the path the admin page actually uses.
func TestAdminPostgresSchemaAgainstRealPostgres(t *testing.T) {
	dsn := pgtestdb.New(t)
	server, admin, _ := newDatabaseAdminServer(t)

	call := func(action string) store.PostgresState {
		t.Helper()
		recorder := httptest.NewRecorder()
		server.apiAdminPostgresSchema(recorder, adminDatabaseRequest(t, server, admin, http.MethodPost, pgSchemaPath,
			map[string]string{"dsn": dsn, "action": action}))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body = %s", action, recorder.Code, recorder.Body.String())
		}
		var state store.PostgresState
		if err := json.Unmarshal(recorder.Body.Bytes(), &state); err != nil {
			t.Fatalf("%s response: %v", action, err)
		}
		return state
	}

	if state := call("inspect"); state.Stage != store.PostgresStageEmpty {
		t.Fatalf("a fresh database inspects as %q", state.Stage)
	}
	created := call("create")
	if created.Stage != store.PostgresStageBaseline || created.Tables == 0 {
		t.Fatalf("after create: %+v", created)
	}
	if created.Summary == "" {
		t.Error("created state carries no summary for the operator")
	}
	if dropped := call("drop"); dropped.Stage != store.PostgresStageEmpty || dropped.Tables != 0 {
		t.Fatalf("after drop: %+v", dropped)
	}
}

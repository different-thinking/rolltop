// File overview: The Activity endpoint and its two write actions. What it must
// report, and what it must refuse: history is cleared, a run still in flight is
// not, and neither reaches another tenant's rows.

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"rolltop/backend/store"
)

type activityFixture struct {
	server  *Server
	db      *store.Store
	ctx     context.Context
	owner   store.User
	other   store.User
	running store.SyncRun
	done    store.SyncRun
}

func newActivityFixture(t *testing.T) activityFixture {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	owner, err := db.CreateUser(ctx, "activity-owner@example.test", "Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateUser(ctx, "activity-other@example.test", "Other", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.CreateMailAccount(ctx, store.MailAccount{
		UserID: owner.ID, Email: "owner@example.test", Host: "imap.example.test", Port: 993,
		Username: "owner@example.test", EncryptedPassword: "secret", UseTLS: true, Mailbox: "*",
	})
	if err != nil {
		t.Fatal(err)
	}
	running, err := db.CreateSyncRun(ctx, owner.ID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	done, err := db.CreateSyncRun(ctx, owner.ID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishSyncRun(ctx, owner.ID, done.ID, "ok", store.SyncProgress{MessagesStored: 3}, ""); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: db, masterKey: bytes.Repeat([]byte{7}, 32), events: newEventHub()}
	return activityFixture{server: server, db: db, ctx: ctx, owner: owner, other: other, running: running, done: done}
}

func activityRequest(t *testing.T, server *Server, user store.User, method, target string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, target, nil)
	request = request.WithContext(context.WithValue(request.Context(), userContextKey, currentUser{User: user}))
	const csrfBase = "activity-action-csrf"
	request.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrfBase})
	request.Header.Set("X-CSRF-Token", server.csrfForBase(csrfBase))
	return request
}

// The whole point of the view is that one read answers "what is running": the
// mail syncs and the category backlog arrive together, not from three pages.
func TestActivityReportsRunsAndTheCategoryBacklog(t *testing.T) {
	f := newActivityFixture(t)
	response := httptest.NewRecorder()
	f.server.handleAPI(response, activityRequest(t, f.server, f.owner, http.MethodGet, "/api/activity"))
	if response.Code != http.StatusOK {
		t.Fatalf("activity status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var payload struct {
		SyncRuns []struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		} `json:"sync_runs"`
		Workers           []map[string]any `json:"workers"`
		Services          []map[string]any `json:"services"`
		CategoriesPending int              `json:"categories_pending"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	seen := map[int64]string{}
	for _, run := range payload.SyncRuns {
		seen[run.ID] = run.Status
	}
	if seen[f.running.ID] != "running" || seen[f.done.ID] != "ok" {
		t.Fatalf("sync runs = %+v, want the running run %d and the finished run %d", payload.SyncRuns, f.running.ID, f.done.ID)
	}
	if payload.Workers == nil || payload.Services == nil {
		t.Fatal("workers and services must be lists even when empty, so the view never has to guard for null")
	}
	if payload.CategoriesPending != 0 {
		t.Fatalf("categories pending = %d, want 0 for an empty mailbox", payload.CategoriesPending)
	}
}

// Clearing is for history. A run still in flight has a worker writing progress
// into it, and deleting the row underneath that worker is not what the user who
// pressed a button labelled "clear finished runs" asked for.
func TestClearingHistoryKeepsRunningRuns(t *testing.T) {
	f := newActivityFixture(t)
	response := httptest.NewRecorder()
	f.server.handleAPI(response, activityRequest(t, f.server, f.owner, http.MethodDelete, "/api/activity/history"))
	if response.Code != http.StatusOK {
		t.Fatalf("clear status = %d, want 200: %s", response.Code, response.Body.String())
	}
	if _, err := f.db.GetSyncRunForUser(f.ctx, f.owner.ID, f.running.ID); err != nil {
		t.Fatalf("running run was removed by a history clear: %v", err)
	}
	if _, err := f.db.GetSyncRunForUser(f.ctx, f.owner.ID, f.done.ID); !store.IsNotFound(err) {
		t.Fatalf("finished run survived a history clear: %v", err)
	}
}

// Deleting one row follows the same rule as clearing all of them, and says so
// rather than reporting a silent success that removed nothing.
func TestDeletingARunningSyncRunIsRefused(t *testing.T) {
	f := newActivityFixture(t)
	response := httptest.NewRecorder()
	f.server.handleAPI(response, activityRequest(t, f.server, f.owner, http.MethodDelete, "/api/sync-runs/"+strconv.FormatInt(f.running.ID, 10)))
	if response.Code != http.StatusConflict {
		t.Fatalf("delete status = %d, want 409: %s", response.Code, response.Body.String())
	}
	if _, err := f.db.GetSyncRunForUser(f.ctx, f.owner.ID, f.running.ID); err != nil {
		t.Fatalf("running run was deleted anyway: %v", err)
	}
}

// Tenant isolation, on the route that deletes: another user's finished run is
// not theirs to remove, and must not even be found.
func TestActivityDeleteCannotReachAnotherTenantsRun(t *testing.T) {
	f := newActivityFixture(t)
	response := httptest.NewRecorder()
	f.server.handleAPI(response, activityRequest(t, f.server, f.other, http.MethodDelete, "/api/sync-runs/"+strconv.FormatInt(f.done.ID, 10)))
	if response.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant delete status = %d, want 404: %s", response.Code, response.Body.String())
	}
	if _, err := f.db.GetSyncRunForUser(f.ctx, f.owner.ID, f.done.ID); err != nil {
		t.Fatalf("another tenant's delete removed the run: %v", err)
	}
}

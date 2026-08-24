// File overview: What the admin search-index rebuild says when it cannot start.
//
// The rebuild reserves every search-visible folder of an account at once, so a
// single folder held by other work refuses the whole tenant. The refusal used
// to be one sentence for every cause — "sync or full-text reindexing is already
// running" — which is a dead end: it names neither the folder to wait for nor
// the case where nothing is running and waiting will not help. These tests hold
// the refusal to naming what it found.

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rolltop/backend/blob"
	"rolltop/backend/search"
	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
	"rolltop/backend/syncer"
)

func TestAdminSearchIndexRebuildConflictNamesTheFolderHoldingIt(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	searchService, err := search.Open(filepath.Join(dir, "bleve"))
	if err != nil {
		t.Fatal(err)
	}
	defer searchService.Close()
	blobStore := blob.New(dir)

	admin, err := db.CreateUser(ctx, "rebuild-admin@example.test", "Rebuild Admin", "hash", true)
	if err != nil {
		t.Fatal(err)
	}
	_, mailbox, _ := createSearchRebuildMessage(t, ctx, db, blobStore, admin, "INBOX", 11, "adminrebuildneedle")

	runnerContext, cancelRunner := context.WithCancel(context.Background())
	defer cancelRunner()
	syncService := &syncer.Service{Store: db, Blobs: blobStore, Search: searchService}
	runner := syncer.NewRunnerWithContext(runnerContext, syncService)
	server := &Server{
		store: db, blobs: blobStore, search: searchService, syncer: syncService, syncRunner: runner,
		masterKey: bytes.Repeat([]byte{7}, 32), events: newEventHub(),
	}

	started := make(chan struct{})
	release := make(chan struct{})
	blockingRun, maintenanceStarted, err := runner.StartMailboxMaintenance(admin.ID, mailbox, "Blocking maintenance",
		func(ctx context.Context, _ int64, _ *store.SyncProgress) error {
			close(started)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	if err != nil || !maintenanceStarted {
		t.Fatalf("start blocking maintenance started=%t err=%v", maintenanceStarted, err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("blocking maintenance did not start")
	}

	recorder := httptest.NewRecorder()
	server.apiAdminSearchIndex(recorder, adminDatabaseRequest(t, server, admin, http.MethodPost, searchIndexPath,
		map[string]int64{"user_id": admin.ID}))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("blocked rebuild status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var conflict struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &conflict); err != nil {
		t.Fatalf("decode conflict: %v (body %s)", err, recorder.Body.String())
	}
	// The account, the folder and the kind of work: each one is a step the
	// operator would otherwise have to guess at.
	for _, want := range []string{admin.Email, `"INBOX"`, "Folder maintenance", "Follow it in Activity"} {
		if !strings.Contains(conflict.Error, want) {
			t.Fatalf("conflict error = %q, want it to name %s", conflict.Error, want)
		}
	}

	close(release)
	if run := waitForSearchRebuildRun(t, ctx, db, admin.ID, blockingRun.ID); run.Status != "ok" {
		t.Fatalf("blocking maintenance run = %+v", run)
	}

	// Once the folder is free the same request starts, so the refusal really was
	// about the reservation and not about the request.
	recorder = httptest.NewRecorder()
	server.apiAdminSearchIndex(recorder, adminDatabaseRequest(t, server, admin, http.MethodPost, searchIndexPath,
		map[string]int64{"user_id": admin.ID}))
	report := decodeSearchIndexReport(t, recorder)
	if report.StartedRuns != 1 || len(report.Blocked) != 0 {
		t.Fatalf("released rebuild report = %+v", report)
	}
}

// The recovery gate is one gate for the whole tenant. Reported once per mail
// server it would read as several independent recoveries, so servers sharing a
// reason are named together — while genuinely different causes stay apart.
func TestDescribeSearchRebuildBlocksGroupsServersSharingAReason(t *testing.T) {
	const recovery = "Folder recovery is still pending for this user, and it holds every mail server until it finishes."
	const held = `Folder sync is already running for the folder "INBOX". Follow it in Activity, then try again.`

	got := describeSearchRebuildBlocks([]searchRebuildBlock{
		{Account: "Gmail", Reason: recovery},
		{Account: "Fastmail", Reason: held},
		{Account: "Work", Reason: recovery},
	})
	want := "Gmail, Work: " + recovery + " Fastmail: " + held
	if got != want {
		t.Fatalf("description = %q, want %q", got, want)
	}

	if got := describeSearchRebuildBlocks(nil); got != "" {
		t.Fatalf("empty description = %q", got)
	}
}

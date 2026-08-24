// File overview: The storage page's full-text figures, and the rebuild a user
// may start for their own index.
//
// The figures are the point: a page that measures the data volume reports zero
// bytes and a healthy-looking absence of missing folders on the Postgres
// backend, where the index is rows rather than files. These tests hold the page
// to what the backend in force actually knows.

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"rolltop/backend/search"
	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
)

type storageSearchFixture struct {
	server  *Server
	db      *store.Store
	search  *search.Service
	ctx     context.Context
	owner   store.User
	other   store.User
	mailbox store.Mailbox
}

func newStorageSearchFixture(t *testing.T) storageSearchFixture {
	t.Helper()
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	owner, err := db.CreateUser(ctx, "storage-owner@example.test", "Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateUser(ctx, "storage-other@example.test", "Other", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.CreateMailAccount(ctx, store.MailAccount{
		UserID: owner.ID, Label: "Test", Email: "storage-owner@example.test",
		Host: "imap.example.test", Port: 993, Username: "owner", EncryptedPassword: "secret",
		UseTLS: true, Mailbox: "*",
	})
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := db.GetOrCreateMailbox(ctx, owner.ID, account.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	svc := search.OpenPostgresBackend(db)
	t.Cleanup(func() { _ = svc.Close() })
	server := &Server{
		store: db, search: svc, events: newEventHub(),
		masterKey: bytes.Repeat([]byte{9}, 32),
	}
	return storageSearchFixture{server: server, db: db, search: svc, ctx: ctx, owner: owner, other: other, mailbox: mailbox}
}

func (f storageSearchFixture) seedMessage(t *testing.T, uid uint32, subject string) store.MessageRecord {
	t.Helper()
	path := fmt.Sprintf("users/%d/storage/uid-%d.eml", f.owner.ID, uid)
	blob, err := f.db.CreateBlob(f.ctx, store.BlobRecord{
		UserID: f.owner.ID, Kind: "message", Path: path, SHA256: fmt.Sprintf("%064d", uid), Size: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := f.db.CreateMessage(f.ctx, store.CreateMessage{
		UserID: f.owner.ID, AccountID: f.mailbox.AccountID, MailboxID: f.mailbox.ID, BlobID: blob.ID,
		MessageIDHeader: fmt.Sprintf("<storage-%d@example.test>", uid),
		CanonicalSHA256: fmt.Sprintf("%064d", uid), MessageIDHash: fmt.Sprintf("storage-hash-%d", uid),
		ThreadKey: fmt.Sprintf("storage-thread-%d", uid), Subject: subject, BodyText: subject,
		FromAddr: "alice@example.test", ToAddr: "bob@example.test",
		Date: time.Now(), InternalDate: time.Now(),
		UID: uid, UIDValidity: f.mailbox.UIDValidity, Size: 1, BlobPath: path,
	})
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

// The Postgres backend has no directory to walk. Measuring the volume there
// reports an index of zero bytes for a search that is working, which is the
// failure this page has to be unable to produce.
func TestStorageStatsReportThePostgresIndexRatherThanTheVolume(t *testing.T) {
	f := newStorageSearchFixture(t)
	indexed := f.seedMessage(t, 1, "quarterly report")
	if err := f.search.IndexMessages(f.ctx, []search.MessageIndexDocument{{Message: indexed}}); err != nil {
		t.Fatal(err)
	}
	// A second message nothing indexed is what makes the coverage figure mean
	// something: without it the page cannot tell a complete index from one
	// missing half the mailbox.
	f.seedMessage(t, 2, "not indexed yet")

	stats := f.server.storageStatsForUser(f.owner.ID)
	if stats.Error != "" {
		t.Fatalf("storage stats reported errors: %s", stats.Error)
	}
	if stats.SearchBackend != search.BackendPostgres {
		t.Fatalf("search backend = %q, want %q", stats.SearchBackend, search.BackendPostgres)
	}
	if !stats.IndexPresent || stats.IndexBytes <= 0 {
		t.Fatalf("index present=%v bytes=%d, want a measured index", stats.IndexPresent, stats.IndexBytes)
	}
	if stats.IndexMessageCount != 1 || stats.FullTextSearchMessageCount != 2 {
		t.Fatalf("coverage = %d of %d, want 1 of 2", stats.IndexMessageCount, stats.FullTextSearchMessageCount)
	}
	// Naming a directory here points an operator at a path that will never
	// exist, and the Bleve breakdown describes files this backend does not have.
	if stats.IndexPath != "" {
		t.Fatalf("index path = %q, want none on the Postgres backend", stats.IndexPath)
	}
	if stats.IndexBreakdown != (StorageIndexBreakdown{}) {
		t.Fatalf("index breakdown = %+v, want empty on the Postgres backend", stats.IndexBreakdown)
	}
}

// A tenant with nothing indexed has to read as exactly that, and without an
// error: it is the ordinary state of a new account, not a failure.
func TestStorageStatsReportAnUnbuiltIndexAsAbsent(t *testing.T) {
	f := newStorageSearchFixture(t)
	f.seedMessage(t, 1, "never indexed")
	stats := f.server.storageStatsForUser(f.owner.ID)
	if stats.Error != "" {
		t.Fatalf("storage stats reported errors: %s", stats.Error)
	}
	if stats.IndexPresent {
		t.Fatal("an index with no documents reported present")
	}
	if stats.IndexMessageCount != 0 || stats.FullTextSearchMessageCount != 1 {
		t.Fatalf("coverage = %d of %d, want 0 of 1", stats.IndexMessageCount, stats.FullTextSearchMessageCount)
	}
}

// Folders waiting for a rebuild are how a reader learns that search is
// answering from less than their mailbox. The count is per tenant, like every
// other figure on the page.
func TestStorageStatsCountFoldersWaitingForTheIndexPerTenant(t *testing.T) {
	f := newStorageSearchFixture(t)
	f.seedMessage(t, 1, "anything")
	marked, err := f.db.MarkUserSearchIndexRepairRequired(f.ctx, f.owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if marked != 1 {
		t.Fatalf("marked %d folders, want 1", marked)
	}
	stats := f.server.storageStatsForUser(f.owner.ID)
	if stats.FoldersNeedingRebuild != 1 {
		t.Fatalf("folders needing a rebuild = %d, want 1", stats.FoldersNeedingRebuild)
	}
	if other := f.server.storageStatsForUser(f.other.ID); other.FoldersNeedingRebuild != 0 || other.IndexMessageCount != 0 {
		t.Fatalf("another tenant's figures leaked: %+v", other)
	}
}

// The rebuild re-reads a whole mailbox, so it is a POST, it is CSRF-protected,
// and it refuses outright when this server has no indexer behind it — reporting
// success there would leave a user waiting for work that never started.
func TestOwnSearchRebuildRefusesWhatItCannotDo(t *testing.T) {
	f := newStorageSearchFixture(t)

	get := httptest.NewRequest(http.MethodGet, "/api/storage/search-index/rebuild", nil)
	get = get.WithContext(context.WithValue(get.Context(), userContextKey, currentUser{User: f.owner}))
	response := httptest.NewRecorder()
	f.server.handleAPI(response, get)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", response.Code)
	}

	noCSRF := httptest.NewRequest(http.MethodPost, "/api/storage/search-index/rebuild", nil)
	noCSRF = noCSRF.WithContext(context.WithValue(noCSRF.Context(), userContextKey, currentUser{User: f.owner}))
	response = httptest.NewRecorder()
	f.server.handleAPI(response, noCSRF)
	if response.Code != http.StatusForbidden {
		t.Fatalf("POST without CSRF status = %d, want 403", response.Code)
	}

	response = httptest.NewRecorder()
	f.server.handleAPI(response, activityRequest(t, f.server, f.owner, http.MethodPost, "/api/storage/search-index/rebuild"))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST without an indexer status = %d, want 503: %s", response.Code, response.Body.String())
	}

	anonymous := httptest.NewRequest(http.MethodPost, "/api/storage/search-index/rebuild", nil)
	response = httptest.NewRecorder()
	f.server.handleAPI(response, anonymous)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", response.Code)
	}
}

// The activity view is where "is it building right now" is answered, so index
// upkeep has to arrive in the same read as the syncs — scoped the same way the
// service scopes it.
func TestActivityReportsIndexMaintenanceAndWaitingFolders(t *testing.T) {
	f := newStorageSearchFixture(t)
	f.seedMessage(t, 1, "anything")
	if _, err := f.db.MarkUserSearchIndexRepairRequired(f.ctx, f.owner.ID); err != nil {
		t.Fatal(err)
	}
	done := f.search.StartMaintenance("search_fuzzy_index", "Building the index for typo-tolerant search", 0, time.Now())
	defer done()

	response := httptest.NewRecorder()
	f.server.handleAPI(response, activityRequest(t, f.server, f.owner, http.MethodGet, "/api/activity"))
	if response.Code != http.StatusOK {
		t.Fatalf("activity status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var payload struct {
		Workers []struct {
			Kind        string `json:"kind"`
			Label       string `json:"label"`
			Cancellable bool   `json:"cancellable"`
		} `json:"workers"`
		SearchIndexPending int `json:"search_index_pending"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Workers) != 1 || payload.Workers[0].Kind != "search_fuzzy_index" {
		t.Fatalf("workers = %+v, want the fuzzy index build", payload.Workers)
	}
	// Nothing a cancel would free: it is one statement against the database.
	if payload.Workers[0].Cancellable {
		t.Fatal("index maintenance offered a stop button")
	}
	if payload.SearchIndexPending != 1 {
		t.Fatalf("folders waiting = %d, want 1", payload.SearchIndexPending)
	}
	response = httptest.NewRecorder()
	f.server.handleAPI(response, activityRequest(t, f.server, f.other, http.MethodGet, "/api/activity"))
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.SearchIndexPending != 0 {
		t.Fatalf("another tenant sees %d waiting folders, want 0", payload.SearchIndexPending)
	}
	// The trigram index is one index for the whole database, so it is the one
	// piece of upkeep every tenant is entitled to see.
	if len(payload.Workers) != 1 || payload.Workers[0].Kind != "search_fuzzy_index" {
		t.Fatalf("other tenant's workers = %+v, want the whole-server build", payload.Workers)
	}
}

// A folder included in search that no sync fills is invisible to every other
// figure on this page: coverage compares the index against the messages table,
// and mail that was never fetched is in neither. Without this the page reports
// full coverage of a mailbox missing whole folders, which is precisely the
// answer a reader gets when their sent mail cannot be found.
func TestStorageStatsNameFoldersSearchedButNeverSynced(t *testing.T) {
	f := newStorageSearchFixture(t)
	indexed := f.seedMessage(t, 1, "in the inbox")
	if err := f.search.IndexMessages(f.ctx, []search.MessageIndexDocument{{Message: indexed}}); err != nil {
		t.Fatal(err)
	}

	// The default for a discovered folder: included in search, and synced only
	// when someone asks for it by hand.
	sent, err := f.db.GetOrCreateMailbox(f.ctx, f.owner.ID, f.mailbox.AccountID, "Sent")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateMailboxSettings(f.ctx, f.owner.ID, sent.ID, store.MailboxSettings{
		SyncMode: "manual", Role: "sent", ShowInSidebar: true, IncludeInSearch: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateMailboxRemoteStatus(f.ctx, f.owner.ID, sent.ID, 42, 0, 43, uint32(sent.UIDValidity)); err != nil {
		t.Fatal(err)
	}

	stats := f.server.storageStatsForUser(f.owner.ID)
	if stats.Error != "" {
		t.Fatalf("storage stats reported errors: %s", stats.Error)
	}
	// The coverage figures are complete and say nothing about the gap, which is
	// the whole reason this is reported separately.
	if stats.IndexMessageCount != stats.FullTextSearchMessageCount {
		t.Fatalf("coverage = %d of %d, want the fixture's index to be complete so the unsynced gap is the only one",
			stats.IndexMessageCount, stats.FullTextSearchMessageCount)
	}
	if stats.UnsyncedSearchFolders != 1 {
		t.Fatalf("folders searched but never synced = %d, want 1", stats.UnsyncedSearchFolders)
	}
	if stats.UnsyncedSearchMessages != 42 {
		t.Fatalf("messages in those folders = %d, want the 42 the server reported", stats.UnsyncedSearchMessages)
	}
	if len(stats.UnsyncedSearchFolderNames) != 1 || stats.UnsyncedSearchFolderNames[0] != "Sent" {
		t.Fatalf("named folders = %v, want just Sent", stats.UnsyncedSearchFolderNames)
	}

	// A folder inheriting its mode from a parent that holds mail. The parent is
	// not itself reportable - it has local mail - so resolving the child means
	// reading the mode of a folder outside the reported set. Every folder's
	// mode is read for that, and a version that only read the candidates'
	// would resolve this child to auto and drop it silently.
	archive, err := f.db.GetOrCreateMailbox(f.ctx, f.owner.ID, f.mailbox.AccountID, "Archive")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateMailboxSettings(f.ctx, f.owner.ID, archive.ID, store.MailboxSettings{
		SyncMode: "manual", ShowInSidebar: true, IncludeInSearch: true,
	}); err != nil {
		t.Fatal(err)
	}
	archived := f.mailbox
	f.mailbox = archive
	f.seedMessage(t, 3, "already fetched into Archive")
	f.mailbox = archived
	year, err := f.db.GetOrCreateMailbox(f.ctx, f.owner.ID, f.mailbox.AccountID, "Archive/2025")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateMailboxSettings(f.ctx, f.owner.ID, year.ID, store.MailboxSettings{
		SyncMode: "inherit", ShowInSidebar: true, IncludeInSearch: true,
	}); err != nil {
		t.Fatal(err)
	}
	f.server.invalidateStorageStats(f.owner.ID)
	stats = f.server.storageStatsForUser(f.owner.ID)
	if stats.UnsyncedSearchFolders != 2 {
		t.Fatalf("folders searched but never synced = %d, want Sent and the folder inheriting manual from Archive", stats.UnsyncedSearchFolders)
	}

	// Gmail's label views carry sync mode never by default, and their mail is
	// already stored in the real folder it also appears in. Reporting one would
	// push a reader toward mirroring most of their mailbox a second time to
	// find mail that search can already find.
	labelView, err := f.db.GetOrCreateMailbox(f.ctx, f.owner.ID, f.mailbox.AccountID, "[Gmail]/Alle Nachrichten")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateMailboxSettings(f.ctx, f.owner.ID, labelView.ID, store.MailboxSettings{
		SyncMode: "never", Role: "all", IncludeInSearch: true,
	}); err != nil {
		t.Fatal(err)
	}
	f.server.invalidateStorageStats(f.owner.ID)
	if stats := f.server.storageStatsForUser(f.owner.ID); stats.UnsyncedSearchFolders != 2 {
		t.Fatalf("folders searched but never synced = %d, want the label view left out of the two already reported", stats.UnsyncedSearchFolders)
	}

	// A manual folder someone has synced by hand is searchable up to that sync,
	// and telling them otherwise while they are looking at the mail is worse
	// than saying nothing.
	f.mailbox = sent
	f.seedMessage(t, 2, "fetched by hand")
	f.server.invalidateStorageStats(f.owner.ID)
	stats = f.server.storageStatsForUser(f.owner.ID)
	if stats.UnsyncedSearchFolders != 1 {
		t.Fatalf("folders searched but never synced = %d after a manual sync, want Sent dropped and the inheriting folder kept",
			stats.UnsyncedSearchFolders)
	}
	for _, name := range stats.UnsyncedSearchFolderNames {
		if name == "Sent" {
			t.Fatalf("named folders = %v, want Sent gone once it holds mail", stats.UnsyncedSearchFolderNames)
		}
	}
	if stats.UnsyncedSearchMessages != 0 {
		t.Fatalf("messages in those folders = %d, want none: only Sent had a server count", stats.UnsyncedSearchMessages)
	}
	if other := f.server.storageStatsForUser(f.other.ID); other.UnsyncedSearchFolders != 0 {
		t.Fatalf("another tenant's folders leaked: %+v", other)
	}
}

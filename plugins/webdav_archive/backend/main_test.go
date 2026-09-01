package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	mmcrypto "rolltop/backend/crypto"
	"rolltop/backend/plugins"
	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
)

var testMasterKey = []byte("0123456789abcdef0123456789abcdef")

// openArchiveStore opens a store carrying this plugin's own migrations. The
// frozen baseline predates them, so its tables reach a test database only this
// way.
func openArchiveStore(t *testing.T) *store.Store {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate the WebDAV archive test source")
	}
	manifests, err := plugins.LoadManifests(filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", "..")))
	if err != nil {
		t.Fatal(err)
	}
	var selected []plugins.Manifest
	for _, manifest := range manifests {
		if manifest.ID == pluginID {
			selected = append(selected, manifest)
		}
	}
	if len(selected) != 1 {
		t.Fatalf("WebDAV archive manifests = %d, want 1", len(selected))
	}
	st, err := storetest.OpenWithManifests(t, selected)
	if err != nil {
		t.Fatalf("open store through the plugin's own migrations: %v", err)
	}
	return st
}

func archiveFixture(t *testing.T, st *store.Store, email string) (store.User, store.MailAccount, store.Mailbox) {
	t.Helper()
	ctx := context.Background()
	user, err := st.CreateUser(ctx, email, "Archive", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := st.CreateMailAccount(ctx, store.MailAccount{
		UserID: user.ID, Email: email, Host: "imap.example.test", Port: 993,
		Username: email, EncryptedPassword: "secret", UseTLS: true, Mailbox: "*",
	})
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := st.GetOrCreateMailbox(ctx, user.ID, account.ID, "Recordings")
	if err != nil {
		t.Fatal(err)
	}
	return user, account, mailbox
}

func newTestTarget(t *testing.T, db *sql.DB, userID, mailboxID int64) target {
	t.Helper()
	encrypted, err := mmcrypto.EncryptString(testMasterKey, "app-password")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := persistTarget(context.Background(), db, target{
		UserID: userID, Name: "Cloud", Enabled: true,
		BaseURL: "https://cloud.example.test/dav/", Username: "me",
		EncryptedPassword: encrypted, WatchMailboxID: mailboxID,
		ContentTypes: "audio/", PathTemplate: defaultPathTemplate,
	})
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

func TestPersistTargetStoresThePasswordEncrypted(t *testing.T) {
	st := openArchiveStore(t)
	db := st.DB()
	user, _, mailbox := archiveFixture(t, st, "archive@example.test")
	saved := newTestTarget(t, db, user.ID, mailbox.ID)

	if saved.EncryptedPassword == "app-password" {
		t.Fatal("the password was stored as plain text")
	}
	plaintext, err := mmcrypto.DecryptString(testMasterKey, saved.EncryptedPassword)
	if err != nil || plaintext != "app-password" {
		t.Fatalf("decrypt = %q, %v", plaintext, err)
	}
	// The view a browser is handed says only whether a password is set.
	if view := presentTarget(saved); !view.HasPassword {
		t.Fatal("the settings view lost the fact that a password is set")
	} else if fmt.Sprint(view) == fmt.Sprint(saved) {
		t.Fatal("the view is the record itself, which would ship the ciphertext to the browser")
	}
}

func TestTargetsAreScopedToTheirOwner(t *testing.T) {
	st := openArchiveStore(t)
	db := st.DB()
	ctx := context.Background()
	owner, _, mailbox := archiveFixture(t, st, "owner@example.test")
	other, _, _ := archiveFixture(t, st, "other@example.test")
	saved := newTestTarget(t, db, owner.ID, mailbox.ID)

	if _, err := getTarget(ctx, db, other.ID, saved.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("another user read the target: %v", err)
	}
	if err := deleteTarget(ctx, db, other.ID, saved.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("another user deleted the target: %v", err)
	}
	if err := setTargetEnabled(ctx, db, other.ID, saved.ID, false); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("another user paused the target: %v", err)
	}
	items, err := listTargets(ctx, db, other.ID, false)
	if err != nil || len(items) != 0 {
		t.Fatalf("targets for another user = %d, %v", len(items), err)
	}
}

// A target watching one folder is offered that folder's mail; a target watching
// nothing is offered every folder's.
func TestListTargetsWatchingSelectsByFolder(t *testing.T) {
	st := openArchiveStore(t)
	db := st.DB()
	ctx := context.Background()
	user, account, mailbox := archiveFixture(t, st, "watch@example.test")
	other, err := st.GetOrCreateMailbox(ctx, user.ID, account.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	watching := newTestTarget(t, db, user.ID, mailbox.ID)
	everywhere, err := persistTarget(ctx, db, target{
		UserID: user.ID, Name: "All", Enabled: true, BaseURL: "https://cloud.example.test/all/",
		ContentTypes: "audio/", PathTemplate: defaultPathTemplate,
	})
	if err != nil {
		t.Fatal(err)
	}
	paused, err := persistTarget(ctx, db, target{
		UserID: user.ID, Name: "Paused", Enabled: false, BaseURL: "https://cloud.example.test/paused/",
		WatchMailboxID: mailbox.ID, ContentTypes: "audio/", PathTemplate: defaultPathTemplate,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := listTargetsWatching(ctx, db, user.ID, mailbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[int64]bool{}
	for _, item := range got {
		ids[item.ID] = true
	}
	if !ids[watching.ID] || !ids[everywhere.ID] {
		t.Fatalf("targets for the watched folder = %+v", ids)
	}
	if ids[paused.ID] {
		t.Fatal("a paused target was offered work")
	}

	got, err = listTargetsWatching(ctx, db, user.ID, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != everywhere.ID {
		t.Fatalf("targets for an unwatched folder = %+v, want only the one watching every folder", got)
	}
}

func TestEnqueueUploadIsIdempotentPerAttachment(t *testing.T) {
	st := openArchiveStore(t)
	db := st.DB()
	ctx := context.Background()
	user, _, mailbox := archiveFixture(t, st, "queue@example.test")
	configured := newTestTarget(t, db, user.ID, mailbox.ID)

	item := upload{UserID: user.ID, TargetID: configured.ID, MessageID: 11, AttachmentID: 3,
		Filename: "memo.m4a", ContentType: "audio/mp4", Size: 4}
	added, err := enqueueUpload(ctx, db, item)
	if err != nil || !added {
		t.Fatalf("first enqueue = %v, %v", added, err)
	}
	// A refetched UID walks the same message past the hook again; it must not
	// queue a second copy of work that is already recorded.
	added, err = enqueueUpload(ctx, db, item)
	if err != nil {
		t.Fatal(err)
	}
	if added {
		t.Fatal("the same attachment was queued twice")
	}
	rows, err := listUploads(ctx, db, user.ID, 0, "", 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("queued rows = %d, %v", len(rows), err)
	}
}

func TestClaimDueUploadsTakesEachRowOnce(t *testing.T) {
	st := openArchiveStore(t)
	db := st.DB()
	ctx := context.Background()
	user, _, mailbox := archiveFixture(t, st, "claim@example.test")
	configured := newTestTarget(t, db, user.ID, mailbox.ID)
	for i := int64(1); i <= 3; i++ {
		if _, err := enqueueUpload(ctx, db, upload{UserID: user.ID, TargetID: configured.ID,
			MessageID: i, AttachmentID: i, Filename: "memo.m4a", ContentType: "audio/mp4"}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	first, err := claimDueUploads(ctx, db, user.ID, now, 10)
	if err != nil || len(first) != 3 {
		t.Fatalf("first claim = %d rows, %v", len(first), err)
	}
	// A second sweep -- the ticker racing a manual run -- must find nothing,
	// because the first claim marked the rows uploading in the same statement.
	second, err := claimDueUploads(ctx, db, user.ID, now, 10)
	if err != nil || len(second) != 0 {
		t.Fatalf("second claim = %d rows, %v; work was handed out twice", len(second), err)
	}
	// A stopped process leaves them uploading forever unless they are released.
	if err := releaseInterruptedUploads(ctx, db); err != nil {
		t.Fatal(err)
	}
	third, err := claimDueUploads(ctx, db, user.ID, now, 10)
	if err != nil || len(third) != 3 {
		t.Fatalf("claim after release = %d rows, %v", len(third), err)
	}
}

func TestFailUploadClimbsTheLadderThenGivesUp(t *testing.T) {
	st := openArchiveStore(t)
	db := st.DB()
	ctx := context.Background()
	user, _, mailbox := archiveFixture(t, st, "fail@example.test")
	configured := newTestTarget(t, db, user.ID, mailbox.ID)
	if _, err := enqueueUpload(ctx, db, upload{UserID: user.ID, TargetID: configured.ID,
		MessageID: 5, AttachmentID: 1, Filename: "memo.m4a"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		rows, err := claimDueUploads(ctx, db, user.ID, now.Add(2*time.Hour), 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("attempt %d claimed %d rows", attempt, len(rows))
		}
		if err := failUpload(ctx, db, rows[0], errors.New("the server refused"), now); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := listUploads(ctx, db, user.ID, 0, "", 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %d, %v", len(rows), err)
	}
	if rows[0].Status != statusAbandoned {
		t.Fatalf("status = %q after %d failures, want it to stop retrying", rows[0].Status, maxAttempts)
	}
	if rows[0].LastError == "" {
		t.Fatal("an abandoned upload kept no reason")
	}
	// Given up is not gone: the reader can put it back on the queue.
	if err := retryUpload(ctx, db, user.ID, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	back, err := listUploads(ctx, db, user.ID, 0, statusQueued, 0)
	if err != nil || len(back) != 1 || back[0].Attempts != 0 {
		t.Fatalf("after retry = %+v, %v", back, err)
	}
}

func TestDuplicateUploadPathFindsTheSameBytesAlreadyFiled(t *testing.T) {
	st := openArchiveStore(t)
	db := st.DB()
	ctx := context.Background()
	user, _, mailbox := archiveFixture(t, st, "dupe@example.test")
	configured := newTestTarget(t, db, user.ID, mailbox.ID)
	if _, err := enqueueUpload(ctx, db, upload{UserID: user.ID, TargetID: configured.ID,
		MessageID: 1, AttachmentID: 1, Filename: "memo.m4a"}); err != nil {
		t.Fatal(err)
	}
	rows, err := claimDueUploads(ctx, db, user.ID, time.Now().UTC(), 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("claim = %d, %v", len(rows), err)
	}
	if err := completeUpload(ctx, db, rows[0], statusDone, "2026/05/memo.m4a", "hash-abc"); err != nil {
		t.Fatal(err)
	}

	// The same recording arriving again -- a resend, a CC filed separately.
	if _, err := enqueueUpload(ctx, db, upload{UserID: user.ID, TargetID: configured.ID,
		MessageID: 2, AttachmentID: 1, Filename: "memo.m4a"}); err != nil {
		t.Fatal(err)
	}
	next, err := claimDueUploads(ctx, db, user.ID, time.Now().UTC(), 10)
	if err != nil || len(next) != 1 {
		t.Fatalf("claim = %d, %v", len(next), err)
	}
	path, err := duplicateUploadPath(ctx, db, user.ID, configured.ID, "hash-abc", next[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if path != "2026/05/memo.m4a" {
		t.Fatalf("duplicate path = %q, want where the same bytes already went", path)
	}
	// A hash nothing has carried is not a duplicate, and neither is one filed
	// for a different target.
	if path, err := duplicateUploadPath(ctx, db, user.ID, configured.ID, "hash-xyz", 0); err != nil || path != "" {
		t.Fatalf("unknown hash = %q, %v", path, err)
	}
	if path, err := duplicateUploadPath(ctx, db, user.ID, configured.ID+999, "hash-abc", 0); err != nil || path != "" {
		t.Fatalf("another target's hash = %q, %v", path, err)
	}
}

func TestUsersWithWorkListsOnlyTenantsWithSomethingDue(t *testing.T) {
	st := openArchiveStore(t)
	db := st.DB()
	ctx := context.Background()
	busy, _, mailbox := archiveFixture(t, st, "busy@example.test")
	idle, _, _ := archiveFixture(t, st, "idle@example.test")
	configured := newTestTarget(t, db, busy.ID, mailbox.ID)
	if _, err := enqueueUpload(ctx, db, upload{UserID: busy.ID, TargetID: configured.ID,
		MessageID: 1, AttachmentID: 1, Filename: "memo.m4a"}); err != nil {
		t.Fatal(err)
	}
	ids, err := usersWithWork(ctx, db, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != busy.ID {
		t.Fatalf("users with work = %v, want only the one with a queued row (idle=%d)", ids, idle.ID)
	}
}

// Removing a target takes its queue with it: a row pointing at a server that is
// no longer configured has nowhere to go.
func TestDeletingATargetClearsItsQueue(t *testing.T) {
	st := openArchiveStore(t)
	db := st.DB()
	ctx := context.Background()
	user, _, mailbox := archiveFixture(t, st, "cascade@example.test")
	configured := newTestTarget(t, db, user.ID, mailbox.ID)
	if _, err := enqueueUpload(ctx, db, upload{UserID: user.ID, TargetID: configured.ID,
		MessageID: 1, AttachmentID: 1, Filename: "memo.m4a"}); err != nil {
		t.Fatal(err)
	}
	if err := deleteTarget(ctx, db, user.ID, configured.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := listUploads(ctx, db, user.ID, 0, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("queue rows after the target was removed = %d", len(rows))
	}
}

func TestUploadCountsTallyByStatus(t *testing.T) {
	st := openArchiveStore(t)
	db := st.DB()
	ctx := context.Background()
	user, _, mailbox := archiveFixture(t, st, "counts@example.test")
	configured := newTestTarget(t, db, user.ID, mailbox.ID)
	for i := int64(1); i <= 2; i++ {
		if _, err := enqueueUpload(ctx, db, upload{UserID: user.ID, TargetID: configured.ID,
			MessageID: i, AttachmentID: 1, Filename: "memo.m4a"}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := claimDueUploads(ctx, db, user.ID, time.Now().UTC(), 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("claim = %d, %v", len(rows), err)
	}
	if err := completeUpload(ctx, db, rows[0], statusDone, "2026/memo.m4a", "hash"); err != nil {
		t.Fatal(err)
	}
	counts, err := uploadCounts(ctx, db, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if counts[statusDone] != 1 || counts[statusQueued] != 1 {
		t.Fatalf("counts = %+v", counts)
	}
}

func TestRecordTargetResultKeepsTheLastOutcome(t *testing.T) {
	st := openArchiveStore(t)
	db := st.DB()
	ctx := context.Background()
	user, _, mailbox := archiveFixture(t, st, "outcome@example.test")
	configured := newTestTarget(t, db, user.ID, mailbox.ID)

	if err := recordTargetResult(ctx, db, user.ID, configured.ID, false, "the server refused"); err != nil {
		t.Fatal(err)
	}
	after, err := getTarget(ctx, db, user.ID, configured.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.LastError != "the server refused" {
		t.Fatalf("last error = %q", after.LastError)
	}
	// A success clears the standing failure, which is what stops a settings
	// page from showing an error the server has since recovered from.
	if err := recordTargetResult(ctx, db, user.ID, configured.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	after, err = getTarget(ctx, db, user.ID, configured.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.LastError != "" || after.UploadedTotal != 1 || after.LastSuccessAt.IsZero() {
		t.Fatalf("after a success = %+v", after)
	}
}

// A first attempt that uploaded the file and then failed to record it must not
// leave a second copy behind when it is retried. The path is written down
// before the upload for exactly this case.
func TestAReservedPathIsReusedByTheRetry(t *testing.T) {
	st := openArchiveStore(t)
	db := st.DB()
	ctx := context.Background()
	user, _, mailbox := archiveFixture(t, st, "reserve@example.test")
	configured := newTestTarget(t, db, user.ID, mailbox.ID)
	if _, err := enqueueUpload(ctx, db, upload{UserID: user.ID, TargetID: configured.ID,
		MessageID: 1, AttachmentID: 1, Filename: "memo.m4a", ContentType: "audio/mp4",
		MessageDate: time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	claimed, err := claimDueUploads(ctx, db, user.ID, time.Now().UTC(), 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %d, %v", len(claimed), err)
	}
	first := claimed[0]
	first.Size = 4
	if err := reserveUploadPath(ctx, db, first, "2026/05/memo.m4a", "hash-abc"); err != nil {
		t.Fatal(err)
	}
	// The upload is taken to have succeeded on the server while the row was
	// left failed, which is the state a crash between PUT and commit produces.
	if err := failUpload(ctx, db, first, errors.New("connection reset"), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	retried, err := claimDueUploads(ctx, db, user.ID, time.Now().UTC().Add(time.Hour), 1)
	if err != nil || len(retried) != 1 {
		t.Fatalf("retry claim = %d, %v", len(retried), err)
	}
	if retried[0].RemotePath != "2026/05/memo.m4a" || retried[0].ContentHash != "hash-abc" {
		t.Fatalf("retried row = %+v, want the reserved path and hash carried forward", retried[0])
	}

	// freeRemotePath must hand back that same path even though the server says
	// something is already there -- because what is there is this row's own
	// earlier attempt.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client, err := newWebDAVClient(server.URL+"/dav/", "", "")
	if err != nil {
		t.Fatal(err)
	}
	archiveWorker := &worker{ctx: ctx}
	got, err := archiveWorker.freeRemotePath(client, configured, retried[0], "hash-abc")
	if err != nil {
		t.Fatal(err)
	}
	if got != "2026/05/memo.m4a" {
		t.Fatalf("path = %q, want the reserved one rather than a suffixed second copy", got)
	}

	// A row with no reservation, whose rendered path is taken by something
	// else, does take a suffix.
	fresh := upload{MessageID: 2, AttachmentID: 1, Filename: "memo.m4a",
		MessageDate: time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC)}
	got, err = archiveWorker.freeRemotePath(client, configured, fresh, "0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if got != "2026/05/memo-01234567.m4a" {
		t.Fatalf("path = %q, want a suffix from the content hash", got)
	}
}

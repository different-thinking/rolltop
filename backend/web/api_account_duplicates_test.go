// File overview: Tests for the duplicate-copy report and cleanup routes: what
// the report counts, that the cleanup targets the aggregating account's Trash,
// and that neither route can see another tenant's mail.

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"rolltop/backend/blob"
	"rolltop/backend/store"
	"rolltop/backend/syncer"
)

// acceptingMoveFetcher stands in for IMAP while the cleanup's background move
// runs. Only the move itself is exercised here; the rest of the interface is
// left unimplemented because this test never reaches it.
type acceptingMoveFetcher struct {
	syncer.Fetcher
}

func (f *acceptingMoveFetcher) MoveMessage(_ context.Context, _ store.MailAccount, _ string, _ string, _ uint32) error {
	return nil
}

type duplicateWebFixture struct {
	server         *Server
	db             *store.Store
	ctx            context.Context
	owner          store.User
	aggregateID    int64
	aggregateTrash store.Mailbox
	original       store.MessageRecord
	copy           store.MessageRecord
}

func newDuplicateWebFixture(t *testing.T) duplicateWebFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	blobStore := blob.New(dir)

	owner, err := db.CreateUser(ctx, "dupe-owner@example.test", "Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	originalAccount, err := db.CreateMailAccount(ctx, store.MailAccount{
		UserID: owner.ID, Email: "info@firma.test", Host: "imap.firma.test", Port: 993,
		Username: "info@firma.test", EncryptedPassword: "secret", UseTLS: true, Mailbox: "*",
	})
	if err != nil {
		t.Fatal(err)
	}
	aggregateAccount, err := db.CreateMailAccount(ctx, store.MailAccount{
		UserID: owner.ID, Email: "owner@gmail.test", Host: "imap.gmail.test", Port: 993,
		Username: "owner@gmail.test", EncryptedPassword: "secret", UseTLS: true, Mailbox: "*",
	})
	if err != nil {
		t.Fatal(err)
	}
	originalInbox, err := db.GetOrCreateMailbox(ctx, owner.ID, originalAccount.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	aggregateInbox, err := db.GetOrCreateMailbox(ctx, owner.ID, aggregateAccount.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	aggregateTrash, err := db.GetOrCreateMailbox(ctx, owner.ID, aggregateAccount.ID, "Trash")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateMailboxSettings(ctx, owner.ID, aggregateTrash.ID, store.MailboxSettings{
		SyncMode: "auto", Role: "trash", ShowInSidebar: true, IncludeInSearch: true,
	}); err != nil {
		t.Fatal(err)
	}
	original := storeDuplicateMessage(t, ctx, db, owner.ID, originalAccount.ID, originalInbox.ID, 1)
	copied := storeDuplicateMessage(t, ctx, db, owner.ID, aggregateAccount.ID, aggregateInbox.ID, 2)
	if _, err := db.RefreshDuplicateCopiesForUser(ctx, owner.ID, ""); err != nil {
		t.Fatal(err)
	}

	server := &Server{
		store: db, blobs: blobStore,
		syncer:    &syncer.Service{Store: db, Blobs: blobStore, Fetcher: &acceptingMoveFetcher{}},
		masterKey: bytes.Repeat([]byte{7}, 32), events: newEventHub(),
	}
	return duplicateWebFixture{
		server: server, db: db, ctx: ctx, owner: owner,
		aggregateID: aggregateAccount.ID, aggregateTrash: aggregateTrash,
		original: original, copy: copied,
	}
}

func storeDuplicateMessage(t *testing.T, ctx context.Context, db *store.Store, userID, accountID, mailboxID int64, uid uint32) store.MessageRecord {
	t.Helper()
	blobRecord, err := db.CreateBlob(ctx, store.BlobRecord{
		UserID: userID, Kind: "message",
		Path:   filepath.Join("blobs", "duplicate", string(rune('a'+int(uid)))),
		SHA256: "duplicate-sha-" + string(rune('a'+int(uid))), Size: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	message, err := db.CreateMessage(ctx, store.CreateMessage{
		UserID: userID, AccountID: accountID, MailboxID: mailboxID, BlobID: blobRecord.ID,
		MessageIDHeader: "<invoice@partner.test>", ThreadKey: "msgid:<invoice@partner.test>",
		Subject: "Invoice", FromAddr: "sender@partner.test", ToAddr: "info@firma.test",
		Date: now, InternalDate: now, UID: uid, Size: 10, BlobPath: blobRecord.Path,
	})
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func duplicateRequest(t *testing.T, server *Server, user store.User, method, target string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, target, nil)
	request = request.WithContext(context.WithValue(request.Context(), userContextKey, currentUser{User: user}))
	const csrfBase = "duplicate-action-csrf"
	request.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrfBase})
	request.Header.Set("X-CSRF-Token", server.csrfForBase(csrfBase))
	return request
}

// The report names the account holding the copy, not the one that was addressed:
// the aggregating account is the one the user has to act on.
func TestAPIAccountDuplicatesReportsTheAggregatingAccount(t *testing.T) {
	f := newDuplicateWebFixture(t)
	response := httptest.NewRecorder()
	f.server.handleAPI(response, duplicateRequest(t, f.server, f.owner, http.MethodGet, "/api/account/duplicates"))
	if response.Code != http.StatusOK {
		t.Fatalf("report status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var payload struct {
		Hidden   int `json:"hidden"`
		Accounts []struct {
			AccountID int64  `json:"account_id"`
			Email     string `json:"email"`
			Hidden    int    `json:"hidden"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Hidden != 1 || len(payload.Accounts) != 1 {
		t.Fatalf("report = %+v, want one hidden copy in one account", payload)
	}
	if payload.Accounts[0].AccountID != f.aggregateID {
		t.Fatalf("report names account %d, want the aggregating account %d",
			payload.Accounts[0].AccountID, f.aggregateID)
	}
}

// The cleanup moves the copy into the Trash of the account holding it. The
// addressed account's message keeps its folder, which is the whole point.
func TestAPIAccountDuplicatesTrashMovesOnlyTheHiddenCopy(t *testing.T) {
	f := newDuplicateWebFixture(t)
	response := httptest.NewRecorder()
	f.server.handleAPI(response, duplicateRequest(t, f.server, f.owner, http.MethodPost, "/api/account/duplicates/trash"))
	if response.Code != http.StatusOK {
		t.Fatalf("cleanup status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var payload struct {
		Queued  bool `json:"queued"`
		Matched int  `json:"matched"`
		Runs    []struct {
			AccountID int64  `json:"account_id"`
			Mailbox   string `json:"mailbox"`
			Messages  int    `json:"messages"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Queued || payload.Matched != 1 || len(payload.Runs) != 1 {
		t.Fatalf("cleanup response = %+v, want one queued move for one message", payload)
	}
	if payload.Runs[0].AccountID != f.aggregateID || payload.Runs[0].Mailbox != f.aggregateTrash.Name {
		t.Fatalf("cleanup targets account %d folder %q, want %d %q",
			payload.Runs[0].AccountID, payload.Runs[0].Mailbox, f.aggregateID, f.aggregateTrash.Name)
	}
	addressed, err := f.db.GetMessageForUser(f.ctx, f.owner.ID, f.original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if addressed.MailboxID != f.original.MailboxID {
		t.Fatalf("addressed copy moved to folder %d, want it left in %d",
			addressed.MailboxID, f.original.MailboxID)
	}
}

// Every duplicate route is tenant scoped through the session user alone. A
// second tenant's identical mail must not appear in either answer.
func TestAPIAccountDuplicatesNeverReportsAnotherTenant(t *testing.T) {
	f := newDuplicateWebFixture(t)
	other, err := f.db.CreateUser(f.ctx, "dupe-other@example.test", "Other", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	f.server.handleAPI(response, duplicateRequest(t, f.server, other, http.MethodGet, "/api/account/duplicates"))
	if response.Code != http.StatusOK {
		t.Fatalf("report status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var payload struct {
		Hidden   int   `json:"hidden"`
		Accounts []any `json:"accounts"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Hidden != 0 || len(payload.Accounts) != 0 {
		t.Fatalf("other tenant report = %+v, want an empty report", payload)
	}
	cleanup := httptest.NewRecorder()
	f.server.handleAPI(cleanup, duplicateRequest(t, f.server, other, http.MethodPost, "/api/account/duplicates/trash"))
	if cleanup.Code != http.StatusOK {
		t.Fatalf("other tenant cleanup status = %d, want 200: %s", cleanup.Code, cleanup.Body.String())
	}
	hiddenCopy, err := f.db.GetMessageForUser(f.ctx, f.owner.ID, f.copy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if hiddenCopy.MailboxID != f.copy.MailboxID {
		t.Fatalf("owner's copy moved to folder %d after another tenant's cleanup, want %d",
			hiddenCopy.MailboxID, f.copy.MailboxID)
	}
}

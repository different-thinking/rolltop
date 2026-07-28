package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"rolltop/backend/store"
	"rolltop/backend/syncer"
)

type folderViewSyncFetcher struct {
	statusCalls chan string
}

func (f *folderViewSyncFetcher) ListMailboxes(context.Context, store.MailAccount) ([]syncer.MailboxInfo, error) {
	return nil, errors.New("unexpected mailbox listing")
}

func (f *folderViewSyncFetcher) MailboxStatus(_ context.Context, _ store.MailAccount, mailbox string) (syncer.MailboxStatus, error) {
	f.statusCalls <- mailbox
	return syncer.MailboxStatus{UIDNext: 1, UIDValidity: 1}, nil
}

func (f *folderViewSyncFetcher) UIDs(context.Context, store.MailAccount, string) ([]uint32, error) {
	return nil, nil
}

func (f *folderViewSyncFetcher) FetchMailbox(context.Context, store.MailAccount, string, uint32, func(syncer.FetchedMessage) error) error {
	return nil
}

func (f *folderViewSyncFetcher) FetchMailboxWithUIDValidity(context.Context, store.MailAccount, string, uint32, uint32, func(syncer.FetchedMessage) error) error {
	return nil
}

func (f *folderViewSyncFetcher) FetchUIDsWithUIDValidity(context.Context, store.MailAccount, string, []uint32, uint32, func(syncer.FetchedMessage) error) error {
	return nil
}

func (f *folderViewSyncFetcher) FetchMessage(context.Context, store.MailAccount, string, uint32) (syncer.FetchedMessage, error) {
	return syncer.FetchedMessage{}, errors.New("unexpected message fetch")
}

func (f *folderViewSyncFetcher) AppendMessage(context.Context, store.MailAccount, string, []byte, string, time.Time) (syncer.FetchedMessage, error) {
	return syncer.FetchedMessage{}, errors.New("unexpected append")
}

func (f *folderViewSyncFetcher) SetSeen(context.Context, store.MailAccount, string, uint32, bool) error {
	return nil
}

func (f *folderViewSyncFetcher) SeenUIDs(context.Context, store.MailAccount, string) ([]uint32, error) {
	return nil, nil
}

func (f *folderViewSyncFetcher) SetFlagged(context.Context, store.MailAccount, string, uint32, bool) error {
	return nil
}

func (f *folderViewSyncFetcher) FlaggedUIDs(context.Context, store.MailAccount, string) ([]uint32, error) {
	return nil, nil
}

func (f *folderViewSyncFetcher) MoveMessage(context.Context, store.MailAccount, string, string, uint32) error {
	return errors.New("unexpected move")
}

func TestAPIAccountFolderSyncQueuesManualAndRejectsNeverAcrossTenants(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owner, err := db.CreateUser(ctx, "folder-view-owner@example.test", "Folder View Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateUser(ctx, "folder-view-other@example.test", "Folder View Other", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	ownerAccount, err := db.CreateMailAccount(ctx, store.MailAccount{
		UserID: owner.ID, Email: owner.Email, Host: "imap.example.test", Port: 993,
		Username: owner.Email, EncryptedPassword: "encrypted", UseTLS: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	otherAccount, err := db.CreateMailAccount(ctx, store.MailAccount{
		UserID: other.ID, Email: other.Email, Host: "imap.example.test", Port: 993,
		Username: other.Email, EncryptedPassword: "encrypted", UseTLS: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	manual, err := db.GetOrCreateMailbox(ctx, owner.ID, ownerAccount.ID, "INBOX.TaxStuff")
	if err != nil {
		t.Fatal(err)
	}
	never, err := db.GetOrCreateMailbox(ctx, owner.ID, ownerAccount.ID, "Offline")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := db.GetOrCreateMailbox(ctx, other.ID, otherAccount.ID, "Private")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateMailboxSyncMode(ctx, owner.ID, manual.ID, "manual"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateMailboxSyncMode(ctx, owner.ID, never.ID, "never"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateMailboxSyncMode(ctx, other.ID, foreign.ID, "manual"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateMailboxRemoteStatus(ctx, owner.ID, manual.ID, 0, 0, 1, 1); err != nil {
		t.Fatal(err)
	}

	runnerCtx, cancelRunner := context.WithCancel(context.Background())
	defer cancelRunner()
	fetcher := &folderViewSyncFetcher{statusCalls: make(chan string, 1)}
	syncService := &syncer.Service{Store: db, Fetcher: fetcher, PluginDir: t.TempDir()}
	runner := syncer.NewRunnerWithContext(runnerCtx, syncService)
	server := &Server{
		store: db, syncer: syncService, syncRunner: runner,
		masterKey: bytes.Repeat([]byte{9}, 32), events: newEventHub(),
	}

	foreignResponse := httptest.NewRecorder()
	server.handleAPI(foreignResponse, authenticatedFolderActionRequest(t, server, owner,
		"/api/account/folders/"+formatInt64(foreign.ID)+"/sync"))
	if foreignResponse.Code != http.StatusNotFound {
		t.Fatalf("foreign folder sync status=%d body=%s", foreignResponse.Code, foreignResponse.Body.String())
	}

	neverResponse := httptest.NewRecorder()
	server.handleAPI(neverResponse, authenticatedFolderActionRequest(t, server, owner,
		"/api/account/folders/"+formatInt64(never.ID)+"/sync"))
	if neverResponse.Code != http.StatusBadRequest {
		t.Fatalf("never folder sync status=%d body=%s", neverResponse.Code, neverResponse.Body.String())
	}
	select {
	case mailbox := <-fetcher.statusCalls:
		t.Fatalf("rejected folder unexpectedly reached IMAP status for %q", mailbox)
	default:
	}

	manualResponse := httptest.NewRecorder()
	server.handleAPI(manualResponse, authenticatedFolderActionRequest(t, server, owner,
		"/api/account/folders/"+formatInt64(manual.ID)+"/sync"))
	if manualResponse.Code != http.StatusOK {
		t.Fatalf("manual folder sync status=%d body=%s", manualResponse.Code, manualResponse.Body.String())
	}
	var queued struct {
		OK     bool `json:"ok"`
		Queued bool `json:"queued"`
	}
	if err := json.NewDecoder(manualResponse.Body).Decode(&queued); err != nil {
		t.Fatal(err)
	}
	if !queued.OK || !queued.Queued {
		t.Fatalf("manual folder sync response=%+v", queued)
	}
	select {
	case mailbox := <-fetcher.statusCalls:
		if mailbox != manual.Name {
			t.Fatalf("queued mailbox=%q, want %q", mailbox, manual.Name)
		}
	case <-time.After(time.Second):
		t.Fatal("manual folder sync did not reach IMAP status")
	}
	waitForAccountMailboxIdle(t, runner, owner.ID, manual)
}

func formatInt64(value int64) string {
	return strconv.FormatInt(value, 10)
}

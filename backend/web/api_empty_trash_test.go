package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"rolltop/backend/store"
	"rolltop/backend/syncer"
)

// plainFetcher is an IMAP connection with no expunge capability, which is what
// a deployment running an older or reduced fetcher looks like.
type plainFetcher struct {
	syncer.Fetcher
}

func emptyTrashRequest(t *testing.T, server *Server, user store.User, mailboxID int64) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"mailbox_id": mailboxID})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/messages/empty-trash", bytes.NewReader(payload))
	req = req.WithContext(context.WithValue(req.Context(), userContextKey, currentUser{User: user}))
	csrfBase := "empty-trash-csrf"
	req.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrfBase})
	req.Header.Set("X-CSRF-Token", server.csrfForBase(csrfBase))
	res := httptest.NewRecorder()
	server.apiEmptyTrash(res, req)
	return res
}

func TestEmptyTrashRejectsFoldersThatAreNotTrash(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tenant := newScopeTestTenant(t, ctx, db, "empty-trash-role@example.test")
	server := &Server{store: db, masterKey: []byte("12345678901234567890123456789012"), syncer: &syncer.Service{}}

	res := emptyTrashRequest(t, server, tenant.user, tenant.inbox.ID)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a folder that is not Trash", res.Code)
	}
}

func TestEmptyTrashCannotReachAnotherTenantsTrash(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owner := newScopeTestTenant(t, ctx, db, "empty-trash-owner@example.test")
	other := newScopeTestTenant(t, ctx, db, "empty-trash-other@example.test")
	server := &Server{store: db, masterKey: []byte("12345678901234567890123456789012"), syncer: &syncer.Service{}}

	res := emptyTrashRequest(t, server, owner.user, other.trash.ID)
	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for another tenant's Trash folder", res.Code)
	}
	// The other tenant's mail is untouched, and its own request still resolves.
	if _, err := db.GetMailboxForUser(ctx, other.user.ID, other.trash.ID); err != nil {
		t.Fatalf("other tenant's trash folder: %v", err)
	}
}

func TestEmptyTrashReportsAnIMAPClientThatCannotDelete(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tenant := newScopeTestTenant(t, ctx, db, "empty-trash-capability@example.test")
	server := &Server{
		store:     db,
		masterKey: []byte("12345678901234567890123456789012"),
		syncer:    &syncer.Service{Store: db, Fetcher: &plainFetcher{}},
	}

	res := emptyTrashRequest(t, server, tenant.user, tenant.trash.ID)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 when the IMAP client cannot delete", res.Code)
	}
}

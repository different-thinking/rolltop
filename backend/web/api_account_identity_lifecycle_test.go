package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"rolltop/backend/store"
)

// TestEditingAMailboxDoesNotRecreateARemovedIdentity pins where identities come
// from. Adding a mailbox is one of the two deliberate acts that produce one;
// editing that mailbox afterwards -- a new password, a different sync interval
// -- is not, and must not hand back an identity the user removed.
func TestEditingAMailboxDoesNotRecreateARemovedIdentity(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "lifecycle@example.test", "Lifecycle", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{store: db, masterKey: bytes.Repeat([]byte{7}, 32), events: newEventHub()}

	created := saveIMAPAccountForTest(t, server, user, map[string]any{
		"email": "lifecycle-box@example.test", "label": "Lifecycle", "host": "imap.lifecycle.test",
		"port": 993, "username": "lifecycle-box@example.test", "password": "secret", "use_tls": true,
		"smtp_host": "smtp.lifecycle.test", "smtp_port": 587, "smtp_same_as_imap": true, "mailbox": "INBOX",
	})
	identities, err := db.ListMailIdentitiesForUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) != 1 || identities[0].Email != "lifecycle-box@example.test" {
		t.Fatalf("identities after adding a mailbox = %+v, want one for the mailbox", identities)
	}

	if err := db.DeleteMailIdentityForUser(ctx, user.ID, identities[0].ID); err != nil {
		t.Fatal(err)
	}

	saveIMAPAccountForTest(t, server, user, map[string]any{
		"id": created, "email": "lifecycle-box@example.test", "label": "Lifecycle renamed",
		"host": "imap.lifecycle.test", "port": 993, "username": "lifecycle-box@example.test",
		"password": "secret", "use_tls": true, "smtp_host": "smtp.lifecycle.test", "smtp_port": 587,
		"smtp_same_as_imap": true, "mailbox": "INBOX", "sync_interval_minutes": 30,
	})
	identities, err = db.ListMailIdentitiesForUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) != 0 {
		t.Fatalf("identities after editing the mailbox = %+v, want the removed one to stay removed", identities)
	}
}

// saveIMAPAccountForTest posts one IMAP account form and returns the stored id.
func saveIMAPAccountForTest(t *testing.T, server *Server, user store.User, form map[string]any) int64 {
	t.Helper()
	body, err := json.Marshal(form)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/account/imap", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(context.WithValue(request.Context(), userContextKey, currentUser{User: user}))
	const csrfBase = "imap-identity-lifecycle-csrf"
	request.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrfBase})
	request.Header.Set("X-CSRF-Token", server.csrfForBase(csrfBase))
	recorder := httptest.NewRecorder()

	server.handleAPI(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("save IMAP account status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Account struct {
			ID int64 `json:"id"`
		} `json:"account"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(fmt.Errorf("decode IMAP account response: %w", err))
	}
	return response.Account.ID
}

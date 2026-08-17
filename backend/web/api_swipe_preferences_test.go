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
)

func TestSwipePreferencesAPIIsUserScopedAndValidatesArchiveFolders(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owner, err := db.CreateUser(ctx, "swipes@example.test", "Swipes", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateUser(ctx, "other-swipes@example.test", "Other", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	ownerAccount, err := db.UpsertMailAccount(ctx, store.MailAccount{UserID: owner.ID, Email: owner.Email, Host: "imap.example.test", Port: 993, Username: owner.Email, EncryptedPassword: "encrypted", Mailbox: "INBOX"})
	if err != nil {
		t.Fatal(err)
	}
	ownerArchive, err := db.GetOrCreateMailbox(ctx, owner.ID, ownerAccount.ID, "Archive")
	if err != nil {
		t.Fatal(err)
	}
	otherAccount, err := db.UpsertMailAccount(ctx, store.MailAccount{UserID: other.ID, Email: other.Email, Host: "imap.example.test", Port: 993, Username: other.Email, EncryptedPassword: "encrypted", Mailbox: "INBOX"})
	if err != nil {
		t.Fatal(err)
	}
	otherArchive, err := db.GetOrCreateMailbox(ctx, other.ID, otherAccount.ID, "Secret Archive")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{store: db, masterKey: bytes.Repeat([]byte{9}, 32), events: newEventHub(), mailListCache: newMailListCache()}
	unauthenticatedResponse := httptest.NewRecorder()
	server.apiSwipePreferences(unauthenticatedResponse, httptest.NewRequest(http.MethodGet, "/api/profile/swipes", nil))
	if unauthenticatedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d body=%s", unauthenticatedResponse.Code, unauthenticatedResponse.Body.String())
	}

	get := authenticatedSwipePreferencesRequest(t, server, owner, http.MethodGet, nil)
	getResponse := httptest.NewRecorder()
	server.apiSwipePreferences(getResponse, get)
	if getResponse.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", getResponse.Code, getResponse.Body.String())
	}
	var defaults struct {
		Preferences apiSwipePreferences `json:"swipe_preferences"`
	}
	if err := json.NewDecoder(getResponse.Body).Decode(&defaults); err != nil {
		t.Fatal(err)
	}
	if defaults.Preferences.LeftAction != "snooze" || defaults.Preferences.RightAction != "mark_read" {
		t.Fatalf("default preferences = %+v", defaults.Preferences)
	}

	valid := apiSwipePreferences{
		LeftAction: "archive", LeftSnoozePreset: "later_today",
		RightAction: "mark_unread", RightSnoozePreset: "next_week",
		ArchiveMailboxes: []apiAccountMailboxChoice{{AccountID: ownerAccount.ID, MailboxID: ownerArchive.ID}},
	}
	validBody, _ := json.Marshal(valid)
	missingCSRF := httptest.NewRequest(http.MethodPost, "/api/profile/swipes", bytes.NewReader(validBody))
	missingCSRF = missingCSRF.WithContext(context.WithValue(missingCSRF.Context(), userContextKey, currentUser{User: owner}))
	missingCSRFResponse := httptest.NewRecorder()
	server.apiSwipePreferences(missingCSRFResponse, missingCSRF)
	if missingCSRFResponse.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d body=%s", missingCSRFResponse.Code, missingCSRFResponse.Body.String())
	}
	post := authenticatedSwipePreferencesRequest(t, server, owner, http.MethodPost, validBody)
	postResponse := httptest.NewRecorder()
	server.apiSwipePreferences(postResponse, post)
	if postResponse.Code != http.StatusOK {
		t.Fatalf("POST status=%d body=%s", postResponse.Code, postResponse.Body.String())
	}

	foreign := valid
	foreign.ArchiveMailboxes = []apiAccountMailboxChoice{{AccountID: otherAccount.ID, MailboxID: otherArchive.ID}}
	foreignBody, _ := json.Marshal(foreign)
	foreignResponse := httptest.NewRecorder()
	server.apiSwipePreferences(foreignResponse, authenticatedSwipePreferencesRequest(t, server, owner, http.MethodPost, foreignBody))
	if foreignResponse.Code != http.StatusBadRequest {
		t.Fatalf("foreign archive status=%d body=%s", foreignResponse.Code, foreignResponse.Body.String())
	}

	invalid := valid
	invalid.LeftAction = "delete_everything"
	invalidBody, _ := json.Marshal(invalid)
	invalidResponse := httptest.NewRecorder()
	server.apiSwipePreferences(invalidResponse, authenticatedSwipePreferencesRequest(t, server, owner, http.MethodPost, invalidBody))
	if invalidResponse.Code != http.StatusBadRequest {
		t.Fatalf("invalid action status=%d body=%s", invalidResponse.Code, invalidResponse.Body.String())
	}

	saved, err := db.GetSwipePreferences(ctx, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.LeftAction != "archive" || saved.RightAction != "mark_unread" || len(saved.ArchiveMailboxes) != 1 || saved.ArchiveMailboxes[0].MailboxID != ownerArchive.ID {
		t.Fatalf("saved preferences changed after rejected requests: %+v", saved)
	}

	bootstrapRequest := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	bootstrapRequest = bootstrapRequest.WithContext(context.WithValue(bootstrapRequest.Context(), userContextKey, currentUser{User: owner}))
	bootstrapResponse := httptest.NewRecorder()
	server.apiBootstrap(bootstrapResponse, bootstrapRequest)
	if bootstrapResponse.Code != http.StatusOK {
		t.Fatalf("bootstrap status=%d body=%s", bootstrapResponse.Code, bootstrapResponse.Body.String())
	}
	var bootstrap struct {
		Preferences apiSwipePreferences `json:"swipe_preferences"`
	}
	if err := json.NewDecoder(bootstrapResponse.Body).Decode(&bootstrap); err != nil {
		t.Fatal(err)
	}
	if bootstrap.Preferences.LeftAction != "archive" || len(bootstrap.Preferences.ArchiveMailboxes) != 1 || bootstrap.Preferences.ArchiveMailboxes[0].MailboxID != ownerArchive.ID {
		t.Fatalf("bootstrap swipe preferences = %+v", bootstrap.Preferences)
	}
}

func authenticatedSwipePreferencesRequest(t *testing.T, server *Server, user store.User, method string, body []byte) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, "/api/profile/swipes", bytes.NewReader(body))
	request = request.WithContext(context.WithValue(request.Context(), userContextKey, currentUser{User: user}))
	if method != http.MethodGet {
		const csrfBase = "swipe-preferences-csrf"
		request.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrfBase})
		request.Header.Set("X-CSRF-Token", server.csrfForBase(csrfBase))
		request.Header.Set("Content-Type", "application/json")
	}
	return request
}

// The Archive mapping decides what the Inbox list leaves out, so saving it
// has to retire that list's cached pages. Without this the browser keeps
// revalidating to 304 and shows the pre-change rows until unrelated mail
// activity happens to bump the generation.
func TestSavingSwipePreferencesRetiresCachedMailLists(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owner, err := db.CreateUser(ctx, "cache-swipes@example.test", "Cache", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.UpsertMailAccount(ctx, store.MailAccount{
		UserID: owner.ID, Email: owner.Email, Host: "imap.example.test", Port: 993,
		Username: owner.Email, EncryptedPassword: "encrypted", Mailbox: "INBOX",
	})
	if err != nil {
		t.Fatal(err)
	}
	archive, err := db.GetOrCreateMailbox(ctx, owner.ID, account.ID, "Archive")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{store: db, masterKey: bytes.Repeat([]byte{9}, 32), events: newEventHub(), mailListCache: newMailListCache()}
	before := server.mailListGeneration(owner.ID)

	body, _ := json.Marshal(apiSwipePreferences{
		LeftAction: "snooze", LeftSnoozePreset: "tomorrow",
		RightAction: "mark_read", RightSnoozePreset: "tomorrow",
		ArchiveMailboxes: []apiAccountMailboxChoice{{AccountID: account.ID, MailboxID: archive.ID}},
	})
	response := httptest.NewRecorder()
	server.apiSwipePreferences(response, authenticatedSwipePreferencesRequest(t, server, owner, http.MethodPost, body))
	if response.Code != http.StatusOK {
		t.Fatalf("POST status=%d body=%s", response.Code, response.Body.String())
	}
	if server.mailListGeneration(owner.ID) == before {
		t.Fatal("saving the archive mapping left the cached mail lists in place")
	}
}

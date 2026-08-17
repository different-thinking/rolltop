package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"rolltop/backend/store"
)

// TestAPIMailSortsByDateInBothDirections covers the list control that flips a
// folder between newest and oldest first, including the cached ETag that must
// not be shared between the two orders.
func TestAPIMailSortsByDateInBothDirections(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "sort@example.test", "Sort", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.UpsertMailAccount(ctx, store.MailAccount{
		UserID: user.ID, Email: user.Email, Host: "imap.example.test", Port: 993,
		Username: user.Email, EncryptedPassword: "encrypted", UseTLS: true, Mailbox: "INBOX",
	})
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := db.GetOrCreateMailbox(ctx, user.ID, account.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Unix(1700000000, 0).UTC()
	subjects := []string{"Oldest", "Middle", "Newest"}
	for index, subject := range subjects {
		blob, blobErr := db.CreateBlob(ctx, store.BlobRecord{
			UserID: user.ID, Kind: "message", Path: fmt.Sprintf("users/%d/sort-%d.eml", user.ID, index),
			SHA256: fmt.Sprintf("sort-%d-%d", user.ID, index), Size: 1,
		})
		if blobErr != nil {
			t.Fatal(blobErr)
		}
		if _, err := db.CreateMessage(ctx, store.CreateMessage{
			UserID: user.ID, AccountID: account.ID, MailboxID: mailbox.ID, BlobID: blob.ID,
			MessageIDHeader: fmt.Sprintf("<sort-%d@example.test>", index),
			FromAddr:        "sender@example.test", Subject: subject,
			Date:         base.Add(time.Duration(index) * time.Hour),
			InternalDate: base.Add(time.Duration(index) * time.Hour),
			UID:          uint32(index + 1), BlobPath: blob.Path,
		}); err != nil {
			t.Fatal(err)
		}
	}
	server := &Server{store: db, mailListCache: newMailListCache()}

	newest, newestETag := mailSortPage(t, server, user, "/api/mail?page=1")
	if got := mailSubjects(newest); !slices.Equal(got, []string{"Newest", "Middle", "Oldest"}) {
		t.Fatalf("all mail newest first = %v", got)
	}
	if newest.Sort != string(store.ThreadListNewestFirst) {
		t.Fatalf("all mail sort = %q", newest.Sort)
	}

	oldest, oldestETag := mailSortPage(t, server, user, "/api/mail?page=1&sort=oldest")
	if got := mailSubjects(oldest); !slices.Equal(got, []string{"Oldest", "Middle", "Newest"}) {
		t.Fatalf("all mail oldest first = %v", got)
	}
	if oldest.Sort != string(store.ThreadListOldestFirst) {
		t.Fatalf("all mail reversed sort = %q", oldest.Sort)
	}

	folder, _ := mailSortPage(t, server, user, fmt.Sprintf("/api/mail?page=1&mailbox=%d&sort=oldest", mailbox.ID))
	if got := mailSubjects(folder); !slices.Equal(got, []string{"Oldest", "Middle", "Newest"}) {
		t.Fatalf("folder oldest first = %v", got)
	}

	// A stale ETag from the other direction must not short-circuit the request,
	// or a reader would keep seeing the order they just turned off.
	req := httptest.NewRequest(http.MethodGet, "/api/mail?page=1&sort=oldest", nil)
	req.Header.Set("If-None-Match", newestETag)
	req = req.WithContext(context.WithValue(req.Context(), userContextKey, currentUser{User: user}))
	rec := httptest.NewRecorder()
	server.apiMail(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cross-order revalidation status = %d", rec.Code)
	}
	if got := rec.Header().Get("ETag"); got != oldestETag {
		t.Fatalf("cross-order etag = %q, want %q", got, oldestETag)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/mail?page=1&sort=oldest", nil)
	req.Header.Set("If-None-Match", oldestETag)
	req = req.WithContext(context.WithValue(req.Context(), userContextKey, currentUser{User: user}))
	rec = httptest.NewRecorder()
	server.apiMail(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("same-order revalidation status = %d", rec.Code)
	}
}

type mailSortResponse struct {
	Conversations []apiConversation `json:"conversations"`
	Sort          string            `json:"sort"`
}

func mailSortPage(t *testing.T, server *Server, user store.User, target string) (mailSortResponse, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req = req.WithContext(context.WithValue(req.Context(), userContextKey, currentUser{User: user}))
	rec := httptest.NewRecorder()
	server.apiMail(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d body = %s", target, rec.Code, rec.Body.String())
	}
	var payload mailSortResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	return payload, rec.Header().Get("ETag")
}

func mailSubjects(page mailSortResponse) []string {
	subjects := make([]string, 0, len(page.Conversations))
	for _, conversation := range page.Conversations {
		subjects = append(subjects, conversation.Message.Subject)
	}
	return subjects
}

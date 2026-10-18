package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"rolltop/backend/blob"
	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
)

// A peek is the offline store warming a copy of a message the reader may never
// open. It has to answer with the same rendered thread an open does and leave
// the read state exactly as it found it; the plain open that follows still
// marks the thread read.
func TestMessagePeekRendersThreadWithoutMarkingRead(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "peek@example.test", "Peek", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.UpsertMailAccount(ctx, store.MailAccount{
		UserID: user.ID, Email: user.Email, Host: "imap.example.test", Port: 993,
		Username: user.Email, EncryptedPassword: "encrypted", Mailbox: "INBOX",
	})
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := db.GetOrCreateMailbox(ctx, user.ID, account.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	raw := strings.Join([]string{
		"From: Alice <alice@example.test>",
		"To: " + user.Email,
		"Subject: Read me later",
		"Content-Type: text/html; charset=utf-8",
		"",
		"<html><body><p><strong>Saved</strong> for the train.</p></body></html>",
	}, "\r\n")
	blobStore := blob.New(dir)
	saved, err := blobStore.SaveRawMessage(user.ID, account.ID, mailbox.Name, 9, []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	blobRecord, err := db.CreateBlob(ctx, store.BlobRecord{
		UserID: user.ID, Kind: "message", Path: saved.Path, SHA256: saved.SHA256, Size: saved.Size,
	})
	if err != nil {
		t.Fatal(err)
	}
	message, err := db.CreateMessage(ctx, store.CreateMessage{
		UserID: user.ID, AccountID: account.ID, MailboxID: mailbox.ID, BlobID: blobRecord.ID,
		Subject: "Read me later", FromAddr: "Alice <alice@example.test>", ToAddr: user.Email,
		Date: time.Now().UTC(), InternalDate: time.Now().UTC(), UID: 9, Size: saved.Size,
		BlobPath: saved.Path, BodyText: "Saved for the train.", IsRead: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Mail that has come back from a snooze heads the newest page, which is
	// the page the warm asks for. An open acknowledges the return and deletes
	// the snooze; a peek has to leave it for the open to find.
	if _, err := db.SnoozeMessage(ctx, user.ID, message.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `UPDATE message_snoozes SET snoozed_until = ? WHERE user_id = ? AND message_id = ?`,
		time.Now().Add(-time.Hour).Unix(), user.ID, message.ID); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: db, blobs: blobStore, masterKey: []byte("12345678901234567890123456789012")}
	// The open below marks mail read, which notifies the tenant and schedules a
	// background web push worker that must not outlive the test database.
	t.Cleanup(func() { waitForWebPushWorker(t, server, user.ID) })

	open := func(query string) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/messages/"+strconvInt64(message.ID)+query, nil)
		req = req.WithContext(context.WithValue(req.Context(), userContextKey, currentUser{User: user}))
		res := httptest.NewRecorder()
		server.apiMessage(res, req, message.ID)
		if res.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d body=%s", query, res.Code, res.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode %s: %v", query, err)
		}
		return payload
	}

	peeked := open("?peek=1")
	thread, _ := peeked["thread"].([]any)
	if len(thread) != 1 {
		t.Fatalf("peek thread length = %d, want 1 (payload %v)", len(thread), peeked)
	}
	first, _ := thread[0].(map[string]any)
	if bodyDoc, _ := first["body_doc"].(string); !strings.Contains(bodyDoc, "<strong>Saved</strong>") {
		t.Fatalf("peek did not render the body: %q", bodyDoc)
	}
	afterPeek, err := db.GetMessageForUser(ctx, user.ID, message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterPeek.IsRead || afterPeek.ReadSyncPending {
		t.Fatalf("peek changed read state: is_read=%t pending=%t", afterPeek.IsRead, afterPeek.ReadSyncPending)
	}
	if _, err := db.MessageSnoozeForUser(ctx, user.ID, message.ID); err != nil {
		t.Fatalf("peek acknowledged the due snooze: %v", err)
	}

	open("")
	afterOpen, err := db.GetMessageForUser(ctx, user.ID, message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !afterOpen.IsRead || !afterOpen.ReadSyncPending {
		t.Fatalf("open left read state: is_read=%t pending=%t", afterOpen.IsRead, afterOpen.ReadSyncPending)
	}
	if _, err := db.MessageSnoozeForUser(ctx, user.ID, message.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("open left the due snooze in place: %v", err)
	}
}

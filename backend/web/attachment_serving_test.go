// File overview: Serving inline attachments out of a message whose stored
// attachment blob is gone.

package web

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"rolltop/backend/blob"
	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
)

func TestInlineAttachmentFallsBackToRawMessageWhenBlobIsGone(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	blobs := blob.New(t.TempDir())

	user, err := db.CreateUser(ctx, "inline@example.test", "Inline", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.CreateMailAccount(ctx, store.MailAccount{
		UserID: user.ID, Email: user.Email, Host: "imap.example.test", Port: 993,
		Username: user.Email, EncryptedPassword: "secret", UseTLS: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := db.GetOrCreateMailbox(ctx, user.ID, account.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}

	image := []byte("\x89PNG\r\n\x1a\n inline pixels")
	raw := []byte(strings.Join([]string{
		"From: sender@example.test",
		"To: " + user.Email,
		"Subject: inline image",
		"Content-Type: multipart/related; boundary=rel",
		"",
		"--rel",
		"Content-Type: text/html; charset=utf-8",
		"",
		`<p>Hello</p><img src="cid:hero@example.test">`,
		"--rel",
		"Content-Type: image/png",
		"Content-ID: <hero@example.test>",
		"Content-Transfer-Encoding: base64",
		"",
		base64.StdEncoding.EncodeToString(image),
		"--rel--",
	}, "\r\n"))
	saved, err := blobs.SaveRawMessage(user.ID, account.ID, mailbox.Name, 1, raw)
	if err != nil {
		t.Fatal(err)
	}
	blobRec, err := db.CreateBlob(ctx, store.BlobRecord{
		UserID: user.ID, Kind: "message", Path: saved.Path, SHA256: saved.SHA256, Size: saved.Size,
	})
	if err != nil {
		t.Fatal(err)
	}
	message, err := db.CreateMessage(ctx, store.CreateMessage{
		UserID: user.ID, AccountID: account.ID, MailboxID: mailbox.ID, BlobID: blobRec.ID,
		UID: 1, Date: time.Now(), InternalDate: time.Now(), Subject: "inline image",
		Size: saved.Size, BlobPath: saved.Path, HasAttachments: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The row still points at a standalone attachment file, but the file is not
	// there any more - retention pruned it, or it never reached disk.
	attachment, err := db.CreateAttachment(ctx, store.Attachment{
		UserID: user.ID, MessageID: message.ID, BlobID: blobRec.ID,
		ContentType: "image/png", ContentID: "hero@example.test", IsInline: true,
		Size: int64(len(image)),
		BlobPath: "users/" + strconv.FormatInt(user.ID, 10) + "/blobs/attachments/" +
			strconv.FormatInt(message.ID, 10) + "/000-pruned-hero.png",
	})
	if err != nil {
		t.Fatal(err)
	}

	server := &Server{store: db, blobs: blobs}
	target := "/attachments/" + strconv.FormatInt(attachment.ID, 10) + "/inline"
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req = req.WithContext(context.WithValue(req.Context(), userContextKey, currentUser{User: user}))
	rec := httptest.NewRecorder()
	server.handleAttachment(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("inline attachment status = %d body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != string(image) {
		t.Fatalf("inline attachment body = %q, want the part from the raw message", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("content type = %q", got)
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "inline;") {
		t.Fatalf("content disposition = %q", got)
	}

	missing := httptest.NewRequest(http.MethodGet, "/attachments/"+strconv.FormatInt(attachment.ID+1000, 10)+"/inline", nil)
	missing = missing.WithContext(context.WithValue(missing.Context(), userContextKey, currentUser{User: user}))
	missingRec := httptest.NewRecorder()
	server.handleAttachment(missingRec, missing)
	if missingRec.Code != http.StatusNotFound {
		t.Fatalf("unknown attachment status = %d", missingRec.Code)
	}
}

func TestExpandedMessageRepairsInlineAttachmentRowFromRawMessage(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	blobs := blob.New(t.TempDir())

	user, err := db.CreateUser(ctx, "repair@example.test", "Repair", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.CreateMailAccount(ctx, store.MailAccount{
		UserID: user.ID, Email: user.Email, Host: "imap.example.test", Port: 993,
		Username: user.Email, EncryptedPassword: "secret", UseTLS: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := db.GetOrCreateMailbox(ctx, user.ID, account.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}

	image := []byte("\x89PNG\r\n\x1a\n signature pixels")
	body := `<p>Gruß</p><img src="cid:sig@example.test">`
	raw := []byte(strings.Join([]string{
		"From: sender@example.test",
		"To: " + user.Email,
		"Subject: inline signature",
		"Content-Type: multipart/related; boundary=rel",
		"",
		"--rel",
		"Content-Type: text/html; charset=utf-8",
		"",
		body,
		"--rel",
		"Content-Type: image/png",
		"Content-ID: <sig@example.test>",
		"Content-Transfer-Encoding: base64",
		"",
		base64.StdEncoding.EncodeToString(image),
		"--rel--",
	}, "\r\n"))
	saved, err := blobs.SaveRawMessage(user.ID, account.ID, mailbox.Name, 2, raw)
	if err != nil {
		t.Fatal(err)
	}
	blobRec, err := db.CreateBlob(ctx, store.BlobRecord{
		UserID: user.ID, Kind: "message", Path: saved.Path, SHA256: saved.SHA256, Size: saved.Size,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Indexed before inline parts without a filename were captured: the picture
	// is in the message, no attachment row points at it, and the message counts
	// as carrying no attachments at all.
	message, err := db.CreateMessage(ctx, store.CreateMessage{
		UserID: user.ID, AccountID: account.ID, MailboxID: mailbox.ID, BlobID: blobRec.ID,
		UID: 2, Date: time.Now(), InternalDate: time.Now(), Subject: "inline signature",
		Size: saved.Size, BlobPath: saved.Path, HasAttachments: false,
	})
	if err != nil {
		t.Fatal(err)
	}

	server := &Server{store: db, blobs: blobs}
	repaired := server.ensureInlineCIDAttachments(ctx, user.ID, message, nil, body, true)
	if len(repaired) != 1 || repaired[0].ContentID != "sig@example.test" || !repaired[0].IsInline {
		t.Fatalf("inline attachment row was not repaired: %+v", repaired)
	}
	// A collapsed message is left alone, and so is one whose cid: references a
	// row that already exists.
	if got := server.ensureInlineCIDAttachments(ctx, user.ID, message, nil, body, false); len(got) != 0 {
		t.Fatalf("collapsed message was repaired: %+v", got)
	}
	if got := server.ensureInlineCIDAttachments(ctx, user.ID, message, repaired, body, true); len(got) != 1 {
		t.Fatalf("message with a matching row was repaired again: %+v", got)
	}

	doc := emailDocumentWithInlineAttachments(body, "", false, nil, repaired)
	inlineURL := "/attachments/" + strconv.FormatInt(repaired[0].ID, 10) + "/inline"
	if !strings.Contains(doc, `src="`+inlineURL+`"`) {
		t.Fatalf("document did not point at the repaired attachment: %s", doc)
	}
	req := httptest.NewRequest(http.MethodGet, inlineURL, nil)
	req = req.WithContext(context.WithValue(req.Context(), userContextKey, currentUser{User: user}))
	rec := httptest.NewRecorder()
	server.handleAttachment(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != string(image) {
		t.Fatalf("repaired inline attachment status = %d body = %q", rec.Code, rec.Body.String())
	}
}

func TestInlineCIDRepairIsAttemptedOncePerMessage(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	blobs := blob.New(t.TempDir())

	user, err := db.CreateUser(ctx, "dangling@example.test", "Dangling", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.CreateMailAccount(ctx, store.MailAccount{
		UserID: user.ID, Email: user.Email, Host: "imap.example.test", Port: 993,
		Username: user.Email, EncryptedPassword: "secret", UseTLS: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := db.GetOrCreateMailbox(ctx, user.ID, account.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}

	// The sender referenced a part it never attached: no repair can ever
	// resolve this cid:, so the evidence stays exactly as the repair found it.
	body := `<p>Hallo</p><img src="cid:typo@example.test">`
	raw := []byte(strings.Join([]string{
		"From: sender@example.test",
		"To: " + user.Email,
		"Subject: dangling reference",
		"Content-Type: text/html; charset=utf-8",
		"",
		body,
	}, "\r\n"))
	saved, err := blobs.SaveRawMessage(user.ID, account.ID, mailbox.Name, 3, raw)
	if err != nil {
		t.Fatal(err)
	}
	blobRec, err := db.CreateBlob(ctx, store.BlobRecord{
		UserID: user.ID, Kind: "message", Path: saved.Path, SHA256: saved.SHA256, Size: saved.Size,
	})
	if err != nil {
		t.Fatal(err)
	}
	message, err := db.CreateMessage(ctx, store.CreateMessage{
		UserID: user.ID, AccountID: account.ID, MailboxID: mailbox.ID, BlobID: blobRec.ID,
		UID: 3, Date: time.Now(), InternalDate: time.Now(), Subject: "dangling reference",
		Size: saved.Size, BlobPath: saved.Path,
	})
	if err != nil {
		t.Fatal(err)
	}

	server := &Server{store: db, blobs: blobs}
	if got := server.ensureInlineCIDAttachments(ctx, user.ID, message, nil, body, true); len(got) != 0 {
		t.Fatalf("repair invented rows for a dangling reference: %+v", got)
	}
	if !server.inlineRepairAttempted[user.ID][message.ID] {
		t.Fatal("first render did not record its attempt")
	}
	// Renders after the first must not reparse the raw message again, and the
	// only evidence available for that here is the claim the repair takes.
	if server.claimInlineRepairAttempt(user.ID, message.ID) {
		t.Fatal("a later render claimed a second repair attempt")
	}
	if got := server.ensureInlineCIDAttachments(ctx, user.ID, message, nil, body, true); len(got) != 0 {
		t.Fatalf("second render returned rows: %+v", got)
	}
	other, err := db.CreateUser(ctx, "dangling-other@example.test", "Other", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	if !server.claimInlineRepairAttempt(other.ID, message.ID) {
		t.Fatal("another tenant was refused an attempt of its own")
	}
}

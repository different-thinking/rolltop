// File overview: Downloading every attachment of one message as a ZIP archive.

package web

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"io"
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

func TestAttachmentArchivePacksVisibleAttachmentsFromTheRawMessage(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	blobs := blob.New(t.TempDir())

	user, err := db.CreateUser(ctx, "zip@example.test", "Zip", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateUser(ctx, "other@example.test", "Other", "hash", false)
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

	first := []byte("first report")
	second := []byte("second report, same name")
	pixel := []byte("\x89PNG\r\n\x1a\n inline pixels")
	part := func(contentType, disposition, extra string, data []byte) []string {
		lines := []string{"--mix", "Content-Type: " + contentType}
		if disposition != "" {
			lines = append(lines, "Content-Disposition: "+disposition)
		}
		if extra != "" {
			lines = append(lines, extra)
		}
		return append(lines, "Content-Transfer-Encoding: base64", "", base64.StdEncoding.EncodeToString(data))
	}
	lines := []string{
		"From: sender@example.test",
		"To: " + user.Email,
		"Subject: Quarterly: reports",
		"Content-Type: multipart/mixed; boundary=mix",
		"",
		"--mix",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"See attached.",
	}
	lines = append(lines, part("text/plain", `attachment; filename="report.txt"`, "", first)...)
	lines = append(lines, part("text/plain", `attachment; filename="../report.txt"`, "", second)...)
	lines = append(lines, part("image/png", "inline", "Content-ID: <pixel@example.test>", pixel)...)
	lines = append(lines, "--mix--")
	raw := []byte(strings.Join(lines, "\r\n"))
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
		UID: 1, Date: time.Now(), InternalDate: time.Now(), Subject: "Quarterly: reports",
		Size: saved.Size, BlobPath: saved.Path, HasAttachments: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, att := range []store.Attachment{
		{Filename: "report.txt", ContentType: "text/plain", Size: int64(len(first))},
		{Filename: "../report.txt", ContentType: "text/plain", Size: int64(len(second))},
		{ContentType: "image/png", ContentID: "pixel@example.test", IsInline: true, Size: int64(len(pixel))},
	} {
		att.UserID, att.MessageID, att.BlobID = user.ID, message.ID, blobRec.ID
		if _, err := db.CreateAttachment(ctx, att); err != nil {
			t.Fatal(err)
		}
	}

	server := &Server{store: db, blobs: blobs}
	get := func(u store.User, target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req = req.WithContext(context.WithValue(req.Context(), userContextKey, currentUser{User: u}))
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		return rec
	}
	target := attachmentArchiveRoutePrefix + strconv.FormatInt(message.ID, 10) + "/zip"
	rec := get(user, target)
	if rec.Code != http.StatusOK {
		t.Fatalf("archive status = %d body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/zip" {
		t.Fatalf("content type = %q", got)
	}
	if got := rec.Header().Get("Content-Disposition"); got != `attachment; filename="Quarterly_ reports.zip"` {
		t.Fatalf("content disposition = %q", got)
	}
	archive, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, file := range archive.File {
		r, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			t.Fatal(err)
		}
		got[file.Name] = string(data)
	}
	want := map[string]string{"report.txt": string(first), "report (2).txt": string(second)}
	if len(got) != len(want) {
		t.Fatalf("archive entries = %v, want %v (inline parts stay out)", got, want)
	}
	for name, data := range want {
		if got[name] != data {
			t.Fatalf("entry %q = %q, want %q (all entries %v)", name, got[name], data, got)
		}
	}

	if rec := get(other, target); rec.Code != http.StatusNotFound {
		t.Fatalf("another user's archive status = %d, want 404", rec.Code)
	}
}

func TestAttachmentArchiveFilenameStaysInOneDirectory(t *testing.T) {
	for in, want := range map[string]string{
		"report.pdf":              "report.pdf",
		"../../etc/passwd":        "passwd",
		`C:\Users\x\Rechnung.pdf`: "Rechnung.pdf",
		"  ..  ":                  "",
		"a:b?.txt":                "a_b_.txt",
	} {
		if got := attachmentArchiveFilename(in); got != want {
			t.Errorf("attachmentArchiveFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

// File overview: Regression test for mirroring mail whose headers and body
// carry bytes no charset in the message declares.

package syncer

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"rolltop/backend/blob"
)

// PostgreSQL rejects invalid UTF-8 in a TEXT column outright, where SQLite
// stored whatever bytes it was handed. A German subject line written as raw
// ISO-8859-1 - "Änderung", the 0xe4 0x6e 0x64 in the reported failure - used to
// abort the whole mailbox with SQLSTATE 22021 rather than storing one message.
func TestStoreFetchedMessageStoresUndeclaredLatin1Headers(t *testing.T) {
	fixture := newMoveTestFixture(t)
	ctx := context.Background()
	fixture.service.Blobs = blob.New(t.TempDir())

	raw := []byte("From: \"M\xfcller, Bj\xf6rn\" <bjoern@example.test>\r\n" +
		"To: receiver@example.test\r\n" +
		"Subject: Wichtige \xc4nderung\r\n" +
		"Date: Tue, 14 Jul 2026 12:00:00 +0000\r\n" +
		"Message-ID: <latin1-headers@example.test>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=rolltop-latin1\r\n\r\n" +
		"--rolltop-latin1\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" +
		"Sch\xf6nen Gru\xdf, die \xc4nderung ist erledigt.\r\n" +
		"--rolltop-latin1\r\nContent-Type: application/pdf; name=\"\xc4nderung.pdf\"\r\n" +
		"Content-Disposition: attachment; filename=\"\xc4nderung.pdf\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\nJVBERi10ZXN0\r\n" +
		"--rolltop-latin1--\r\n")
	item := FetchedMessage{
		Mailbox:      fixture.source.Name,
		UID:          46215,
		UIDValidity:  uint32(fixture.source.UIDValidity),
		InternalDate: time.Date(2026, 7, 14, 12, 1, 0, 0, time.UTC),
		Raw:          raw,
	}

	msg, _, _, err := fixture.service.storeFetchedMessage(ctx, fixture.userID,
		fixture.account, fixture.source, item, false)
	if err != nil {
		t.Fatalf("store message with undeclared ISO-8859-1 headers: %v", err)
	}
	if msg.Subject != "Wichtige Änderung" {
		t.Fatalf("stored subject = %q, want %q", msg.Subject, "Wichtige Änderung")
	}
	if !strings.Contains(msg.FromAddr, "Müller, Björn") {
		t.Fatalf("stored sender = %q, want the repaired display name", msg.FromAddr)
	}
	if !utf8.ValidString(msg.BodyText) || !strings.Contains(msg.BodyText, "Schönen Gruß") {
		t.Fatalf("stored body = %q, want repaired UTF-8", msg.BodyText)
	}
	attachments, err := fixture.store.ListAttachmentsForMessage(ctx, fixture.userID, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attachments) != 1 || attachments[0].Filename != "Änderung.pdf" {
		t.Fatalf("stored attachment metadata = %+v", attachments)
	}

	stored, err := fixture.store.GetMessageForUser(ctx, fixture.userID, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Subject != "Wichtige Änderung" {
		t.Fatalf("reloaded subject = %q, want %q", stored.Subject, "Wichtige Änderung")
	}
}

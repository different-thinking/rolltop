// File overview: The mimetype: operator, across the parser and both backends.
//
// It is tested in all three places on purpose. The operator's whole value is
// that a filter rule can say "mail with a recording in it" and mean the same
// thing whichever search backend the install runs, and the two backends answer
// it by different means -- an analyzed phrase against Bleve's attachment_types
// field, an anchored ILIKE against the attachments table in PostgreSQL. A test
// of only one of them would let them drift apart silently.

package search

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"rolltop/backend/store"
)

func TestParseQueryReadsMIMETypeAndRemovesItFromTheText(t *testing.T) {
	parsed := parseQuery("memo mimetype:audio/ subject:Notes")
	if parsed.MIMEType != "audio/" {
		t.Fatalf("MIMEType = %q", parsed.MIMEType)
	}
	if parsed.Subject != "Notes" {
		t.Fatalf("Subject = %q, want the other operators still parsed", parsed.Subject)
	}
	if parsed.Text != "memo" {
		t.Fatalf("Text = %q, want the operator lifted out of the free text", parsed.Text)
	}
}

func TestParseQueryLowercasesAndUnquotesTheMIMEType(t *testing.T) {
	if got := parseQuery(`mimetype:"AUDIO/MPEG"`).MIMEType; got != "audio/mpeg" {
		t.Fatalf("MIMEType = %q, want it unquoted and lowercased", got)
	}
	if got := parseQuery("mimetype:Application/PDF").MIMEType; got != "application/pdf" {
		t.Fatalf("MIMEType = %q", got)
	}
}

// The Bleve field is analyzed, so `audio/` has to reach it as the token every
// audio type carries rather than as the literal string, which would match
// nothing at all.
func TestBleveMIMETypeOperatorSelectsAFamilyAndAnExactType(t *testing.T) {
	ctx := context.Background()
	svc, err := Open(filepath.Join(t.TempDir(), "bleve"))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	now := time.Now()
	recording := store.MessageRecord{ID: 1, UserID: 1, Subject: "Memo", BodyText: "recorded", Date: now, HasAttachments: true}
	compressed := store.MessageRecord{ID: 2, UserID: 1, Subject: "Memo", BodyText: "recorded", Date: now, HasAttachments: true}
	invoice := store.MessageRecord{ID: 3, UserID: 1, Subject: "Memo", BodyText: "recorded", Date: now, HasAttachments: true}
	if err := svc.IndexMessage(ctx, recording, []AttachmentDoc{{Filename: "voice.m4a", ContentType: "audio/mp4"}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.IndexMessage(ctx, compressed, []AttachmentDoc{{Filename: "voice.mp3", ContentType: "audio/mpeg"}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.IndexMessage(ctx, invoice, []AttachmentDoc{{Filename: "bill.pdf", ContentType: "application/pdf"}}); err != nil {
		t.Fatal(err)
	}

	family, err := svc.Search(ctx, 1, "memo mimetype:audio/", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !containsIDs(family, 1, 2) || containsIDs(family, 3) {
		t.Fatalf("mimetype:audio/ = %v, want both recordings and not the PDF", family)
	}

	exact, err := svc.Search(ctx, 1, "memo mimetype:audio/mpeg", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(exact) != 1 || exact[0] != 2 {
		t.Fatalf("mimetype:audio/mpeg = %v, want only the mp3", exact)
	}

	other, err := svc.Search(ctx, 1, "memo mimetype:application/pdf", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 1 || other[0] != 3 {
		t.Fatalf("mimetype:application/pdf = %v, want only the invoice", other)
	}
}

func TestPostgresMIMETypeOperatorSelectsAFamilyAndAnExactType(t *testing.T) {
	svc, db, user, mailbox := openPostgresSearchFixtures(t)
	ctx := context.Background()

	recording := seedPostgresSearchMessage(t, db, user, mailbox, 20, "Memo", "recorded on the way home")
	compressed := seedPostgresSearchMessage(t, db, user, mailbox, 21, "Memo", "recorded on the way home")
	invoice := seedPostgresSearchMessage(t, db, user, mailbox, 22, "Memo", "recorded on the way home")
	for _, seeded := range []struct {
		message     store.MessageRecord
		filename    string
		contentType string
	}{
		{recording, "voice.m4a", "audio/mp4"},
		{compressed, "voice.mp3", "audio/mpeg"},
		{invoice, "bill.pdf", "application/pdf"},
	} {
		if _, err := db.DB().ExecContext(ctx, `UPDATE messages SET has_attachments = 1 WHERE id = $1`, seeded.message.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.CreateAttachment(ctx, store.Attachment{
			UserID: user.ID, MessageID: seeded.message.ID, BlobID: seeded.message.BlobID,
			Filename: seeded.filename, ContentType: seeded.contentType, Size: 1, BlobPath: seeded.message.BlobPath,
		}); err != nil {
			t.Fatal(err)
		}
		if err := svc.IndexMessages(ctx, []MessageIndexDocument{{
			Message:     seeded.message,
			Attachments: []AttachmentDoc{{Filename: seeded.filename, ContentType: seeded.contentType}},
		}}); err != nil {
			t.Fatal(err)
		}
	}

	hits, err := svc.SearchHitsWithOptions(ctx, user.ID, "recorded mimetype:audio/", 10, 0, SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ids := hitIDs(hits); !containsIDs(ids, recording.ID, compressed.ID) || containsIDs(ids, invoice.ID) {
		t.Fatalf("mimetype:audio/ = %v, want both recordings and not the PDF", ids)
	}

	hits, err = svc.SearchHitsWithOptions(ctx, user.ID, "recorded mimetype:audio/mpeg", 10, 0, SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ids := hitIDs(hits); len(ids) != 1 || ids[0] != compressed.ID {
		t.Fatalf("mimetype:audio/mpeg = %v, want only the mp3", ids)
	}
}

// The anchor is what keeps a family from being a substring match: without it
// `mimetype:audio/` would also take `application/x-audio-playlist`.
func TestPostgresMIMETypeOperatorIsAnchoredAtTheStart(t *testing.T) {
	svc, db, user, mailbox := openPostgresSearchFixtures(t)
	ctx := context.Background()

	playlist := seedPostgresSearchMessage(t, db, user, mailbox, 30, "Memo", "recorded on the way home")
	if _, err := db.DB().ExecContext(ctx, `UPDATE messages SET has_attachments = 1 WHERE id = $1`, playlist.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateAttachment(ctx, store.Attachment{
		UserID: user.ID, MessageID: playlist.ID, BlobID: playlist.BlobID,
		Filename: "list.xspf", ContentType: "application/x-audio-playlist", Size: 1, BlobPath: playlist.BlobPath,
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.IndexMessages(ctx, []MessageIndexDocument{{
		Message:     playlist,
		Attachments: []AttachmentDoc{{Filename: "list.xspf", ContentType: "application/x-audio-playlist"}},
	}}); err != nil {
		t.Fatal(err)
	}

	hits, err := svc.SearchHitsWithOptions(ctx, user.ID, "recorded mimetype:audio/", 10, 0, SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ids := hitIDs(hits); len(ids) != 0 {
		t.Fatalf("mimetype:audio/ = %v, want nothing: the only attachment merely has audio in its name", ids)
	}
}

func hitIDs(hits []Hit) []int64 {
	out := make([]int64, 0, len(hits))
	for _, hit := range hits {
		out = append(out, hit.ID)
	}
	return out
}

func containsIDs(ids []int64, wanted ...int64) bool {
	present := map[int64]bool{}
	for _, id := range ids {
		present[id] = true
	}
	for _, id := range wanted {
		if !present[id] {
			return false
		}
	}
	return true
}

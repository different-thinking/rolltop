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
	"slices"
	"strings"
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

// The bug this guards: `attachment_types` is analyzed, so `audio/mpeg` is
// indexed as the tokens `audio` and `mpeg` -- and a family query that can only
// ask for `audio` also reaches `application/x-audio-playlist`. Both backends
// have to refuse it, or `mimetype:` means one thing on Bleve and another on
// PostgreSQL.
func TestBleveMIMETypeOperatorIsAnchoredAtTheStart(t *testing.T) {
	ctx := context.Background()
	svc, err := Open(filepath.Join(t.TempDir(), "bleve"))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	now := time.Now()
	if err := svc.IndexMessage(ctx, store.MessageRecord{ID: 1, UserID: 1, Subject: "Memo", BodyText: "recorded", Date: now, HasAttachments: true},
		[]AttachmentDoc{{Filename: "list.xspf", ContentType: "application/x-audio-playlist"}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.IndexMessage(ctx, store.MessageRecord{ID: 2, UserID: 1, Subject: "Memo", BodyText: "recorded", Date: now, HasAttachments: true},
		[]AttachmentDoc{{Filename: "voice.m4a", ContentType: "audio/mp4"}}); err != nil {
		t.Fatal(err)
	}

	ids, err := svc.Search(ctx, 1, "memo mimetype:audio/", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != 2 {
		t.Fatalf("mimetype:audio/ = %v, want only the real recording: a playlist merely has the word in its subtype", ids)
	}
}

// Documents indexed before the anchored tokens existed carry no marker, and
// have to keep matching. Precision improves when they are reindexed; nothing
// stops working in the meantime, which is what makes this shippable without a
// forced index rebuild.
func TestBleveMIMETypeOperatorStillMatchesDocumentsWithoutAnchors(t *testing.T) {
	ctx := context.Background()
	svc, err := Open(filepath.Join(t.TempDir(), "bleve"))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	// Indexed the way an older build wrote the field: the content types alone.
	legacy := buildMessageDocument(
		store.MessageRecord{ID: 1, UserID: 1, Subject: "Memo", BodyText: "recorded", Date: time.Now(), HasAttachments: true},
		[]AttachmentDoc{{Filename: "voice.m4a", ContentType: "audio/mp4"}})
	legacy["attachment_types"] = "audio/mp4"
	index, err := svc.indexForUser(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := index.Index("1", legacy); err != nil {
		t.Fatal(err)
	}

	ids, err := svc.Search(ctx, 1, "memo mimetype:audio/", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("mimetype:audio/ = %v, want the document indexed before anchoring to still match", ids)
	}
}

func TestMIMEAnchorTokensAreUnambiguous(t *testing.T) {
	// The family and the exact type are separate tokens, and the separator is
	// the one character the analyzer keeps inside a word.
	tokens := mimeAnchorTokens(`audio/mpeg; codecs="mp3"`)
	if len(tokens) != 2 || tokens[0] != "zmt_audio" || tokens[1] != "zmt_audio_mpeg" {
		t.Fatalf("tokens = %v", tokens)
	}
	// A subtype that is punctuation-heavy still reduces the same way on both
	// sides, which is the only thing that has to hold.
	if got := mimeAnchorQueryToken("image/svg+xml"); got != "zmt_image_svgxml" {
		t.Fatalf("query token = %q", got)
	}
	if got := mimeAnchorTokens("image/svg+xml"); len(got) != 2 || got[1] != "zmt_image_svgxml" {
		t.Fatalf("index tokens = %v, want the query token among them", got)
	}
	// A family query, spelled either way, asks for the family token.
	for _, value := range []string{"audio/", "audio", "AUDIO/"} {
		if got := mimeAnchorQueryToken(value); got != "zmt_audio" {
			t.Errorf("mimeAnchorQueryToken(%q) = %q", value, got)
		}
	}
	// `x-audio-playlist` must not reduce to anything an audio query asks for.
	if got := mimeAnchorTokens("application/x-audio-playlist"); got[0] == "zmt_audio" || got[1] == "zmt_audio" {
		t.Fatalf("tokens = %v, want nothing an audio family query would reach", got)
	}
}

// The bounding that keeps a pending batch proportional to its document count
// collapses a message's attachments into one entry holding the joined content
// types. The anchored tokens have to come out the same either way, or a bounded
// document stops indexing like the unbounded one it stands in for -- which is
// the invariant TestBoundIndexDocumentProjectsIdentically exists to hold.
func TestMIMEIndexValuesReadACollapsedAttachmentList(t *testing.T) {
	separate := mimeTypeIndexValues([]string{"audio/mp4", "application/pdf"})
	collapsed := mimeTypeIndexValues([]string{"audio/mp4 application/pdf"})
	// The content-type half differs in grouping and joins to the same string;
	// the anchored tokens after it have to be identical.
	if strings.Join(separate[2:], " ") != strings.Join(collapsed[1:], " ") {
		t.Fatalf("separate = %v, collapsed = %v", separate, collapsed)
	}
	if !slices.Contains(collapsed, "zmt_audio") || !slices.Contains(collapsed, "zmt_application_pdf") {
		t.Fatalf("collapsed = %v, want a token per type in the joined entry", collapsed)
	}
}

// The marker is written last so that a document which kept it kept every
// anchored token before it. A truncation can cost precision; it must never cost
// a match.
func TestMIMEIndexValuesPutTheMarkerLast(t *testing.T) {
	values := mimeTypeIndexValues([]string{"audio/mp4", "application/pdf"})
	if values[len(values)-1] != mimeAnchorMarker {
		t.Fatalf("values = %v, want the marker last", values)
	}
	if values[0] != "audio/mp4" || values[1] != "application/pdf" {
		t.Fatalf("values = %v, want the content types first", values)
	}
	// A message with no attachments carries no marker, so it reads as a
	// document from before anchoring rather than as one with nothing to offer.
	if got := mimeTypeIndexValues(nil); len(got) != 0 {
		t.Fatalf("values for no attachments = %v", got)
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

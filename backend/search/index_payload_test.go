// File overview: Pending index document payload bounds.

package search

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"rolltop/backend/store"
)

func oversizedIndexDocument() MessageIndexDocument {
	return MessageIndexDocument{
		Message: store.MessageRecord{
			UserID:          7,
			ID:              11,
			MailboxID:       3,
			Subject:         strings.Repeat("subject ", 40*1024),
			FromAddr:        strings.Repeat("sender@example.test ", 8*1024),
			ToAddr:          strings.Repeat("recipient@example.test ", 8*1024),
			CCAddr:          strings.Repeat("copied@example.test ", 8*1024),
			MessageIDHeader: strings.Repeat("<message-id@example.test>", 8*1024),
			BodyText:        strings.Repeat("body text that keeps going ", 400*1024),
			BodyHTML:        strings.Repeat("<p>ignored by the projection</p>", 1024),
			LanguageCode:    "de",
		},
		Attachments: []AttachmentDoc{
			{
				Filename:    strings.Repeat("first-attachment-", 16*1024) + ".pdf",
				ContentType: "application/pdf",
				Text:        strings.Repeat("extracted attachment text ", 200*1024),
			},
			{
				Filename:    "second.pdf",
				ContentType: "application/pdf",
				Text:        strings.Repeat("more extracted text ", 200*1024),
			},
			{Filename: "third.txt", ContentType: "text/plain", Text: "short tail"},
		},
	}
}

// The bound is only safe if it is invisible: sync commits bounded documents, so
// a bounded document has to project exactly like the unbounded one it replaced.
func TestBoundIndexDocumentProjectsIdentically(t *testing.T) {
	document := oversizedIndexDocument()
	bounded, _ := BoundIndexDocument(document)

	want := buildMessageDocument(document.Message, document.Attachments)
	got := buildMessageDocument(bounded.Message, bounded.Attachments)
	if !reflect.DeepEqual(got, want) {
		for field, wantValue := range want {
			gotValue := got[field]
			if reflect.DeepEqual(gotValue, wantValue) {
				continue
			}
			wantText, _ := wantValue.(string)
			gotText, _ := gotValue.(string)
			t.Errorf("field %q differs: bounded %d bytes, unbounded %d bytes", field, len(gotText), len(wantText))
		}
		t.FailNow()
	}
}

func TestBoundIndexDocumentReleasesUnindexablePayload(t *testing.T) {
	document := oversizedIndexDocument()
	bounded, bytes := BoundIndexDocument(document)

	if got := len(bounded.Message.BodyText); got != maxIndexedBodyBytes {
		t.Fatalf("bounded body = %d bytes, want %d", got, maxIndexedBodyBytes)
	}
	if got := len(bounded.Message.Subject); got > maxIndexedHeaderBytes {
		t.Fatalf("bounded subject = %d bytes, want at most %d", got, maxIndexedHeaderBytes)
	}
	if bounded.Message.BodyHTML != "" {
		t.Fatal("bounded document still carries HTML the projection never reads")
	}
	attachmentText := 0
	for _, attachment := range bounded.Attachments {
		attachmentText += len(attachment.Text)
	}
	if attachmentText > maxIndexedAttachmentsBytes {
		t.Fatalf("bounded attachment text = %d bytes, want at most %d", attachmentText, maxIndexedAttachmentsBytes)
	}
	if bytes == 0 || bytes > maxIndexedBodyBytes+maxIndexedAttachmentsBytes+8*maxIndexedHeaderBytes {
		t.Fatalf("reported payload = %d bytes, want a bounded non-zero size", bytes)
	}
	// The point of the exercise: a batch of these no longer scales with the
	// largest message in the folder.
	if unbounded := indexDocumentPayloadBytes(document); unbounded < 5*bytes {
		t.Fatalf("payload only shrank from %d to %d bytes", unbounded, bytes)
	}
}

// An encrypted message indexes neither its body nor its attachments, so a batch
// must not carry them around until the commit drops them.
func TestBoundIndexDocumentDropsEncryptedPayload(t *testing.T) {
	document := oversizedIndexDocument()
	document.Message.IsEncrypted = true
	document.Message.HasAttachments = true

	bounded, bytes := BoundIndexDocument(document)
	if bounded.Message.BodyText != "" || len(bounded.Attachments) != 0 {
		t.Fatalf("encrypted document kept %d body bytes and %d attachments",
			len(bounded.Message.BodyText), len(bounded.Attachments))
	}
	if bytes > 8*maxIndexedHeaderBytes {
		t.Fatalf("encrypted payload = %d bytes, want headers only", bytes)
	}
	want := buildMessageDocument(document.Message, document.Attachments)
	if got := buildMessageDocument(bounded.Message, bounded.Attachments); !reflect.DeepEqual(got, want) {
		t.Fatal("bounded encrypted document projects differently")
	}
}

func TestBoundIndexDocumentKeepsSmallDocumentsIntact(t *testing.T) {
	document := MessageIndexDocument{
		Message: store.MessageRecord{
			UserID: 1, ID: 2, Subject: "Rechnung", FromAddr: "a@example.test",
			BodyText: "Anbei die Rechnung.",
		},
		Attachments: []AttachmentDoc{{Filename: "rechnung.pdf", ContentType: "application/pdf", Text: "Rechnung Nr. 4"}},
	}
	bounded, bytes := BoundIndexDocument(document)
	if !reflect.DeepEqual(bounded, document) {
		t.Fatalf("bounded ordinary document = %+v, want it unchanged", bounded)
	}
	if bytes != indexDocumentPayloadBytes(document) {
		t.Fatalf("reported payload = %d bytes, want %d", bytes, indexDocumentPayloadBytes(document))
	}
}

// Bounding copies before it trims, so the caller's slice keeps whatever it had.
func TestBoundIndexDocumentDoesNotMutateItsInput(t *testing.T) {
	document := oversizedIndexDocument()
	original := make([]AttachmentDoc, len(document.Attachments))
	copy(original, document.Attachments)

	if _, _ = BoundIndexDocument(document); !reflect.DeepEqual(document.Attachments, original) {
		t.Fatal("BoundIndexDocument trimmed the caller's attachments in place")
	}
}

// Attachment budgets are shared across the whole message, so many medium
// attachments cannot add up past the limit either.
func TestBoundIndexDocumentSpendsOneAttachmentBudgetForAllParts(t *testing.T) {
	document := MessageIndexDocument{Message: store.MessageRecord{UserID: 1, ID: 2}}
	for i := range 40 {
		document.Attachments = append(document.Attachments, AttachmentDoc{
			Filename:    fmt.Sprintf("part-%02d.pdf", i),
			ContentType: "application/pdf",
			Text:        strings.Repeat("x", 100*1024),
		})
	}
	bounded, bytes := BoundIndexDocument(document)
	if bytes > maxIndexedAttachmentsBytes+maxIndexedNamesBytes*2+8*maxIndexedHeaderBytes {
		t.Fatalf("payload = %d bytes, want one shared attachment budget", bytes)
	}
	want := buildMessageDocument(document.Message, document.Attachments)
	if got := buildMessageDocument(bounded.Message, bounded.Attachments); !reflect.DeepEqual(got, want) {
		t.Fatal("bounded multi-attachment document projects differently")
	}
}

// Attachment text is extracted from arbitrary files, so it is not always valid
// UTF-8. The projection drops the invalid bytes while it joins, which means the
// budget an attachment spends is not its own length. Accounting per attachment
// got that wrong and silently stopped indexing the tail of the next one.
func TestBoundIndexDocumentKeepsIndexableTextAfterInvalidUTF8(t *testing.T) {
	invalid := strings.Repeat("\xff\xfe", 300*1024)
	document := MessageIndexDocument{
		Message: store.MessageRecord{UserID: 3, ID: 4, Subject: "Scans"},
		Attachments: []AttachmentDoc{
			{Filename: "latin1.txt", ContentType: "text/plain", Text: "lesbar " + invalid},
			{Filename: "report.pdf", ContentType: "application/pdf", Text: strings.Repeat("indexierbarer text ", 45*1024)},
		},
	}
	want := buildMessageDocument(document.Message, document.Attachments)
	bounded, _ := BoundIndexDocument(document)
	got := buildMessageDocument(bounded.Message, bounded.Attachments)

	wantText, _ := want["attachments"].(string)
	gotText, _ := got["attachments"].(string)
	if gotText != wantText {
		t.Fatalf("bounded attachment text = %d bytes, want the %d bytes the unbounded document indexes",
			len(gotText), len(wantText))
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("bounded document with undecodable attachment text projects differently")
	}
}

// Many small attachments are joined into the fields the projection indexes, and
// the joined form is what the batch has to carry.
func TestBoundIndexDocumentCollapsesAttachmentsToTheirIndexedForm(t *testing.T) {
	document := MessageIndexDocument{Message: store.MessageRecord{UserID: 1, ID: 2}}
	for i := range 5 {
		document.Attachments = append(document.Attachments, AttachmentDoc{
			Filename:    fmt.Sprintf("part-%d.pdf", i),
			ContentType: "application/pdf",
			Text:        fmt.Sprintf("text of part %d", i),
		})
	}
	bounded, _ := BoundIndexDocument(document)
	if len(bounded.Attachments) != 1 {
		t.Fatalf("bounded attachments = %d, want the single joined value", len(bounded.Attachments))
	}
	if want := "part-0.pdf part-1.pdf part-2.pdf part-3.pdf part-4.pdf"; bounded.Attachments[0].Filename != want {
		t.Fatalf("joined filenames = %q, want %q", bounded.Attachments[0].Filename, want)
	}
	want := buildMessageDocument(document.Message, document.Attachments)
	if got := buildMessageDocument(bounded.Message, bounded.Attachments); !reflect.DeepEqual(got, want) {
		t.Fatal("collapsed attachments project differently")
	}
	// A message with attachments still reports that it has them.
	if want["has_attachment"] != "true" {
		t.Fatalf("has_attachment = %v, want true", want["has_attachment"])
	}
}

// Whitespace-only extractions are not part of the joined text, so they must not
// take a separator with them either.
func TestBoundIndexDocumentSkipsAttachmentsWithoutReadableText(t *testing.T) {
	document := MessageIndexDocument{
		Message: store.MessageRecord{UserID: 1, ID: 2, HasAttachments: true},
		Attachments: []AttachmentDoc{
			{Filename: "blank.pdf", ContentType: "application/pdf", Text: "   \n\t "},
			{Filename: "signed.pdf", ContentType: "application/pdf", Text: "Vertrag"},
			{Filename: "image.png", ContentType: "image/png"},
		},
	}
	bounded, bytes := BoundIndexDocument(document)
	if got := bounded.Attachments[0].Text; got != "Vertrag" {
		t.Fatalf("joined attachment text = %q, want %q", got, "Vertrag")
	}
	if bytes == 0 {
		t.Fatal("reported payload = 0 bytes")
	}
	want := buildMessageDocument(document.Message, document.Attachments)
	if got := buildMessageDocument(bounded.Message, bounded.Attachments); !reflect.DeepEqual(got, want) {
		t.Fatal("bounded document with blank extractions projects differently")
	}
}

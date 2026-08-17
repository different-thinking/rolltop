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

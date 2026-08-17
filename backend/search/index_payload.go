// File overview: Bounding of the payload a pending index document retains
// between Bleve commits.

package search

import "strings"

// BoundIndexDocument trims the text a pending document carries to the byte
// limits the Bleve projection applies anyway, and reports the payload the
// document still holds.
//
// This exists because sync accumulates documents between commits. A batch that
// keeps whole message bodies and whole extracted attachment texts has a size
// decided by the largest mail in the folder rather than by its document count,
// so one mailbox holding a handful of very large messages can make an ordinary
// twenty-five document batch cost hundreds of megabytes. Trimming on the way in
// keeps a batch proportional to its count.
//
// The projection is unchanged by this. Each header and the body are cut with the
// same bounding function buildMessageDocument applies to them, which is
// idempotent, and the attachments are reduced to the joined values it derives
// from them, so a bounded document indexes byte for byte like the unbounded one
// it replaces.
func BoundIndexDocument(document MessageIndexDocument) (MessageIndexDocument, uint64) {
	message := document.Message
	message.Subject = boundedIndexText(message.Subject, maxIndexedHeaderBytes)
	message.FromAddr = boundedIndexText(message.FromAddr, maxIndexedHeaderBytes)
	message.ToAddr = boundedIndexText(message.ToAddr, maxIndexedHeaderBytes)
	message.CCAddr = boundedIndexText(message.CCAddr, maxIndexedHeaderBytes)
	message.MessageIDHeader = boundedIndexText(message.MessageIDHeader, maxIndexedHeaderBytes)
	message.BodyHTML = ""
	attachments := document.Attachments
	if message.IsEncrypted {
		// The projection discards the body and every attachment of an encrypted
		// message, so retaining either only costs memory.
		message.BodyText = ""
		attachments = nil
	} else {
		message.BodyText = boundedIndexText(message.BodyText, maxIndexedBodyBytes)
		attachments = boundIndexAttachments(attachments)
	}
	bounded := MessageIndexDocument{Message: message, Attachments: attachments}
	return bounded, indexDocumentPayloadBytes(bounded)
}

// boundIndexAttachments replaces the attachment list with the single value the
// projection derives from it. Each of the three attachment fields is indexed as
// one joined string, and the join is what decides how much of each attachment
// survives - not the attachment's own size - so the joined form is the smallest
// payload that still indexes identically. Reproducing that split across the
// original entries would mean reimplementing the join's budget accounting, and
// an accounting that differs by a byte silently stops indexing text.
//
// The result is a projection input, not an attachment list: nothing between here
// and the Bleve commit reads per-attachment fields. Whether the message has
// attachments is preserved, because buildMessageDocument only tests the slice
// for emptiness and sync carries the visible-attachment flag separately.
func boundIndexAttachments(attachments []AttachmentDoc) []AttachmentDoc {
	if len(attachments) == 0 {
		return attachments
	}
	names := make([]string, 0, len(attachments))
	contentTypes := make([]string, 0, len(attachments))
	texts := make([]string, 0, len(attachments))
	for _, attachment := range attachments {
		names = append(names, attachment.Filename)
		contentTypes = append(contentTypes, attachment.ContentType)
		// The same filter buildMessageDocument applies: an attachment with no
		// readable text is not part of the joined text at all.
		if strings.TrimSpace(attachment.Text) != "" {
			texts = append(texts, attachment.Text)
		}
	}
	return []AttachmentDoc{{
		Filename:    boundedIndexJoin(names, maxIndexedNamesBytes),
		ContentType: boundedIndexJoin(contentTypes, maxIndexedNamesBytes),
		Text:        boundedIndexJoin(texts, maxIndexedAttachmentsBytes),
	}}
}

func indexDocumentPayloadBytes(document MessageIndexDocument) uint64 {
	message := document.Message
	bytes := len(message.Subject) + len(message.FromAddr) + len(message.ToAddr) +
		len(message.CCAddr) + len(message.MessageIDHeader) + len(message.BodyText) +
		len(message.BodyHTML) + len(message.LanguageCode) + len(message.BlobPath)
	for _, attachment := range document.Attachments {
		bytes += len(attachment.Filename) + len(attachment.ContentType) + len(attachment.Text)
	}
	return uint64(bytes)
}

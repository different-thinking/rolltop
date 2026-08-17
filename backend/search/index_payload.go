// File overview: Bounding of the payload a pending index document retains
// between Bleve commits.

package search

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
// The projection is unchanged by this. Every field is cut to a prefix that
// buildMessageDocument would have cut to itself, so a bounded document indexes
// byte for byte like the unbounded one it replaces.
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

// boundIndexAttachments spends one shared budget per joined field, in the order
// buildMessageDocument joins them. Once a budget is gone the remaining values
// cannot reach the index, because the join stops at the first value it has to
// truncate, so dropping them here changes nothing that gets indexed.
func boundIndexAttachments(attachments []AttachmentDoc) []AttachmentDoc {
	if len(attachments) == 0 {
		return attachments
	}
	bounded := make([]AttachmentDoc, len(attachments))
	copy(bounded, attachments)
	names := maxIndexedNamesBytes
	types := maxIndexedNamesBytes
	texts := maxIndexedAttachmentsBytes
	for i := range bounded {
		bounded[i].Filename, names = spendJoinBudget(bounded[i].Filename, names)
		bounded[i].ContentType, types = spendJoinBudget(bounded[i].ContentType, types)
		bounded[i].Text, texts = spendJoinBudget(bounded[i].Text, texts)
	}
	return bounded
}

// spendJoinBudget keeps at least as much of value as boundedIndexJoin can still
// use and reports the budget left. The join also pays for separators, so its own
// budget runs out no later than this one: keeping a slightly longer prefix is
// safe, keeping a shorter one would silently drop indexed text.
func spendJoinBudget(value string, remaining int) (string, int) {
	if value == "" {
		return value, remaining
	}
	if remaining <= 0 {
		return "", 0
	}
	if len(value) > remaining {
		return boundedIndexText(value, remaining), 0
	}
	return value, remaining - len(value)
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

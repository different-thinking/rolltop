// File overview: The PostgreSQL search backend. The Service keeps its type and
// its consumers; with a store attached (OpenPostgresBackend) every write and
// maintenance call routes here into message_search rows instead of Bleve
// segments, and none of the mmap-era machinery — writer gates, stall watchdog,
// recovery markers, quarantine — is ever entered. The projection reuses the
// exact bounded-text helpers the Bleve document builder uses, so the two
// backends index the same text; only the container differs.

package search

import (
	"context"
	"fmt"
	"strings"

	"rolltop/backend/store"
)

// Postgres-mode text budgets. A tsvector must stay under 1MiB; these caps keep
// the four weighted streams' sum well below it while preserving substantially
// more text than the list preview. A message that still overflows fails its
// batch with a Postgres error and is retried by the repair path — the same
// contract a Bleve write error has today.
const (
	pgMaxSubjectBytes    = 64 * 1024
	pgMaxAddressBytes    = 128 * 1024
	pgMaxBodyBytes       = 512 * 1024
	pgMaxAttachmentBytes = 256 * 1024
)

// OpenPostgresBackend serves search from the given store's message_search
// table. No directory, no per-tenant index files, no writer lifecycle: the
// store's pool is the only resource, and it belongs to the caller.
func OpenPostgresBackend(db *store.Store) *Service {
	service := newSearchService()
	service.pg = db
	return service
}

// PostgresBackend reports whether this service writes to message_search rather
// than Bleve, which is what startup wiring branches on.
func (s *Service) PostgresBackend() bool {
	return s != nil && s.pg != nil
}

// buildMessageSearchTexts projects one message onto the four weighted text
// streams of the tsvector: subject (A), addresses (B), body (C), attachments
// (D). It mirrors buildMessageDocument field for field — same helpers, same
// compound handling, same encryption rule — so a query finds the same message
// under either backend.
func buildMessageSearchTexts(doc MessageIndexDocument) (a, b, c, d string) {
	msg := doc.Message
	attachments := doc.Attachments
	if msg.IsEncrypted {
		attachments = nil
	}
	names := make([]string, 0, len(attachments))
	contentTypes := make([]string, 0, len(attachments))
	texts := make([]string, 0, len(attachments))
	for _, att := range attachments {
		names = append(names, att.Filename)
		contentTypes = append(contentTypes, att.ContentType)
		if strings.TrimSpace(att.Text) != "" {
			texts = append(texts, att.Text)
		}
	}
	bodyForIndex := msg.BodyText
	if msg.IsEncrypted {
		bodyForIndex = ""
	}
	subject := boundedIndexText(msg.Subject, maxIndexedHeaderBytes)
	fromAddr := boundedIndexText(msg.FromAddr, maxIndexedHeaderBytes)
	toAddr := boundedIndexText(msg.ToAddr, maxIndexedHeaderBytes)
	ccAddr := boundedIndexText(msg.CCAddr, maxIndexedHeaderBytes)
	messageID := boundedIndexText(msg.MessageIDHeader, maxIndexedHeaderBytes)

	a = boundedIndexText(joinIndexTexts(subject, compoundSearchText(subject)), pgMaxSubjectBytes)
	b = boundedIndexText(joinIndexTexts(
		fromAddr, compoundSearchText(fromAddr), emailDomainTerms(fromAddr),
		toAddr, ccAddr, messageID,
	), pgMaxAddressBytes)
	c = boundedIndexText(bodyForIndex, pgMaxBodyBytes)
	d = boundedIndexText(joinIndexTexts(
		boundedIndexJoin(names, maxIndexedNamesBytes),
		boundedIndexJoin(contentTypes, maxIndexedNamesBytes),
		boundedIndexJoin(texts, maxIndexedAttachmentsBytes),
	), pgMaxAttachmentBytes)
	return a, b, c, d
}

func joinIndexTexts(values ...string) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		parts = append(parts, value)
	}
	return strings.Join(parts, " ")
}

func (s *Service) pgIndexMessages(ctx context.Context, documents []MessageIndexDocument) error {
	byUser := make(map[int64][]store.MessageSearchDoc)
	order := make([]int64, 0, 1)
	for _, doc := range documents {
		if doc.Message.UserID <= 0 {
			return fmt.Errorf("search document for message %d without a user", doc.Message.ID)
		}
		if doc.Message.ID <= 0 {
			return fmt.Errorf("search document without a message id for user %d", doc.Message.UserID)
		}
		a, b, c, d := buildMessageSearchTexts(doc)
		if _, seen := byUser[doc.Message.UserID]; !seen {
			order = append(order, doc.Message.UserID)
		}
		byUser[doc.Message.UserID] = append(byUser[doc.Message.UserID], store.MessageSearchDoc{
			MessageID: doc.Message.ID,
			UserID:    doc.Message.UserID,
			TextA:     a, TextB: b, TextC: c, TextD: d,
		})
	}
	for _, userID := range order {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.pg.UpsertMessageSearch(ctx, userID, byUser[userID]); err != nil {
			return fmt.Errorf("index messages for user %d: %w", userID, err)
		}
	}
	return nil
}

// pgDeleteMessages mirrors the Bleve batch cadence for the progress callback:
// bounded chunks, the callback told how many rows each chunk removed.
func (s *Service) pgDeleteMessages(ctx context.Context, userID int64, messageIDs []int64, onBatch func(int) error) error {
	const chunkSize = 500
	for start := 0; start < len(messageIDs); start += chunkSize {
		end := min(start+chunkSize, len(messageIDs))
		if err := ctx.Err(); err != nil {
			return err
		}
		deleted, err := s.pg.DeleteMessageSearch(ctx, userID, messageIDs[start:end])
		if err != nil {
			return err
		}
		if onBatch != nil && deleted > 0 {
			if err := onBatch(int(deleted)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) pgPurgeMailbox(ctx context.Context, userID, mailboxID int64, onBatch func(int) error) (int, error) {
	deleted, err := s.pg.PurgeMessageSearchForMailbox(ctx, userID, mailboxID)
	if err != nil {
		return 0, err
	}
	if onBatch != nil && deleted > 0 {
		if err := onBatch(int(deleted)); err != nil {
			return int(deleted), err
		}
	}
	return int(deleted), nil
}

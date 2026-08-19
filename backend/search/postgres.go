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
	"unicode/utf8"

	"rolltop/backend/store"
)

// Postgres-mode text budgets. Per-stream ceilings preserve substantially more
// text than the list preview; pgMaxVectorInputBytes then bounds the four
// together, because PostgreSQL rejects a tsvector over 1MiB outright and that
// error is not recoverable by retrying: the repair path re-projects the same
// message and fails again forever. Measured worst case is a doubling —
// 409KB of short distinct words produced an 819KB vector, while realistic
// prose stayed near 1.2x — so 384KiB of input cannot reach the limit.
//
// Streams are filled in weight order, so a message that hits the budget loses
// attachment text before body text and never loses its subject.
const (
	pgMaxSubjectBytes     = 64 * 1024
	pgMaxAddressBytes     = 128 * 1024
	pgMaxBodyBytes        = 512 * 1024
	pgMaxAttachmentBytes  = 256 * 1024
	pgMaxVectorInputBytes = 384 * 1024
	// pgMaxWordsBytes bounds the fuzzy word list. Distinct words only, so this
	// covers the vocabulary of even a large message comfortably.
	pgMaxWordsBytes = 128 * 1024
	// pgMaxLexemeBytes is PostgreSQL's per-lexeme ceiling. A longer run is
	// dropped with a notice rather than indexed, so runs are split at this
	// width instead — a base64 blob then stays findable in pieces, which is
	// what the Bleve tokenizer did with it.
	pgMaxLexemeBytes = 2000
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

	budget := pgMaxVectorInputBytes
	take := func(text string, limit int) string {
		out := pgIndexText(text, min(limit, budget))
		budget -= len(out)
		return out
	}
	a = take(joinIndexTexts(subject, compoundSearchText(subject)), pgMaxSubjectBytes)
	b = take(joinIndexTexts(
		fromAddr, compoundSearchText(fromAddr), emailDomainTerms(fromAddr),
		toAddr, ccAddr, messageID,
	), pgMaxAddressBytes)
	c = take(bodyForIndex, pgMaxBodyBytes)
	d = take(joinIndexTexts(
		boundedIndexJoin(names, maxIndexedNamesBytes),
		boundedIndexJoin(contentTypes, maxIndexedNamesBytes),
		boundedIndexJoin(texts, maxIndexedAttachmentsBytes),
	), pgMaxAttachmentBytes)
	return a, b, c, d
}

// pgIndexText prepares one stream for to_tsvector. It tokenizes app-side -
// the same normalization the query side runs - rather than leaving it to
// PostgreSQL's parser, which keeps an address, URL, or host as a single
// lexeme: a body mentioning kontakt@firma-beispiel.de would then be
// unreachable by "firma", where the Bleve tokenizer split it into words. One
// tokenizer on both sides is the only way index and query agree.
func pgIndexText(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	normalized := splitOversizedRuns(normalizeSearchText(value), pgMaxLexemeBytes)
	if len(normalized) <= limit {
		return normalized
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(normalized[cut]) {
		cut--
	}
	return strings.TrimSpace(normalized[:cut])
}

// splitOversizedRuns breaks alphanumeric runs longer than limit into pieces at
// rune boundaries. Normalization has already separated words, so a run this
// long is a hash or an encoded blob rather than prose.
func splitOversizedRuns(value string, limit int) string {
	var out strings.Builder
	run := 0
	for offset, r := range value {
		if r == ' ' {
			run = 0
			out.WriteRune(r)
			continue
		}
		size := utf8.RuneLen(r)
		if run+size > limit {
			out.WriteByte(' ')
			run = 0
		}
		run += size
		out.WriteString(value[offset : offset+size])
	}
	return out.String()
}

// messageSearchWords renders the distinct normalized words of the four
// streams, the haystack pg_trgm probes for fuzzy matching. Order follows first
// appearance so truncation drops the tail of the attachment text, not the
// subject.
func messageSearchWords(streams ...string) string {
	seen := map[string]bool{}
	var b strings.Builder
	for _, stream := range streams {
		for _, word := range strings.Fields(normalizeSearchText(stream)) {
			if seen[word] {
				continue
			}
			if b.Len()+len(word)+1 > pgMaxWordsBytes {
				return b.String()
			}
			seen[word] = true
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(word)
		}
	}
	return b.String()
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
			Words: messageSearchWords(a, b, c, d),
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

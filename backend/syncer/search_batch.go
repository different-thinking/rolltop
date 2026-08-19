// File overview: Batched search-index writes for fetched and repaired message documents.

package syncer

import (
	"context"
	"errors"

	"rolltop/backend/search"
	"rolltop/backend/store"
)

const (
	fetchedSearchIndexBatchSize     = 25
	maintenanceSearchCheckpointSize = 5
	// Explicit rebuilds use the same 25-document Bleve cadence as normal import.
	// Larger batches reduced commit count but performed worse on real mailboxes,
	// where document projection and Bleve segment merges were less responsive.
	explicitSearchRepairBatchSize = 25
	// fetchedSearchIndexBatchBytes caps what a batch may hold while it waits for
	// its commit. The document count alone does not bound memory: a folder with
	// large messages fills twenty-five documents with tens of megabytes of body
	// and attachment text, and a first sync that keeps hitting that is what makes
	// the process grow until it is killed mid-write. Documents are bounded to the
	// indexable limits on the way in, so this is a real ceiling and not a guess.
	fetchedSearchIndexBatchBytes uint64 = 8 * 1024 * 1024
)

// pendingFetchedSearchIndex carries a Bleve document plus the metadata flag
// that can only be marked after the document has been committed successfully.
type pendingFetchedSearchIndex struct {
	Document              search.MessageIndexDocument
	HasVisibleAttachments bool
	// KeepPending commits a fallback document without claiming that raw-body
	// enrichment completed. The attachment worker can retry the message later.
	KeepPending bool
}

// fetchedSearchIndexBatch amortizes Bleve commit cost during IMAP fetches and
// repair indexing. SQLite/blob writes still happen one message at a time, but
// search documents are flushed in small groups; if a flush fails,
// attachment_indexed_at remains unset so the normal repair path can retry safely.
type fetchedSearchIndexBatch struct {
	service  *Service
	maxItems int
	maxBytes uint64
	items    []pendingFetchedSearchIndex
	bytes    uint64
}

type messageImportCompletionBatch struct {
	service    *Service
	userID     int64
	messageIDs []int64
}

func newFetchedSearchIndexBatch(service *Service) *fetchedSearchIndexBatch {
	return newFetchedSearchIndexBatchWithSize(service, fetchedSearchIndexBatchSize)
}

func newFetchedSearchIndexBatchWithSize(service *Service, maxItems int) *fetchedSearchIndexBatch {
	if maxItems <= 0 {
		maxItems = fetchedSearchIndexBatchSize
	}
	// Every caller shares one payload budget: the smaller checkpoint sizes exist
	// to bound how much work a cancelled maintenance pass discards, which is a
	// different question from how much memory a batch may hold.
	return &fetchedSearchIndexBatch{service: service, maxItems: maxItems, maxBytes: fetchedSearchIndexBatchBytes}
}

func newMessageImportCompletionBatch(service *Service, userID int64) *messageImportCompletionBatch {
	return &messageImportCompletionBatch{service: service, userID: userID}
}

func (b *messageImportCompletionBatch) Add(messageID int64) {
	if b == nil || messageID <= 0 {
		return
	}
	b.messageIDs = append(b.messageIDs, messageID)
}

func (b *messageImportCompletionBatch) Empty() bool {
	return b == nil || len(b.messageIDs) == 0
}

func (b *messageImportCompletionBatch) Flush(ctx context.Context) error {
	if b.Empty() {
		return nil
	}
	if err := b.service.Store.MarkMessagesImportCompleted(ctx, b.userID, b.messageIDs); err != nil {
		return err
	}
	b.messageIDs = b.messageIDs[:0]
	return nil
}

// Add queues one prepared message and flushes when the batch reaches its
// document count or its retained payload budget, whichever comes first. Nil
// entries represent mailboxes that are not search-visible.
//
// The document is trimmed to the indexable limits before it is queued. Nothing
// that could reach Bleve is discarded by that; what is discarded is the part of
// a large body or attachment extraction that the projection would drop at commit
// time anyway, after the batch had already carried it.
func (b *fetchedSearchIndexBatch) Add(ctx context.Context, item *pendingFetchedSearchIndex) error {
	if item == nil {
		return nil
	}
	queued := *item
	bounded, bytes := search.BoundIndexDocument(queued.Document)
	queued.Document = bounded
	b.items = append(b.items, queued)
	b.bytes += bytes
	if len(b.items) < b.maxItems && b.bytes < b.maxBytes {
		return nil
	}
	return b.Flush(ctx)
}

func (b *fetchedSearchIndexBatch) Empty() bool {
	return b == nil || len(b.items) == 0
}

// Flush commits all pending search documents, then marks their attachment text
// extraction as complete. The mark intentionally happens after the Bleve batch
// so interrupted syncs leave rows eligible for reindex repair.
//
// That ordering is also what lets a failed commit be survivable. Search is a
// derived view of mail that is already stored, so a batch that cannot be
// written is dropped and its rows stay pending for the attachment-index worker
// to pick up. Returning the error instead would abort the mailbox, which is how
// an unreadable index or a full disk used to stop mail arriving entirely -
// the index is rebuildable, the sync window is not.
func (b *fetchedSearchIndexBatch) Flush(ctx context.Context) error {
	if len(b.items) == 0 {
		return nil
	}
	documents := make([]search.MessageIndexDocument, 0, len(b.items))
	for _, item := range b.items {
		documents = append(documents, item.Document)
	}
	if b.service.Search != nil {
		generationRecoveryPhase(ctx, "search-index-batch", "bleve")
		if err := b.service.Search.IndexMessages(ctx, documents); err != nil {
			if !searchIndexFailureIsSurvivable(ctx, err) {
				return err
			}
			b.service.reportDroppedSearchIndexBatch(documents, err)
			b.reset()
			return nil
		}
	}
	updatesByUser := map[int64][]store.MessageAttachmentIndexUpdate{}
	for _, item := range b.items {
		message := item.Document.Message
		if item.KeepPending {
			if _, err := b.service.Store.GetMessageForUser(ctx, message.UserID, message.ID); err != nil {
				if store.IsNotFound(err) && b.service.Search != nil {
					if deleteErr := b.service.Search.DeleteMessage(ctx, message.UserID, message.ID); deleteErr != nil {
						return deleteErr
					}
					continue
				}
				return err
			}
			continue
		}
		updatesByUser[message.UserID] = append(updatesByUser[message.UserID], store.MessageAttachmentIndexUpdate{
			MessageID: message.ID, HasAttachments: item.HasVisibleAttachments,
		})
	}
	for userID, updates := range updatesByUser {
		generationRecoveryPhase(ctx, "sqlite-mark-search-indexed", "batch")
		if err := b.service.Store.MarkMessagesAttachmentIndexed(ctx, userID, updates); err == nil {
			continue
		} else if !store.IsNotFound(err) {
			return err
		}
		// A move can remove one row while this batch is waiting for Bleve. Fall
		// back to the old per-row handling only for that unusual race so the
		// normal rebuild path retains its single SQLite transaction per batch.
		for _, item := range b.items {
			message := item.Document.Message
			if item.KeepPending || message.UserID != userID {
				continue
			}
			if err := b.service.Store.MarkMessageAttachmentIndexed(ctx, message.UserID, message.ID, item.HasVisibleAttachments); err != nil {
				if store.IsNotFound(err) && b.service.Search != nil {
					if deleteErr := b.service.Search.DeleteMessage(ctx, message.UserID, message.ID); deleteErr != nil {
						return deleteErr
					}
					continue
				}
				return err
			}
		}
	}
	b.reset()
	return nil
}

// reset releases the payload instead of parking it in the backing array until
// the next twenty-five messages overwrite it entry by entry.
func (b *fetchedSearchIndexBatch) reset() {
	clear(b.items)
	b.items = b.items[:0]
	b.bytes = 0
}

// searchIndexFailureIsSurvivable separates "this batch could not be indexed"
// from "stop what you are doing". A cancelled context is the sync turn's own
// budget or a shutdown, and a closing service is the process going away: in
// both cases the work belongs to the next run, and swallowing them here would
// turn a stop signal into a silent loss of the pending rows the caller was
// about to checkpoint.
func searchIndexFailureIsSurvivable(ctx context.Context, err error) bool {
	if err == nil {
		return true
	}
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return !search.IsServiceClosingError(err)
}

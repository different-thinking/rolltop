// File overview: Category classification for stored mail. Newly fetched
// messages are classified while their headers are still parsed; this is the
// catch-up pass for everything that was already in SQLite when categories
// arrived, and for messages whose headers could not be read at fetch time.

package syncer

import (
	"context"

	"rolltop/backend/mailparse"
	"rolltop/backend/store"
)

// ClassifyPendingCategoriesForUser gives a category to messages that do not have
// one yet, reading the stored raw message for the list and automation headers.
// It returns how many rows it filed so the caller knows whether to come back.
//
// Every candidate is always filed, even when its raw message is gone or
// unreadable: an unclassifiable row that stayed pending would be selected again
// on every pass and the backfill would never finish. Those rows fall back to
// what the sender address alone says, and a later correction can still move them.
func (s *Service) ClassifyPendingCategoriesForUser(ctx context.Context, userID int64, limit int) (int, error) {
	if s == nil || s.Store == nil || userID <= 0 {
		return 0, nil
	}
	if limit <= 0 {
		limit = store.CategoryBackfillLimit
	}
	candidates, err := s.Store.ListMessagesNeedingCategory(ctx, userID, limit)
	if err != nil || len(candidates) == 0 {
		return 0, err
	}
	updates := make([]store.MessageCategoryUpdate, 0, len(candidates))
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			break
		}
		updates = append(updates, store.MessageCategoryUpdate{
			MessageID: candidate.ID,
			FromAddr:  candidate.FromAddr,
			Category:  s.categorizeStoredMessage(userID, candidate),
		})
	}
	if len(updates) == 0 {
		return 0, ctx.Err()
	}
	if err := s.Store.SetMessageCategories(ctx, userID, updates); err != nil {
		return 0, err
	}
	return len(updates), nil
}

// categorizeStoredMessage decides one candidate from its stored message, and
// decides what to do when that message is not there to be read.
//
// The two kinds of candidate part company here. A message that has never been
// classified must leave the pass with something, or it is selected forever, so
// it falls back to what its address alone says. A message an older generation
// filed already has an answer that was made with the headers in hand, and every
// fallback below knows less than that: it keeps what it has and is only stamped
// as seen. Otherwise a mailbox whose raw messages have aged out of blob
// retention would empty its category lists into the default one.
func (s *Service) categorizeStoredMessage(userID int64, candidate store.CategoryCandidate) string {
	if s.Blobs == nil || candidate.BlobPath == "" {
		return keptOrGuessedCategory(candidate)
	}
	f, err := s.Blobs.OpenUserBlob(userID, candidate.BlobPath)
	if err != nil {
		return keptOrGuessedCategory(candidate)
	}
	defer f.Close()
	category, complete := mailparse.CategorizeReaderScan(f, candidate.FromAddr)
	// A scan that stopped at its budget may not have reached the attachment
	// that names the message, while the parse that filed it read all of it.
	if !complete && candidate.Category != "" {
		return candidate.Category
	}
	return category
}

// keptOrGuessedCategory is the answer for a message that could not be read: the
// one it already has, or the sender address for a message that has none.
func keptOrGuessedCategory(candidate store.CategoryCandidate) string {
	if candidate.Category != "" {
		return candidate.Category
	}
	return mailparse.CategorizeAddress(candidate.FromAddr)
}

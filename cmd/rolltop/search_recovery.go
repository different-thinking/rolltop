package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"rolltop/backend/search"
	"rolltop/backend/store"
)

// recoverMarkedSearchIndexes consumes durable writer-stall markers before any
// per-user Bleve handle is opened. SQLite completion flags are reset first, so
// an interrupted recovery can never leave a fresh index with rows marked done.
func recoverMarkedSearchIndexes(ctx context.Context, db *store.Store, searchSvc *search.Service, searchRoot string, users []store.User, now time.Time) ([]int64, error) {
	if db == nil || searchSvc == nil {
		return nil, fmt.Errorf("stalled search recovery is not configured")
	}
	recovered := make([]int64, 0)
	for _, user := range users {
		recovery, err := searchSvc.SearchIndexRecoveryPlan(user.ID)
		if err != nil {
			return recovered, fmt.Errorf("inspect search recovery marker for user %d: %w", user.ID, err)
		}
		if !recovery.Required {
			continue
		}
		// Keeping the index is only safe if it still opens. An index that does
		// not is the one case a range cannot repair, and it falls through to the
		// rebuild below rather than leaving the tenant without search.
		if recovery.Targeted() {
			if openErr := searchSvc.VerifyPerUserIndexOpens(user.ID); openErr != nil {
				log.Printf("stalled search index user_id=%d does not open, rebuilding instead of repairing %s: %v",
					user.ID, recovery.Scope(), openErr)
			} else {
				repaired, err := repairMarkedSearchIndexRange(ctx, db, searchSvc, user.ID, recovery)
				if err != nil {
					return recovered, err
				}
				recovered = append(recovered, repaired)
				continue
			}
		}
		marked, err := db.MarkSearchVisibleMessagesPendingIndex(ctx, user.ID)
		if err != nil {
			return recovered, fmt.Errorf("mark search rows pending for user %d: %w", user.ID, err)
		}
		quarantine, err := search.QuarantinePerUserIndex(searchRoot, user.ID, now)
		if err != nil {
			return recovered, fmt.Errorf("quarantine stalled search index for user %d after marking %d rows pending: %w", user.ID, marked, err)
		}
		if err := searchSvc.ClearSearchIndexRecoveryRequired(user.ID); err != nil {
			return recovered, fmt.Errorf("clear search recovery marker for user %d after quarantine: %w", user.ID, err)
		}
		recovered = append(recovered, user.ID)
		log.Printf("recovered stalled search index user_id=%d pending_messages=%d quarantine=%q", user.ID, marked, quarantine.QuarantinePath)
	}
	return recovered, nil
}

// repairMarkedSearchIndexRange consumes a marker that names the messages an
// abandoned Bleve batch owned. Only those rows are queued for reindexing and
// the index stays where it is, because a writer that outran its stall threshold
// is a report about how long a commit took, not about the index it was writing
// to. Rebuilding the whole index for that costs hours of reindexing, and the
// load it adds is the same load that produced the slow commit.
func repairMarkedSearchIndexRange(ctx context.Context, db *store.Store, searchSvc *search.Service, userID int64, recovery search.SearchIndexRecovery) (int64, error) {
	marked, err := db.MarkSearchMessagesPendingIndexRange(ctx, userID, recovery.FirstDocumentID, recovery.LastDocumentID)
	if err != nil {
		return 0, fmt.Errorf("mark search rows %d-%d pending for user %d: %w",
			recovery.FirstDocumentID, recovery.LastDocumentID, userID, err)
	}
	// The marker is cleared only after the pending write is durable, so a crash
	// in between repeats a repair that is idempotent rather than skipping one.
	if err := searchSvc.ClearSearchIndexRecoveryRequired(userID); err != nil {
		return 0, fmt.Errorf("clear search recovery marker for user %d after marking %d rows pending: %w", userID, marked, err)
	}
	log.Printf("repaired stalled search index user_id=%d pending_messages=%d first_document_id=%d last_document_id=%d index_retained=true",
		userID, marked, recovery.FirstDocumentID, recovery.LastDocumentID)
	return userID, nil
}

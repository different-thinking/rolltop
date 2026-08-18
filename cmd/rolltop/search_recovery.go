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
		// Keeping the index is only safe when the repair can be carried out in
		// full. A range too wide to sweep for deleted messages, and an index that
		// no longer opens, both fall through to the rebuild below rather than
		// clearing the marker on a repair that was only partly done.
		if recovery.Targeted() && !repairableInPlace(recovery) {
			log.Printf("stalled search index user_id=%d spans %d ids over the %d the in-place repair sweeps, rebuilding instead of repairing %s",
				user.ID, recoveryWidth(recovery), maxTargetedRepairWidth, recovery.Scope())
		} else if recovery.Targeted() {
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
	removed, err := deleteVanishedDocumentsInRange(ctx, db, searchSvc, userID, recovery)
	if err != nil {
		return 0, err
	}
	// The marker is cleared only after the pending write is durable, so a crash
	// in between repeats a repair that is idempotent rather than skipping one.
	if err := searchSvc.ClearSearchIndexRecoveryRequired(userID); err != nil {
		return 0, fmt.Errorf("clear search recovery marker for user %d after marking %d rows pending: %w", userID, marked, err)
	}
	log.Printf("repaired stalled search index user_id=%d pending_messages=%d deleted_documents=%d first_document_id=%d last_document_id=%d index_retained=true",
		userID, marked, removed, recovery.FirstDocumentID, recovery.LastDocumentID)
	return userID, nil
}

// maxTargetedRepairWidth bounds the row-ID window an in-place repair covers. A
// marker's range comes from one batch and is narrow in practice; a pathological
// one is rebuilt rather than turned into a scan of the whole ID space.
//
// The bound decides whether the repair happens at all, not how much of it
// happens. Sweeping only part of a range and then clearing the marker would
// leave a message that was deleted during the stall searchable for good, with
// nothing left to notice.
const maxTargetedRepairWidth = 10000

func recoveryWidth(recovery search.SearchIndexRecovery) int64 {
	return recovery.LastDocumentID - recovery.FirstDocumentID + 1
}

// repairableInPlace reports whether this range is narrow enough to repair
// without discarding the index.
func repairableInPlace(recovery search.SearchIndexRecovery) bool {
	return recoveryWidth(recovery) <= maxTargetedRepairWidth
}

// deleteVanishedDocumentsInRange removes index documents whose messages no longer
// exist. Marking rows pending only repairs documents SQLite can still describe;
// a message deleted while the stalled batch held the writer gate never got its
// Bleve delete, and the retained index would keep serving it. The full rebuild
// this replaces discarded such documents by discarding everything.
func deleteVanishedDocumentsInRange(ctx context.Context, db *store.Store, searchSvc *search.Service, userID int64, recovery search.SearchIndexRecovery) (int, error) {
	if !repairableInPlace(recovery) {
		// Unreachable: recoverMarkedSearchIndexes rebuilds such a range instead.
		// Refusing here keeps the guarantee local to the function that has to
		// hold it, rather than resting on its one caller.
		return 0, fmt.Errorf("range %d-%d is too wide to sweep for deleted messages",
			recovery.FirstDocumentID, recovery.LastDocumentID)
	}
	surviving, err := db.MessageIDsInRange(ctx, userID, recovery.FirstDocumentID, recovery.LastDocumentID)
	if err != nil {
		return 0, fmt.Errorf("list surviving messages %d-%d for user %d: %w",
			recovery.FirstDocumentID, recovery.LastDocumentID, userID, err)
	}
	present := make(map[int64]struct{}, len(surviving))
	for _, id := range surviving {
		present[id] = struct{}{}
	}
	vanished := make([]int64, 0)
	for id := recovery.FirstDocumentID; id <= recovery.LastDocumentID; id++ {
		if _, ok := present[id]; !ok {
			vanished = append(vanished, id)
		}
	}
	if len(vanished) == 0 {
		return 0, nil
	}
	// Most of these IDs were never indexed - the range is inclusive and message
	// IDs within it are sparse. Deleting a document that is not there is a no-op,
	// which is what makes the sweep safe to run over the whole range.
	if err := searchSvc.DeleteMessages(ctx, userID, vanished); err != nil {
		return 0, fmt.Errorf("delete vanished search documents %d-%d for user %d: %w",
			recovery.FirstDocumentID, recovery.LastDocumentID, userID, err)
	}
	return len(vanished), nil
}

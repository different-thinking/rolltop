// File overview: Recording search documents a failed index write dropped.

package syncer

import (
	"context"
	"log"
	"time"

	"rolltop/backend/search"
)

// droppedSearchIndexLogInterval bounds how often a persistently failing index is
// reported per tenant. A full disk or an unreadable index fails every batch, and
// one line per twenty-five messages turns a first sync into hundreds of
// thousands of identical lines - which buries the one line an operator needs.
const droppedSearchIndexLogInterval = time.Minute

// droppedSearchIndexTally is one tenant's running count of dropped documents.
// Counting per tenant is what keeps the log honest: a shared counter reports
// everyone's losses under whichever user_id happened to trip the interval.
type droppedSearchIndexTally struct {
	lastLog   time.Time
	batches   int
	documents int
	// unmarked counts the folders whose repair mark itself failed, carried in
	// the same tally so a database outage cannot outrun the throttle.
	unmarked      int
	unmarkedCause error
}

// reportDroppedSearchIndexBatch records that a batch of search documents could
// not be written.
//
// The messages themselves are stored; what is lost is their place in the index.
// Nothing retries that on its own - the attachment worker's pending flag is not
// a reindex queue, and in the default configuration it clears those rows
// without indexing them - so the folders involved are marked as coverage that
// nothing has verified. That is the state an explicit rebuild acts on, and the
// state the folder list and the admin page already show.
func (s *Service) reportDroppedSearchIndexBatch(ctx context.Context, documents []search.MessageIndexDocument, cause error) {
	if s == nil || len(documents) == 0 {
		return
	}
	byUser := map[int64]int{}
	mailboxes := map[int64]map[int64]struct{}{}
	for _, document := range documents {
		message := document.Message
		byUser[message.UserID]++
		if mailboxes[message.UserID] == nil {
			mailboxes[message.UserID] = map[int64]struct{}{}
		}
		mailboxes[message.UserID][message.MailboxID] = struct{}{}
	}
	for userID, dropped := range byUser {
		marks := droppedSearchIndexMarks{}
		for mailboxID := range mailboxes[userID] {
			// Recorded on a context of its own: the caller's may be the one
			// that just failed, and losing the mark would leave a folder
			// reporting coverage it does not have.
			markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), droppedSearchIndexMarkTimeout)
			err := s.Store.MarkMailboxSearchIndexRepairRequired(markCtx, userID, mailboxID)
			cancel()
			if err != nil {
				marks.failed++
				if marks.cause == nil {
					marks.cause = err
				}
			}
		}
		// Reported through the same throttle as the write failure itself. A
		// database that is down fails both, once per folder per batch, and a
		// second unthrottled line would bury the first at exactly the moment
		// the first is the one worth reading.
		_ = s.logDroppedSearchIndexBatch(userID, dropped, len(mailboxes[userID]), marks, cause)
	}
}

// droppedSearchIndexMarks is what recording the folders for rebuild cost: how
// many marks failed and the first reason, both reported with the drop itself.
type droppedSearchIndexMarks struct {
	failed int
	cause  error
}

// droppedSearchIndexMarkTimeout bounds the one UPDATE per affected folder.
const droppedSearchIndexMarkTimeout = 15 * time.Second

// logDroppedSearchIndexBatch reports one tenant's losses, at most once per
// interval. It returns whether this call reported, which is what makes the
// throttle testable without reading the process-wide log: other goroutines in
// this package log while a test runs, so counting lines there measures them
// too.
func (s *Service) logDroppedSearchIndexBatch(userID int64, dropped, folders int, marks droppedSearchIndexMarks, cause error) bool {
	s.droppedSearchIndexMu.Lock()
	if s.droppedSearchIndex == nil {
		s.droppedSearchIndex = map[int64]*droppedSearchIndexTally{}
	}
	tally := s.droppedSearchIndex[userID]
	if tally == nil {
		tally = &droppedSearchIndexTally{}
		s.droppedSearchIndex[userID] = tally
	}
	tally.documents += dropped
	tally.batches++
	tally.unmarked += marks.failed
	if tally.unmarkedCause == nil {
		tally.unmarkedCause = marks.cause
	}
	now := time.Now()
	if !tally.lastLog.IsZero() && now.Sub(tally.lastLog) < droppedSearchIndexLogInterval {
		s.droppedSearchIndexMu.Unlock()
		return false
	}
	tally.lastLog = now
	batches := tally.batches
	documents := tally.documents
	unmarked := tally.unmarked
	unmarkedCause := tally.unmarkedCause
	tally.batches = 0
	tally.documents = 0
	tally.unmarked = 0
	tally.unmarkedCause = nil
	s.droppedSearchIndexMu.Unlock()

	// The cause is logged by type and value, never the documents: this runs on
	// message content, and none of it may reach the log.
	log.Printf("search index write failed, mail stored and folders marked for rebuild user_id=%d batches=%d documents=%d folders=%d error_type=%T error=%v",
		userID, batches, documents, folders, cause, cause)
	if unmarked > 0 {
		// A folder nothing could mark is the worse half of this failure: it
		// still reports verified coverage it no longer has, and no rebuild will
		// be offered for it until something marks or rebuilds it.
		log.Printf("could not mark folders for full-text repair user_id=%d folders=%d error_type=%T error=%v",
			userID, unmarked, unmarkedCause, unmarkedCause)
	}
	return true
}

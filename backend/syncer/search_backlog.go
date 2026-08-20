// File overview: The sweep that finds mail which is in no queue and in no index.
//
// A message is normally indexed as it is fetched, and anything left over waits
// in the attachment-index queue. Both of those can be lost at once: a stalled
// index is quarantined and its rows queued for reindexing, a generation rebuild
// defers documents to that same queue, and any bug or interruption that empties
// the queue without writing the documents leaves rows that are marked done and
// are in no index. Nothing notices - search simply answers from less mail than
// the mailbox holds, and the shortfall on the storage page is the only sign.
//
// This sweep is what notices. It compares what the index holds against what the
// tenant's search-visible folders hold, and when the index is short it walks the
// rows in id order and puts the missing ones back in the queue, where the
// ordinary worker publishes them from stored data. It never fetches anything
// remotely: an explicit rebuild is still the only thing that re-downloads bodies
// this install no longer caches.

package syncer

import (
	"context"
	"log"
	"time"
)

// searchBacklogSettleFor is how long a tenant whose index is not short is left
// alone. The gate below is two counts, which are cheap but not free, and the
// worker's turns come back-to-back while it drains.
const searchBacklogSettleFor = 15 * time.Minute

// requeueMissingSearchDocuments marks one page of unindexed search-visible mail
// as pending, so the ordinary attachment-index turn publishes it. It returns how
// many rows it queued.
//
// The count comparison in front of the walk is a gate and not a proof: an index
// holding documents for deleted messages could match the total while still
// missing live ones. That case is what the explicit rebuild is for. What the
// gate does guarantee is that a tenant whose index is complete pays two counts
// every fifteen minutes rather than a scan of their mailbox on every turn.
//
// Both counts describe the same population - mail that should have a document
// now - so a purged folder is outside both. Counting its mail on one side only
// would leave a shortfall the walk cannot close, and this would never settle.
func (s *Service) requeueMissingSearchDocuments(ctx context.Context, userID int64, limit int) (int, error) {
	if s.Search == nil || s.Store == nil || userID <= 0 {
		return 0, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if s.searchBacklogSettledFor(userID, time.Now()) {
		return 0, nil
	}
	indexed, err := s.Search.CountUserMessages(ctx, userID)
	if err != nil {
		return 0, err
	}
	searchable, err := s.Store.CountIndexableMessagesForUser(ctx, userID)
	if err != nil {
		return 0, err
	}
	if indexed >= searchable {
		// Nothing to look for. Reset the cursor as well: the next shortfall is a
		// new question and deserves a walk from the beginning rather than a
		// resume from wherever the last one stopped.
		s.settleSearchBacklog(userID, time.Now())
		return 0, nil
	}
	cursor := s.searchBacklogCursorFor(userID)
	ids, err := s.Store.ListSearchVisibleMessageIDsAfter(ctx, userID, cursor, limit)
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		// A complete cycle that did not close the gap. Start over - rows below
		// the cursor may have lost their documents while this pass was above
		// them, and the counts still say mail is missing - but not immediately:
		// a shortfall no walk can reach, an index counting documents for mail
		// that is gone, would otherwise cost two counts and a page on every
		// turn for as long as it lasts. Settling puts the next cycle behind the
		// same interval a complete index gets.
		s.settleSearchBacklog(userID, time.Now())
		return 0, nil
	}
	s.advanceSearchBacklogCursor(userID, ids[len(ids)-1])
	present, err := s.Search.MessageIDsIndexed(ctx, userID, ids)
	if err != nil {
		return 0, err
	}
	missing := make([]int64, 0, len(ids))
	for _, id := range ids {
		if !present[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return 0, nil
	}
	queued, err := s.Store.MarkMessagesAttachmentIndexPending(ctx, userID, missing)
	if err != nil {
		return 0, err
	}
	if queued > 0 {
		log.Printf("search backlog requeued user_id=%d messages=%d indexed=%d searchable=%d after_message_id=%d",
			userID, queued, indexed, searchable, cursor)
	}
	return int(queued), nil
}

func (s *Service) searchBacklogSettledFor(userID int64, now time.Time) bool {
	s.searchBacklogMu.Lock()
	defer s.searchBacklogMu.Unlock()
	settledAt, ok := s.searchBacklogSettled[userID]
	if !ok {
		return false
	}
	interval := s.searchBacklogInterval
	if interval <= 0 {
		interval = searchBacklogSettleFor
	}
	return now.Sub(settledAt) < interval
}

func (s *Service) settleSearchBacklog(userID int64, now time.Time) {
	s.searchBacklogMu.Lock()
	defer s.searchBacklogMu.Unlock()
	if s.searchBacklogSettled == nil {
		s.searchBacklogSettled = make(map[int64]time.Time)
	}
	s.searchBacklogSettled[userID] = now
	delete(s.searchBacklogCursor, userID)
}

func (s *Service) searchBacklogCursorFor(userID int64) int64 {
	s.searchBacklogMu.Lock()
	defer s.searchBacklogMu.Unlock()
	return s.searchBacklogCursor[userID]
}

func (s *Service) advanceSearchBacklogCursor(userID, messageID int64) {
	s.searchBacklogMu.Lock()
	defer s.searchBacklogMu.Unlock()
	if messageID <= 0 {
		delete(s.searchBacklogCursor, userID)
		return
	}
	if s.searchBacklogCursor == nil {
		s.searchBacklogCursor = make(map[int64]int64)
	}
	s.searchBacklogCursor[userID] = messageID
}

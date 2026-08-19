// File overview: Reporting search documents a failed index write dropped.

package syncer

import (
	"log"
	"time"

	"rolltop/backend/search"
)

// droppedSearchIndexLogInterval bounds how often a persistently failing index is
// reported. A full disk or an unreadable index fails every batch, and one line
// per twenty-five messages turns a first sync into hundreds of thousands of
// identical lines - which buries the one line an operator needs to see.
const droppedSearchIndexLogInterval = time.Minute

// reportDroppedSearchIndexBatch records that a batch of search documents could
// not be written. The messages themselves are stored; only their search rows
// stay pending, and the attachment-index worker retries them.
func (s *Service) reportDroppedSearchIndexBatch(documents []search.MessageIndexDocument, cause error) {
	if s == nil || len(documents) == 0 {
		return
	}
	userID := documents[0].Message.UserID
	s.droppedSearchIndexMu.Lock()
	s.droppedSearchIndexDocuments += len(documents)
	s.droppedSearchIndexBatches++
	now := time.Now()
	if !s.droppedSearchIndexLastLog.IsZero() && now.Sub(s.droppedSearchIndexLastLog) < droppedSearchIndexLogInterval {
		s.droppedSearchIndexMu.Unlock()
		return
	}
	s.droppedSearchIndexLastLog = now
	batches := s.droppedSearchIndexBatches
	dropped := s.droppedSearchIndexDocuments
	s.droppedSearchIndexBatches = 0
	s.droppedSearchIndexDocuments = 0
	s.droppedSearchIndexMu.Unlock()

	// The cause is logged by type and value, never the documents: this runs on
	// message content, and none of it may reach the log.
	log.Printf("search index write failed, mail stored and reindex pending user_id=%d batches=%d documents=%d error_type=%T error=%v",
		userID, batches, dropped, cause, cause)
}

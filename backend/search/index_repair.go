// File overview: Recovering a per-user Bleve index that no longer opens.
//
// A live index is a directory of scorch segments plus a bbolt root file. When
// that root file is truncated or half-written - a data volume restored from an
// incomplete copy, a container killed mid-commit - every open of it fails with
// the same error forever, and because indexing runs inside the same call that
// stores a fetched message, the whole mailbox stops syncing over a search index
// nobody asked about. Recovering the index automatically is what keeps a
// derived, rebuildable artefact from holding the mail hostage.

package search

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/blevesearch/bleve/v2"
	bolt "go.etcd.io/bbolt"
)

// CorruptIndexHandler is told which tenant's index is about to be moved aside.
// It runs before the quarantine so the rows it marks for reindexing are marked
// while the old index is still in place: the reverse order leaves a fresh empty
// index behind if the process dies in between, with every row still flagged as
// indexed and nothing left to rebuild them.
//
// Returning an error abandons the repair and the original open error reaches
// the caller, because an index nothing will refill is worse than one that does
// not open - the second is visibly broken, the first silently answers every
// search with nothing.
type CorruptIndexHandler func(userID int64) error

// SetCorruptIndexHandler installs the callback used when a tenant's index turns
// out to be unopenable. Without one, a corrupt index is reported and left alone.
func (s *Service) SetCorruptIndexHandler(handler CorruptIndexHandler) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.corruptIndexHandler = handler
	s.mu.Unlock()
}

// IsIndexCorruptionError reports whether err means this index cannot be opened
// again, as opposed to a condition that may pass.
//
// The distinction decides whether an index is thrown away, so it is drawn
// narrowly: only errors that name a damaged file qualify. Everything else -
// a lock another process holds, a full disk, too many open files - is a
// transient failure whose index is fine, and rebuilding on one of those would
// answer a five-second problem by reindexing a mailbox for hours.
//
// bbolt's errors arrive unwrapped: scorch returns what bleve's OpenBolt helper
// returned, which is what bolt.Open returned, so errors.Is reaches them through
// the one wrap openIndex adds.
func IsIndexCorruptionError(err error) bool {
	if err == nil {
		return false
	}
	for _, target := range []error{
		bolt.ErrInvalid,         // "invalid database" - the root file is not a bolt database
		bolt.ErrVersionMismatch, // a bolt file from another architecture or a torn header
		bolt.ErrChecksum,        // the meta page does not match its checksum
		bleve.ErrorIndexMetaMissing,
		bleve.ErrorIndexMetaCorrupt,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// repairUnopenableIndex moves a damaged index aside and returns a fresh empty
// one in its place. The caller holds no lock; this takes the repair gate, which
// every open of a per-user index goes through, so nothing can cache a handle on
// the directory between the drop and the rename.
func (s *Service) repairUnopenableIndex(userID int64, path string, cause error) (bleve.Index, error) {
	s.repairMu.Lock()
	defer s.repairMu.Unlock()

	// Another goroutine may have finished this exact repair while this one
	// waited for the gate, so the cache decides before the filesystem does.
	s.mu.Lock()
	cached := s.indexes[userID]
	handler := s.corruptIndexHandler
	s.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	if handler == nil {
		return nil, cause
	}
	if err := handler(userID); err != nil {
		return nil, errors.Join(cause, fmt.Errorf("schedule reindex for user %d before quarantine: %w", userID, err))
	}
	quarantine, err := QuarantinePerUserIndex(s.root, userID, time.Now())
	if err != nil {
		return nil, errors.Join(cause, err)
	}
	index, err := openIndex(path)
	if err != nil {
		return nil, errors.Join(cause, fmt.Errorf("open replacement search index for user %d: %w", userID, err))
	}
	s.logf()("search index quarantined and rebuilt user_id=%d quarantine=%q cause_type=%T cause=%v",
		userID, quarantine.QuarantinePath, cause, cause)
	return index, nil
}

// RebuildPerUserIndex moves one tenant's live index aside while the server is
// running and leaves the next indexing write to create an empty one.
//
// The caller marks the tenant's rows for reindexing first, for the reason
// CorruptIndexHandler describes. Writes already holding a handle when this runs
// land in the quarantined directory and are lost, which costs nothing: every
// row is queued for reindexing anyway.
func (s *Service) RebuildPerUserIndex(ctx context.Context, userID int64) (IndexQuarantine, error) {
	if s == nil || !s.perUser {
		return IndexQuarantine{}, errors.New("per-user search indexes are not configured")
	}
	if userID <= 0 {
		return IndexQuarantine{}, errors.New("user id must be positive")
	}
	s.repairMu.Lock()
	defer s.repairMu.Unlock()
	// DropUser takes the tenant's writer gate, so an in-flight batch commit
	// finishes before the directory moves.
	if err := s.DropUser(ctx, userID); err != nil {
		return IndexQuarantine{}, err
	}
	return QuarantinePerUserIndex(s.root, userID, time.Now())
}

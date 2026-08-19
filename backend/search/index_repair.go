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
	"errors"
	"fmt"
	"time"

	"github.com/blevesearch/bleve/v2"
	bbolterrors "go.etcd.io/bbolt/errors"
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
		bbolterrors.ErrInvalid,         // "invalid database" - the root file is not a bolt database
		bbolterrors.ErrVersionMismatch, // a bolt file from another architecture or a torn header
		bbolterrors.ErrChecksum,        // the meta page does not match its checksum
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
// one in its place. The caller holds the tenant's open gate, so no other
// goroutine is opening or caching a handle on the directory being renamed.
func (s *Service) repairUnopenableIndex(userID int64, path string, cause error) (bleve.Index, error) {
	s.mu.Lock()
	handler := s.corruptIndexHandler
	s.mu.Unlock()
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

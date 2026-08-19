// File overview: How much disk a tenant's live Bleve index occupies. Scorch
// reads its segments through mmap, so this number is also the memory the index
// wants in the page cache — and the page cache shares the container's memory
// limit with the Go heap. Startup compares the two so a container that cannot
// hold both says so, instead of discovering it as commits that run minutes long.

package search

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// IndexFootprint reports the live per-user indexes under a root.
type IndexFootprint struct {
	// Bytes is the total size of every tenant's live index. Quarantined
	// directories are excluded: nothing maps them, so they cost disk, not memory.
	Bytes int64
	// Tenants is how many live indexes were found.
	Tenants int
}

// MeasureIndexFootprint sums the live per-user indexes under root. A directory
// it cannot read is skipped rather than failing the caller: this measurement
// feeds a warning, and a warning that can block startup is worse than a warning
// that occasionally undercounts.
func MeasureIndexFootprint(root string) (IndexFootprint, error) {
	footprint := IndexFootprint{}
	if strings.TrimSpace(root) == "" {
		return footprint, fmt.Errorf("per-user index root is required")
	}
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return footprint, nil
	}
	if err != nil {
		return footprint, fmt.Errorf("read per-user index root: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, ok := parseUserDirectoryName(entry.Name()); !ok {
			continue
		}
		indexPath := filepath.Join(root, entry.Name(), LiveIndexDirName)
		info, err := os.Stat(indexPath)
		if err != nil || !info.IsDir() {
			continue
		}
		footprint.Bytes += directoryBytes(indexPath)
		footprint.Tenants++
	}
	return footprint, nil
}

// PerUserIndexBytes reports the disk one tenant's live index occupies and
// whether it exists at all. It reads the directory rather than opening the
// index, so an admin page can report a damaged index without the side effect of
// repairing it: that belongs to the write path, or to an explicit rebuild.
func (s *Service) PerUserIndexBytes(userID int64) (int64, bool) {
	if s == nil || userID <= 0 {
		return 0, false
	}
	if s.pg != nil {
		return s.postgresIndexBytes(userID)
	}
	if !s.perUser {
		return 0, false
	}
	path := filepath.Join(s.root, strconv.FormatInt(userID, 10), LiveIndexDirName)
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return 0, false
	}
	return directoryBytes(path), true
}

// postgresSearchBytesTTL is how long one tenant's measured size is reused.
//
// The measurement sums pg_column_size over every one of the tenant's rows, so
// no index can serve it and the cost grows with the mailbox. It answers an
// admin page that lists tenants, which would otherwise pay one full scan per
// tenant per render, so the figure is cached: a size that moves by the minute
// is not what an operator reads it for.
const postgresSearchBytesTTL = time.Minute

type searchBytesSample struct {
	bytes      int64
	measuredAt time.Time
}

// postgresIndexBytes serves the cached size, refreshing it when stale. A failed
// measurement is logged rather than swallowed: without that, a query that
// always fails is indistinguishable from a tenant with no rows.
func (s *Service) postgresIndexBytes(userID int64) (int64, bool) {
	s.mu.Lock()
	sample, ok := s.searchBytes[userID]
	s.mu.Unlock()
	if ok && time.Since(sample.measuredAt) < postgresSearchBytesTTL {
		return sample.bytes, true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	bytes, err := s.pg.MessageSearchBytes(ctx, userID)
	if err != nil {
		s.logf()("measure postgres search bytes user_id=%d: %v", userID, err)
		// A stale figure beats none while the database is unhappy.
		if ok {
			return sample.bytes, true
		}
		return 0, false
	}
	s.mu.Lock()
	if s.searchBytes == nil {
		s.searchBytes = make(map[int64]searchBytesSample)
	}
	s.searchBytes[userID] = searchBytesSample{bytes: bytes, measuredAt: time.Now()}
	s.mu.Unlock()
	return bytes, true
}

// VerifyPerUserIndexOpens reports whether a tenant's live index is present and
// can be opened. Targeted recovery keeps the index an abandoned batch was
// writing to, so it has to establish that the index is actually usable; one that
// is missing or unopenable is the case that still needs the full rebuild.
//
// The presence check is not redundant with the open: openIndex creates a fresh
// empty index when the path does not exist, so opening alone cannot tell "the
// index is fine" from "the index is gone and I just made an empty one". A
// targeted repair must never accept the second, because it reindexes only its
// own range and clears the marker — leaving the tenant with an empty index, all
// other rows still flagged as indexed, and nothing left to rebuild them.
//
// The handle a successful open produces is the one the service goes on to use,
// so nothing is opened twice.
func (s *Service) VerifyPerUserIndexOpens(userID int64) error {
	if s == nil {
		return fmt.Errorf("search service is not configured")
	}
	if s.pg != nil {
		// There is no per-tenant index directory to open on this backend, and
		// s.root is empty, so the Bleve path below would look for one relative
		// to the working directory. Nothing calls this in Postgres mode today;
		// the guard is here so nothing can start to.
		return nil
	}
	_, exists, err := inspectPerUserIndex(s.root, userID)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("user %d has no live search index to repair", userID)
	}
	_, err = s.indexForUser(userID)
	return err
}

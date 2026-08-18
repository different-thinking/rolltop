// File overview: How much disk a tenant's live Bleve index occupies. Scorch
// reads its segments through mmap, so this number is also the memory the index
// wants in the page cache — and the page cache shares the container's memory
// limit with the Go heap. Startup compares the two so a container that cannot
// hold both says so, instead of discovering it as commits that run minutes long.

package search

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// IndexFootprint reports the live per-user indexes under a root.
type IndexFootprint struct {
	// Bytes is the total size of every tenant's live index. Quarantined
	// directories are excluded: nothing maps them, so they cost disk, not memory.
	Bytes int64
	// LargestBytes is the biggest single tenant index, which is what one
	// merge or one query has to page through.
	LargestBytes int64
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
		indexPath := filepath.Join(root, entry.Name(), "bleve")
		info, err := os.Stat(indexPath)
		if err != nil || !info.IsDir() {
			continue
		}
		bytes := directoryBytes(indexPath)
		footprint.Bytes += bytes
		footprint.Tenants++
		if bytes > footprint.LargestBytes {
			footprint.LargestBytes = bytes
		}
	}
	return footprint, nil
}

// VerifyPerUserIndexOpens reports whether a tenant's live index can be opened.
// Targeted recovery keeps the index that an abandoned batch was writing to, so
// it has to establish that the index is actually usable; an index that no longer
// opens is the one case that still needs the full rebuild. The handle this opens
// is the one the service goes on to use, so nothing is opened twice.
func (s *Service) VerifyPerUserIndexOpens(userID int64) error {
	if s == nil {
		return fmt.Errorf("search service is not configured")
	}
	_, err := s.indexForUser(userID)
	return err
}

// File overview: Retention for quarantined per-user Bleve indexes. Quarantine
// renames a whole index aside, so every incident leaves a full copy of it on the
// data volume. Nothing else removes those, and on a deployment that quarantines
// repeatedly they become the largest thing in the directory: two of them held
// two thirds of one production volume's used space.

package search

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultIndexQuarantineKeep is how many quarantined indexes per tenant survive
// pruning. One is what an operator needs to inspect the index an incident
// produced; older ones answer nothing a newer one does not.
const DefaultIndexQuarantineKeep = 1

// DefaultIndexQuarantineMaxAge bounds even the newest quarantine. Past it the
// incident has either been investigated or will not be.
const DefaultIndexQuarantineMaxAge = 48 * time.Hour

// PrunedQuarantine describes one removed directory.
type PrunedQuarantine struct {
	Path   string
	UserID int64
	Age    time.Duration
	Bytes  int64
}

type quarantineEntry struct {
	path    string
	userID  int64
	created time.Time
	bytes   int64
}

// PruneIndexQuarantines removes quarantined indexes that are no longer worth
// their disk. Per tenant it keeps the newest `keep` directories, and removes any
// directory older than maxAge regardless. A directory whose name carries no
// readable timestamp falls back to its modification time, so a quarantine this
// build did not write is still bounded rather than kept forever.
func PruneIndexQuarantines(root string, keep int, maxAge time.Duration, now time.Time) ([]PrunedQuarantine, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("per-user index root is required")
	}
	if keep < 0 {
		keep = 0
	}
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read per-user index root: %w", err)
	}
	pruned := make([]PrunedQuarantine, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		userID, ok := parseUserDirectoryName(entry.Name())
		if !ok {
			continue
		}
		userDir := filepath.Join(root, entry.Name())
		candidates, err := collectIndexQuarantines(userDir, userID)
		if err != nil {
			return pruned, err
		}
		for _, candidate := range expiredIndexQuarantines(candidates, keep, maxAge, now) {
			if err := os.RemoveAll(candidate.path); err != nil {
				return pruned, fmt.Errorf("remove quarantined search index %s: %w", candidate.path, err)
			}
			pruned = append(pruned, PrunedQuarantine{
				Path: candidate.path, UserID: candidate.userID,
				Age: now.Sub(candidate.created), Bytes: candidate.bytes,
			})
		}
	}
	return pruned, nil
}

// expiredIndexQuarantines applies both rules to one tenant's directories:
// anything past maxAge goes, and of what remains only the newest `keep` stay.
func expiredIndexQuarantines(candidates []quarantineEntry, keep int, maxAge time.Duration, now time.Time) []quarantineEntry {
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].created.After(candidates[j].created) })
	expired := make([]quarantineEntry, 0, len(candidates))
	kept := 0
	for _, candidate := range candidates {
		if maxAge > 0 && now.Sub(candidate.created) > maxAge {
			expired = append(expired, candidate)
			continue
		}
		if kept < keep {
			kept++
			continue
		}
		expired = append(expired, candidate)
	}
	return expired
}

func collectIndexQuarantines(userDir string, userID int64) ([]quarantineEntry, error) {
	entries, err := os.ReadDir(userDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read user %d search directory: %w", userID, err)
	}
	prefix := "bleve.quarantine-"
	collected := make([]quarantineEntry, 0)
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		path := filepath.Join(userDir, entry.Name())
		created, err := quarantineCreationTime(path, strings.TrimPrefix(entry.Name(), prefix))
		if err != nil {
			return nil, err
		}
		collected = append(collected, quarantineEntry{
			path: path, userID: userID, created: created, bytes: directoryBytes(path),
		})
	}
	return collected, nil
}

// quarantineCreationTime prefers the stamp quarantine encodes in the directory
// name, because a copy or a restore rewrites modification times.
func quarantineCreationTime(path, stamp string) (time.Time, error) {
	if created, err := time.Parse("20060102T150405.000000000Z", stamp); err == nil {
		return created.UTC(), nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}, fmt.Errorf("inspect quarantined search index %s: %w", path, err)
	}
	return info.ModTime().UTC(), nil
}

// directoryBytes is best effort: the number only reaches a log line, so an
// unreadable entry is worth skipping rather than failing a retention pass for.
func directoryBytes(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		if info, statErr := entry.Info(); statErr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// parseUserDirectoryName accepts only the plain decimal tenant directories the
// per-user root is built from, so nothing else under the data volume is walked.
func parseUserDirectoryName(name string) (int64, bool) {
	userID, err := strconv.ParseInt(name, 10, 64)
	if err != nil || userID <= 0 {
		return 0, false
	}
	return userID, true
}

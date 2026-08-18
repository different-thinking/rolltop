// File overview: Retention for quarantined per-user Bleve indexes. Quarantine
// renames a whole index aside, so every incident leaves a full copy of it on the
// data volume. Nothing else removes those, and on a deployment that quarantines
// repeatedly they become the largest thing in the directory: two of them held
// two thirds of one production volume's used space.

package search

import (
	"errors"
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
}

// PruneIndexQuarantines removes quarantined indexes that are no longer worth
// their disk. Per tenant it keeps the newest `keep` directories, and removes any
// directory older than maxAge regardless. A directory whose name carries no
// readable timestamp falls back to its modification time, so a quarantine this
// build did not write is still bounded rather than kept forever.
//
// A tenant that cannot be read is reported and skipped, never fatal to the pass.
// Housekeeping that stops at the first bad entry would leave every later tenant
// unpruned on this run and on every run after it, which is how quarantines came
// to fill a volume in the first place. Errors are joined and returned once the
// tenants that could be pruned have been.
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
	problems := make([]error, 0)
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
			problems = append(problems, err)
			continue
		}
		for _, candidate := range expiredIndexQuarantines(candidates, keep, maxAge, now) {
			// Sized here rather than while collecting, so the directories
			// retention keeps - the multi-gigabyte copies this exists to bound -
			// are not walked on every pass for a number nothing reads.
			bytes := directoryBytes(candidate.path)
			if err := os.RemoveAll(candidate.path); err != nil {
				problems = append(problems, fmt.Errorf("remove quarantined search index %s: %w", candidate.path, err))
				continue
			}
			pruned = append(pruned, PrunedQuarantine{
				Path: candidate.path, UserID: candidate.userID,
				Age: now.Sub(candidate.created), Bytes: bytes,
			})
		}
	}
	return pruned, errors.Join(problems...)
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

// collectIndexQuarantines lists one tenant's quarantined indexes with the time
// each was set aside. Sizes are deliberately not measured here; only the
// directories retention actually removes are worth walking.
func collectIndexQuarantines(userDir string, userID int64) ([]quarantineEntry, error) {
	entries, err := os.ReadDir(userDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read user %d search directory: %w", userID, err)
	}
	prefix := quarantineDirPrefix()
	collected := make([]quarantineEntry, 0)
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		path := filepath.Join(userDir, entry.Name())
		created, found, err := quarantineCreationTime(path, strings.TrimPrefix(entry.Name(), prefix))
		if err != nil {
			return nil, err
		}
		if !found {
			// Removed since the listing above. Nothing left to prune.
			continue
		}
		collected = append(collected, quarantineEntry{path: path, userID: userID, created: created})
	}
	return collected, nil
}

// quarantineCreationTime prefers the stamp quarantine encodes in the directory
// name, because a copy or a restore rewrites modification times. It reports
// whether the directory is still there: one removed between the listing and this
// stat is not an error, it is simply nothing left to prune.
func quarantineCreationTime(path, stamp string) (time.Time, bool, error) {
	if created, err := time.Parse(quarantineStampLayout, stamp); err == nil {
		return created.UTC(), true, nil
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("inspect quarantined search index %s: %w", path, err)
	}
	return info.ModTime().UTC(), true, nil
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

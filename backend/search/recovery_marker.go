package search

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	searchIndexRecoveryMarker    = "bleve.recovery-required"
	searchIndexRecoveryHeaderV1  = "rolltop-search-recovery-v1"
	searchIndexRecoveryHeaderV2  = "rolltop-search-recovery-v2"
	searchIndexRecoveryDocuments = "documents"
	// maxSearchIndexRecoveryMarkerBytes bounds what a marker read accepts. The
	// file this process writes is at most two short lines, so anything larger
	// did not come from Rolltop and is read as an unattributable failure.
	maxSearchIndexRecoveryMarkerBytes = 4096
)

// SearchIndexRecovery is what a durable marker asks the next start to do.
//
// A writer that ran past its stall threshold says nothing about the index on
// disk. Bleve publishes each snapshot atomically, so what survives an abandoned
// batch is a consistent index in which only the documents that batch owned are
// in doubt. A marker naming those documents therefore repairs the index in
// place; one that names none falls back to rebuilding it from SQLite, which is
// what a failure nothing could attribute still needs.
type SearchIndexRecovery struct {
	// Required reports that a marker exists at all.
	Required bool
	// FirstDocumentID and LastDocumentID bound, inclusively, the message IDs the
	// unfinished batch owned. Both are zero when the marker names no range.
	FirstDocumentID int64
	LastDocumentID  int64
}

// Targeted reports that reindexing the named range is enough, which lets the
// existing index stay where it is.
func (r SearchIndexRecovery) Targeted() bool {
	return r.Required && r.FirstDocumentID > 0 && r.LastDocumentID >= r.FirstDocumentID
}

// widen merges another recovery into this one. Recovery scope only ever grows:
// two stalls in one process must not let the second narrow what the first
// recorded, and a full rebuild outranks any range.
func (r SearchIndexRecovery) widen(other SearchIndexRecovery) SearchIndexRecovery {
	if !r.Required {
		return other
	}
	if !other.Required {
		return r
	}
	if !r.Targeted() || !other.Targeted() {
		return SearchIndexRecovery{Required: true}
	}
	widened := SearchIndexRecovery{Required: true, FirstDocumentID: r.FirstDocumentID, LastDocumentID: r.LastDocumentID}
	if other.FirstDocumentID < widened.FirstDocumentID {
		widened.FirstDocumentID = other.FirstDocumentID
	}
	if other.LastDocumentID > widened.LastDocumentID {
		widened.LastDocumentID = other.LastDocumentID
	}
	return widened
}

// Scope names what this recovery costs, for the log lines an operator reads
// when deciding whether an incident cost a range or the whole index.
func (r SearchIndexRecovery) Scope() string {
	if !r.Targeted() {
		return "full-rebuild"
	}
	return fmt.Sprintf("documents:%d-%d", r.FirstDocumentID, r.LastDocumentID)
}

// marshal renders the marker file. The v1 header is kept for a recovery that
// names no documents, so a marker this build writes stays readable by an older
// one, which treats every marker as a full rebuild.
func (r SearchIndexRecovery) marshal() string {
	if !r.Targeted() {
		return searchIndexRecoveryHeaderV1 + "\n"
	}
	return fmt.Sprintf("%s\n%s %d %d\n", searchIndexRecoveryHeaderV2,
		searchIndexRecoveryDocuments, r.FirstDocumentID, r.LastDocumentID)
}

// parseSearchIndexRecovery reads a marker's payload. Anything it cannot make
// sense of is a full rebuild: a marker exists, so recovery is required, and
// without a trustworthy range there is nothing narrower to do.
func parseSearchIndexRecovery(content []byte) SearchIndexRecovery {
	recovery := SearchIndexRecovery{Required: true}
	if len(content) > maxSearchIndexRecoveryMarkerBytes {
		return recovery
	}
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[0] != searchIndexRecoveryDocuments {
			continue
		}
		first, firstErr := strconv.ParseInt(fields[1], 10, 64)
		last, lastErr := strconv.ParseInt(fields[2], 10, 64)
		if firstErr != nil || lastErr != nil {
			return SearchIndexRecovery{Required: true}
		}
		candidate := SearchIndexRecovery{Required: true, FirstDocumentID: first, LastDocumentID: last}
		if !candidate.Targeted() {
			return SearchIndexRecovery{Required: true}
		}
		return candidate
	}
	return recovery
}

// MarkSearchIndexRecoveryRequired durably records that a tenant index must be
// rebuilt from SQLite on the next process start. The marker is a sibling of the
// live Bleve directory, so moving that directory cannot accidentally consume it.
func (s *Service) MarkSearchIndexRecoveryRequired(userID int64) error {
	return s.markSearchIndexRecovery(userID, SearchIndexRecovery{Required: true})
}

// MarkSearchIndexRecoveryRequiredForDocuments records a recovery that only has
// to reindex one message range, which leaves the index itself in place. The
// range is inclusive and may cover messages the stalled batch did not own;
// reindexing a few extra documents is the cost of not tracking every ID.
func (s *Service) MarkSearchIndexRecoveryRequiredForDocuments(userID, firstDocumentID, lastDocumentID int64) error {
	return s.markSearchIndexRecovery(userID, SearchIndexRecovery{
		Required: true, FirstDocumentID: firstDocumentID, LastDocumentID: lastDocumentID,
	})
}

func (s *Service) markSearchIndexRecovery(userID int64, recovery SearchIndexRecovery) error {
	markerPath, userDir, err := s.searchIndexRecoveryMarkerPath(userID, true)
	if err != nil {
		return err
	}
	return writeSearchIndexRecoveryMarker(markerPath, userDir, recovery, syncDirectory)
}

func writeSearchIndexRecoveryMarker(markerPath, userDir string, recovery SearchIndexRecovery, syncDir func(string) error) error {
	if !recovery.Required {
		return fmt.Errorf("refusing to write a search recovery marker for no recovery")
	}
	if existing, found, err := readSearchIndexRecoveryMarker(markerPath); err != nil {
		return err
	} else if found {
		merged := existing.widen(recovery)
		if merged == existing {
			// A prior publish may have succeeded while its directory sync
			// failed. Re-sync an existing marker before treating it as durable.
			if err := syncDir(userDir); err != nil {
				return fmt.Errorf("sync existing search recovery marker directory: %w", err)
			}
			return nil
		}
		recovery = merged
	}

	temporary, err := os.CreateTemp(userDir, ".bleve.recovery-required-*")
	if err != nil {
		return fmt.Errorf("create search recovery marker: %w", err)
	}
	temporaryPath := temporary.Name()
	keepTemporary := true
	defer func() {
		_ = temporary.Close()
		if keepTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("secure search recovery marker: %w", err)
	}
	if _, err := temporary.WriteString(recovery.marshal()); err != nil {
		return fmt.Errorf("write search recovery marker: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync search recovery marker: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close search recovery marker: %w", err)
	}
	if err := os.Rename(temporaryPath, markerPath); err != nil {
		return fmt.Errorf("publish search recovery marker: %w", err)
	}
	keepTemporary = false
	if err := syncDir(userDir); err != nil {
		return fmt.Errorf("sync search recovery directory: %w", err)
	}
	return nil
}

// SearchIndexRecoveryRequired reports whether startup must repair this tenant's
// index before any Bleve handle is opened.
func (s *Service) SearchIndexRecoveryRequired(userID int64) (bool, error) {
	recovery, err := s.SearchIndexRecoveryPlan(userID)
	return recovery.Required, err
}

// SearchIndexRecoveryPlan reports what that repair has to do: reindex the
// message range the marker names, or rebuild the whole index when it names none.
func (s *Service) SearchIndexRecoveryPlan(userID int64) (SearchIndexRecovery, error) {
	markerPath, _, err := s.searchIndexRecoveryMarkerPath(userID, false)
	if err != nil {
		return SearchIndexRecovery{}, err
	}
	recovery, found, err := readSearchIndexRecoveryMarker(markerPath)
	if err != nil || !found {
		return SearchIndexRecovery{}, err
	}
	return recovery, nil
}

// readSearchIndexRecoveryMarker reads a marker if one is there. A marker whose
// payload cannot be read is still a marker: the read failure is reported as a
// full rebuild rather than as an error, because refusing to start would leave
// the tenant with no search at all and no way out.
func readSearchIndexRecoveryMarker(markerPath string) (SearchIndexRecovery, bool, error) {
	info, err := os.Lstat(markerPath)
	if os.IsNotExist(err) {
		return SearchIndexRecovery{}, false, nil
	}
	if err != nil {
		return SearchIndexRecovery{}, false, fmt.Errorf("inspect search recovery marker: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return SearchIndexRecovery{}, false, fmt.Errorf("search recovery marker is not a regular file")
	}
	content, err := os.ReadFile(markerPath)
	if err != nil {
		return SearchIndexRecovery{Required: true}, true, nil
	}
	return parseSearchIndexRecovery(content), true, nil
}

// ClearSearchIndexRecoveryRequired acknowledges successful offline quarantine.
// Callers must first reset SQLite index-completion state and durably move the old
// index. A failed marker-removal sync restores the marker for a later retry when
// possible and always returns an error.
func (s *Service) ClearSearchIndexRecoveryRequired(userID int64) error {
	return s.clearSearchIndexRecoveryRequiredWithSync(userID, syncDirectory)
}

func (s *Service) clearSearchIndexRecoveryRequiredWithSync(userID int64, syncDir func(string) error) error {
	markerPath, userDir, err := s.searchIndexRecoveryMarkerPath(userID, false)
	if err != nil {
		return err
	}
	// Read the payload before removing the file: restoring the marker below has
	// to ask for the same recovery, and a restored full rebuild where the
	// original named a range would undo the repair this build exists to make.
	recovery, found, err := readSearchIndexRecoveryMarker(markerPath)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if err := os.Remove(markerPath); err != nil {
		return fmt.Errorf("clear search recovery marker: %w", err)
	}
	if err := syncDir(userDir); err != nil {
		clearErr := fmt.Errorf("sync cleared search recovery marker: %w", err)
		// The caller will fail startup, so restore a durable marker for the next
		// attempt whenever possible. The index rename was synced before this
		// method was called, making either crash outcome safe even if restoration
		// also fails.
		if restoreErr := writeSearchIndexRecoveryMarker(markerPath, userDir, recovery, syncDir); restoreErr != nil {
			return errors.Join(clearErr, fmt.Errorf("restore search recovery marker after clear failure: %w", restoreErr))
		}
		return fmt.Errorf("%w; marker restored for retry", clearErr)
	}
	return nil
}

func (s *Service) searchIndexRecoveryMarkerPath(userID int64, createUserDir bool) (string, string, error) {
	if s == nil || !s.perUser || s.root == "" {
		return "", "", fmt.Errorf("search recovery markers require a per-user index service")
	}
	if userID <= 0 {
		return "", "", fmt.Errorf("user id must be positive")
	}
	root, err := filepath.Abs(filepath.Clean(s.root))
	if err != nil {
		return "", "", fmt.Errorf("resolve per-user index root: %w", err)
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return "", "", fmt.Errorf("inspect per-user index root: %w", err)
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return "", "", fmt.Errorf("per-user index root is not a regular directory: %s", root)
	}
	userDir := filepath.Join(root, strconv.FormatInt(userID, 10))
	relative, err := filepath.Rel(root, userDir)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("user index path is outside the configured root")
	}
	if createUserDir {
		if err := os.Mkdir(userDir, 0o700); err != nil && !os.IsExist(err) {
			return "", "", fmt.Errorf("create user search directory: %w", err)
		}
	}
	userInfo, err := os.Lstat(userDir)
	if os.IsNotExist(err) && !createUserDir {
		return filepath.Join(userDir, searchIndexRecoveryMarker), userDir, nil
	}
	if err != nil {
		return "", "", fmt.Errorf("inspect user %d data directory: %w", userID, err)
	}
	if !userInfo.IsDir() || userInfo.Mode()&os.ModeSymlink != 0 {
		return "", "", fmt.Errorf("user %d data directory is not a regular directory", userID)
	}
	return filepath.Join(userDir, searchIndexRecoveryMarker), userDir, nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

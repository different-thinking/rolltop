// File overview: Per-user storage usage measurement and caching.

package web

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"rolltop/backend/search"
)

// StorageStats is the per-user disk usage summary shown on the settings page.
//
// There is no per-tenant database figure here. PostgreSQL reports one size for
// the whole database, which is a number for the admin page rather than for a
// user's own settings, and no honest share of it can be attributed to one
// tenant. MessageHeaderCount is what this view can say about the relational
// side, and it says it as a count.
//
// The full-text figures are asked of the search service rather than measured on
// the volume, because where the index lives is the backend's business: Bleve
// leaves a directory this process can walk, PostgreSQL leaves rows only a query
// can size. Walking the volume on the Postgres backend reports zero bytes and
// no missing index, which reads as "search is empty" for a search that is
// working perfectly.
type StorageStats struct {
	MessageHeaderCount int
	// SearchBackend names where the index lives ("bleve" or "postgres"), so a
	// page reporting zero bytes can say which of the two it measured.
	SearchBackend string
	IndexPath     string
	IndexBytes    int64
	// IndexPresent is false when this tenant has no index yet — a new user, or
	// the interim right after a rebuild was started. It separates "nothing
	// indexed" from "index of zero bytes", which the number alone cannot.
	IndexPresent               bool
	IndexMessageCount          int
	FullTextSearchMessageCount int
	// FoldersNeedingRebuild counts this tenant's search-visible folders whose
	// coverage nothing has verified. It is what the rebuild acts on.
	FoldersNeedingRebuild int64
	// FuzzyAvailable reports whether typo-tolerant matching can answer. See
	// search.Service.FuzzyAvailable.
	FuzzyAvailable bool
	// IndexBreakdown describes the Bleve directory and is empty on any other
	// backend, which has no files to break down.
	IndexBreakdown   StorageIndexBreakdown
	BlobPath         string
	BlobBytes        int64
	MessageBodyCount int
	TotalBytes       int64
	Error            string
}

// StorageIndexBreakdown describes the per-user Bleve directory without exposing
// message content or data from another tenant's storage tree.
type StorageIndexBreakdown struct {
	FileCount       int
	ZapCount        int
	ZapBytes        int64
	LargestZapPath  string
	LargestZapBytes int64
	RootBytes       int64
	OtherBytes      int64
}

const storageStatsCacheTTL = 5 * time.Minute

type storageStatsCacheEntry struct {
	Stats    StorageStats
	CachedAt time.Time
}

func (s *Server) cachedStorageStats(userID int64) StorageStats {
	now := time.Now()
	s.storageMu.Lock()
	if entry, ok := s.storageCached[userID]; ok && now.Sub(entry.CachedAt) < storageStatsCacheTTL {
		stats := entry.Stats
		s.storageMu.Unlock()
		return stats
	}
	s.storageMu.Unlock()

	stats := s.storageStatsForUser(userID)

	s.storageMu.Lock()
	if s.storageCached == nil {
		s.storageCached = make(map[int64]storageStatsCacheEntry)
	}
	s.storageCached[userID] = storageStatsCacheEntry{Stats: stats, CachedAt: now}
	s.storageMu.Unlock()
	return stats
}

func (s *Server) storageStatsForUser(userID int64) StorageStats {
	indexPath, blobPath := s.userStoragePaths(userID)
	backend := search.BackendBleve
	if s.search != nil {
		backend = s.search.Backend()
	}
	stats := StorageStats{
		SearchBackend: backend,
		BlobPath:      joinedStoragePaths(blobPath),
	}
	if backend == search.BackendBleve {
		// The path is the directory this tenant's index occupies. There is none
		// on the Postgres backend, and naming one there points an operator at a
		// directory that will never exist.
		stats.IndexPath = joinedStoragePaths(indexPath)
	}
	var errs []string
	var err error
	if s.store != nil {
		stats.MessageHeaderCount, err = s.store.CountMessagesForUser(context.Background(), userID)
		if err != nil {
			errs = append(errs, fmt.Sprintf("message header count: %v", err))
		}
	}
	if stats.SearchBackend == search.BackendBleve {
		// Only Bleve has files, and the breakdown is the whole reason this
		// walks the directory rather than asking the service for one number:
		// segment counts and the largest segment are what a stalling index
		// looks like from outside.
		stats.IndexBytes, stats.IndexBreakdown, err = bleveIndexBreakdown(indexPath)
		if err != nil {
			errs = append(errs, fmt.Sprintf("full text index: %v", err))
		}
	} else if s.search != nil {
		stats.IndexBytes, _ = s.search.PerUserIndexBytes(userID)
	}
	// One rule for both backends: an index is present when it holds something.
	// The service's own presence flag answers a different question — the admin
	// page asks whether a directory exists at all — and reading it here would
	// report "indexed" for a tenant whose rows are not there yet.
	stats.IndexPresent = stats.IndexBytes > 0
	if s.search != nil {
		stats.FuzzyAvailable = s.search.FuzzyAvailable()
		stats.IndexMessageCount, err = s.search.CountUserMessages(context.Background(), userID)
		if err != nil {
			errs = append(errs, fmt.Sprintf("full text index message count: %v", err))
		}
	}
	if s.store != nil {
		stats.FullTextSearchMessageCount, err = s.store.CountSearchEnabledMessagesForUser(context.Background(), userID)
		if err != nil {
			errs = append(errs, fmt.Sprintf("search-enabled message count: %v", err))
		}
		stats.FoldersNeedingRebuild, err = s.store.CountMailboxesNeedingSearchIndexRepair(context.Background(), userID)
		if err != nil {
			errs = append(errs, fmt.Sprintf("folders needing a search rebuild: %v", err))
		}
	}
	stats.BlobBytes, err = pathSize(blobPath)
	if err != nil {
		errs = append(errs, fmt.Sprintf("message bodies: %v", err))
	}
	if s.store != nil {
		stats.MessageBodyCount, err = s.store.CountCachedMessageBodiesForUser(context.Background(), userID)
		if err != nil {
			errs = append(errs, fmt.Sprintf("message body count: %v", err))
		}
	}
	stats.TotalBytes = stats.IndexBytes + stats.BlobBytes
	stats.Error = strings.Join(errs, "; ")
	return stats
}

// userStoragePaths resolves the two directories a tenant still owns on this
// volume. The relational data is in PostgreSQL and has no per-user path.
func (s *Server) userStoragePaths(userID int64) (indexPath, blobPath string) {
	if userID <= 0 {
		return "", ""
	}
	id := strconv.FormatInt(userID, 10)
	if strings.TrimSpace(s.dataDir) != "" {
		userDir := filepath.Join(s.dataDir, "users", id)
		return filepath.Join(userDir, search.LiveIndexDirName), filepath.Join(userDir, "blobs")
	}
	if s.blobs != nil && strings.TrimSpace(s.blobs.Root) != "" {
		blobPath = filepath.Join(s.blobs.Root, "users", id, "blobs")
	}
	return s.indexPath, blobPath
}

func joinedStoragePaths(paths ...string) string {
	var clean []string
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path != "" && !strings.Contains(path, string(filepath.Separator)+"*"+string(filepath.Separator)) {
			if _, err := os.Stat(path); os.IsNotExist(err) {
				continue
			}
		}
		if path != "" {
			clean = append(clean, path)
		}
	}
	return strings.Join(clean, " + ")
}

func bleveIndexBreakdown(path string) (int64, StorageIndexBreakdown, error) {
	var breakdown StorageIndexBreakdown
	if strings.TrimSpace(path) == "" {
		return 0, breakdown, nil
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return 0, breakdown, nil
	}
	if err != nil {
		return 0, breakdown, err
	}
	if !info.IsDir() {
		breakdown.FileCount = 1
		breakdown.OtherBytes = info.Size()
		return info.Size(), breakdown, nil
	}

	var total int64
	err = filepath.WalkDir(path, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		size := info.Size()
		total += size
		breakdown.FileCount++
		switch {
		case filepath.Ext(entry.Name()) == ".zap":
			breakdown.ZapCount++
			breakdown.ZapBytes += size
			if size > breakdown.LargestZapBytes {
				breakdown.LargestZapBytes = size
				breakdown.LargestZapPath = relativeStoragePath(path, filePath)
			}
		case entry.Name() == "root.bolt":
			breakdown.RootBytes += size
		default:
			breakdown.OtherBytes += size
		}
		return nil
	})
	return total, breakdown, err
}

func relativeStoragePath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return filepath.Base(path)
	}
	return filepath.ToSlash(rel)
}

func pathSize(path string) (int64, error) {
	if strings.TrimSpace(path) == "" {
		return 0, nil
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return info.Size(), nil
	}
	var total int64
	err = filepath.WalkDir(path, func(_ string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// searchBackendName reports where the index lives, so a page can say which of
// the two it is describing. Empty when there is no search service, which the
// pages read as "nothing to say about search".
func (s *Server) searchBackendName() string {
	if s.search == nil {
		return ""
	}
	return s.search.Backend()
}

// invalidateStorageStats drops one tenant's cached figures. The cache holds for
// five minutes, which is right for a page someone opens to look at and wrong
// the moment they act on it: a rebuild that started has already changed the
// answer to every number on that page.
func (s *Server) invalidateStorageStats(userID int64) {
	s.storageMu.Lock()
	delete(s.storageCached, userID)
	s.storageMu.Unlock()
}

// apiStorageSearchRebuild rebuilds the signed-in user's own search index. It is
// the same work the admin page offers per tenant and the folder settings offer
// per account — startSearchRebuildForUser, one run per mail server — reachable
// by the person whose search is incomplete rather than only by an admin.
//
// It takes no user id. A reader may rebuild their own index and nothing else;
// the admin route is where acting on another tenant lives, behind an admin
// check that this one deliberately does not repeat.
func (s *Server) apiStorageSearchRebuild(w http.ResponseWriter, r *http.Request) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !s.verifyCSRF(w, r) {
		return
	}
	if s.syncer == nil || s.syncer.Search == nil || s.syncRunner == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "Search indexing is not configured on this server.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), searchIndexTimeout)
	defer cancel()
	started, busy, err := s.startSearchRebuildForUser(ctx, cu.User.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if started == 0 {
		if busy > 0 {
			writeAPIError(w, http.StatusConflict,
				"Sync or full-text reindexing is already running for your mail servers.")
			return
		}
		writeAPIError(w, http.StatusBadRequest, "There are no search-visible folders to rebuild.")
		return
	}
	s.invalidateStorageStats(cu.User.ID)
	s.events.Notify(cu.User.ID)
	writeJSON(w, map[string]any{
		"ok":            true,
		"started_runs":  started,
		"busy_accounts": busy,
		"storage":       s.cachedStorageStats(cu.User.ID),
	})
}

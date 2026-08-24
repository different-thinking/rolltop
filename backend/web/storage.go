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
// The database figure here is this tenant's own rows, not the database. What
// PostgreSQL reports about itself covers every tenant plus its indexes, and that
// belongs to the admin page; store.UserMailRowBytes measures the share this
// reader's mail actually occupies, which is what makes it showable next to the
// two directory sizes and addable into a total.
//
// The full-text figures are asked of the search service rather than measured on
// the volume, because where the index lives is the backend's business: Bleve
// leaves a directory this process can walk, PostgreSQL leaves rows only a query
// can size. Walking the volume on the Postgres backend reports zero bytes and
// no missing index, which reads as "search is empty" for a search that is
// working perfectly.
type StorageStats struct {
	MessageHeaderCount int
	// DatabaseBytes is what this tenant's mail rows occupy in PostgreSQL, and
	// DatabaseMeasured says the figure was actually read. The measurement is a
	// scan behind a timeout, so it can fail while the database is perfectly
	// healthy, and a failed measurement reported as zero would announce a
	// mailbox that costs nothing to store.
	DatabaseBytes    int64
	DatabaseMeasured bool
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
	// FoldersPurged counts folders whose documents were deliberately deleted and
	// not rebuilt since. Their mail is in the shortfall below and is invisible to
	// background indexing, which skips purged folders on purpose: only a rebuild
	// brings them back, and a page that does not say so leaves a number that
	// never moves with no explanation.
	FoldersPurged int64
	// UnsyncedSearchFolders counts folders included in search that hold no mail
	// here and are only synced on request, and UnsyncedSearchMessages is what
	// those folders last reported holding on the server. They are not part of the shortfall below and
	// cannot be: coverage compares the index against the messages table, and
	// mail that was never fetched is in neither, so a mailbox missing whole
	// folders reports full coverage. UnsyncedSearchFolderNames names a few of
	// them, because the answer here is a folder setting rather than a rebuild.
	UnsyncedSearchFolders     int64
	UnsyncedSearchMessages    int64
	UnsyncedSearchFolderNames []string
	// SearchCoverageMeasured says both sides of the shortfall - the documents in
	// the index and the mail that should be in it - were actually read. A page
	// that announces a shortfall built from a figure that failed is announcing a
	// number it made up, and any other figure on this page failing is not a
	// reason to withhold this one.
	SearchCoverageMeasured bool
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

// storageDatabaseBytesTimeout bounds the per-tenant row measurement. It is the
// same shape of query as the Postgres search-index size, and it gets the same
// treatment: an answer or nothing, never a page that waits on it.
const storageDatabaseBytesTimeout = 10 * time.Second

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
		// Bounded on purpose: this walks the tenant's rows, and a busy database
		// must cost the page a missing figure rather than a request that hangs.
		databaseCtx, cancel := context.WithTimeout(context.Background(), storageDatabaseBytesTimeout)
		stats.DatabaseBytes, err = s.store.UserMailRowBytes(databaseCtx, userID)
		cancel()
		if err != nil {
			stats.DatabaseBytes = 0
			errs = append(errs, fmt.Sprintf("database rows: %v", err))
		} else {
			stats.DatabaseMeasured = true
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
		// A measurement that failed is not a measurement of zero. Sizing the
		// Postgres rows is a full scan behind a timeout, so it can fail on a
		// busy database while search itself is fine, and reporting that as an
		// empty index is precisely the false alarm this page exists to avoid.
		var measured bool
		stats.IndexBytes, measured = s.search.PerUserIndexBytes(userID)
		if !measured {
			errs = append(errs, "full text index: size could not be measured")
		}
	}
	indexCountMeasured := false
	if s.search != nil {
		stats.FuzzyAvailable = s.search.FuzzyAvailable()
		stats.IndexMessageCount, err = s.search.CountUserMessages(context.Background(), userID)
		if err != nil {
			errs = append(errs, fmt.Sprintf("full text index message count: %v", err))
		} else {
			indexCountMeasured = true
		}
	}
	// An index is present when it holds documents, not when it occupies bytes:
	// the document count is one cheap query on either backend, while the size
	// is a directory walk or a full scan that can legitimately fail. Deriving
	// presence from the size would let one slow query tell a reader their
	// search is empty.
	stats.IndexPresent = stats.IndexMessageCount > 0
	if s.store != nil {
		stats.FullTextSearchMessageCount, err = s.store.CountSearchEnabledMessagesForUser(context.Background(), userID)
		if err != nil {
			errs = append(errs, fmt.Sprintf("search-enabled message count: %v", err))
		} else {
			stats.SearchCoverageMeasured = indexCountMeasured
		}
		stats.FoldersNeedingRebuild, err = s.store.CountMailboxesNeedingSearchIndexRepair(context.Background(), userID)
		if err != nil {
			errs = append(errs, fmt.Sprintf("folders needing a search rebuild: %v", err))
		}
		stats.FoldersPurged, err = s.store.CountMailboxesWithPurgedSearchIndexForUser(context.Background(), userID)
		if err != nil {
			errs = append(errs, fmt.Sprintf("folders with a purged search index: %v", err))
		}
		unsynced, err := s.store.ListUnsyncedSearchFoldersForUser(context.Background(), userID)
		if err != nil {
			errs = append(errs, fmt.Sprintf("folders included in search but not synced: %v", err))
		} else {
			stats.UnsyncedSearchFolders = unsynced.Folders
			stats.UnsyncedSearchMessages = unsynced.Messages
			stats.UnsyncedSearchFolderNames = unsynced.Names
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
	stats.TotalBytes = stats.DatabaseBytes + stats.IndexBytes + stats.BlobBytes
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
	started, blocked, err := s.startSearchRebuildForUser(ctx, cu.User.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if started == 0 {
		if len(blocked) > 0 {
			writeAPIError(w, http.StatusConflict,
				"Rebuilding did not start. "+describeSearchRebuildBlocks(blocked)+
					" Follow it in Activity, then try again.")
			return
		}
		writeAPIError(w, http.StatusBadRequest, "There are no search-visible folders to rebuild.")
		return
	}
	s.invalidateStorageStats(cu.User.ID)
	s.events.Notify(cu.User.ID)
	// Measured rather than cached, and deliberately not written back into the
	// cache: the runs have started but not finished, so these figures are the
	// state at the moment of the click. Caching them would pin a pre-rebuild
	// answer for the next five minutes, which is the opposite of what dropping
	// the entry above was for. The GET that follows recomputes into the cache
	// in the ordinary way.
	writeJSON(w, map[string]any{
		"ok":            true,
		"started_runs":  started,
		"busy_accounts": len(blocked),
		"blocked":       blocked,
		"storage":       s.storageStatsForUser(cu.User.ID),
	})
}

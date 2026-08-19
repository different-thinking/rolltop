// File overview: What the admin database page can still say about storage now
// that the relational data lives in PostgreSQL.
//
// The jobs this file used to run — integrity check, backup, scheduled repair —
// were all answers to SQLite-on-a-network-volume, and all three are gone with
// it. A committed transaction is durable, corruption is the server's problem
// rather than the application's, and backups are the operator's `pg_dump`
// (README documents the schedule) rather than a button that writes into the
// same volume it is protecting against.
//
// What is left is reporting: a connectivity and size card for the database, and
// the data volume's free space — which still matters, because blobs and the
// Bleve indexes are on it.

package web

import (
	"context"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"rolltop/backend/search"
)

// databaseStatus is the connection and size card for the one database.
type databaseStatus struct {
	// Target names role@host/database, never the password.
	Target string `json:"target"`
	// Reachable is false when the status query itself failed, in which case
	// Error carries the redacted reason.
	Reachable bool   `json:"reachable"`
	Error     string `json:"error,omitempty"`
	// ServerVersion is the server's version string, e.g. "PostgreSQL 16.6".
	ServerVersion string `json:"server_version,omitempty"`
	// Bytes is what pg_database_size reports for the whole database.
	Bytes int64 `json:"bytes"`
	// LatencyMillis is the round trip the status query itself measured, which
	// is the number the syncer's per-row loops are budgeted against.
	LatencyMillis float64 `json:"latency_millis"`
	// InRecovery reports a standby, where writes would fail.
	InRecovery bool `json:"in_recovery"`
	// Connections is how many of the role's connections are in use.
	Connections int `json:"connections"`
	// PoolMaxConns is what this process is configured to open at most.
	PoolMaxConns int `json:"pool_max_conns"`
}

// volumeStatus is the data volume, which still holds blobs and search indexes.
type volumeStatus struct {
	DataDir    string `json:"data_dir"`
	FreeBytes  int64  `json:"free_bytes"`
	TotalBytes int64  `json:"total_bytes"`
	BlobBytes  int64  `json:"blob_bytes"`
	IndexBytes int64  `json:"index_bytes"`
	// OtherBytes is what is on the volume under users/ but is neither a blob
	// nor an index — a quarantined index waiting to be deleted, most often.
	// Without it the two figures silently fail to add up to the used space.
	OtherBytes int64 `json:"other_bytes"`
	// MeasuredAtUnix is when the walk behind the three figures above ran, or 0
	// if none has finished yet. Measuring means one stat per stored file, so it
	// happens on a timer rather than per request and the page says how old the
	// answer is.
	MeasuredAtUnix int64 `json:"measured_at_unix"`
}

// databaseOverview is the whole admin page payload.
type databaseOverview struct {
	Database databaseStatus `json:"database"`
	Volume   volumeStatus   `json:"volume"`
	// SearchBackend names where the full-text index lives ("bleve" or
	// "postgres"). The volume figures beside it only describe an index on the
	// Bleve backend; on the other one the index is rows in the database above,
	// and a page that does not say so reports an index of zero bytes as if
	// search had stopped working.
	SearchBackend string `json:"search_backend,omitempty"`
}

// databaseOverview assembles the page. A database that cannot be reached is
// reported as unreachable rather than failing the request: the page's whole
// purpose in that moment is to say what is wrong with it.
func (s *Server) databaseOverview(ctx context.Context) (databaseOverview, error) {
	overview := databaseOverview{
		Database: databaseStatus{
			Target:       s.databaseTarget,
			PoolMaxConns: s.databaseMaxConns,
		},
		Volume:        volumeStatus{DataDir: s.dataDir},
		SearchBackend: s.searchBackendName(),
	}
	// Free and total come from one statfs, which is cheap enough to do per
	// request. The per-directory split does not: it walks every stored file.
	overview.Volume.FreeBytes, overview.Volume.TotalBytes = filesystemCapacityBytes(s.dataDir)
	usage := s.cachedVolumeUsage()
	overview.Volume.BlobBytes = usage.BlobBytes
	overview.Volume.IndexBytes = usage.IndexBytes
	overview.Volume.OtherBytes = usage.OtherBytes
	if !usage.MeasuredAt.IsZero() {
		overview.Volume.MeasuredAtUnix = usage.MeasuredAt.Unix()
	}

	status, err := s.store.DatabaseStatus(ctx)
	if err != nil {
		overview.Database.Error = err.Error()
		return overview, nil
	}
	overview.Database.Reachable = true
	overview.Database.ServerVersion = status.ServerVersion
	overview.Database.Bytes = status.Bytes
	overview.Database.LatencyMillis = float64(status.RoundTrip.Microseconds()) / 1000
	overview.Database.InRecovery = status.InRecovery
	overview.Database.Connections = status.Connections
	return overview, nil
}

// volumeUsage is what the data volume is spent on, split the way the layout
// actually is: <dataDir>/users/<id>/blobs holds raw .eml, <id>/bleve holds that
// tenant's search index, and anything else under the tenant's directory is
// neither.
type volumeUsage struct {
	BlobBytes  int64
	IndexBytes int64
	OtherBytes int64
	MeasuredAt time.Time
}

// volumeUsageTTL is how long a measurement is served before another is started.
// The admin page polls every 15 seconds and the walk costs one stat per stored
// file, so a poll must never be what triggers it.
const volumeUsageTTL = 5 * time.Minute

// cachedVolumeUsage returns the last completed measurement and starts a new one
// in the background when it has gone stale.
//
// It never blocks and never fails: an admin page that had to wait for a walk of
// a few hundred thousand blobs would spend its whole request budget there and
// then report a healthy database as unreachable, which is what it used to do.
// The first call after start returns a zero measurement, which the page renders
// as "measuring".
func (s *Server) cachedVolumeUsage() volumeUsage {
	s.volumeUsageMu.Lock()
	usage := s.volumeUsageCached
	stale := usage.MeasuredAt.IsZero() || time.Since(usage.MeasuredAt) >= volumeUsageTTL
	start := stale && !s.volumeUsageMeasuring
	if start {
		s.volumeUsageMeasuring = true
	}
	s.volumeUsageMu.Unlock()

	if start {
		go s.measureVolumeUsage()
	}
	return usage
}

func (s *Server) measureVolumeUsage() {
	measured, err := measureVolumeUsage(s.dataDir)
	if err != nil {
		log.Printf("measure data volume %s: %v", s.dataDir, err)
	}
	s.volumeUsageMu.Lock()
	// A partial walk is still worth more than nothing — an unreadable tenant
	// directory should not blank the card — so the result is kept either way,
	// and the timestamp keeps the next attempt a TTL out rather than immediate.
	s.volumeUsageCached = measured
	s.volumeUsageMeasuring = false
	s.volumeUsageMu.Unlock()
}

// measureVolumeUsage walks <dataDir>/users once and attributes every file to a
// tenant subdirectory. One walk rather than three keeps the directory entries
// warm in the page cache for the whole measurement.
func measureVolumeUsage(dataDir string) (volumeUsage, error) {
	usage := volumeUsage{MeasuredAt: time.Now()}
	if strings.TrimSpace(dataDir) == "" {
		return usage, nil
	}
	root := filepath.Join(dataDir, "users")
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		// Nothing has synced yet, which is a legitimate zero rather than a
		// failure to measure.
		return usage, nil
	}
	if err != nil {
		return usage, err
	}
	var firstErr error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Only numeric names are tenants. Anything else under users/ is not
		// this layout and is left out rather than guessed at.
		if _, convErr := strconv.ParseInt(entry.Name(), 10, 64); convErr != nil {
			continue
		}
		userDir := filepath.Join(root, entry.Name())
		subEntries, subErr := os.ReadDir(userDir)
		if subErr != nil {
			if firstErr == nil {
				firstErr = subErr
			}
			continue
		}
		for _, sub := range subEntries {
			size, sizeErr := treeSize(filepath.Join(userDir, sub.Name()), sub)
			if sizeErr != nil && firstErr == nil {
				firstErr = sizeErr
			}
			// A quarantined index is "bleve.quarantine-<stamp>" and lands in
			// OtherBytes on purpose: it is space an operator can reclaim.
			switch sub.Name() {
			case "blobs":
				usage.BlobBytes += size
			case search.LiveIndexDirName:
				usage.IndexBytes += size
			default:
				usage.OtherBytes += size
			}
		}
	}
	return usage, firstErr
}

// treeSize measures one entry, recursing only when it is a directory. Errors
// are returned with whatever was counted, so one unreadable directory costs its
// own bytes rather than the whole measurement.
func treeSize(path string, entry fs.DirEntry) (int64, error) {
	if !entry.IsDir() {
		info, err := entry.Info()
		if err != nil {
			return 0, err
		}
		return info.Size(), nil
	}
	var total int64
	err := filepath.WalkDir(path, func(_ string, walked fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if walked.IsDir() {
			return nil
		}
		info, err := walked.Info()
		if err != nil {
			// A file removed mid-walk is normal here: retention and quarantine
			// cleanup both run while the page is open.
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// filesystemCapacityBytes reports free and total bytes on the data volume.
// Blobs and Bleve segments still live there, and a full volume still stops both.
func filesystemCapacityBytes(path string) (free, total int64) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0
	}
	return int64(stat.Bavail) * int64(stat.Bsize), int64(stat.Blocks) * int64(stat.Bsize)
}

// maintenanceStatusTimeout bounds the status query so an unreachable database
// makes the admin page slow rather than hanging it.
const maintenanceStatusTimeout = 5 * time.Second

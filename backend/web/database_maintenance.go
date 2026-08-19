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
	"strings"
	"syscall"
	"time"
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
}

// databaseOverview is the whole admin page payload.
type databaseOverview struct {
	Database databaseStatus `json:"database"`
	Volume   volumeStatus   `json:"volume"`
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
		Volume: volumeStatus{DataDir: s.dataDir},
	}
	overview.Volume.FreeBytes, overview.Volume.TotalBytes = filesystemCapacityBytes(s.dataDir)
	overview.Volume.BlobBytes, _ = pathSizeIfPresent(s.dataDir, "users")
	overview.Volume.IndexBytes, _ = pathSizeIfPresent(s.indexPath, "")

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

// pathSizeIfPresent measures a directory, treating a missing one as zero. The
// blob store and the index directory are both created lazily, so an
// installation that has not synced yet has neither.
func pathSizeIfPresent(root, sub string) (int64, error) {
	path := root
	if sub != "" {
		path = root + "/" + sub
	}
	if strings.TrimSpace(root) == "" {
		return 0, nil
	}
	size, err := pathSize(path)
	if err != nil {
		return 0, nil
	}
	return size, nil
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

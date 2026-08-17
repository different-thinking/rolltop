// File overview: Consistent SQLite backups taken while Rolltop is running.
// Copying rolltop.db with cp, rsync, or a volume snapshot while the server
// writes produces a torn file, because the committed state is spread across the
// database and its WAL. VACUUM INTO instead writes a fresh, fully checkpointed
// database from one read transaction, which is the only safe way to copy a live
// SQLite file.

package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// BackupDatabaseFile writes a consistent copy of a live SQLite database to
// destPath and returns the size of the copy. destPath must not exist. The
// source is only read; the copy contains every transaction committed before the
// backup's read transaction started.
func BackupDatabaseFile(ctx context.Context, sourcePath, destPath string) (int64, error) {
	if strings.TrimSpace(sourcePath) == "" || strings.TrimSpace(destPath) == "" {
		return 0, fmt.Errorf("backup requires a source and destination database path")
	}
	if sourcePath == destPath {
		return 0, fmt.Errorf("backup destination must differ from the source database")
	}
	// The source must exist: SQLite would otherwise create it here and report an
	// empty database as a successful backup.
	if err := requireDatabaseFile(sourcePath); err != nil {
		return 0, err
	}
	if _, err := os.Lstat(destPath); err == nil {
		return 0, fmt.Errorf("backup destination already exists: %s", destPath)
	} else if !os.IsNotExist(err) {
		return 0, fmt.Errorf("inspect backup destination %s: %w", destPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o700); err != nil {
		return 0, err
	}

	db, err := sql.Open("sqlite3", sourcePath+"?_busy_timeout=10000")
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", sourcePath, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, destPath); err != nil {
		// A partial destination is worse than none: it looks like a usable
		// backup while missing the pages the failure interrupted.
		_ = os.Remove(destPath)
		return 0, err
	}
	info, err := os.Stat(destPath)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

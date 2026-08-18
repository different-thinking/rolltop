// File overview: Integrity checks and backups of a database this process has
// open. In exclusive access mode the serving connection holds the file lock, so
// a second handle - the one the path-based helpers open - cannot read the file
// at all. Running both through the live connection keeps the admin Database page
// working there, and costs nothing in shared mode.

package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
)

// liveDatabase returns the open handle for a tenant, or for the system database
// when userID is zero. A tenant with no handle - never opened, or latched as
// corrupt - reports false, and the caller falls back to opening the file
// itself, which is safe precisely because nothing holds it.
func (s *Store) liveDatabase(userID int64) (*sql.DB, bool) {
	if s == nil {
		return nil, false
	}
	if userID == 0 || !s.split {
		return s.db, s.db != nil
	}
	s.mu.Lock()
	us := s.userStores[userID]
	s.mu.Unlock()
	if us == nil || us.db == nil {
		return nil, false
	}
	return us.db, true
}

// CheckDatabase runs quick_check against one database, preferring the handle
// this process already holds. userID zero checks the system database.
func (s *Store) CheckDatabase(ctx context.Context, userID int64, path string) ([]string, error) {
	db, live := s.liveDatabase(userID)
	if !live {
		return CheckDatabaseFile(ctx, path)
	}
	problems, err := quickCheck(ctx, db)
	if err != nil {
		if IsCorrupt(err) {
			// A file too damaged to scan reports the failure itself as the
			// problem rather than pretending the check did not run.
			return append(problems, summarizeProblem(err.Error())), nil
		}
		return problems, err
	}
	return problems, nil
}

// BackupDatabase writes a consistent copy of one database, preferring the handle
// this process already holds. userID zero backs up the system database.
func (s *Store) BackupDatabase(ctx context.Context, userID int64, path, destPath string) (int64, error) {
	db, live := s.liveDatabase(userID)
	if !live {
		return BackupDatabaseFile(ctx, path, destPath)
	}
	if err := requireDatabaseFile(path); err != nil {
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
	// VACUUM INTO reads one snapshot of the live database, so a backup taken
	// while sync writes is still internally consistent.
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

// AccessMode reports how this store reaches its files.
func (s *Store) AccessMode() AccessMode {
	if s == nil {
		return AccessShared
	}
	return s.access
}

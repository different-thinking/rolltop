// File overview: SQLite corruption classification, per-tenant corruption
// health, and the integrity probe used by "rolltop check-db". SQLITE_CORRUPT is
// a property of the file, not of the failing statement, so a bare "database
// disk image is malformed" line tells an operator nothing about which tenant
// database broke or how to repair it. Everything here exists to turn that
// driver message into a named file plus the offline command that repairs it.

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
)

// ErrCorrupt marks every error this package classifies as on-disk SQLite
// corruption, so callers can branch on it without importing the driver.
var ErrCorrupt = errors.New("sqlite database is corrupt")

// CorruptionError names the corrupt file and the command that repairs it.
type CorruptionError struct {
	UserID int64
	Path   string
	Err    error
}

func (e *CorruptionError) Error() string {
	subject := "system database"
	if e.UserID > 0 {
		subject = fmt.Sprintf("user %d database", e.UserID)
	}
	location := e.Path
	if strings.TrimSpace(location) == "" {
		location = "unknown path"
	}
	return fmt.Sprintf("%s %s is corrupt: %v; stop rolltop and run %s", subject, location, e.Err, e.repairCommand())
}

func (e *CorruptionError) repairCommand() string {
	if e.UserID > 0 {
		return fmt.Sprintf("\"rolltop recover-db --user-id %d --confirm-offline\"", e.UserID)
	}
	return "\"rolltop check-db --confirm-offline\""
}

func (e *CorruptionError) Unwrap() error { return e.Err }

// Is lets errors.Is(err, ErrCorrupt) and errors.Is(err, sql.ErrNoRows)-style
// checks work through the wrapper.
func (e *CorruptionError) Is(target error) bool { return target == ErrCorrupt }

func newCorruptionError(userID int64, path string, err error) *CorruptionError {
	return &CorruptionError{UserID: userID, Path: path, Err: err}
}

// IsCorrupt reports whether err came from an unreadable SQLite file. The driver
// error codes are authoritative; the message fallback catches errors that lost
// their type on the way up through a %v format verb.
func IsCorrupt(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrCorrupt) {
		return true
	}
	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code == sqlite3.ErrCorrupt || sqliteErr.Code == sqlite3.ErrNotADB
	}
	message := err.Error()
	for _, marker := range []string{
		"database disk image is malformed",
		"file is not a database",
		"database corruption",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// DatabaseHealth is the latched record of one tenant database that has already
// reported corruption in this process.
type DatabaseHealth struct {
	UserID     int64
	Path       string
	Detail     string
	DetectedAt time.Time
}

// NoteError latches tenant database corruption and rewrites err so the operator
// log names the broken file and the repair command. Errors that are not
// corruption are returned unchanged, so callers can wrap every store error they
// already log without changing existing behavior.
func (s *Store) NoteError(userID int64, err error) error {
	if s == nil || err == nil || !IsCorrupt(err) {
		return err
	}
	path := s.DatabaseFileForUser(userID)
	corrupt := newCorruptionError(userID, path, err)
	// open() already wrapped failures it saw during migration; keep that
	// message instead of nesting two identical repair instructions.
	var existing *CorruptionError
	if errors.As(err, &existing) {
		corrupt = existing
		if corrupt.UserID == 0 && userID > 0 {
			corrupt = newCorruptionError(userID, corrupt.Path, corrupt.Err)
		}
	}
	s.latchCorruption(userID, path, corrupt.Err.Error())
	return corrupt
}

// MarkCorrupt latches corruption that was found by inspecting a file rather
// than by a failing statement, such as the startup integrity check. It returns
// the same operator-facing error NoteError produces.
func (s *Store) MarkCorrupt(userID int64, detail string) error {
	if s == nil {
		return nil
	}
	path := s.DatabaseFileForUser(userID)
	s.latchCorruption(userID, path, detail)
	return newCorruptionError(userID, path, errors.New(detail))
}

// latchCorruption records the first corruption seen for one tenant. Later
// reports do not overwrite it, so the log keeps the original evidence.
func (s *Store) latchCorruption(userID int64, path, detail string) {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	if s.health == nil {
		s.health = make(map[int64]DatabaseHealth)
	}
	if _, latched := s.health[userID]; latched {
		return
	}
	s.health[userID] = DatabaseHealth{
		UserID:     userID,
		Path:       path,
		Detail:     detail,
		DetectedAt: time.Now().UTC(),
	}
}

// DatabaseCorrupt reports whether this process has already seen corruption for
// one tenant. Background loops use it to stop scheduling work that cannot
// succeed until the database is repaired offline.
func (s *Store) DatabaseCorrupt(userID int64) bool {
	if s == nil {
		return false
	}
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	_, corrupt := s.health[userID]
	return corrupt
}

// CorruptDatabases returns every latched corruption record, for admin and
// startup diagnostics.
func (s *Store) CorruptDatabases() []DatabaseHealth {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	records := make([]DatabaseHealth, 0, len(s.health))
	for _, record := range s.health {
		records = append(records, record)
	}
	return records
}

// DatabaseFileForUser returns the SQLite file that holds one tenant's mail
// data. Combined (test) stores keep everything in the receiver's own file.
func (s *Store) DatabaseFileForUser(userID int64) string {
	if !s.split || userID <= 0 {
		return s.path
	}
	dir := s.UserDataDir(userID)
	if dir == "" {
		return s.path
	}
	return filepath.Join(dir, databaseFilename)
}

// IntegrityCheck runs SQLite's quick_check against one tenant database and
// returns the reported problems. An empty slice means the file verified clean.
// quick_check reads every page, so this is an offline maintenance operation
// rather than something to run on each startup.
func (s *Store) IntegrityCheck(ctx context.Context, userID int64) ([]string, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, s.NoteError(userID, err)
	}
	problems, err := quickCheck(ctx, db)
	if err != nil {
		return nil, s.NoteError(userID, err)
	}
	return problems, nil
}

// CheckDatabaseFile runs quick_check against a SQLite file without migrating
// it. Offline maintenance uses it so inspecting a suspect database never writes
// schema changes into a file that is about to be recovered.
func CheckDatabaseFile(ctx context.Context, path string) ([]string, error) {
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	problems, err := quickCheck(ctx, db)
	if err != nil {
		if IsCorrupt(err) {
			// A file too damaged to scan reports the failure itself as the
			// problem rather than pretending the check did not run.
			return append(problems, err.Error()), nil
		}
		return problems, err
	}
	return problems, nil
}

// quickCheck reports the rows of PRAGMA quick_check other than the single "ok"
// row SQLite returns for an intact database.
func quickCheck(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA quick_check`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var problems []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return problems, err
		}
		if strings.EqualFold(strings.TrimSpace(line), "ok") {
			continue
		}
		problems = append(problems, line)
	}
	if err := rows.Err(); err != nil {
		return problems, err
	}
	return problems, nil
}

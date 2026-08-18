// File overview: How SQLite is allowed to reach its files. WAL normally keeps
// its index in a `-shm` file that every connection maps with MAP_SHARED, and it
// relies on POSIX byte-range locks. Neither is dependable on a network or FUSE
// filesystem - CephFS, NFS, virtiofs - where the mapping is not guaranteed
// coherent, and an incoherent WAL index corrupts the database within hours.
// SQLite supports WAL without shared memory as long as one connection holds the
// file exclusively, which is exactly the shape Rolltop already has: the instance
// lock guarantees a single process per data directory.

package store

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/mattn/go-sqlite3"
)

// exclusiveDriverName is a second registration of the same SQLite driver whose
// connect hook turns WAL on after the connection is built.
//
// The order is the whole point. SQLite only leaves the shared index out of a WAL
// database when EXCLUSIVE locking is set before the first WAL access, and the
// driver applies its DSN pragmas in its own fixed order - journal_mode first,
// locking_mode after it. Asking for both in the DSN therefore still creates a
// `-shm` file for a database that is already in WAL mode, which is every
// database that has ever run. Leaving journal_mode out of the DSN and setting it
// from the hook, which runs last, puts the locking mode first.
const exclusiveDriverName = "sqlite3-rolltop-exclusive"

func init() {
	sql.Register(exclusiveDriverName, &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			_, err := conn.Exec("PRAGMA journal_mode = WAL;", nil)
			return err
		},
	})
}

// AccessMode selects how SQLite coordinates access to a database file.
type AccessMode int

const (
	// AccessAuto picks a mode from the filesystem carrying the data directory.
	AccessAuto AccessMode = iota
	// AccessShared is ordinary WAL: a `-shm` index and several connections per
	// database, so readers do not queue behind the sync writer.
	AccessShared
	// AccessExclusive is WAL without shared memory. SQLite keeps the index in
	// heap memory and holds the file lock for as long as the connection lives,
	// which means exactly one connection per database.
	AccessExclusive
)

func (m AccessMode) String() string {
	switch m {
	case AccessShared:
		return "shared"
	case AccessExclusive:
		return "exclusive"
	default:
		return "auto"
	}
}

// ParseAccessMode reads the operator-facing value.
func ParseAccessMode(raw string) (AccessMode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "auto":
		return AccessAuto, nil
	case "shared", "normal":
		return AccessShared, nil
	case "exclusive":
		return AccessExclusive, nil
	default:
		return AccessAuto, fmt.Errorf("%q must be %q, %q, or %q", raw, "auto", "shared", "exclusive")
	}
}

// driverName selects the driver registration that carries the WAL pragma for
// this mode.
func (m AccessMode) driverName() string {
	if m == AccessExclusive {
		return exclusiveDriverName
	}
	return "sqlite3"
}

// dataSourceName builds the DSN for one database file.
//
// BEGIN IMMEDIATE queues transactions for SQLite's single writer at the start,
// instead of allowing concurrent readers to deadlock while both try to upgrade a
// stale WAL snapshot to a writer.
func (m AccessMode) dataSourceName(path string) string {
	dsn := path + "?_foreign_keys=on&_busy_timeout=5000&_synchronous=NORMAL&_txlock=immediate"
	if m == AccessExclusive {
		// journal_mode is deliberately absent here and set by the connect hook
		// instead, so that this lands before the first WAL access.
		return dsn + "&_locking_mode=exclusive"
	}
	return dsn + "&_journal_mode=WAL"
}

// applyConnectionLimits sizes the pool for the mode.
func (m AccessMode) applyConnectionLimits(db *sql.DB) {
	if m == AccessExclusive {
		// The connection holds the file lock, so a second one could only wait
		// for a lock the first never releases. Every query for this database
		// queues on one connection instead.
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		return
	}
	// SQLite permits exactly one writer per database. The sync runner serializes
	// those writer turns per tenant; retain several additional connections so
	// message rendering, account settings, and sender decoration reads can take
	// WAL snapshots without queueing behind the active mirror writer. Separate
	// users still have separate Store instances.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
}

// SharesFiles reports whether another process may open these databases while
// Rolltop serves. Online backups and integrity checks from a second handle are
// impossible in exclusive mode, so callers route them through the live one.
func (m AccessMode) SharesFiles() bool { return m != AccessExclusive }

// FilesystemReport describes the storage under a data directory.
type FilesystemReport struct {
	// Name is the filesystem as identified from its superblock, or "unknown".
	Name string
	// SharedMemorySafe reports whether SQLite's WAL index can be trusted there.
	SharedMemorySafe bool
}

// ResolveAccessMode turns a configured mode into the one to use, and describes
// what it found. An explicit choice is never overridden: an operator who knows
// their storage outranks a superblock lookup.
func ResolveAccessMode(mode AccessMode, dataDir string) (AccessMode, FilesystemReport) {
	report := DetectFilesystem(dataDir)
	if mode != AccessAuto {
		return mode, report
	}
	if report.SharedMemorySafe {
		return AccessShared, report
	}
	return AccessExclusive, report
}

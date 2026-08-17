// File overview: Store-level error helpers.

package store

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
)

var ErrDuplicateMailboxRole = errors.New("mailbox role already assigned")
var ErrInvalidMailboxSettings = errors.New("invalid mailbox settings")
var ErrInvalidSwipePreferences = errors.New("invalid swipe preferences")
var ErrMailboxGenerationChanged = errors.New("mailbox generation changed")
var ErrMailboxGenerationArrivalUIDFloorRequired = errors.New("mailbox generation reset requires an arrival UID floor")

// IsNotFound normalizes sql.ErrNoRows checks across store and web packages.
func IsNotFound(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}

// IsClosed reports that a query failed because its database is gone: the store
// was closed while a background worker was still running. Such a worker has
// nothing left to read, nothing to retry, and nothing an operator could act on,
// so callers treat this as "stop quietly" rather than as a failure.
func IsClosed(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, sql.ErrConnDone) || errors.Is(err, driver.ErrBadConn) {
		return true
	}
	// database/sql keeps its closed-database error unexported, so the text is
	// the only handle on it.
	return strings.Contains(err.Error(), "sql: database is closed")
}

// WrapNotFound converts sql.ErrNoRows to the store package sentinel used by callers.
func WrapNotFound(thing string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s: %w", thing, ErrNotFound)
	}
	return err
}

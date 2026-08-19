// File overview: Store-level error helpers.
//
// PostgreSQL narrowed what callers have to distinguish (WP4 of
// docs/postgres-migration-plan.md). SQLite made "the file is damaged" and "the
// writer is busy" everyday conditions with their own recovery paths; neither
// exists here. What remains is three questions: did this find nothing, did the
// database refuse the write, and is the database reachable at all.

package store

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
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

// uniqueViolation is PostgreSQL's SQLSTATE for a duplicate key.
const uniqueViolation = "23505"

// IsUniqueConstraint reports that a write lost a race for a unique key: the row
// it wanted to insert is already there. Callers that look a row up and then
// create it when it was missing need this, because nothing stops a second
// worker from creating the same row between those two statements.
//
// The SQLSTATE is matched rather than the message, which the SQLite version had
// to do: PostgreSQL localizes error text, so an operator running a server with
// a non-English lc_messages would otherwise silently lose every one of these
// checks and see the race as a failed sync instead.
func IsUniqueConstraint(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == uniqueViolation
	}
	return false
}

// IsUniqueConstraintOn reports a duplicate-key failure against one named
// constraint, for callers that recover from exactly one collision and must not
// swallow another.
//
// The name is the constraint's, as PostgreSQL generates it from the table and
// its columns: a `UNIQUE(user_id, account_id, mailbox_id, uid)` on `messages`
// becomes `messages_user_id_account_id_mailbox_id_uid_key`. Matching on it is
// what keeps "this UID is already mirrored, adopt the existing row" from also
// catching, say, a duplicate message-id hash and adopting the wrong message.
func IsUniqueConstraintOn(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == uniqueViolation && pgErr.ConstraintName == constraint
}

// MessagesUIDConstraint names the uniqueness of one mailbox UID per tenant
// account. Reaching for it means "this message is already mirrored".
const MessagesUIDConstraint = "messages_user_id_account_id_mailbox_id_uid_key"

// IsUnavailable reports that the database could not be reached — it is down,
// failing over, or the network to it is broken — as opposed to having answered
// with a refusal.
//
// The distinction decides who is told. An unreachable database is an
// operational condition that resolves itself: background work should log it
// quietly and retry, and the web layer should serve the app shell with a
// "database unavailable" banner rather than a 500, so the admin page stays
// reachable while it recovers. Anything the server did answer — a constraint
// violation, a syntax error, a missing column — is a bug in this program and
// must not be dressed up as a temporary outage.
func IsUnavailable(err error) bool {
	if err == nil {
		return false
	}
	// A PgError means the server received the statement and rejected it, so
	// whatever went wrong, reachability is not it. The two exceptions are the
	// classes the server itself raises while going down or refusing new work.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case strings.HasPrefix(pgErr.Code, "08"): // connection exception
			return true
		case strings.HasPrefix(pgErr.Code, "57"): // operator intervention: shutdown, cannot connect now
			return true
		default:
			return false
		}
	}
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var connectErr *pgconn.ConnectError
	return errors.As(err, &connectErr)
}

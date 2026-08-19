// File overview: What the error classifiers are allowed to mean.

package store

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestIsClosedMeansOnlyAClosedHandle is the guard on a permanent decision.
// Every caller leaves a background loop for the rest of the process lifetime
// when this returns true, so a database restart — which surfaces as a bad
// connection on a statement in flight — must not reach it.
func TestIsClosedMeansOnlyAClosedHandle(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"closed database", errors.New("sql: database is closed"), true},
		{"wrapped closed database", fmt.Errorf("list users: %w", errors.New("sql: database is closed")), true},
		{"bad connection", driver.ErrBadConn, false},
		{"connection done", sql.ErrConnDone, false},
		{"server shutting down", &pgconn.PgError{Code: "57P01"}, false},
		{"no rows", sql.ErrNoRows, false},
	} {
		if got := IsClosed(tc.err); got != tc.want {
			t.Errorf("IsClosed(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestIsUnavailableCoversTheTransientFailures is the other half: what IsClosed
// hands back has to be recognised as retryable by something, or a failover
// would be reported to the user as a bug in this program.
func TestIsUnavailableCoversTheTransientFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"bad connection", driver.ErrBadConn, true},
		{"connection done", sql.ErrConnDone, true},
		{"connection exception", &pgconn.PgError{Code: "08006"}, true},
		{"server shutting down", &pgconn.PgError{Code: "57P01"}, true},
		{"unique violation", &pgconn.PgError{Code: uniqueViolation}, false},
		{"undefined column", &pgconn.PgError{Code: "42703"}, false},
	} {
		if got := IsUnavailable(tc.err); got != tc.want {
			t.Errorf("IsUnavailable(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestIsUniqueConstraintMatchesSQLState(t *testing.T) {
	if !IsUniqueConstraint(fmt.Errorf("insert: %w", &pgconn.PgError{Code: uniqueViolation})) {
		t.Error("a wrapped duplicate key was not recognised")
	}
	// The SQLSTATE rather than the message, so a server with a non-English
	// lc_messages classifies the race the same way.
	if IsUniqueConstraint(errors.New("duplicate key value violates unique constraint")) {
		t.Error("a bare message was treated as a duplicate key")
	}
}

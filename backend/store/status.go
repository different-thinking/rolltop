// File overview: What the admin database page asks the database about itself.
//
// This replaces the integrity check. Under SQLite the honest health question
// was "are these pages still readable", and answering it meant reading every
// one of them. PostgreSQL answers a different and much cheaper set of
// questions: is it reachable, is it a standby that would reject writes, how
// much room is the data taking, and how long does one round trip cost — the
// last of which is the number §8.2 of docs/postgres-migration-plan.md budgets
// the syncer's loops against.

package store

import (
	"context"
	"time"
)

// Status is the database's answer about itself.
type Status struct {
	// ServerVersion is the server's own version string.
	ServerVersion string
	// Bytes is pg_database_size for the connected database.
	Bytes int64
	// InRecovery reports a standby. Writes fail against one, so the page has
	// to be able to say so rather than letting every sync fail mysteriously.
	InRecovery bool
	// Connections counts the backends this role currently has open, across
	// every process — this server, a pg_dump, somebody's psql.
	Connections int
	// RoundTrip is how long the status query took, network included.
	RoundTrip time.Duration
}

// DatabaseStatus queries the server. Errors are *PostgresError, so the DSN
// cannot reach the admin page through the message.
func (s *Store) DatabaseStatus(ctx context.Context) (Status, error) {
	started := time.Now()
	var status Status
	err := s.db.QueryRowContext(ctx, `
		SELECT version(),
		       pg_database_size(current_database()),
		       pg_is_in_recovery(),
		       (SELECT count(*) FROM pg_stat_activity WHERE usename = current_user)`).
		Scan(&status.ServerVersion, &status.Bytes, &status.InRecovery, &status.Connections)
	if err != nil {
		return Status{}, postgresError("read the database status", err)
	}
	status.RoundTrip = time.Since(started)
	return status, nil
}

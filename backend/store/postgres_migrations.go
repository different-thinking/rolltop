// File overview: Incremental schema migrations layered on the frozen baseline.
// This is the upgrade path §WP7 of docs/postgres-migration-plan.md left open:
// the baseline's checksum stays the recorded identity of a database's origin,
// and every schema change after the first durable database is a numbered entry
// here. The model is plugin_migrations — one checksummed row per migration,
// applied under the schema lock only when something is outstanding — with the
// rows kept in schema_migrations under the existing "postgres" scope so a
// database created before this mechanism needs nothing retrofitted.

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// postgresMigration is one schema change layered on the baseline. Shipped
// entries are immutable: the checksum recorded at apply time is re-verified on
// every start, and editing an applied entry is refused the same way an edited
// baseline is.
type postgresMigration struct {
	// Version orders and identifies the migration ("0001-message-search").
	// Zero-padded so the lexicographic order in the database matches the list.
	Version string
	// Statements run in order inside one implicit transaction, together with
	// the INSERT that records the row — all or nothing, like the baseline.
	Statements []string
}

// postgresMigrations is append-only. The list order is the apply order, and a
// database's applied rows must always be a prefix of it: this binary refuses
// rows it does not know (a newer binary wrote them) and gaps it cannot explain.
var postgresMigrations = []postgresMigration{
	{
		// The full-text search rows for the Postgres search backend
		// (docs/search-postgres-plan.md §3). Deliberately narrow: volatile
		// flags and the mailbox stay in messages and are joined at query time,
		// and deleting a message deletes its search row through the cascade.
		Version: "0001-message-search",
		Statements: []string{
			`CREATE TABLE message_search (
				message_id bigint PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
				user_id bigint NOT NULL,
				tsv tsvector NOT NULL
			)`,
			`CREATE INDEX idx_message_search_tsv ON message_search USING GIN (tsv)`,
			`CREATE INDEX idx_message_search_user ON message_search (user_id)`,
		},
	},
	{
		// The fuzzy-match word list beside the vector: the distinct normalized
		// words of the indexed text, probed with pg_trgm word similarity. Only
		// the column is a migration — the extension and its trigram index are
		// runtime-optional (EnsureTrigramSearch), because a hoster may not
		// allow CREATE EXTENSION and search must degrade to exact matching
		// then, not refuse to start.
		Version: "0002-message-search-words",
		Statements: []string{
			`ALTER TABLE message_search ADD COLUMN words text NOT NULL DEFAULT ''`,
		},
	},
	{
		// Gmail submission moves from implicit TLS on 465 to STARTTLS on 587
		// (gmailSMTPPort, backend/web/api_account.go). The endpoint was written
		// by the server rather than typed, so an account saved before this
		// change carries a port its owner never chose, and on a network that
		// blocks 465 it fails with a connection that times out. Only rows that
		// still carry the written endpoint are moved: a host somebody entered
		// themselves is theirs. The move is not a one-way door -- 465 is
		// offered in the settings for the network where it is 587 that is
		// blocked -- so a row this puts on the wrong port can be put back.
		//
		// Both tables hold the endpoint. smtp_accounts is the outgoing server,
		// and it is the row every send builds its envelope from.
		// mail_accounts.smtp_* is what an incoming account was saved with, and
		// it is copied into a new outgoing server when a user who has none
		// saves a mailbox (ensureMailAccountOnboarding, api_account.go).
		// Moving only the first would let the old port arrive in smtp_accounts
		// afterwards, through a row this migration can no longer reach.
		Version: "0003-gmail-submission-port",
		Statements: []string{
			`UPDATE smtp_accounts SET port = 587
				WHERE auth_type = 'google_oauth' AND host = 'smtp.gmail.com' AND port = 465`,
			`UPDATE mail_accounts SET smtp_port = 587
				WHERE auth_type = 'google_oauth' AND smtp_host = 'smtp.gmail.com' AND smtp_port = 465`,
		},
	},
}

func postgresMigrationChecksum(m postgresMigration) string {
	return schemaChecksum(postgresSchemaScope, m.Version, m.Statements...)
}

// postgresSchemaState is what one read of schema_migrations says about this
// database, classified against the binary's baseline checksum and migration
// list. Errors carry the disagreements; the ordinary cases are the two bools
// and the outstanding suffix.
type postgresSchemaState struct {
	// BaselinePresent reports a matching baseline row. False with a nil error
	// means the empty-database case.
	BaselinePresent bool
	// Outstanding holds the migrations this database has not applied yet, in
	// apply order. Meaningful only when BaselinePresent is true; a fresh
	// database gets the whole list after its baseline.
	Outstanding []postgresMigration
}

// readPostgresSchemaState classifies the database in a single query, so the
// every-restart fast path stays one read: baseline row and migration rows come
// back together, and only a database with work to do takes the schema lock.
func readPostgresSchemaState(ctx context.Context, conn *sql.Conn, baselineChecksum string, migrations []postgresMigration) (postgresSchemaState, error) {
	var table sql.NullString
	if err := conn.QueryRowContext(ctx, `SELECT to_regclass($1)::text`,
		schemaMigrationsQualified()).Scan(&table); err != nil {
		return postgresSchemaState{}, postgresError("inspect the schema", err)
	}
	if !table.Valid {
		return postgresSchemaState{Outstanding: migrations}, nil
	}
	rows, err := conn.QueryContext(ctx,
		`SELECT version, checksum FROM schema_migrations WHERE scope = $1`, postgresSchemaScope)
	if err != nil {
		return postgresSchemaState{}, postgresError("read the schema version", err)
	}
	defer rows.Close()
	applied := make(map[string]string)
	for rows.Next() {
		var version, checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return postgresSchemaState{}, postgresError("read the schema version", err)
		}
		applied[version] = checksum
	}
	if err := rows.Err(); err != nil {
		return postgresSchemaState{}, postgresError("read the schema version", err)
	}
	return classifyPostgresSchemaState(applied, baselineChecksum, migrations)
}

// classifyPostgresSchemaState turns the recorded rows into a decision. It is
// split from the read so the refusal cases can be tested without a database.
func classifyPostgresSchemaState(applied map[string]string, baselineChecksum string, migrations []postgresMigration) (postgresSchemaState, error) {
	recorded, baselinePresent := applied[postgresSchemaVersion]
	if baselinePresent && recorded != baselineChecksum {
		return postgresSchemaState{}, errors.New("postgres: schema baseline checksum mismatch: this database was created from a different baseline than the running binary carries, and there is no upgrade path between the two")
	}
	if !baselinePresent {
		if len(applied) > 0 {
			// Rows without a baseline is not a state any Rolltop start can
			// produce; whatever wrote them, this database is not ours to use.
			return postgresSchemaState{}, fmt.Errorf("postgres: schema_migrations carries %d row(s) but no baseline; this database was not created by Rolltop", len(applied))
		}
		return postgresSchemaState{Outstanding: migrations}, nil
	}
	known := make(map[string]bool, len(migrations)+1)
	known[postgresSchemaVersion] = true
	for _, m := range migrations {
		known[m.Version] = true
	}
	var unknown []string
	for version := range applied {
		if !known[version] {
			unknown = append(unknown, version)
		}
	}
	if len(unknown) > 0 {
		return postgresSchemaState{}, fmt.Errorf("postgres: the database has applied migration(s) this binary does not know (%s); it was written by a newer build, and running an older one against it is how data gets lost", strings.Join(sortedStrings(unknown), ", "))
	}
	firstOutstanding := -1
	for i, m := range migrations {
		checksum, isApplied := applied[m.Version]
		if !isApplied {
			if firstOutstanding < 0 {
				firstOutstanding = i
			}
			continue
		}
		if firstOutstanding >= 0 {
			return postgresSchemaState{}, fmt.Errorf("postgres: migration %s is applied but earlier migration %s is not; the migration history of this database cannot be explained", m.Version, migrations[firstOutstanding].Version)
		}
		if checksum != postgresMigrationChecksum(m) {
			return postgresSchemaState{}, fmt.Errorf("postgres: migration %s was edited after this database applied it; shipped migrations are immutable", m.Version)
		}
	}
	state := postgresSchemaState{BaselinePresent: true}
	if firstOutstanding >= 0 {
		state.Outstanding = migrations[firstOutstanding:]
	}
	return state, nil
}

// applyPostgresMigration runs one migration as a single simple-protocol script
// whose last statement records its row — the same atomicity argument as
// applyPostgresBaseline: DDL without its row would read back as tampering on
// the next start, so both land in the same implicit transaction or not at all.
func applyPostgresMigration(ctx context.Context, conn *sql.Conn, m postgresMigration) error {
	var script strings.Builder
	for _, stmt := range m.Statements {
		script.WriteString(strings.TrimSpace(stmt))
		script.WriteString(";\n")
	}
	script.WriteString(recordMigrationStatement(m))
	if err := execPostgresScript(ctx, conn, script.String()); err != nil {
		return postgresError(fmt.Sprintf("apply schema migration %s", m.Version), err)
	}
	return nil
}

// recordMigrationStatement renders the schema_migrations row as literal SQL,
// inlined for the simple protocol exactly as recordBaselineStatement is.
func recordMigrationStatement(m postgresMigration) string {
	return fmt.Sprintf(
		`INSERT INTO schema_migrations (scope, version, applied_at, checksum) VALUES (%s, %s, %d, %s);`,
		quoteSQLLiteral(postgresSchemaScope), quoteSQLLiteral(m.Version), nowUnix(), quoteSQLLiteral(postgresMigrationChecksum(m)))
}

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

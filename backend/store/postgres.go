// File overview: The PostgreSQL side of the storage migration (WP1 of
// docs/postgres-migration-plan.md). It owns connecting to the server, sizing
// the pool, and putting the generated baseline schema into an empty database.
//
// It deliberately does not run the SQLite migration chain. PostgreSQL gets the
// squashed baseline from backend/store/pgschema instead, recorded as one row in
// the same schema_migrations table the SQLite runner uses, so drift is caught by
// the same checksum rule rather than a second mechanism.
//
// Every error leaving this file goes through pgdsn.Redact: pgx echoes the
// connection string from several of its own error paths, and the DSN carries the
// database password.

package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/stdlib"

	"rolltop/backend/pgdsn"
	"rolltop/backend/plugins"
	"rolltop/backend/store/pgschema"
)

const (
	// postgresSchemaScope and postgresSchemaVersion identify the squashed
	// baseline in schema_migrations. The version is not a sequence number: the
	// baseline is regenerated in place from the SQLite schema, and a change to
	// it shows up as a checksum mismatch, not as a new version.
	postgresSchemaScope   = "postgres"
	postgresSchemaVersion = "baseline"

	// defaultPostgresMaxConns sizes the pool when the caller sets none. The
	// hosted target allows 20 connections per role, so half of that leaves room
	// for the migration tool, a scheduled pg_dump, and a manual psql session.
	defaultPostgresMaxConns = 10

	// schemaAdvisoryLock serializes the create-the-schema window across
	// processes. Two servers started against the same empty database would
	// otherwise both find it empty and both apply the baseline; the loser
	// crashes on the first duplicate object. The value is arbitrary but fixed,
	// and shares PostgreSQL's single advisory-lock space, so it is picked to be
	// unlikely to collide with another application's.
	schemaAdvisoryLock int64 = 0x726F6C6C746F7000 // "rolltop\0"
)

// PostgresOptions carries what OpenPostgres needs beyond the DSN.
type PostgresOptions struct {
	// MaxConns caps the pool. Zero selects a default sized for the hosted
	// per-role connection limit.
	MaxConns int
	// Manifests supplies the plugin catalog, matching ServerOptions.
	Manifests []plugins.Manifest
	// Progress receives schema progress during startup.
	Progress MigrationReporter
}

// OpenPostgres connects to a PostgreSQL database and makes sure it carries the
// generated baseline schema.
//
// The store it returns is not yet a drop-in replacement for the SQLite one: the
// query layer still speaks SQLite's dialect and is converted per package in WP3.
// What this function establishes is the layer underneath that work — a pool, a
// schema, and a recorded schema version — so the conversion has something to run
// its tests against.
func OpenPostgres(ctx context.Context, dsn string, opts PostgresOptions) (*Store, error) {
	if dsn == "" {
		return nil, errors.New("postgres: empty connection string")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, postgresError("open", err)
	}
	maxConns := opts.MaxConns
	if maxConns <= 0 {
		maxConns = defaultPostgresMaxConns
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	// Server-side idle timeouts and connection poolers both drop connections a
	// long-lived pool would otherwise hand out dead. An hour is far below any
	// such timeout while still keeping connection churn negligible.
	db.SetConnMaxLifetime(time.Hour)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, postgresError("connect", err)
	}
	catalog := pluginCatalogFromManifests(opts.Manifests)
	// Not split: the plan's decision 3.1 puts every tenant in one database, so
	// dataDB resolves to this pool for every user.
	s := &Store{
		db:                db,
		schema:            schemaCombined,
		pluginDefinitions: append([]plugins.Definition(nil), catalog.definitions...),
		pluginMigrations:  append([]plugins.Migration(nil), catalog.migrations...),
	}
	if err := s.ensurePostgresSchema(ctx, opts.Progress); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// ensurePostgresSchema applies the baseline to an empty database and verifies it
// on every later start.
//
// Three states are distinguished rather than collapsed into "apply if missing",
// because the two failure states are the ones that lose data quietly: pointing
// the server at somebody else's database, and running a binary whose baseline no
// longer matches the schema in the database.
//
// All of it runs on one pinned connection under an advisory lock, so a second
// process starting at the same moment waits for the first to finish and then
// sees a complete schema rather than a half-created one.
func (s *Store) ensurePostgresSchema(ctx context.Context, progress MigrationReporter) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return postgresError("acquire connection", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, schemaAdvisoryLock); err != nil {
		return postgresError("take the schema lock", err)
	}
	// Closing the connection releases a session lock too, but unlocking
	// explicitly returns it to the pool usable instead of holding the lock for
	// as long as the pool keeps the connection idle.
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(unlockCtx, `SELECT pg_advisory_unlock($1)`, schemaAdvisoryLock)
	}()

	checksum := baselineChecksum()
	recorded, present, err := postgresBaselineRow(ctx, conn)
	if err != nil {
		return err
	}
	if present {
		if recorded != checksum {
			return errors.New("postgres: schema baseline checksum mismatch: the database was created from a different baseline than this binary carries")
		}
		reportMigration(progress, MigrationProgress{Scope: postgresSchemaScope, Migration: postgresSchemaVersion, Step: "already applied", Done: 1, Total: 1})
		return nil
	}
	empty, err := postgresSchemaIsEmpty(ctx, conn)
	if err != nil {
		return err
	}
	if !empty {
		return errors.New("postgres: the target database already contains tables but no recorded Rolltop baseline; point the server at an empty database")
	}
	reportMigration(progress, MigrationProgress{Scope: postgresSchemaScope, Migration: postgresSchemaVersion, Step: "create schema", Done: 0, Total: 1})
	if err := applyPostgresBaseline(ctx, conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO schema_migrations (scope, version, applied_at, checksum) VALUES ($1, $2, $3, $4)`,
		postgresSchemaScope, postgresSchemaVersion, nowUnix(), checksum); err != nil {
		return postgresError("record the baseline", err)
	}
	reportMigration(progress, MigrationProgress{Scope: postgresSchemaScope, Migration: postgresSchemaVersion, Step: "complete", Done: 1, Total: 1})
	return nil
}

// postgresBaselineRow reads the recorded baseline checksum. A database without a
// schema_migrations table is reported as "no row" rather than as an error, since
// that is the ordinary empty-database case.
func postgresBaselineRow(ctx context.Context, conn *sql.Conn) (string, bool, error) {
	var table sql.NullString
	if err := conn.QueryRowContext(ctx, `SELECT to_regclass('public.schema_migrations')::text`).Scan(&table); err != nil {
		return "", false, postgresError("inspect the schema", err)
	}
	if !table.Valid {
		return "", false, nil
	}
	var checksum string
	err := conn.QueryRowContext(ctx,
		`SELECT checksum FROM schema_migrations WHERE scope = $1 AND version = $2`,
		postgresSchemaScope, postgresSchemaVersion).Scan(&checksum)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, postgresError("read the schema version", err)
	}
	return checksum, true, nil
}

func postgresSchemaIsEmpty(ctx context.Context, conn *sql.Conn) (bool, error) {
	var tables int
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_type = 'BASE TABLE'`).Scan(&tables); err != nil {
		return false, postgresError("count the tables", err)
	}
	return tables == 0, nil
}

// applyPostgresBaseline runs the whole baseline as one simple-protocol query.
//
// database/sql sends statements over the extended protocol, which accepts one
// statement per call, so the baseline would have to be split on semicolons —
// and it contains a dollar-quoted function body full of them. Handing the script
// to pgx directly avoids inventing that parser, and PostgreSQL wraps a
// multi-statement simple query in one implicit transaction, so a failure halfway
// through leaves no partial schema behind.
func applyPostgresBaseline(ctx context.Context, conn *sql.Conn) error {
	return conn.Raw(func(driverConn any) error {
		c, ok := driverConn.(*stdlib.Conn)
		if !ok {
			return fmt.Errorf("postgres: unexpected driver connection %T", driverConn)
		}
		if _, err := c.Conn().Exec(ctx, pgschema.Baseline); err != nil {
			return postgresError("apply the schema baseline", err)
		}
		return nil
	})
}

// postgresError attaches what failed while keeping the driver's own message,
// with any credential material the driver echoed removed.
func postgresError(what string, err error) error {
	return fmt.Errorf("postgres: %s: %s", what, pgdsn.Redact(err.Error()))
}

func baselineChecksum() string {
	sum := sha256.Sum256([]byte(pgschema.Baseline))
	return hex.EncodeToString(sum[:])
}

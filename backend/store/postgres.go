// File overview: The PostgreSQL side of the storage migration (WP1 of
// docs/postgres-migration-plan.md). It owns connecting to the server, sizing
// the pool, and putting the generated baseline schema into an empty database.
//
// It deliberately does not run the SQLite migration chain. PostgreSQL gets the
// squashed baseline from backend/store/pgschema instead, recorded as one row in
// the same schema_migrations table and with the same checksum function the
// SQLite runner uses, so drift is caught by one rule rather than two.
//
// Every error leaving this file is a *PostgresError, which redacts the DSN when
// printed and keeps the driver's error reachable through errors.Is/As: pgx
// quotes the whole connection string back from its parse paths, and the DSN
// carries the database password.

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/stdlib"

	"rolltop/backend/pgdsn"
	"rolltop/backend/plugins"
	"rolltop/backend/sqlident"
	"rolltop/backend/store/pgschema"
)

const (
	// postgresSchemaScope and postgresSchemaVersion identify the squashed
	// baseline in schema_migrations. The version is not a sequence number: the
	// baseline is regenerated in place from the SQLite schema, so a change to it
	// shows up as a checksum mismatch rather than as a new version. That is the
	// right behaviour while every PostgreSQL database is a throwaway test one;
	// the first durable database needs the upgrade path recorded under WP7 in
	// docs/postgres-migration-plan.md, because a regenerated baseline would
	// otherwise refuse to start against a database that is merely older.
	postgresSchemaScope   = "postgres"
	postgresSchemaVersion = "baseline"

	// defaultPostgresMaxConns sizes the pool when the caller sets none. The
	// hosted target allows 20 connections per role, so half of that leaves room
	// for the migration tool, a scheduled pg_dump, and a manual psql session.
	defaultPostgresMaxConns = 10

	// postgresConnMaxLifetime bounds how long a pooled connection is reused.
	// Server-side idle timeouts and connection poolers both drop connections a
	// long-lived pool would otherwise hand out dead.
	postgresConnMaxLifetime = time.Hour

	// postgresConnMaxIdleTime returns connections the pool is no longer using.
	// Without it a single burst pins the whole pool idle for a full lifetime,
	// and during a rolling deploy the outgoing and incoming processes together
	// hold the role's entire connection budget — including the headroom
	// defaultPostgresMaxConns exists to leave.
	postgresConnMaxIdleTime = 5 * time.Minute

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
	db.SetConnMaxLifetime(postgresConnMaxLifetime)
	db.SetConnMaxIdleTime(postgresConnMaxIdleTime)
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
// The recorded-and-matching case — every start after the first — is answered
// with one read and no locking. Only a database with no baseline row takes the
// advisory lock, and then re-reads under it, so ordinary restarts never queue
// behind each other on a server-wide lock.
func (s *Store) ensurePostgresSchema(ctx context.Context, progress MigrationReporter) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return postgresError("acquire a connection", err)
	}
	defer func() { _ = conn.Close() }()
	if err := pinPostgresSearchPath(ctx, conn); err != nil {
		return err
	}

	checksum := baselineChecksum()
	present, err := verifyPostgresBaseline(ctx, conn, checksum)
	if err != nil {
		return err
	}
	if present {
		reportMigration(progress, MigrationProgress{Scope: postgresSchemaScope, Migration: postgresSchemaVersion, Step: "already applied", Done: 1, Total: 1})
		return nil
	}

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

	// Another process may have created the schema while this one waited.
	present, err = verifyPostgresBaseline(ctx, conn, checksum)
	if err != nil {
		return err
	}
	if present {
		reportMigration(progress, MigrationProgress{Scope: postgresSchemaScope, Migration: postgresSchemaVersion, Step: "already applied", Done: 1, Total: 1})
		return nil
	}
	blocking, err := postgresBlockingObjects(ctx, conn)
	if err != nil {
		return err
	}
	if len(blocking) > 0 {
		return fmt.Errorf("postgres: the target database already contains objects but no recorded Rolltop baseline (%s); point the server at an empty database",
			describeBlockingObjects(blocking))
	}
	reportMigration(progress, MigrationProgress{Scope: postgresSchemaScope, Migration: postgresSchemaVersion, Step: "create schema", Done: 0, Total: 1})
	if err := applyPostgresBaseline(ctx, conn, checksum); err != nil {
		return err
	}
	reportMigration(progress, MigrationProgress{Scope: postgresSchemaScope, Migration: postgresSchemaVersion, Step: "complete", Done: 1, Total: 1})
	return nil
}

// verifyPostgresBaseline reports whether the database already carries this
// binary's baseline. A database without a schema_migrations table, or with one
// that has no baseline row, is reported as absent rather than as an error, since
// that is the ordinary empty-database case. A row that disagrees is an error:
// serving a schema this binary was not built for is how data gets lost.
func verifyPostgresBaseline(ctx context.Context, conn *sql.Conn, checksum string) (bool, error) {
	stage, _, err := postgresStage(ctx, conn, checksum)
	if err != nil {
		return false, err
	}
	switch stage {
	case PostgresStageBaseline:
		return true, nil
	case PostgresStageMismatch:
		return false, errors.New("postgres: schema baseline checksum mismatch: this database was created from a different baseline than the running binary carries, and there is no upgrade path between the two yet")
	default:
		return false, nil
	}
}

// postgresStage classifies a database without judging it, which is what the
// admin migration console needs: it has to describe a mismatched or foreign
// database rather than refuse to look at one.
//
// The distinction between "no baseline row" and "no baseline row but objects
// present" is the load-bearing one. Both are "not ours to use"; only the first
// can be created into.
func postgresStage(ctx context.Context, conn *sql.Conn, checksum string) (string, int64, error) {
	// The qualifier is derived rather than written out, so this classification
	// and pinPostgresSearchPath can never read and write different namespaces.
	var table sql.NullString
	if err := conn.QueryRowContext(ctx, `SELECT to_regclass($1)::text`,
		pgschema.Schema+".schema_migrations").Scan(&table); err != nil {
		return "", 0, postgresError("inspect the schema", err)
	}
	if table.Valid {
		var recorded string
		var appliedAt int64
		err := conn.QueryRowContext(ctx,
			`SELECT checksum, applied_at FROM schema_migrations WHERE scope = $1 AND version = $2`,
			postgresSchemaScope, postgresSchemaVersion).Scan(&recorded, &appliedAt)
		if err == nil {
			if recorded == checksum {
				return PostgresStageBaseline, appliedAt, nil
			}
			return PostgresStageMismatch, appliedAt, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", 0, postgresError("read the schema version", err)
		}
	}
	blocking, err := postgresBlockingObjects(ctx, conn)
	if err != nil {
		return "", 0, err
	}
	if len(blocking) == 0 {
		return PostgresStageEmpty, 0, nil
	}
	return PostgresStageForeign, 0, nil
}

// maxListedBlockingObjects bounds how many object names an error or a console
// summary names. Enough to recognise what is in the way, not so many that the
// message becomes a schema dump.
const maxListedBlockingObjects = 10

// postgresBlockingObjects lists the objects that stop this database from being
// created into, schema-qualified and name-sorted.
//
// Three catalogs are searched, because a database is not empty in three
// different ways and each was found to slip through in turn:
//
//   - Relations, in every non-system schema and every user kind. Counting only
//     base tables in `public` let an application that keeps its tables in
//     another schema, or a public schema holding only views or sequences, read
//     as empty and receive the baseline on top.
//   - Functions and procedures. The baseline creates one of its own, so a
//     foreign function is both a sign the database is somebody else's and a
//     potential name collision during the apply.
//   - Domains, enums, and standalone composite types. Row types of tables are
//     excluded, since the table already accounts for them.
//
// Objects belonging to an extension are excluded throughout, because the
// preflight installs pg_trgm, citext and unaccent into a database that is
// otherwise still untouched. The dependency lookup is qualified by catalog:
// object ids are only unique per catalog, so an unqualified match could exempt
// a table because some function shares its id.
//
// Empty schemas are deliberately *not* blocking. PostgreSQL's default search
// path names a schema after the connecting role, and managed providers create
// it; refusing that would refuse the very shape pinPostgresSearchPath exists to
// support.
//
// The names matter as much as the count. A database that is not empty cannot be
// created into, and the console cannot decide on the operator's behalf whether
// what remains is their data or an older build's — so it has to say which
// objects are in the way instead of only that some are.
func postgresBlockingObjects(ctx context.Context, conn *sql.Conn) ([]string, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT quote_ident(n.nspname) || '.' || quote_ident(c.relname)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
		  AND n.nspname NOT LIKE 'pg_toast%'
		  AND n.nspname NOT LIKE 'pg_temp%'
		  AND c.relkind IN ('r', 'p', 'v', 'm', 'f', 'S')
		  AND NOT EXISTS (
		      SELECT 1 FROM pg_depend d
		      WHERE d.objid = c.oid AND d.classid = 'pg_class'::regclass AND d.deptype = 'e'
		  )
		UNION ALL
		SELECT quote_ident(n.nspname) || '.' || quote_ident(p.proname) || '()'
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
		  AND n.nspname NOT LIKE 'pg_toast%'
		  AND n.nspname NOT LIKE 'pg_temp%'
		  AND NOT EXISTS (
		      SELECT 1 FROM pg_depend d
		      WHERE d.objid = p.oid AND d.classid = 'pg_proc'::regclass AND d.deptype = 'e'
		  )
		UNION ALL
		SELECT quote_ident(n.nspname) || '.' || quote_ident(t.typname)
		FROM pg_type t
		JOIN pg_namespace n ON n.oid = t.typnamespace
		WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
		  AND n.nspname NOT LIKE 'pg_toast%'
		  AND n.nspname NOT LIKE 'pg_temp%'
		  AND t.typtype IN ('d', 'e', 'c')
		  AND (
		      t.typrelid = 0
		      OR EXISTS (SELECT 1 FROM pg_class c2 WHERE c2.oid = t.typrelid AND c2.relkind = 'c')
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM pg_depend d
		      WHERE d.objid = t.oid AND d.classid = 'pg_type'::regclass AND d.deptype = 'e'
		  )
		ORDER BY 1`)
	if err != nil {
		return nil, postgresError("list the existing objects", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, postgresError("list the existing objects", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, postgresError("list the existing objects", err)
	}
	return names, nil
}

// describeBlockingObjects renders a bounded, readable list for a message.
func describeBlockingObjects(names []string) string {
	if len(names) == 0 {
		return ""
	}
	if len(names) <= maxListedBlockingObjects {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more",
		strings.Join(names[:maxListedBlockingObjects], ", "), len(names)-maxListedBlockingObjects)
}

// applyPostgresBaseline creates the schema and records its version as one
// simple-protocol script.
//
// Both halves have to land together. The baseline alone is atomic — PostgreSQL
// wraps a multi-statement simple query in one implicit transaction — but a
// separate INSERT afterwards leaves a window in which a crash or a cancelled
// startup context produces a database full of tables with no baseline row, which
// every later start then refuses as somebody else's database. Appending the
// INSERT to the same script puts it inside the same implicit transaction.
//
// The script is handed to pgx directly rather than run through database/sql
// because the extended protocol accepts one statement per call, and splitting
// the baseline on semicolons would mean parsing around a dollar-quoted function
// body full of them.
func applyPostgresBaseline(ctx context.Context, conn *sql.Conn, checksum string) error {
	script := pgschema.Baseline + "\n" + recordBaselineStatement(checksum)
	return conn.Raw(func(driverConn any) error {
		c, ok := driverConn.(*stdlib.Conn)
		if !ok {
			return fmt.Errorf("postgres: unexpected driver connection %T", driverConn)
		}
		if _, err := c.Conn().Exec(ctx, script); err != nil {
			return postgresError("apply the schema baseline", err)
		}
		return nil
	})
}

// recordBaselineStatement renders the schema_migrations row as literal SQL.
// The simple protocol carries no parameters, so the values are inlined; all
// four are values this package produces (two constants, a unix timestamp, and a
// hex digest), and quoteSQLLiteral covers the text ones regardless.
func recordBaselineStatement(checksum string) string {
	return fmt.Sprintf(
		`INSERT INTO schema_migrations (scope, version, applied_at, checksum) VALUES (%s, %s, %d, %s);`,
		quoteSQLLiteral(postgresSchemaScope), quoteSQLLiteral(postgresSchemaVersion), nowUnix(), quoteSQLLiteral(checksum))
}

func quoteSQLLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// baselineChecksum reuses the SQLite runner's checksum function so every row in
// schema_migrations is checksummed the same way, whichever runner wrote it.
func baselineChecksum() string {
	return migrationChecksum(migrationSet{
		Scope:      postgresSchemaScope,
		Version:    postgresSchemaVersion,
		Statements: []string{pgschema.Baseline},
	})
}

// pinPostgresSearchPath makes unqualified names resolve to the schema the
// baseline is meant to live in.
//
// The generated SQL is unqualified, and PostgreSQL's default search path is
// `"$user", public` — so on a server where a schema named after the connecting
// role exists, every CREATE TABLE lands there instead. The schema checks look
// in public by name, so the result is a database that has just been created
// into and immediately reads back as somebody else's: create refused, drop
// refused, no way forward. Pinning the path costs one statement per connection
// and removes the whole class.
func pinPostgresSearchPath(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, `SET search_path TO `+sqlident.Quote(pgschema.Schema)); err != nil {
		return postgresError("set the search path", err)
	}
	return nil
}

// PostgresError carries a database failure without printing the DSN.
//
// It exists because the two obligations conflict under a plain %w wrap: the
// message must not contain the password pgx echoes from its parse paths, and
// callers must still be able to reach the driver's error to tell a cancelled
// startup from a broken database. Redacting at format time and keeping Unwrap
// satisfies both. Code that unwraps and prints the inner error defeats it, which
// is why nothing in the store does.
type PostgresError struct {
	// Op names what failed, in the imperative ("connect", "inspect the schema").
	Op string
	// Err is the driver error, reachable through errors.Is and errors.As.
	Err error
}

func (e *PostgresError) Error() string {
	return "postgres: " + e.Op + ": " + pgdsn.Redact(e.Err.Error())
}

func (e *PostgresError) Unwrap() error { return e.Err }

func postgresError(op string, err error) error {
	return &PostgresError{Op: op, Err: err}
}

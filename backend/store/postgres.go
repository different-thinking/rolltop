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
	"log"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"rolltop/backend/pgbind"
	"rolltop/backend/pgdsn"
	"rolltop/backend/plugins"
	"rolltop/backend/sqlident"
	"rolltop/backend/store/pgschema"
)

const (
	// postgresSchemaScope and postgresSchemaVersion identify the squashed
	// baseline in schema_migrations. The baseline is frozen: its checksum is
	// the recorded identity of a database's origin, and a mismatch still means
	// tampering, never age. Schema changes are numbered entries layered on top
	// (postgresMigrations, postgres_migrations.go), each with its own row in
	// the same table, applied at startup when outstanding. Editing baseline.sql
	// is therefore never the way to change the schema again — it would refuse
	// to start against every database that already exists.
	postgresSchemaScope   = "postgres"
	postgresSchemaVersion = "baseline"

	// defaultPostgresMaxConns sizes the pool when the caller sets none. The
	// hosted target allows 20 connections per role, so half of that leaves room
	// for a scheduled pg_dump and a manual psql session.
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

	// postgresPingTimeout bounds one connection attempt during the startup
	// wait. Without it a database that accepts the TCP connection but never
	// answers consumes the whole budget in a single attempt.
	postgresPingTimeout = 5 * time.Second

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
	// DataDir is where blobs and search indexes live. The relational data no
	// longer does, but UserDataDir still answers for those two.
	DataDir string
	// baselineOnly stops after the baseline, skipping plugin migrations. Only
	// PrepareTestTemplate sets it: the template must carry exactly what
	// SchemaTag names, and the plugin catalog is not part of that.
	baselineOnly bool
	// ConnectTimeout bounds the wait for a database that is not up yet. The
	// application container regularly starts before its database does, so a
	// failed first connection is a normal event rather than a broken
	// deployment. Zero disables the wait: one attempt, then the error.
	ConnectTimeout time.Duration
	// ExclusiveInstance refuses to open a database another rolltop server is
	// already serving. The running server sets it; tests do not, because they
	// legitimately open several stores against one database at once.
	//
	// See instance_lock.go for why the guard exists and what it does not cover.
	ExclusiveInstance bool
	// InstanceLockWait is how long ExclusiveInstance waits for the previous
	// process to let go. A rolling deployment overlaps the two containers, so
	// zero here would make every deploy a crash loop.
	InstanceLockWait time.Duration
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
	// search_path is pinned on every connection in the pool, not just the one
	// that creates the schema. The generated SQL is unqualified, and
	// PostgreSQL's default path is `"$user", public` — so on a server where a
	// schema named after the connecting role exists, a CREATE TABLE from a
	// plugin migration lands there while the app's reads keep falling through
	// to public. The result is a schema split in half with nothing reporting it.
	connConfig, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, postgresError("parse the connection string", err)
	}
	if connConfig.RuntimeParams == nil {
		connConfig.RuntimeParams = map[string]string{}
	}
	connConfig.RuntimeParams["search_path"] = pgschema.Schema
	pgbind.Register()
	registered := stdlib.RegisterConnConfig(connConfig)
	db, err := sql.Open(pgbind.DriverName, registered)
	if err != nil {
		stdlib.UnregisterConnConfig(registered)
		return nil, postgresError("open", err)
	}
	// The registration is process-global, so it has to be released with the
	// pool or a long-lived process that reopens the store leaks one per open.
	s0 := &registeredDSN{name: registered}
	maxConns := opts.MaxConns
	if maxConns <= 0 {
		maxConns = defaultPostgresMaxConns
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	db.SetConnMaxLifetime(postgresConnMaxLifetime)
	db.SetConnMaxIdleTime(postgresConnMaxIdleTime)
	if err := waitForPostgres(ctx, db, opts.ConnectTimeout); err != nil {
		_ = db.Close()
		s0.release()
		return nil, err
	}
	// Before the schema, so a second server pointed at this database says so
	// rather than joining in and writing over the first one's sync runs.
	var instance *instanceLock
	if opts.ExclusiveInstance {
		instance, err = acquireInstanceLock(ctx, registered, opts.InstanceLockWait)
		if err != nil {
			_ = db.Close()
			s0.release()
			return nil, postgresError("claim this database", err)
		}
	}
	catalog := pluginCatalogFromManifests(opts.Manifests)
	// Every tenant lives in this one database, scoped by user_id (decision 3.1),
	// so dataDB resolves to this pool for every user.
	s := &Store{
		db:                db,
		registered:        s0,
		instance:          instance,
		maxConns:          maxConns,
		dataDir:           opts.DataDir,
		pluginDefinitions: append([]plugins.Definition(nil), catalog.definitions...),
		pluginMigrations:  append([]plugins.Migration(nil), catalog.migrations...),
	}
	if err := s.ensurePostgresSchema(ctx, opts.Progress); err != nil {
		_ = s.Close()
		return nil, err
	}
	// The baseline already contains every plugin's tables, but not the rows that
	// record which of their migrations ran. Applying them here is what keeps the
	// two in step: the statements are idempotent (CREATE TABLE IF NOT EXISTS and
	// EnsureColumns), so this costs one no-op pass on an up-to-date database and
	// is the only thing that stops the *next* plugin migration from being
	// replayed against tables that already have it.
	//
	// Under the schema lock when there is anything to do, because idempotent
	// statements are not the same as concurrency-safe ones: two servers starting
	// together both read the migration as unapplied and both insert its
	// bookkeeping row, and the loser fails on the primary key. The check that
	// decides whether there is anything to do needs no lock, so the ordinary
	// start takes none.
	if opts.baselineOnly {
		return s, nil
	}
	// Read first, without the lock: on every start after the first there is
	// nothing to apply, and that answer is one query.
	upToDate, err := s.pluginMigrationsUpToDate(ctx)
	if err != nil {
		_ = s.Close()
		return nil, postgresError("check plugin migrations", err)
	}
	if upToDate {
		return s, nil
	}
	if err := s.withSchemaLock(ctx, func(ctx context.Context) error {
		for _, scope := range s.pluginMigrationScopes() {
			if err := s.applyPluginMigrationsForScope(ctx, scope); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		_ = s.Close()
		return nil, postgresError("apply plugin migrations", err)
	}
	return s, nil
}

// lockPostgresSchema takes the advisory lock that serializes schema changes
// across every process pointed at this database: a starting server applying the
// baseline, another applying plugin migrations, the admin console creating or
// dropping the schema.
//
// The returned function releases the lock. Closing the connection would release
// it too — it is a session lock — but unlocking explicitly returns the
// connection to the pool usable rather than holding a server-wide lock for as
// long as the pool keeps that connection idle. The release runs on its own
// short-lived context so a caller whose context has already been cancelled
// still gives the lock back.
//
// This is the only place that spells this out. Every caller in this package
// goes through it, because a copy that forgot the explicit unlock, or gave the
// release no timeout of its own, would be invisible until a deploy hung.
func lockPostgresSchema(ctx context.Context, conn *sql.Conn) (func(), error) {
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, schemaAdvisoryLock); err != nil {
		return nil, postgresError("take the schema lock", err)
	}
	return func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), schemaUnlockTimeout)
		defer cancel()
		_, _ = conn.ExecContext(unlockCtx, `SELECT pg_advisory_unlock($1)`, schemaAdvisoryLock)
	}, nil
}

// schemaUnlockTimeout bounds giving the lock back. It is generous because
// failing to unlock leaves the lock held until the connection dies, and short
// because a hung unlock must not delay shutdown indefinitely.
const schemaUnlockTimeout = 10 * time.Second

// withSchemaLock runs fn on a pooled connection while holding the schema lock.
func (s *Store) withSchemaLock(ctx context.Context, fn func(context.Context) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	unlock, err := lockPostgresSchema(ctx, conn)
	if err != nil {
		return err
	}
	defer unlock()
	return fn(ctx)
}

// waitForPostgres blocks until the database answers, the budget runs out, or
// the caller's context is cancelled.
//
// The wait exists because the app container and the database start
// independently: under compose the database is usually a few seconds behind,
// and a managed instance can be briefly unreachable during a failover. Exiting
// on the first refused connection turns that into a crash loop whose restarts
// are themselves the slowest way to retry.
//
// The failure that starts the wait is logged immediately, and the one that ends
// it is carried into the returned error. Neither is optional: a start that is
// killed before the budget runs out — an orchestrator restarting a container it
// considers unhealthy, which is the normal state of a deployment whose database
// is misconfigured — would otherwise report only "context canceled" and never
// say what was wrong. Where the database is unreachable *and* the process is
// killed after a second, that silence is permanent: every restart repeats it,
// and the reason is never printed at all.
//
// Both errors reach the log through postgresError, which redacts. pgx quotes
// the whole connection string in its parse and dial failures, so printing one
// of these raw would put the password in the container log.
func waitForPostgres(ctx context.Context, db *sql.DB, budget time.Duration) error {
	const retryEvery = time.Second
	attemptCtx, cancel := context.WithTimeout(ctx, postgresPingTimeout)
	err := db.PingContext(attemptCtx)
	cancel()
	if err == nil {
		return nil
	}
	if budget <= 0 {
		return postgresError("connect", err)
	}
	first := err
	last := err
	deadline := time.Now().Add(budget)
	log.Printf("waiting up to %s for the database to accept connections: %v",
		budget.Round(time.Second), postgresError("connect", first))
	for {
		select {
		case <-ctx.Done():
			// The cancellation says the wait was cut short; the attempt error
			// says what it was waiting for. Reporting only the first loses the
			// diagnosis, and only the second hides that this was a shutdown.
			// ctx.Err() stays wrapped so errors.Is keeps working.
			return postgresError("connect", fmt.Errorf("%w (last connection attempt: %v)", ctx.Err(), last))
		case <-time.After(retryEvery):
		}
		attemptCtx, cancel := context.WithTimeout(ctx, postgresPingTimeout)
		err := db.PingContext(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}
		last = err
		if time.Now().After(deadline) {
			// The first error rather than the last: a database that never came
			// up fails the same way throughout, and the first one is the one
			// whose timing matches the start of the wait.
			return postgresError("connect", first)
		}
	}
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
	state, err := readPostgresSchemaState(ctx, conn, checksum, postgresMigrations)
	if err != nil {
		return err
	}
	if state.BaselinePresent && len(state.Outstanding) == 0 {
		reportMigration(progress, MigrationProgress{Scope: postgresSchemaScope, Migration: postgresSchemaVersion, Step: "already applied", Done: 1, Total: 1})
		return nil
	}

	unlock, err := lockPostgresSchema(ctx, conn)
	if err != nil {
		return err
	}
	defer unlock()

	// Another process may have created or advanced the schema while this one
	// waited on the lock.
	state, err = readPostgresSchemaState(ctx, conn, checksum, postgresMigrations)
	if err != nil {
		return err
	}
	if state.BaselinePresent && len(state.Outstanding) == 0 {
		reportMigration(progress, MigrationProgress{Scope: postgresSchemaScope, Migration: postgresSchemaVersion, Step: "already applied", Done: 1, Total: 1})
		return nil
	}
	if !state.BaselinePresent {
		blocking, err := postgresBlockingObjects(ctx, conn)
		if err != nil {
			return err
		}
		if len(blocking) > 0 {
			return fmt.Errorf("postgres: the target database already contains objects but no recorded Rolltop baseline (%s); point the server at an empty database",
				describeBlockingObjects(blocking))
		}
		reportMigration(progress, MigrationProgress{Scope: postgresSchemaScope, Migration: postgresSchemaVersion, Step: "create schema", Done: 0, Total: 1 + len(state.Outstanding)})
		if err := applyPostgresBaseline(ctx, conn, checksum); err != nil {
			return err
		}
	}
	total := 1 + len(state.Outstanding)
	for i, m := range state.Outstanding {
		reportMigration(progress, MigrationProgress{Scope: postgresSchemaScope, Migration: m.Version, Step: "apply migration", Done: 1 + i, Total: total})
		if err := applyPostgresMigration(ctx, conn, m); err != nil {
			return err
		}
	}
	reportMigration(progress, MigrationProgress{Scope: postgresSchemaScope, Migration: postgresSchemaVersion, Step: "complete", Done: total, Total: total})
	return nil
}

// maxListedBlockingObjects bounds how many object names the refusal names.
// Enough to recognise what is in the way, not so many that the message becomes
// a schema dump.
const maxListedBlockingObjects = 10

// postgresBlockingObjects lists the objects that stop this database from being
// created into, schema-qualified and name-sorted.
//
// Only the schema the baseline writes into is searched. That schema is the only
// place an existing object can collide with what the apply is about to create,
// and it is the whole question this check has to answer.
//
// It used to search every non-system schema, on the theory that an application
// keeping its tables elsewhere would otherwise read as empty and receive the
// baseline alongside its data. That reasoning does not survive contact with a
// managed provider: the operators that run hosted PostgreSQL put their own
// management objects in dedicated schemas of *every* database they hand out —
// `metric_helpers` and `user_management` under the Zalando operator, others
// elsewhere — so the broad search refused to start against a database that had
// just been created for Rolltop and was empty in every sense that matters.
// Hardcoding those names would only move the problem to the next provider.
//
// What the narrow search gives up is small: an application with its tables in
// its own schema is not harmed by Rolltop creating its own in public, and the
// recorded baseline row still tells the two apart on every later start.
//
// Three catalogs are searched, because a schema is not empty in three different
// ways and each was found to slip through in turn:
//
//   - Relations of every user kind. Counting only base tables let a schema
//     holding only views or sequences read as empty.
//   - Functions and procedures. The baseline creates one of its own, so a
//     foreign function is a name collision waiting to happen during the apply.
//   - Domains, enums, and standalone composite types. Row types of tables are
//     excluded, since the table already accounts for them. Undefined types are
//     included whatever their kind: `CREATE TYPE name;` declares a shell, which
//     the catalog records as a pseudo-type with typisdefined false, so matching
//     on kind alone let a schema holding one read as empty.
//
// Objects belonging to an extension are excluded throughout, because the
// preflight installs pg_trgm, citext and unaccent into a database that is
// otherwise still untouched. The dependency lookup is qualified by catalog:
// object ids are only unique per catalog, so an unqualified match could exempt
// a table because some function shares its id.
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
		WHERE n.nspname = $1
		  AND c.relkind IN ('r', 'p', 'v', 'm', 'f', 'S')
		  AND NOT EXISTS (
		      SELECT 1 FROM pg_depend d
		      WHERE d.objid = c.oid AND d.classid = 'pg_class'::regclass AND d.deptype = 'e'
		  )
		UNION ALL
		SELECT quote_ident(n.nspname) || '.' || quote_ident(p.proname) || '()'
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = $1
		  AND NOT EXISTS (
		      SELECT 1 FROM pg_depend d
		      WHERE d.objid = p.oid AND d.classid = 'pg_proc'::regclass AND d.deptype = 'e'
		  )
		UNION ALL
		SELECT quote_ident(n.nspname) || '.' || quote_ident(t.typname)
		FROM pg_type t
		JOIN pg_namespace n ON n.oid = t.typnamespace
		WHERE n.nspname = $1
		  AND (
		      NOT t.typisdefined
		      OR (
		          t.typtype IN ('d', 'e', 'c')
		          AND (
		              t.typrelid = 0
		              OR EXISTS (SELECT 1 FROM pg_class c2 WHERE c2.oid = t.typrelid AND c2.relkind = 'c')
		          )
		      )
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM pg_depend d
		      WHERE d.objid = t.oid AND d.classid = 'pg_type'::regclass AND d.deptype = 'e'
		  )
		ORDER BY 1`, pgschema.Schema)
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
	if err := execPostgresScript(ctx, conn, script); err != nil {
		return postgresError("apply the schema baseline", err)
	}
	return nil
}

// execPostgresScript runs a multi-statement script over the simple protocol,
// which wraps it in one implicit transaction. Handed to pgx directly rather
// than run through database/sql because the extended protocol accepts one
// statement per call, and splitting scripts on semicolons would mean parsing
// around dollar-quoted bodies full of them.
func execPostgresScript(ctx context.Context, conn *sql.Conn, script string) error {
	return conn.Raw(func(driverConn any) error {
		c, ok := pgbind.Unwrap(driverConn).(*stdlib.Conn)
		if !ok {
			return fmt.Errorf("postgres: unexpected driver connection %T", driverConn)
		}
		if _, err := c.Conn().Exec(ctx, script); err != nil {
			return err
		}
		return nil
	})
}

// schemaMigrationsQualified names the bookkeeping table in the schema the
// baseline writes into, for to_regclass probes.
func schemaMigrationsQualified() string {
	return pgschema.Schema + ".schema_migrations"
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

// baselineChecksum fingerprints the baseline as schema_migrations records it.
// A mismatch at open means baseline.sql was edited after a database was built
// from it, which is the one thing the row exists to catch.
func baselineChecksum() string {
	return schemaChecksum(postgresSchemaScope, postgresSchemaVersion, pgschema.Baseline)
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

// registeredDSN owns one stdlib.RegisterConnConfig entry. The registry is
// process-global and keyed by a generated name, so a store that does not
// release its entry leaks one per open — which a test binary opening hundreds
// of stores turns into a real leak rather than a theoretical one.
type registeredDSN struct {
	name string
	once sync.Once
}

func (r *registeredDSN) release() {
	if r == nil {
		return
	}
	r.once.Do(func() { stdlib.UnregisterConnConfig(r.name) })
}

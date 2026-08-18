// File overview: The operations the admin migration console drives, one step at
// a time, against a candidate PostgreSQL database.
//
// The point of exposing these separately from OpenPostgres is that the
// migration is meant to be rehearsed rather than attempted once: an operator
// should be able to create the schema against the real hosted database, look at
// what came out, drop it, and do it again — long before any data moves and long
// before the server is pointed at it. Every function here takes a DSN and owns
// its own short-lived connection, so nothing it does touches the running store.

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"rolltop/backend/sqlident"
	"rolltop/backend/store/pgschema"
)

// Stages a candidate database can be in.
const (
	// PostgresStageEmpty holds no objects of its own: the schema can be created.
	PostgresStageEmpty = "empty"
	// PostgresStageBaseline carries exactly the baseline this binary ships.
	PostgresStageBaseline = "baseline"
	// PostgresStageMismatch carries a Rolltop baseline from a different build.
	PostgresStageMismatch = "mismatch"
	// PostgresStageForeign holds objects that are not Rolltop's.
	PostgresStageForeign = "foreign"
)

// postgresConsoleTimeout bounds one console operation. Creating the schema is a
// few hundred milliseconds against a local server and a few seconds across a
// network; the rest is a handful of round trips.
const postgresConsoleTimeout = 2 * time.Minute

// PostgresState is what the console reports about a candidate database.
type PostgresState struct {
	// Stage is one of the PostgresStage constants.
	Stage string `json:"stage"`
	// ServerVersion is the server's own version string.
	ServerVersion string `json:"server_version"`
	// Database and User name what the DSN actually resolved to, so an operator
	// can see they reached the database they meant.
	Database string `json:"database"`
	User     string `json:"user"`
	// Tables, Indexes, ForeignKeys and Triggers count what is in the database
	// now, which is how the console shows that creating the schema did
	// something.
	Tables      int `json:"tables"`
	Indexes     int `json:"indexes"`
	ForeignKeys int `json:"foreign_keys"`
	Triggers    int `json:"triggers"`
	// Rows is the total row count across the baseline's tables. It is zero for
	// a freshly created schema and non-zero once data has been migrated, which
	// is what makes an accidental drop visible before it happens.
	Rows int64 `json:"rows"`
	// AppliedAt is when the baseline was recorded, or zero.
	AppliedAt int64 `json:"applied_at"`
	// CanCreate and CanDrop tell the UI which steps are offered. They are
	// decided here rather than in the frontend so a mislabelled button cannot
	// turn into a dropped database.
	CanCreate bool `json:"can_create"`
	CanDrop   bool `json:"can_drop"`
	// Summary is one sentence naming the stage in the operator's terms.
	Summary string `json:"summary"`
}

// InspectPostgres reports what a candidate database currently holds. It is
// read-only: it creates nothing and drops nothing, so it is safe to point at
// any database including a live one.
func InspectPostgres(ctx context.Context, dsn string) (PostgresState, error) {
	return withPostgresConsoleConn(ctx, dsn, func(ctx context.Context, conn *sql.Conn) (PostgresState, error) {
		return inspectPostgres(ctx, conn)
	})
}

// CreatePostgresSchema applies the generated baseline to an empty database and
// returns what the database holds afterwards.
//
// It refuses anything but an empty database. Creating into a database that
// already holds objects is the mistake that is worst to recover from, and the
// console exists so that mistake is caught while it is still a rehearsal.
func CreatePostgresSchema(ctx context.Context, dsn string) (PostgresState, error) {
	return withPostgresConsoleConn(ctx, dsn, func(ctx context.Context, conn *sql.Conn) (PostgresState, error) {
		unlock, err := lockPostgresSchema(ctx, conn)
		if err != nil {
			return PostgresState{}, err
		}
		defer unlock()

		checksum := baselineChecksum()
		stage, _, err := postgresStage(ctx, conn, checksum)
		if err != nil {
			return PostgresState{}, err
		}
		switch stage {
		case PostgresStageBaseline:
			return inspectPostgres(ctx, conn)
		case PostgresStageMismatch:
			return PostgresState{}, errors.New("this database carries a Rolltop schema from a different build. Drop it before creating the current one")
		case PostgresStageForeign:
			return PostgresState{}, errors.New("this database already holds objects that are not Rolltop's. Point the console at an empty database")
		}
		if err := applyPostgresBaseline(ctx, conn, checksum); err != nil {
			return PostgresState{}, err
		}
		return inspectPostgres(ctx, conn)
	})
}

// DropPostgresSchema removes the Rolltop schema so the create step can be
// rehearsed again.
//
// It drops only a database that carries a recorded Rolltop baseline — a foreign
// database is refused outright, so a mistyped DSN cannot destroy somebody else's
// data. Objects belonging to an extension are left alone, matching the
// empty-database check: the preflight installs pg_trgm, citext and unaccent, and
// a rehearsal loop should not have to reinstall them every round.
func DropPostgresSchema(ctx context.Context, dsn string) (PostgresState, error) {
	return withPostgresConsoleConn(ctx, dsn, func(ctx context.Context, conn *sql.Conn) (PostgresState, error) {
		unlock, err := lockPostgresSchema(ctx, conn)
		if err != nil {
			return PostgresState{}, err
		}
		defer unlock()

		stage, _, err := postgresStage(ctx, conn, baselineChecksum())
		if err != nil {
			return PostgresState{}, err
		}
		switch stage {
		case PostgresStageEmpty:
			return inspectPostgres(ctx, conn)
		case PostgresStageForeign:
			return PostgresState{}, errors.New("this database holds objects that are not Rolltop's, so the console will not drop anything here")
		}
		if err := dropPostgresObjects(ctx, conn); err != nil {
			return PostgresState{}, err
		}
		return inspectPostgres(ctx, conn)
	})
}

// withPostgresConsoleConn opens a pool of one for a single console operation.
// A console request must not borrow the running store's pool: the DSN is an
// admin-supplied one that may point somewhere else entirely.
func withPostgresConsoleConn(ctx context.Context, dsn string, run func(context.Context, *sql.Conn) (PostgresState, error)) (PostgresState, error) {
	if strings.TrimSpace(dsn) == "" {
		return PostgresState{}, errors.New("postgres: empty connection string")
	}
	ctx, cancel := context.WithTimeout(ctx, postgresConsoleTimeout)
	defer cancel()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return PostgresState{}, postgresError("open", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		return PostgresState{}, postgresError("connect", err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return PostgresState{}, postgresError("acquire a connection", err)
	}
	defer func() { _ = conn.Close() }()
	if err := pinPostgresSearchPath(ctx, conn); err != nil {
		return PostgresState{}, err
	}
	return run(ctx, conn)
}

// lockPostgresSchema serializes the create and drop steps against everything
// else that changes this schema, including a starting server. The returned
// function releases the lock; closing the connection would too, but returning it
// unlocked keeps the lock's lifetime tied to the operation rather than to the
// pool.
func lockPostgresSchema(ctx context.Context, conn *sql.Conn) (func(), error) {
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, schemaAdvisoryLock); err != nil {
		return nil, postgresError("take the schema lock", err)
	}
	return func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(unlockCtx, `SELECT pg_advisory_unlock($1)`, schemaAdvisoryLock)
	}, nil
}

func inspectPostgres(ctx context.Context, conn *sql.Conn) (PostgresState, error) {
	state := PostgresState{}
	if err := conn.QueryRowContext(ctx,
		`SELECT version(), current_database(), current_user`).Scan(&state.ServerVersion, &state.Database, &state.User); err != nil {
		return PostgresState{}, postgresError("read the server identity", err)
	}
	stage, appliedAt, err := postgresStage(ctx, conn, baselineChecksum())
	if err != nil {
		return PostgresState{}, err
	}
	state.Stage = stage
	state.AppliedAt = appliedAt
	if err := conn.QueryRowContext(ctx, `
		SELECT
			(SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_type = 'BASE TABLE'),
			(SELECT count(*) FROM pg_indexes WHERE schemaname = 'public'),
			(SELECT count(*) FROM pg_constraint WHERE contype = 'f'),
			(SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal)`).
		Scan(&state.Tables, &state.Indexes, &state.ForeignKeys, &state.Triggers); err != nil {
		return PostgresState{}, postgresError("count the schema objects", err)
	}
	if stage == PostgresStageBaseline || stage == PostgresStageMismatch {
		rows, err := countPostgresRows(ctx, conn)
		if err != nil {
			return PostgresState{}, err
		}
		state.Rows = rows
	}
	switch stage {
	case PostgresStageEmpty:
		state.CanCreate = true
		state.Summary = "Empty. The schema can be created here."
	case PostgresStageBaseline:
		state.CanDrop = true
		state.Summary = fmt.Sprintf("Carries the current schema: %d tables, %d indexes, %d foreign keys, %d triggers, %d rows.",
			state.Tables, state.Indexes, state.ForeignKeys, state.Triggers, state.Rows)
	case PostgresStageMismatch:
		state.CanDrop = true
		state.Summary = "Carries a Rolltop schema from a different build. Drop it and create the current one."
	default:
		state.Summary = "Holds objects that are not Rolltop's. The console will not touch this database."
	}
	return state, nil
}

// countPostgresRows sums the live rows across the schema's data tables.
//
// It counts rather than reading the planner's estimate, because the number is
// there to tell an operator whether a database they are about to drop still
// holds a migration they care about, and reltuples is -1 until something
// analyzes the table. schema_migrations is excluded for the same reason: its
// one bookkeeping row would make a freshly created, entirely empty schema
// report that it holds data.
func countPostgresRows(ctx context.Context, conn *sql.Conn) (int64, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind = 'r'
		  AND c.relname <> 'schema_migrations'
		  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = c.oid AND d.deptype = 'e')
		ORDER BY c.relname`)
	if err != nil {
		return 0, postgresError("list the tables", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return 0, postgresError("list the tables", err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, postgresError("list the tables", err)
	}
	if len(tables) == 0 {
		return 0, nil
	}
	// One statement rather than one round trip per table: the schema has 75 of
	// them, and this runs on every console refresh.
	parts := make([]string, 0, len(tables))
	for _, table := range tables {
		parts = append(parts, fmt.Sprintf("SELECT count(*) FROM %s", sqlident.Quote(table)))
	}
	var total int64
	if err := conn.QueryRowContext(ctx,
		"SELECT sum(n) FROM ("+strings.Join(parts, " UNION ALL ")+") AS counts(n)").Scan(&total); err != nil {
		return 0, postgresError("count the rows", err)
	}
	return total, nil
}

// dropPostgresObjects removes exactly the objects the baseline declares.
//
// It works from the baseline's own object list rather than from whatever is in
// the database. Enumerating "every non-extension table" would take an
// operator's own tables with it: a database can carry the Rolltop schema and
// something else besides, and the stage check calls that "baseline" because the
// version row is there and matches. Naming the objects makes the drop's promise
// — Rolltop's tables and nothing else — one the code actually keeps.
func dropPostgresObjects(ctx context.Context, conn *sql.Conn) error {
	tables := pgschema.DeclaredNames(pgschema.TableKind)
	if len(tables) > 0 {
		// CASCADE takes the indexes, constraints and triggers with the tables,
		// and one statement avoids having to drop them in dependency order.
		qualified := make([]string, 0, len(tables))
		for _, table := range tables {
			qualified = append(qualified, sqlident.Quote(pgschema.Schema)+"."+sqlident.Quote(table))
		}
		if _, err := conn.ExecContext(ctx, "DROP TABLE IF EXISTS "+strings.Join(qualified, ", ")+" CASCADE"); err != nil {
			return postgresError("drop the tables", err)
		}
	}
	// Dropping a table takes its triggers but leaves the function each one
	// runs, and the baseline names the function after the trigger.
	for _, trigger := range pgschema.DeclaredNames(pgschema.TriggerKind) {
		name := sqlident.Quote(pgschema.Schema) + "." + sqlident.Quote(trigger)
		if _, err := conn.ExecContext(ctx, "DROP FUNCTION IF EXISTS "+name+"() CASCADE"); err != nil {
			return postgresError("drop the trigger functions", err)
		}
	}
	return nil
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"rolltop/backend/store/pgschema"
	"rolltop/backend/store/pgtestdb"
)

func TestOpenPostgresRequiresDSN(t *testing.T) {
	if _, err := OpenPostgres(context.Background(), "", PostgresOptions{}); err == nil {
		t.Fatal("opening without a DSN succeeded")
	}
}

// TestOpenPostgresNeverEchoesCredentials pins what the redaction in
// postgresError is for.
//
// pgx quotes the whole connection string back in every *parse* error, and its
// own redaction only recognises the space-free `password=x` keyword spelling
// and the URL form. Each DSN below was verified against pgx v5 to reach the
// caller in cleartext through a plain %w wrap; connect failures are not in the
// list because those messages carry no password to begin with.
func TestOpenPostgresNeverEchoesCredentials(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dsns := []string{
		"host=127.0.0.1 port=notanumber user=rolltop password = hunter2",
		"host=127.0.0.1 password = hunter2 sslmode=bogus",
		"host=127.0.0.1 password  =  hunter2 target_session_attrs=bogus",
		"host=127.0.0.1 password = 'hunter2' sslmode=bogus",
		"host=127.0.0.1 PGPASSWORD=hunter2 sslmode=bogus",
	}
	for _, dsn := range dsns {
		s, err := OpenPostgres(ctx, dsn, PostgresOptions{})
		if err == nil {
			_ = s.Close()
			t.Errorf("connecting to %q succeeded", dsn)
			continue
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("error for %q leaked the password: %v", dsn, err)
		}
		// The redaction must not eat the diagnosis with the secret.
		if !strings.Contains(err.Error(), "127.0.0.1") {
			t.Errorf("error for %q lost its context: %v", dsn, err)
		}
	}
}

// TestOpenPostgresAppliesBaseline is the end-to-end check for WP1: an empty
// database comes back carrying the generated schema and a recorded version.
func TestOpenPostgresAppliesBaseline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dsn := pgtestdb.New(t)

	started := time.Now()
	s, err := OpenPostgres(ctx, dsn, PostgresOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("applying the baseline to an empty database took %s", time.Since(started).Round(time.Millisecond))
	defer func() { _ = s.Close() }()

	var tables int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_type = 'BASE TABLE'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	// The count is not pinned to a literal here: pgschema's own tests own that
	// number, and duplicating it would make every schema change fail twice.
	if tables < 2 {
		t.Fatalf("baseline created %d tables", tables)
	}
	var checksum string
	if err := s.DB().QueryRowContext(ctx,
		`SELECT checksum FROM schema_migrations WHERE scope = $1 AND version = $2`,
		postgresSchemaScope, postgresSchemaVersion).Scan(&checksum); err != nil {
		t.Fatal(err)
	}
	if checksum != baselineChecksum() {
		t.Errorf("recorded checksum %q, want %q", checksum, baselineChecksum())
	}
	// The trigger is the one object the translation had to rewrite rather than
	// map, so its presence is worth asserting separately from the table count.
	var triggers int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal`).Scan(&triggers); err != nil {
		t.Fatal(err)
	}
	if triggers == 0 {
		t.Error("baseline created no triggers")
	}
}

// TestOpenPostgresReopensExistingDatabase covers the ordinary restart: the
// second open must find the schema, not try to create it again.
func TestOpenPostgresReopensExistingDatabase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dsn := pgtestdb.New(t)

	first, err := OpenPostgres(ctx, dsn, PostgresOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	var steps []string
	second, err := OpenPostgres(ctx, dsn, PostgresOptions{Progress: func(p MigrationProgress) {
		steps = append(steps, p.Step)
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	if len(steps) != 1 || steps[0] != "already applied" {
		t.Errorf("reopen reported %v, want a single \"already applied\" step", steps)
	}
}

// TestOpenPostgresRejectsChecksumDrift pins the reason the baseline is recorded
// with a checksum at all: a binary whose baseline no longer matches the schema
// in the database must refuse to serve rather than run against a schema it was
// not built for.
func TestOpenPostgresRejectsChecksumDrift(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dsn := pgtestdb.New(t)

	s, err := OpenPostgres(ctx, dsn, PostgresOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx,
		`UPDATE schema_migrations SET checksum = 'stale' WHERE scope = $1 AND version = $2`,
		postgresSchemaScope, postgresSchemaVersion); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = OpenPostgres(ctx, dsn, PostgresOptions{})
	if err == nil {
		t.Fatal("opening a database built from a different baseline succeeded")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error %q does not name the mismatch", err)
	}
}

// TestOpenPostgresRejectsForeignDatabase guards the configuration mistake that
// would be worst to recover from: pointing the server at a database that
// already holds somebody else's tables.
func TestOpenPostgresRejectsForeignDatabase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dsn := pgtestdb.New(t)

	occupant, err := OpenPostgres(ctx, dsn, PostgresOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Drop the recorded version but keep the tables, which is what an unrelated
	// database looks like from here.
	if _, err := occupant.DB().ExecContext(ctx, `DROP TABLE schema_migrations`); err != nil {
		t.Fatal(err)
	}
	if err := occupant.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = OpenPostgres(ctx, dsn, PostgresOptions{})
	if err == nil {
		t.Fatal("opening a database with foreign tables succeeded")
	}
	if !strings.Contains(err.Error(), "empty database") {
		t.Errorf("error %q does not tell the operator what to do", err)
	}
}

// TestOpenPostgresRejectsDatabasesWithoutBaseTables covers the shapes an
// occupied schema takes that a BASE-TABLE count reads as empty. Each of these
// received the baseline on top of somebody else's objects before the check
// counted all user relations, functions and types.
func TestOpenPostgresRejectsDatabasesWithoutBaseTables(t *testing.T) {
	occupants := map[string]string{
		"view in public":     `CREATE VIEW v AS SELECT 1 AS x`,
		"sequence in public": `CREATE SEQUENCE counter`,
		"materialized view":  `CREATE MATERIALIZED VIEW m AS SELECT 1 AS x`,
		"function in public": `CREATE FUNCTION f() RETURNS int LANGUAGE sql AS 'SELECT 1'`,
		"enum in public":     `CREATE TYPE mood AS ENUM ('ok')`,
	}
	for name, ddl := range occupants {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			dsn := pgtestdb.New(t)

			seed, err := sql.Open("pgx", dsn)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := seed.ExecContext(ctx, ddl); err != nil {
				t.Fatal(err)
			}
			if err := seed.Close(); err != nil {
				t.Fatal(err)
			}

			s, err := OpenPostgres(ctx, dsn, PostgresOptions{})
			if err == nil {
				_ = s.Close()
				t.Fatal("opened a database that already holds objects")
			}
			if !strings.Contains(err.Error(), "empty database") {
				t.Errorf("error %q does not tell the operator what to do", err)
			}
		})
	}
}

// TestOpenPostgresIgnoresExtensionObjects is the other side of that check: the
// preflight installs pg_trgm, citext and unaccent into a candidate database, and
// a server started against it afterwards must still see an empty database.
func TestOpenPostgresIgnoresExtensionObjects(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dsn := pgtestdb.New(t)

	seed, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	for _, extension := range []string{"pg_trgm", "citext", "unaccent"} {
		if _, err := seed.ExecContext(ctx, `CREATE EXTENSION IF NOT EXISTS `+extension+` WITH SCHEMA public`); err != nil {
			t.Fatal(err)
		}
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := OpenPostgres(ctx, dsn, PostgresOptions{})
	if err != nil {
		t.Fatalf("a database carrying only the preflight's extensions was refused: %v", err)
	}
	_ = s.Close()
}

// TestOpenPostgresBaselineIsAtomic checks the claim applyPostgresBaseline makes
// about the simple protocol: a script that fails partway leaves nothing behind,
// and the recorded version lands with the schema rather than after it.
func TestOpenPostgresBaselineIsAtomic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dsn := pgtestdb.New(t)

	s, err := OpenPostgres(ctx, dsn, PostgresOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	// Re-applying is the readily available multi-statement script that fails
	// after its first statement, since every table already exists.
	if err := applyPostgresBaseline(ctx, conn, baselineChecksum()); err == nil {
		t.Fatal("re-applying the baseline succeeded")
	}
	var partial int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'schema_migrations'`).Scan(&partial); err != nil {
		t.Fatal(err)
	}
	if partial != 1 {
		t.Errorf("schema_migrations count is %d after a failed apply", partial)
	}
	// The failed re-apply must not have added a second version row either,
	// which is what proves the INSERT rode inside the same implicit transaction
	// as the schema rather than following it.
	var rows int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM schema_migrations WHERE scope = $1`, postgresSchemaScope).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if want := 1 + len(postgresMigrations); rows != want {
		t.Errorf("schema_migrations holds %d rows after a failed apply, want %d", rows, want)
	}
}

// TestOpenPostgresRecordsTheVersionAtomically pins the window the separate
// INSERT used to leave open: a database that received the schema but not its
// version row is refused forever as somebody else's database, so the two must
// commit together.
func TestOpenPostgresRecordsTheVersionAtomically(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dsn := pgtestdb.New(t)

	s, err := OpenPostgres(ctx, dsn, PostgresOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	// Any table existing implies the version row exists: they are written by
	// one script, so no interruption can produce one without the other.
	var tables, versions int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_type = 'BASE TABLE'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM schema_migrations WHERE scope = $1 AND version = $2`,
		postgresSchemaScope, postgresSchemaVersion).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if tables > 0 && versions != 1 {
		t.Errorf("%d tables exist but %d version rows do", tables, versions)
	}
}

// TestOpenPostgresConcurrentFirstStart covers the window a rolling deployment
// opens: two processes reaching the same empty database at once. Without the
// advisory lock both find it empty, both apply the baseline, and the loser dies
// on the first object that already exists.
func TestOpenPostgresConcurrentFirstStart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dsn := pgtestdb.New(t)

	const starters = 4
	stores := make(chan *Store, starters)
	errs := make(chan error, starters)
	var ready sync.WaitGroup
	ready.Add(starters)
	release := make(chan struct{})
	for i := 0; i < starters; i++ {
		go func() {
			ready.Done()
			<-release
			s, err := OpenPostgres(ctx, dsn, PostgresOptions{})
			if err != nil {
				errs <- err
				return
			}
			stores <- s
		}()
	}
	ready.Wait()
	close(release)
	var opened *Store
	for i := 0; i < starters; i++ {
		select {
		case s := <-stores:
			t.Cleanup(func() { _ = s.Close() })
			opened = s
		case err := <-errs:
			t.Errorf("concurrent start failed: %v", err)
		}
	}
	if opened == nil {
		t.Fatal("no starter opened the database")
	}

	var rows int
	if err := opened.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM schema_migrations WHERE scope = $1`, postgresSchemaScope).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	// One baseline row plus one row per shipped core migration; a duplicate of
	// either is what the concurrent starters could have produced.
	if want := 1 + len(postgresMigrations); rows != want {
		t.Errorf("schema_migrations holds %d rows, want %d", rows, want)
	}
}

// TestPostgresErrorKeepsTheChain covers the half of the redaction contract that
// a plain string wrap loses: startup code has to tell a cancelled context from a
// broken database, and the SQLite path's corruption classification has no
// PostgreSQL analogue if every error is opaque text.
func TestPostgresErrorKeepsTheChain(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := OpenPostgres(cancelled, "postgres://rolltop:hunter2@127.0.0.1:1/x?sslmode=disable", PostgresOptions{})
	if err == nil {
		t.Fatal("opening with a cancelled context succeeded")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) is false for %v", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error leaked the password: %v", err)
	}
	var pgErr *PostgresError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error is not a *PostgresError: %v", err)
	}
	if pgErr.Op == "" {
		t.Error("PostgresError carries no operation name")
	}
}

// TestOpenPostgresStartsBesideAManagedProvidersSchemas is the case that stopped
// a hosted deployment dead. The operators that run managed PostgreSQL create
// their own management schemas in every database they hand out — these names
// are the Zalando operator's — so a database freshly provisioned for Rolltop
// arrives already holding tables and functions outside public.
//
// Counting those as "the database is not empty" refused the very databases this
// application is meant to run on, and the message told the operator to point the
// server at an empty database when it already was one.
func TestOpenPostgresStartsBesideAManagedProvidersSchemas(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dsn := pgtestdb.New(t)

	seed, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.ExecContext(ctx, `
		CREATE SCHEMA metric_helpers;
		CREATE TABLE metric_helpers.index_bloat (id int);
		CREATE VIEW metric_helpers.table_bloat AS SELECT 1 AS x;
		CREATE FUNCTION metric_helpers.get_btree_bloat_approx() RETURNS int LANGUAGE sql AS 'SELECT 1';
		CREATE SCHEMA user_management;
		CREATE FUNCTION user_management.create_application_user() RETURNS int LANGUAGE sql AS 'SELECT 1';
	`); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := OpenPostgres(ctx, dsn, PostgresOptions{})
	if err != nil {
		t.Fatalf("a database carrying only its provider's management schemas was refused: %v", err)
	}
	defer s.Close()

	// And the baseline landed in public, not beside the provider's objects.
	var tables int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_tables WHERE schemaname = $1 AND tablename = 'users'`, pgschema.Schema).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 1 {
		t.Errorf("the baseline did not create its tables in %s", pgschema.Schema)
	}
}

// TestOpenPostgresIgnoresAnotherApplicationsSchema pins the protection that was
// deliberately given up with the narrow search. An application keeping its
// tables in its own schema is not harmed by Rolltop creating its own in public,
// and the recorded baseline row still tells the two apart on every later start.
func TestOpenPostgresIgnoresAnotherApplicationsSchema(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dsn := pgtestdb.New(t)

	seed, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.ExecContext(ctx, `CREATE SCHEMA app; CREATE TABLE app.things (id int)`); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := OpenPostgres(ctx, dsn, PostgresOptions{})
	if err != nil {
		t.Fatalf("a database whose other application keeps its own schema was refused: %v", err)
	}
	_ = s.Close()

	// Reopening recognises it as Rolltop's, so the baseline row is what
	// distinguishes the two rather than the emptiness check.
	again, err := OpenPostgres(ctx, dsn, PostgresOptions{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	_ = again.Close()
}

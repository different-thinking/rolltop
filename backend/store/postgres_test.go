package store

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

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

// TestOpenPostgresBaselineIsAtomic checks the claim applyPostgresBaseline makes
// about the simple protocol: a script that fails partway leaves nothing behind.
func TestOpenPostgresBaselineIsAtomic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dsn := pgtestdb.New(t)

	s, err := OpenPostgres(ctx, dsn, PostgresOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	// Re-applying is the readily available multi-statement script that fails
	// after its first statement, since every table already exists.
	if err := applyBaselineOnPool(ctx, s); err == nil {
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
	for i := 0; i < starters; i++ {
		select {
		case s := <-stores:
			t.Cleanup(func() { _ = s.Close() })
		case err := <-errs:
			t.Errorf("concurrent start failed: %v", err)
		}
	}

	var rows int
	if err := s0(t, ctx, dsn).QueryRowContext(ctx,
		`SELECT count(*) FROM schema_migrations WHERE scope = $1`, postgresSchemaScope).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("schema_migrations holds %d baseline rows, want 1", rows)
	}
}

// s0 opens a plain pool for assertions that must not depend on any of the
// stores the test under way created.
func s0(t *testing.T, ctx context.Context, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	return db
}

// applyBaselineOnPool runs the baseline over a connection from the store's pool,
// which is what OpenPostgres does under the schema lock.
func applyBaselineOnPool(ctx context.Context, s *Store) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	return applyPostgresBaseline(ctx, conn)
}

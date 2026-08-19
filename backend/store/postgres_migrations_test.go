package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"rolltop/backend/store/pgtestdb"
)

func testMigration(version string, statements ...string) postgresMigration {
	return postgresMigration{Version: version, Statements: statements}
}

func TestClassifyPostgresSchemaState(t *testing.T) {
	baseline := "baseline-checksum"
	m1 := testMigration("0001-first", `CREATE TABLE first (id bigint)`)
	m2 := testMigration("0002-second", `CREATE TABLE second (id bigint)`)
	list := []postgresMigration{m1, m2}

	t.Run("empty database gets everything", func(t *testing.T) {
		state, err := classifyPostgresSchemaState(map[string]string{}, baseline, list)
		if err != nil {
			t.Fatalf("classify: %v", err)
		}
		if state.BaselinePresent {
			t.Fatal("baseline reported present on an empty database")
		}
		if len(state.Outstanding) != 2 {
			t.Fatalf("outstanding = %d, want 2", len(state.Outstanding))
		}
	})

	t.Run("up to date answers with nothing outstanding", func(t *testing.T) {
		applied := map[string]string{
			postgresSchemaVersion: baseline,
			m1.Version:            postgresMigrationChecksum(m1),
			m2.Version:            postgresMigrationChecksum(m2),
		}
		state, err := classifyPostgresSchemaState(applied, baseline, list)
		if err != nil {
			t.Fatalf("classify: %v", err)
		}
		if !state.BaselinePresent || len(state.Outstanding) != 0 {
			t.Fatalf("state = %+v, want baseline present and nothing outstanding", state)
		}
	})

	t.Run("older database gets the suffix", func(t *testing.T) {
		applied := map[string]string{
			postgresSchemaVersion: baseline,
			m1.Version:            postgresMigrationChecksum(m1),
		}
		state, err := classifyPostgresSchemaState(applied, baseline, list)
		if err != nil {
			t.Fatalf("classify: %v", err)
		}
		if len(state.Outstanding) != 1 || state.Outstanding[0].Version != m2.Version {
			t.Fatalf("outstanding = %+v, want just %s", state.Outstanding, m2.Version)
		}
	})

	t.Run("baseline mismatch is refused", func(t *testing.T) {
		applied := map[string]string{postgresSchemaVersion: "someone-elses"}
		_, err := classifyPostgresSchemaState(applied, baseline, list)
		if err == nil || !strings.Contains(err.Error(), "baseline checksum mismatch") {
			t.Fatalf("err = %v, want baseline checksum mismatch", err)
		}
	})

	t.Run("edited applied migration is refused", func(t *testing.T) {
		applied := map[string]string{
			postgresSchemaVersion: baseline,
			m1.Version:            "stale-checksum",
		}
		_, err := classifyPostgresSchemaState(applied, baseline, list)
		if err == nil || !strings.Contains(err.Error(), "was edited after this database applied it") {
			t.Fatalf("err = %v, want immutability refusal", err)
		}
	})

	t.Run("unknown applied version is refused as newer", func(t *testing.T) {
		applied := map[string]string{
			postgresSchemaVersion: baseline,
			"0009-from-the-future": "whatever",
		}
		_, err := classifyPostgresSchemaState(applied, baseline, list)
		if err == nil || !strings.Contains(err.Error(), "newer build") {
			t.Fatalf("err = %v, want newer-build refusal", err)
		}
	})

	t.Run("gap in the history is refused", func(t *testing.T) {
		applied := map[string]string{
			postgresSchemaVersion: baseline,
			m2.Version:            postgresMigrationChecksum(m2),
		}
		_, err := classifyPostgresSchemaState(applied, baseline, list)
		if err == nil || !strings.Contains(err.Error(), "cannot be explained") {
			t.Fatalf("err = %v, want unexplainable-history refusal", err)
		}
	})

	t.Run("rows without a baseline are refused", func(t *testing.T) {
		applied := map[string]string{m1.Version: postgresMigrationChecksum(m1)}
		_, err := classifyPostgresSchemaState(applied, baseline, list)
		if err == nil || !strings.Contains(err.Error(), "not created by Rolltop") {
			t.Fatalf("err = %v, want not-ours refusal", err)
		}
	})
}

// openMigrationTestConn opens a baselined store on the throwaway database and
// hands back one raw connection with the search path pinned, the way
// ensurePostgresSchema holds one.
func openMigrationTestConn(t *testing.T, dsn string) (*sql.Conn, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	s, err := OpenPostgres(ctx, dsn, PostgresOptions{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := pinPostgresSearchPath(ctx, conn); err != nil {
		t.Fatalf("pin search path: %v", err)
	}
	return conn, ctx
}

func TestApplyPostgresMigrationIsAtomicAndRecorded(t *testing.T) {
	dsn := pgtestdb.New(t)
	conn, ctx := openMigrationTestConn(t, dsn)

	good := testMigration("0001-good", `CREATE TABLE migration_probe (id bigint PRIMARY KEY)`)
	if err := applyPostgresMigration(ctx, conn, good); err != nil {
		t.Fatalf("apply migration: %v", err)
	}
	state, err := readPostgresSchemaState(ctx, conn, baselineChecksum(), []postgresMigration{good})
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if !state.BaselinePresent || len(state.Outstanding) != 0 {
		t.Fatalf("state after apply = %+v, want applied", state)
	}

	// A migration that fails midway must leave neither its row nor its DDL:
	// the first statement is valid, the second is not.
	bad := testMigration("0002-bad",
		`CREATE TABLE migration_probe_two (id bigint PRIMARY KEY)`,
		`CREATE TABLE migration_probe_two (id bigint PRIMARY KEY)`)
	if err := applyPostgresMigration(ctx, conn, bad); err == nil {
		t.Fatal("apply of a failing migration reported success")
	}
	var probeTwo sql.NullString
	if err := conn.QueryRowContext(ctx, `SELECT to_regclass('migration_probe_two')::text`).Scan(&probeTwo); err != nil {
		t.Fatalf("probe table: %v", err)
	}
	if probeTwo.Valid {
		t.Fatal("failed migration left its DDL behind")
	}
	var rows int
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM schema_migrations WHERE scope = $1 AND version = $2`,
		postgresSchemaScope, bad.Version).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 0 {
		t.Fatal("failed migration left its row behind")
	}
}

func TestEnsurePostgresSchemaAppliesOutstandingMigrations(t *testing.T) {
	dsn := pgtestdb.New(t)

	// First open: baseline only, the shipped list is empty in this build state
	// or already applied — either way the second open below must add the test
	// entry exactly once. The list is swapped, not appended to, so this test
	// cannot depend on what the binary ships.
	saved := postgresMigrations
	defer func() { postgresMigrations = saved }()

	postgresMigrations = nil
	first, err := OpenPostgres(context.Background(), dsn, PostgresOptions{})
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first open: %v", err)
	}

	postgresMigrations = []postgresMigration{
		testMigration("0001-ensure-probe", `CREATE TABLE ensure_probe (id bigint PRIMARY KEY)`),
	}
	second, err := OpenPostgres(context.Background(), dsn, PostgresOptions{})
	if err != nil {
		t.Fatalf("second open with outstanding migration: %v", err)
	}
	defer func() { _ = second.Close() }()

	conn, ctx := openMigrationTestConn(t, dsn)
	var probe sql.NullString
	if err := conn.QueryRowContext(ctx, `SELECT to_regclass('ensure_probe')::text`).Scan(&probe); err != nil {
		t.Fatalf("probe table: %v", err)
	}
	if !probe.Valid {
		t.Fatal("outstanding migration was not applied at open")
	}
	var checksum string
	if err := conn.QueryRowContext(ctx,
		`SELECT checksum FROM schema_migrations WHERE scope = $1 AND version = $2`,
		postgresSchemaScope, "0001-ensure-probe").Scan(&checksum); err != nil {
		t.Fatalf("read recorded row: %v", err)
	}
	if checksum != postgresMigrationChecksum(postgresMigrations[0]) {
		t.Fatal("recorded checksum does not match the applied migration")
	}
}

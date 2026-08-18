package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"rolltop/backend/store/pgtestdb"
)

// TestPostgresConsoleRehearsalLoop is the whole point of the console: an
// operator must be able to create the schema, look at it, drop it, and do it
// again, without any step leaving the database in a state the next one refuses.
func TestPostgresConsoleRehearsalLoop(t *testing.T) {
	ctx := context.Background()
	dsn := pgtestdb.New(t)

	for round := 1; round <= 2; round++ {
		before, err := InspectPostgres(ctx, dsn)
		if err != nil {
			t.Fatalf("round %d inspect: %v", round, err)
		}
		if before.Stage != PostgresStageEmpty {
			t.Fatalf("round %d starts at stage %q, want %q", round, before.Stage, PostgresStageEmpty)
		}
		if !before.CanCreate || before.CanDrop {
			t.Errorf("round %d empty database offers create=%v drop=%v", round, before.CanCreate, before.CanDrop)
		}

		created, err := CreatePostgresSchema(ctx, dsn)
		if err != nil {
			t.Fatalf("round %d create: %v", round, err)
		}
		if created.Stage != PostgresStageBaseline {
			t.Fatalf("round %d after create stage is %q", round, created.Stage)
		}
		if created.Tables == 0 || created.Indexes == 0 || created.ForeignKeys == 0 || created.Triggers == 0 {
			t.Errorf("round %d created %+v", round, created)
		}
		if created.Rows != 0 {
			t.Errorf("round %d created schema holds %d rows", round, created.Rows)
		}
		if created.CanCreate || !created.CanDrop {
			t.Errorf("round %d created database offers create=%v drop=%v", round, created.CanCreate, created.CanDrop)
		}
		if created.Database == "" || created.User == "" || created.ServerVersion == "" {
			t.Errorf("round %d identity is incomplete: %+v", round, created)
		}

		dropped, err := DropPostgresSchema(ctx, dsn)
		if err != nil {
			t.Fatalf("round %d drop: %v", round, err)
		}
		if dropped.Stage != PostgresStageEmpty {
			t.Fatalf("round %d after drop stage is %q, want %q", round, dropped.Stage, PostgresStageEmpty)
		}
		if dropped.Tables != 0 || dropped.Triggers != 0 {
			t.Errorf("round %d drop left %d tables and %d triggers", round, dropped.Tables, dropped.Triggers)
		}
	}
}

// TestPostgresConsoleCountsRows covers the number an operator reads before
// dropping: a schema holding a rehearsed migration must not look empty.
func TestPostgresConsoleCountsRows(t *testing.T) {
	ctx := context.Background()
	dsn := pgtestdb.New(t)

	if _, err := CreatePostgresSchema(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for i := 0; i < 3; i++ {
		if _, err := db.ExecContext(ctx, `INSERT INTO users (email, name, password_hash, created_at, updated_at)
			VALUES ($1, 'Rehearsal', 'hash', 0, 0)`, "rehearsal"+string(rune('a'+i))+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	state, err := InspectPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if state.Rows != 3 {
		t.Errorf("inspect reports %d rows, want 3", state.Rows)
	}
	if !strings.Contains(state.Summary, "3 rows") {
		t.Errorf("summary does not name the row count: %s", state.Summary)
	}
}

// TestPostgresConsoleRefusesForeignDatabase is the safety rule that matters
// most: a mistyped DSN must not let the console create into, or drop from,
// somebody else's database.
func TestPostgresConsoleRefusesForeignDatabase(t *testing.T) {
	ctx := context.Background()
	dsn := pgtestdb.New(t)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE somebody_elses_data (id int); INSERT INTO somebody_elses_data VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	state, err := InspectPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if state.Stage != PostgresStageForeign {
		t.Fatalf("stage is %q, want %q", state.Stage, PostgresStageForeign)
	}
	if state.CanCreate || state.CanDrop {
		t.Errorf("foreign database offers create=%v drop=%v", state.CanCreate, state.CanDrop)
	}
	if _, err := CreatePostgresSchema(ctx, dsn); err == nil {
		t.Error("create into a foreign database succeeded")
	}
	if _, err := DropPostgresSchema(ctx, dsn); err == nil {
		t.Error("drop against a foreign database succeeded")
	}

	// The refusal has to be real, not just an error message.
	verify, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = verify.Close() }()
	var rows int
	if err := verify.QueryRowContext(ctx, `SELECT count(*) FROM somebody_elses_data`).Scan(&rows); err != nil {
		t.Fatalf("the foreign table did not survive: %v", err)
	}
	if rows != 1 {
		t.Errorf("the foreign table holds %d rows, want 1", rows)
	}
}

// TestPostgresConsoleDropsAMismatchedSchema covers the recovery path the
// checksum rule needs: a database created by a different build is exactly what
// an operator has to be able to clear before rehearsing again.
func TestPostgresConsoleDropsAMismatchedSchema(t *testing.T) {
	ctx := context.Background()
	dsn := pgtestdb.New(t)

	if _, err := CreatePostgresSchema(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE schema_migrations SET checksum = 'from-another-build' WHERE scope = $1`, postgresSchemaScope); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	state, err := InspectPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if state.Stage != PostgresStageMismatch {
		t.Fatalf("stage is %q, want %q", state.Stage, PostgresStageMismatch)
	}
	if state.CanCreate || !state.CanDrop {
		t.Errorf("mismatched database offers create=%v drop=%v", state.CanCreate, state.CanDrop)
	}
	if _, err := CreatePostgresSchema(ctx, dsn); err == nil {
		t.Error("create over a mismatched schema succeeded")
	}
	dropped, err := DropPostgresSchema(ctx, dsn)
	if err != nil {
		t.Fatalf("drop a mismatched schema: %v", err)
	}
	if dropped.Stage != PostgresStageEmpty {
		t.Errorf("after dropping a mismatched schema the stage is %q", dropped.Stage)
	}
}

// TestPostgresConsoleLeavesExtensionsAlone pins the rehearsal loop's cost: the
// preflight installs three extensions, and dropping the schema must not make an
// operator reinstall them for every round.
func TestPostgresConsoleLeavesExtensionsAlone(t *testing.T) {
	ctx := context.Background()
	dsn := pgtestdb.New(t)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, extension := range []string{"pg_trgm", "citext", "unaccent"} {
		if _, err := db.ExecContext(ctx, `CREATE EXTENSION IF NOT EXISTS `+extension+` WITH SCHEMA public`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := CreatePostgresSchema(ctx, dsn); err != nil {
		t.Fatalf("create over a database carrying only extensions: %v", err)
	}
	if _, err := DropPostgresSchema(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	var extensions int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_extension WHERE extname IN ('pg_trgm', 'citext', 'unaccent')`).Scan(&extensions); err != nil {
		t.Fatal(err)
	}
	if extensions != 3 {
		t.Errorf("%d of 3 extensions survived the drop", extensions)
	}
}

// TestPostgresConsoleNeverEchoesCredentials keeps the console under the same
// rule as everything else that touches a DSN.
func TestPostgresConsoleNeverEchoesCredentials(t *testing.T) {
	ctx := context.Background()
	const dsn = "host=127.0.0.1 password = hunter2 sslmode=bogus"
	operations := map[string]func(context.Context, string) (PostgresState, error){
		"inspect": InspectPostgres,
		"create":  CreatePostgresSchema,
		"drop":    DropPostgresSchema,
	}
	for name, run := range operations {
		if _, err := run(ctx, dsn); err == nil {
			t.Errorf("%s against an unparseable DSN succeeded", name)
		} else if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s leaked the password: %v", name, err)
		}
	}
}

func TestPostgresConsoleRequiresDSN(t *testing.T) {
	ctx := context.Background()
	if _, err := InspectPostgres(ctx, "   "); err == nil {
		t.Error("inspect without a DSN succeeded")
	}
}

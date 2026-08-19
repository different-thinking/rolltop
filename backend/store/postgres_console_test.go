package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"rolltop/backend/sqlident"
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

// TestPostgresConsoleDropSparesForeignObjects is the sharper version of the
// refusal test. A database can carry the Rolltop schema *and* something else —
// the stage check calls that "baseline", because the version row is there and
// matches — so the drop has to remove the objects the baseline declares and
// nothing more. Enumerating every non-extension relation took the operator's
// own tables, in their own schemas, with it.
func TestPostgresConsoleDropSparesForeignObjects(t *testing.T) {
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
	// A table beside the schema, a table in the operator's own schema, and a
	// function of their own: all three were destroyed by the previous drop.
	for _, ddl := range []string{
		`CREATE TABLE public.operator_notes (id int)`,
		`INSERT INTO public.operator_notes VALUES (1)`,
		`CREATE SCHEMA reporting`,
		`CREATE TABLE reporting.monthly (id int)`,
		`INSERT INTO reporting.monthly VALUES (1)`,
		`CREATE FUNCTION public.operator_helper() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$`,
	} {
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			t.Fatalf("%s: %v", ddl, err)
		}
	}

	if _, err := DropPostgresSchema(ctx, dsn); err != nil {
		t.Fatal(err)
	}

	for _, query := range []string{
		`SELECT count(*) FROM public.operator_notes`,
		`SELECT count(*) FROM reporting.monthly`,
		`SELECT public.operator_helper()`,
	} {
		var value int
		if err := db.QueryRowContext(ctx, query).Scan(&value); err != nil {
			t.Errorf("the drop destroyed something it does not own (%s): %v", query, err)
		}
	}
	// And it did do its job: no Rolltop table is left.
	var rolltopTables int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'schema_migrations'`).Scan(&rolltopTables); err != nil {
		t.Fatal(err)
	}
	if rolltopTables != 0 {
		t.Error("the drop left the Rolltop schema behind")
	}
}

// TestPostgresConsoleWithRoleNamedSchema covers a server shape the generated
// SQL is blind to. PostgreSQL's default search path is `"$user", public`, so a
// schema named after the connecting role captures every unqualified CREATE
// TABLE. The baseline is unqualified, and the checks look in public by name, so
// without a pinned search path the console creates the schema into the wrong
// place and then reports the database as somebody else's — refusing both the
// create and the drop, with no way forward.
func TestPostgresConsoleWithRoleNamedSchema(t *testing.T) {
	ctx := context.Background()
	dsn := pgtestdb.New(t)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var role string
	if err := db.QueryRowContext(ctx, `SELECT current_user`).Scan(&role); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS `+sqlident.Quote(role)); err != nil {
		t.Fatal(err)
	}

	created, err := CreatePostgresSchema(ctx, dsn)
	if err != nil {
		t.Fatalf("create with a role-named schema present: %v", err)
	}
	if created.Stage != PostgresStageBaseline {
		t.Fatalf("after create the stage is %q, want %q", created.Stage, PostgresStageBaseline)
	}
	// The tables have to be in public, not in the role's schema, or every
	// later check looks in the wrong place.
	var elsewhere int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = $1`, role).Scan(&elsewhere); err != nil {
		t.Fatal(err)
	}
	if elsewhere != 0 {
		t.Errorf("%d tables landed in the role-named schema %q instead of public", elsewhere, role)
	}
	if dropped, err := DropPostgresSchema(ctx, dsn); err != nil {
		t.Fatalf("drop with a role-named schema present: %v", err)
	} else if dropped.Stage != PostgresStageEmpty {
		t.Errorf("after drop the stage is %q", dropped.Stage)
	}
}

// TestOpenPostgresWithRoleNamedSchema is the same hazard on the startup path,
// which applies the baseline through its own connection.
func TestOpenPostgresWithRoleNamedSchema(t *testing.T) {
	ctx := context.Background()
	dsn := pgtestdb.New(t)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var role string
	if err := db.QueryRowContext(ctx, `SELECT current_user`).Scan(&role); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS `+sqlident.Quote(role)); err != nil {
		t.Fatal(err)
	}

	s, err := OpenPostgres(ctx, dsn, PostgresOptions{})
	if err != nil {
		t.Fatalf("open with a role-named schema present: %v", err)
	}
	defer func() { _ = s.Close() }()
	var elsewhere int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = $1`, role).Scan(&elsewhere); err != nil {
		t.Fatal(err)
	}
	if elsewhere != 0 {
		t.Errorf("%d tables landed in the role-named schema %q instead of public", elsewhere, role)
	}
	// Reopening must find the schema rather than declare the database foreign.
	second, err := OpenPostgres(ctx, dsn, PostgresOptions{})
	if err != nil {
		t.Fatalf("reopen with a role-named schema present: %v", err)
	}
	_ = second.Close()
}

// TestPostgresConsoleRejectsNonRelationObjects covers the third way a database
// is not empty. A relation-only check saw nothing in a database holding a
// function, an enum, or a domain — verified against Postgres 16 — and created
// the baseline on top of it.
func TestPostgresConsoleRejectsNonRelationObjects(t *testing.T) {
	occupants := map[string]string{
		"function":       `CREATE FUNCTION someones_helper() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$`,
		"enum":           `CREATE TYPE mood AS ENUM ('good', 'bad')`,
		"domain":         `CREATE DOMAIN postcode AS text`,
		"composite type": `CREATE TYPE address AS (street text, city text)`,
		// A shell type: the catalog records it as a pseudo-type with
		// typisdefined false, so a predicate matching on kind alone misses it.
		"shell type": `CREATE TYPE someones_shell`,
	}
	for name, ddl := range occupants {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
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

			state, err := InspectPostgres(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			if state.Stage != PostgresStageForeign {
				t.Fatalf("a database holding a %s inspects as %q", name, state.Stage)
			}
			if len(state.Blocking) == 0 {
				t.Errorf("state names nothing as blocking: %+v", state)
			}
			if _, err := CreatePostgresSchema(ctx, dsn); err == nil {
				t.Errorf("created the schema over a %s", name)
			}
		})
	}
}

// TestPostgresConsoleToleratesEmptySchemas is the counterweight. PostgreSQL's
// default search path names a schema after the connecting role and managed
// providers create it, so an empty schema must not read as somebody else's
// database — refusing it would refuse the very shape pinPostgresSearchPath
// exists to support.
func TestPostgresConsoleToleratesEmptySchemas(t *testing.T) {
	ctx := context.Background()
	dsn := pgtestdb.New(t)

	seed, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.ExecContext(ctx, `CREATE SCHEMA someones_empty_schema`); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	state, err := InspectPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if state.Stage != PostgresStageEmpty {
		t.Fatalf("a database holding only an empty schema inspects as %q (%v)", state.Stage, state.Blocking)
	}
	if _, err := CreatePostgresSchema(ctx, dsn); err != nil {
		t.Fatalf("create over an empty schema: %v", err)
	}
}

// TestPostgresConsoleNamesWhatBlocksIt covers the dead end a bare refusal
// leaves behind. Dropping the schema from a database that also holds an older
// build's tables — indistinguishable from an operator's own, which is why
// neither is dropped — leaves objects the console will not create over. Saying
// only "not ours" gives the operator nothing to act on; the names do.
func TestPostgresConsoleNamesWhatBlocksIt(t *testing.T) {
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
	// A table the current baseline does not declare, which is what an older
	// build's leftovers look like from here.
	if _, err := db.ExecContext(ctx, `CREATE TABLE legacy_from_another_build (id int)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE schema_migrations SET checksum = 'from-another-build' WHERE scope = $1`, postgresSchemaScope); err != nil {
		t.Fatal(err)
	}

	dropped, err := DropPostgresSchema(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if dropped.Stage != PostgresStageForeign {
		t.Fatalf("after dropping a mismatched schema with leftovers the stage is %q", dropped.Stage)
	}
	if !containsSubstring(dropped.Blocking, "legacy_from_another_build") {
		t.Errorf("state does not name the leftover object: %v", dropped.Blocking)
	}
	if !strings.Contains(dropped.Summary, "legacy_from_another_build") {
		t.Errorf("summary does not name the leftover object: %s", dropped.Summary)
	}
	// And the refusals that follow have to name it too, or the operator is back
	// to guessing.
	_, err = CreatePostgresSchema(ctx, dsn)
	if err == nil {
		t.Fatal("create over leftovers succeeded")
	}
	if !strings.Contains(err.Error(), "legacy_from_another_build") {
		t.Errorf("create refusal does not name what is in the way: %v", err)
	}
	_, err = DropPostgresSchema(ctx, dsn)
	if err == nil {
		t.Fatal("drop against leftovers succeeded")
	}
	if !strings.Contains(err.Error(), "legacy_from_another_build") {
		t.Errorf("drop refusal does not name what is in the way: %v", err)
	}
}

func containsSubstring(values []string, want string) bool {
	for _, value := range values {
		if strings.Contains(value, want) {
			return true
		}
	}
	return false
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

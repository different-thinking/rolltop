package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"rolltop/backend/store/pgschema"
)

// baselinePath is the committed PostgreSQL baseline, generated from the
// schema this package's migrations produce.
const baselinePath = "pgschema/baseline.sql"

// TestBaselineMatchesSQLiteSchema regenerates the PostgreSQL baseline from a
// freshly migrated combined SQLite database and compares it with the
// committed file. It is what keeps WP2's "the baseline is derived, not
// hand-written" claim true: adding a SQLite migration without regenerating
// fails here rather than silently leaving the two schemas apart.
//
// Run with -update to rewrite the file.
func TestBaselineMatchesSQLiteSchema(t *testing.T) {
	generated := generateBaseline(t)
	committed, err := os.ReadFile(baselinePath)
	if err != nil {
		if os.IsNotExist(err) && updateGolden() {
			writeBaseline(t, generated)
			return
		}
		t.Fatal(err)
	}
	if string(committed) == generated {
		return
	}
	if updateGolden() {
		writeBaseline(t, generated)
		return
	}
	t.Fatalf("%s is out of date with the SQLite schema.\n"+
		"Regenerate it with:\n\n    go test ./backend/store/ -run TestBaselineMatchesSQLiteSchema -update\n\n"+
		"and review the diff: every change there is a change the migration's\n"+
		"data copy and the ported queries have to account for.", baselinePath)
}

// TestBaselineCoversEverySQLiteTable guards against a translation that drops
// objects silently: every table and index in the SQLite schema must appear in
// the baseline.
func TestBaselineCoversEverySQLiteTable(t *testing.T) {
	objects := readSQLiteSchema(t)
	translated, err := pgschema.Translate(objects)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, object := range translated {
		seen[object.Kind+" "+object.Name] = true
	}
	for _, object := range objects {
		if !seen[object.Kind+" "+object.Name] {
			t.Errorf("baseline is missing %s %s", object.Kind, object.Name)
		}
	}
	// Foreign keys are emitted as their own phase, one object per table that
	// has any, so the translated set is a superset rather than a 1:1 mapping.
	for _, object := range translated {
		if object.Kind == "foreign keys" {
			continue
		}
		if !hasSQLiteObject(objects, object.Kind, object.Name) {
			t.Errorf("baseline invents %s %s", object.Kind, object.Name)
		}
	}
}

func hasSQLiteObject(objects []pgschema.SQLiteObject, kind, name string) bool {
	for _, object := range objects {
		if object.Kind == kind && object.Name == name {
			return true
		}
	}
	return false
}

func generateBaseline(t *testing.T) string {
	t.Helper()
	translated, err := pgschema.Translate(readSQLiteSchema(t))
	if err != nil {
		t.Fatal(err)
	}
	return pgschema.Render(translated)
}

// readSQLiteSchema opens a throwaway combined store, which runs every
// migration, and reads the resulting schema.
func readSQLiteSchema(t *testing.T) []pgschema.SQLiteObject {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// Autoindexes have no CREATE statement and are recreated by the
	// constraints they belong to, so sql IS NULL filters exactly the objects
	// the baseline gets for free.
	rows, err := db.DB().QueryContext(context.Background(), `
		SELECT type, name, sql FROM sqlite_master
		WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'
		ORDER BY CASE type WHEN 'table' THEN 0 WHEN 'index' THEN 1 ELSE 2 END, rootpage`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var objects []pgschema.SQLiteObject
	for rows.Next() {
		var object pgschema.SQLiteObject
		if err := rows.Scan(&object.Kind, &object.Name, &object.SQL); err != nil {
			t.Fatal(err)
		}
		objects = append(objects, object)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(objects) == 0 {
		t.Fatal("the migrated SQLite schema is empty")
	}
	return objects
}

func writeBaseline(t *testing.T, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(baselinePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(baselinePath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s", baselinePath)
}

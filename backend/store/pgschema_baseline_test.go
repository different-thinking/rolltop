package store

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"rolltop/backend/plugins"
	"rolltop/backend/store/pgschema"
)

// updateBaseline rewrites the generated PostgreSQL baseline instead of
// failing when it differs.
var updateBaseline = flag.Bool("update", false, "rewrite generated golden files")

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
		if os.IsNotExist(err) && *updateBaseline {
			writeBaseline(t, generated)
			return
		}
		t.Fatal(err)
	}
	if string(committed) == generated {
		return
	}
	if *updateBaseline {
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
	source := map[string]bool{}
	for _, object := range objects {
		source[object.Kind+" "+object.Name] = true
	}
	translatedSet := map[string]bool{}
	for _, object := range translated {
		translatedSet[object.Kind+" "+object.Name] = true
	}
	for key := range source {
		if !translatedSet[key] {
			t.Errorf("baseline is missing %s", key)
		}
	}
	// Foreign keys are emitted as their own phase, one object per table that
	// has any, so the translated set is a superset rather than a 1:1 mapping.
	for _, object := range translated {
		if object.Kind == pgschema.ForeignKeysKind {
			continue
		}
		if !source[object.Kind+" "+object.Name] {
			t.Errorf("baseline invents %s %s", object.Kind, object.Name)
		}
	}
}

// TestEveryPluginWithMigrationsIsInTheCatalog guards the input side of the
// derivation. TestBaselineCoversEverySQLiteTable proves the baseline contains
// everything the derivation *ran*; this proves the derivation runs everything
// there is. A plugin directory carrying migrations but no manifest.json would
// otherwise be skipped by LoadManifests, and its tables would go missing from
// the baseline exactly the way the file-backed migrations did.
func TestEveryPluginWithMigrationsIsInTheCatalog(t *testing.T) {
	entries, err := os.ReadDir(pluginRoot)
	if err != nil {
		t.Fatal(err)
	}
	manifests, err := plugins.LoadManifests(pluginRoot)
	if err != nil {
		t.Fatal(err)
	}
	catalog := pluginCatalogFromManifests(manifests)
	known := map[string]bool{}
	for _, definition := range catalog.definitions {
		known[definition.ID] = true
	}
	checked := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(pluginRoot, entry.Name(), "migrations")); err != nil {
			// No migrations directory: nothing this plugin can contribute to
			// the schema. plugins/bundled is such a directory today.
			continue
		}
		checked++
		if !known[entry.Name()] {
			t.Errorf("plugin %s has migrations but is not in the catalog, so its tables would be missing from the baseline", entry.Name())
		}
	}
	if checked == 0 {
		t.Fatal("no plugin migration directories found; this test would prove nothing")
	}
	t.Logf("verified %d plugins with migrations are in the catalog", checked)
}

// TestBaselineIncludesPluginTables pins the coverage gap that the derivation
// had at first: Open() installs only the statically compiled plugin catalog,
// so every table from a file-backed plugin migration was missing from the
// baseline and no test could see it.
func TestBaselineIncludesPluginTables(t *testing.T) {
	baseline, err := os.ReadFile(baselinePath)
	if err != nil {
		t.Fatal(err)
	}
	manifests, err := plugins.LoadManifests(pluginRoot)
	if err != nil {
		t.Fatal(err)
	}
	migrations := plugins.MigrationsFromManifests(manifests, "")
	if len(migrations) == 0 {
		t.Fatal("no file-backed plugin migrations found; this test would prove nothing")
	}
	tableRE := regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z0-9_]+)`)
	checked := 0
	for _, migration := range migrations {
		for _, statement := range migration.Statements {
			for _, match := range tableRE.FindAllStringSubmatch(statement, -1) {
				checked++
				if !strings.Contains(string(baseline), "\nCREATE TABLE "+match[1]+" (") {
					t.Errorf("plugin table %s (from %s) is missing from the baseline", match[1], migration.PluginID)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no plugin CREATE TABLE statements were checked")
	}
	t.Logf("verified %d plugin tables are in the baseline", checked)
}

func generateBaseline(t *testing.T) string {
	t.Helper()
	translated, err := pgschema.Translate(readSQLiteSchema(t))
	if err != nil {
		t.Fatal(err)
	}
	return pgschema.Render(translated)
}

// pluginRoot is the repository's plugin tree, relative to this package.
const pluginRoot = "../../plugins"

// readSQLiteSchema opens a throwaway combined store carrying the *production*
// plugin catalog and reads the resulting schema.
//
// Two things here are load-bearing and were both wrong in the first revision:
//
//   - Open() installs only the statically compiled plugin catalog, while
//     production merges in the file-backed migrations under
//     plugins/*/migrations/. Deriving from Open() left 20 plugin tables out of
//     the baseline, and the coverage test could not see it because it compared
//     the translation against the same incomplete source.
//   - Plugin migrations run when a plugin is enabled, not when the store is
//     opened. The baseline must contain every table any plugin could create,
//     because after the cutover enabling a plugin would otherwise replay
//     SQLite-dialect DDL against PostgreSQL.
func readSQLiteSchema(t *testing.T) []pgschema.SQLiteObject {
	t.Helper()
	manifests, err := plugins.LoadManifests(pluginRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) == 0 {
		t.Fatalf("no plugin manifests under %s; the baseline would silently omit plugin tables", pluginRoot)
	}
	db, err := open(filepath.Join(t.TempDir(), "rolltop.db"), "", false, schemaCombined, nil,
		pluginCatalogFromManifests(manifests), AccessAuto)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, definition := range db.pluginDefinitions {
		if err := db.ApplyPluginMigrations(context.Background(), definition.ID); err != nil {
			t.Fatalf("apply migrations for plugin %s: %v", definition.ID, err)
		}
	}
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

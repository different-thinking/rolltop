package pgschema

import (
	_ "embed"
	"strings"
	"sync"
)

// Baseline is the generated PostgreSQL schema, embedded so the server can
// create an empty database without shipping the .sql file next to the binary.
// TestBaselineMatchesSQLiteSchema regenerates the file this embeds from the
// migrated SQLite schema, so what runs in production is what that test checked.
//
//go:embed baseline.sql
var Baseline string

// Schema is the namespace the baseline creates its objects in. Nothing in the
// generated SQL says so — the statements are unqualified — so every connection
// that applies or inspects it has to pin the search path to this, or a
// role-named schema ahead of public silently takes the whole schema.
const Schema = "public"

var declaredOnce struct {
	sync.Once
	objects []Object
}

// Declared lists what the baseline creates, parsed from the "-- <kind> <name>"
// headers Render writes.
//
// It exists so callers can act on the baseline's objects by name rather than by
// enumerating whatever happens to be in the database. Dropping "every table in
// a non-system schema" would take an operator's own tables with it; dropping
// the names listed here cannot.
//
// The returned slice is shared, so callers must not modify it.
func Declared() []Object {
	declaredOnce.Do(func() {
		declaredOnce.objects = parseDeclared(Baseline)
	})
	return declaredOnce.objects
}

// DeclaredNames returns the names of the baseline's objects of one kind, in the
// order the baseline declares them.
func DeclaredNames(kind string) []string {
	var names []string
	for _, object := range Declared() {
		if object.Kind == kind {
			names = append(names, object.Name)
		}
	}
	return names
}

// parseDeclared reads the object headers. Only a header at the start of a line
// counts, so a "-- table x" inside a trigger body or a column comment is not
// mistaken for a declaration.
func parseDeclared(baseline string) []Object {
	kinds := []string{ForeignKeysKind, TableKind, IndexKind, TriggerKind}
	var objects []Object
	for _, line := range strings.Split(baseline, "\n") {
		if !strings.HasPrefix(line, "-- ") {
			continue
		}
		for _, kind := range kinds {
			prefix := "-- " + kind + " "
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			name := strings.TrimSpace(strings.TrimPrefix(line, prefix))
			if name != "" {
				objects = append(objects, Object{Kind: kind, Name: name})
			}
			break
		}
	}
	return objects
}

// File overview: A short, stable name for "this build's schema", and the one
// way tests get that schema into a database.
//
// Tests clone their database from a template, and the template has to be
// invalidated whenever the schema changes. Naming it after the schema is what
// makes that automatic and race-free: a changed schema selects a new template
// rather than requiring the old one to be dropped, and dropping it would race
// the test binaries still cloning from it.

package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"rolltop/backend/store/pgschema"
)

// SchemaTag identifies the schema a template database carries.
//
// It covers the baseline and deliberately nothing else. Plugin migrations are
// excluded because the catalog is not a constant: a test binary that loads a
// compiled plugin registers more migrations than one that does not, so
// including them made the tag change part-way through a run and every test
// after that point asked for a template nobody had built.
//
// Leaving them out costs nothing, because the plugin migrations are applied to
// each cloned database when the store opens it — which is what they have to do
// in production anyway.
func SchemaTag() string {
	sum := sha256.Sum256([]byte(pgschema.Baseline))
	return hex.EncodeToString(sum[:])[:16]
}

// PrepareTestTemplate puts exactly the schema SchemaTag names into a database.
// It is the builder side of that contract, kept here so the tag and the schema
// it describes cannot drift apart.
func PrepareTestTemplate(ctx context.Context, dsn string) error {
	db, err := OpenPostgres(ctx, dsn, PostgresOptions{MaxConns: 2, baselineOnly: true})
	if err != nil {
		return err
	}
	return db.Close()
}

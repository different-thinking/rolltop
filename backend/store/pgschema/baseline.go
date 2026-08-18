package pgschema

import _ "embed"

// Baseline is the generated PostgreSQL schema, embedded so the server can
// create an empty database without shipping the .sql file next to the binary.
// TestBaselineMatchesSQLiteSchema regenerates the file this embeds from the
// migrated SQLite schema, so what runs in production is what that test checked.
//
//go:embed baseline.sql
var Baseline string

// File overview: The schema-version bookkeeping every schema change is recorded
// through: one row per applied migration in schema_migrations, protected by a
// checksum so a migration cannot silently change after it has been used.
//
// There is no migration *runner* here any more, and that is the point. The
// SQLite chain this file used to drive is gone; PostgreSQL gets the squashed
// baseline (backend/store/pgschema) applied in one shot by ensurePostgresSchema
// and recorded as a single row. Plugin migrations keep their own bookkeeping in
// plugin_migrations. What is left is the two pieces both of those share: the
// checksum, and the progress type the startup page renders.
//
// The first core schema change after the cutover needs a mechanism this does
// not yet have — a migration layered on top of the baseline, for databases that
// already carry it (§11 of docs/postgres-migration-plan.md). Until then,
// changing the schema means changing baseline.sql, which only a database that
// does not exist yet can pick up.

package store

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// MigrationProgress is emitted while the schema is applied at open. cmd/rolltop
// turns these fields into the startup page and /api/startup response.
type MigrationProgress struct {
	Scope     string `json:"scope"`
	Migration string `json:"migration"`
	Step      string `json:"step"`
	Done      int    `json:"done"`
	Total     int    `json:"total"`
}

// MigrationReporter receives best-effort migration progress. It must not log
// secrets or message contents because startup status is exposed over HTTP.
type MigrationReporter func(MigrationProgress)

// schemaChecksum fingerprints what a schema_migrations row stands for, so an
// edit to already-applied SQL is refused at open rather than half-applied.
//
// The statements are joined with a separator byte rather than concatenated,
// which keeps two different splits of the same text from hashing alike.
func schemaChecksum(scope, version string, statements ...string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(scope))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(version))
	for _, stmt := range statements {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(strings.TrimSpace(stmt)))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func reportMigration(progress MigrationReporter, p MigrationProgress) {
	if progress != nil {
		progress(p)
	}
}

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
// Core schema changes after the cutover are incremental migrations layered on
// the frozen baseline (postgresMigrations, postgres_migrations.go), applied by
// ensurePostgresSchema when outstanding. baseline.sql itself is never edited
// again: only a database that does not exist yet could pick the edit up, and
// every database that does exist would refuse to start.

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
// which keeps two different splits of the same text from hashing alike, and
// each one goes through normalizeSQL first so that reformatting a migration is
// not the same event as editing one. See sqlnorm.go for why that distinction
// had to be made.
func schemaChecksum(scope, version string, statements ...string) string {
	return schemaDigest(scope, version, normalizeSQL, statements...)
}

// schemaChecksumLegacy reproduces the byte-exact checksum shipped builds
// recorded before normalizeSQL existed. It is never written; it exists so a row
// an older build left behind is recognised as the same schema rather than read
// as tampering.
func schemaChecksumLegacy(scope, version string, statements ...string) string {
	return schemaDigest(scope, version, strings.TrimSpace, statements...)
}

func schemaDigest(scope, version string, normalize func(string) string, statements ...string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(scope))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(version))
	for _, stmt := range statements {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(normalize(stmt)))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// checksumRecognised reports whether a checksum recorded in a database stands
// for the text this binary carries, under any algorithm a shipped build wrote.
//
// Recognising an older checksum is what lets an upgrade proceed without
// rewriting anything, and *not* rewriting is deliberate: a recorded checksum is
// the one thing an older binary reads to decide whether it may run. Repairing
// rows to the current algorithm would close the door behind the upgrade — a
// rollback, or a not-yet-restarted replica in a rolling deploy, would meet the
// very refusal this recognition exists to prevent. The cost of leaving them is
// two more hashes per start, which is nothing.
//
// superseded carries checksums of specific historical texts that differ from
// today's only in layout. Note what leaving legacy rows alone costs: a database
// that recorded one is still tied to the exact bytes it applied, so reformatting
// an already-released migration needs its previous checksum listed here. Only
// databases written since normalisation are free of that.
func checksumRecognised(stored, current, legacy string, superseded ...string) bool {
	stored = strings.TrimSpace(stored)
	if stored == current || stored == legacy {
		return true
	}
	for _, candidate := range superseded {
		if stored == candidate {
			return true
		}
	}
	return false
}

// checksumIdentity pairs the checksum a start records for some text with the
// one an older build recorded for the same text. Passing the pair around keeps
// the recognition rule in one place instead of at each comparison.
type checksumIdentity struct {
	// current is what this binary writes.
	current string
	// legacy is what a build before normalizeSQL wrote for the same text.
	legacy string
}

// recognises reports whether a recorded checksum stands for this text.
func (c checksumIdentity) recognises(stored string) bool {
	return checksumRecognised(stored, c.current, c.legacy)
}

func reportMigration(progress MigrationReporter, p MigrationProgress) {
	if progress != nil {
		progress(p)
	}
}

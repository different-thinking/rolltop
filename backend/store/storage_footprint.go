// File overview: What one tenant's mail occupies inside PostgreSQL, as a figure
// that belongs to that tenant rather than to the server.

package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"

	"rolltop/backend/sqlident"
)

// mailFootprintTables are the tables one tenant's mail is made of: the message
// headers and previews, the attachment and location rows hanging off them, and
// the blob records that point at the raw files on the volume. Each is measured
// for the rows carrying this tenant's user_id.
var mailFootprintTables = []string{"messages", "attachments", "locations", "blobs"}

// mailFootprintRowOverhead is the per-row cost no column accounts for: the
// 23-byte heap tuple header rounded up to the 24-byte alignment boundary. It is
// a constant rather than a measurement because the alternative - asking
// PostgreSQL to size the whole row - is what makes this query expensive.
const mailFootprintRowOverhead = 24

// UserMailRowBytes sums the stored size of one tenant's mail rows.
//
// The database reports one size for itself and nothing per tenant, and that
// total is an operator's number: it covers every tenant at once, plus indexes,
// plus the space autovacuum has not reclaimed. Summing this tenant's own rows is
// the share a reader can be shown without describing somebody else's mailbox. It
// measures the data and not the indexes built on it, so it reads slightly under
// what the tenant costs the server - the direction an under-count belongs in for
// a figure sitting next to two measured directory sizes.
//
// Deliberately not `pg_column_size(m.*)`: sizing a whole row flattens it, which
// reads every out-of-line TOAST chunk the tenant's message previews occupy. That
// turns a heap scan into a full read of the body corpus - measured at roughly
// fifty times the cost - for a figure that comes out the same. Variable-width
// columns are sized individually, which reports what they occupy as stored,
// compression included, without fetching anything; the fixed-width ones are
// arithmetic on the row count.
//
// The scan is still a scan, so callers cache it; the settings page holds every
// storage figure for five minutes.
func (s *Store) UserMailRowBytes(ctx context.Context, userID int64) (int64, error) {
	if userID <= 0 {
		return 0, fmt.Errorf("user id must be positive")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, table := range mailFootprintTables {
		layout, err := s.mailFootprintLayout(ctx, db, table)
		if err != nil {
			return 0, err
		}
		var rows, variable int64
		if err := db.QueryRowContext(ctx, `SELECT count(*), `+layout.variableSum+`
			FROM `+sqlident.Quote(table)+` t WHERE t.user_id = ?`, userID).Scan(&rows, &variable); err != nil {
			return 0, err
		}
		total += variable + rows*(layout.fixedBytes+mailFootprintRowOverhead)
	}
	return total, nil
}

// mailFootprintLayout is how one table's columns divide into the two halves of
// the measurement above.
type mailFootprintLayout struct {
	// variableSum is a SQL expression summing every variable-width column of
	// one row, or the literal 0 when the table has none.
	variableSum string
	// fixedBytes is what one row's fixed-width columns occupy.
	fixedBytes int64
}

// mailFootprintLayout resolves and caches one table's layout from the catalog.
//
// Reading it from the catalog rather than listing columns here is what keeps the
// figure honest as the schema grows: a migration that adds a text column is
// measured the next time the process starts, instead of silently falling out of
// a list nobody remembers to update.
func (s *Store) mailFootprintLayout(ctx context.Context, db *sql.DB, table string) (mailFootprintLayout, error) {
	if cached, ok := s.mailFootprint.Load(table); ok {
		return cached.(mailFootprintLayout), nil
	}
	rows, err := db.QueryContext(ctx, `SELECT a.attname, t.typlen
		FROM pg_attribute a
		JOIN pg_type t ON t.oid = a.atttypid
		WHERE a.attrelid = to_regclass(?) AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attnum`, table)
	if err != nil {
		return mailFootprintLayout{}, err
	}
	defer rows.Close()
	layout := mailFootprintLayout{}
	terms := make([]string, 0, 16)
	for rows.Next() {
		var name string
		var length int
		if err := rows.Scan(&name, &length); err != nil {
			return mailFootprintLayout{}, err
		}
		if length > 0 {
			layout.fixedBytes += int64(length)
			continue
		}
		terms = append(terms, "pg_column_size(t."+sqlident.Quote(name)+")")
	}
	if err := rows.Err(); err != nil {
		return mailFootprintLayout{}, err
	}
	if layout.fixedBytes == 0 && len(terms) == 0 {
		// to_regclass answers NULL for a name the connection's search_path cannot
		// resolve, and the join then yields no columns. Sizing a table as zero
		// bytes because it was not found is a wrong answer wearing a right one's
		// clothes, and it would be cached.
		return mailFootprintLayout{}, fmt.Errorf("table %q has no readable columns", table)
	}
	if len(terms) == 0 {
		layout.variableSum = "0"
	} else {
		layout.variableSum = "coalesce(sum(" + strings.Join(terms, " + ") + "), 0)"
	}
	s.mailFootprint.Store(table, layout)
	return layout, nil
}

// mailFootprintCache is the per-table layout cache. It is a package-level type
// alias so the Store field stays readable.
type mailFootprintCache = sync.Map

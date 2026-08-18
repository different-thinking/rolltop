// File overview: Quoting SQL identifiers. Both engines in this repository take
// the SQL-standard form — double quotes, with an embedded quote doubled — so
// one implementation serves the SQLite salvage paths and the PostgreSQL test
// helper alike. It lives in its own leaf package because those two callers
// cannot import each other.

package sqlident

import "strings"

// Quote renders name as a quoted SQL identifier.
//
// Callers pass names they produced or read from the database's own catalog, so
// this guards against a name that collides with a keyword or carries a quote,
// not against a hostile identifier.
func Quote(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

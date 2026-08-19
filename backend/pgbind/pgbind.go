// File overview: Translating `?` parameter placeholders into PostgreSQL's
// numbered `$1..$n` form, on the way to the driver.
//
// Why this is done here rather than in the SQL text at each of the ~1,000 call
// sites: a large share of this codebase's statements are assembled at run time
// from fragments — a shared SELECT prefix, a filter clause chosen by the
// caller, an `IN (…)` list sized to its arguments. Numbering those correctly in
// the source means every fragment knowing how many parameters every fragment
// before it contributed, which is not a property the source can carry. Doing it
// on the finished statement is correct by construction: the string being
// numbered is exactly the string the server will parse.
//
// It composes with converting the SQL text instead of competing with it.
// Already-numbered statements pass through untouched, so a file may be
// converted to `$n` at any time without a flag day, and this layer can be
// removed once none are left. What it will not do is let one statement use both
// styles at once: numbering `?` from $1 in a statement that already says `$1`
// binds two different arguments to the same slot, so that combination is a
// refusal rather than a rewrite (see MixedPlaceholderError).
//
// The scanner is deliberately literal-aware. A `?` inside a string literal, a
// quoted identifier, a comment, or a dollar-quoted body is data, not a
// placeholder, and rewriting one would corrupt the statement — the trigger
// function in the baseline is a dollar-quoted body, and mail queries compare
// against strings that really do contain question marks.

package pgbind

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// MixedPlaceholderError reports a statement that uses both `?` and `$n`.
//
// This is a programming error in the assembling code, not a runtime condition:
// the fragment that was converted to `$n` and the fragment that was not have
// been concatenated, and there is no numbering that satisfies both. Refusing is
// the only safe answer, because the plausible-looking one — numbering the `?`
// from $1 — silently binds a caller's second argument to the slot the first
// fragment meant, which returns wrong rows rather than failing.
type MixedPlaceholderError struct {
	// Query is the finished statement, as assembled. It carries no user data:
	// values travel as parameters, which is what the placeholders are for.
	Query string
	// NumberedAt and PlaceholderAt are byte offsets into Query of the first
	// `$n` reference and the first `?` placeholder, so the fragment boundary
	// that produced the mix can be found.
	NumberedAt    int
	PlaceholderAt int
}

func (e *MixedPlaceholderError) Error() string {
	return fmt.Sprintf("pgbind: statement mixes $n (offset %d) and ? (offset %d) placeholders, which cannot be numbered consistently: %s",
		e.NumberedAt, e.PlaceholderAt, e.Query)
}

// Rebind rewrites `?` placeholders as `$1..$n`.
//
// A statement that carries no `?` outside quoted text is returned unchanged and
// without allocating, which is both the fast path for converted SQL and what
// makes the two styles coexist across statements. Mixing them within one
// statement returns a *MixedPlaceholderError.
func Rebind(query string) (string, error) {
	if cached, ok := loadRebound(query); ok {
		return cached.sql, cached.err
	}
	sql, err := rebind(query)
	storeRebound(query, sql, err)
	return sql, err
}

// rebind is the scanner. It walks the statement once, copying nothing until it
// meets a placeholder that actually has to be rewritten; from there it appends
// each stretch of untouched text as it goes.
func rebind(query string) (string, error) {
	var b strings.Builder
	// copied marks how much of query has been written to b. It is meaningless
	// until n > 0, which is what keeps the untouched case allocation-free.
	copied := 0
	n := 0
	numberedAt := -1
	placeholderAt := -1

	for i := 0; i < len(query); {
		switch c := query[i]; c {
		case '\'', '"':
			i = skipQuoted(query, i, c)
		case '-':
			if end, ok := skipLineComment(query, i); ok {
				i = end
				continue
			}
			i++
		case '/':
			if end, ok := skipBlockComment(query, i); ok {
				i = end
				continue
			}
			i++
		case '$':
			if end, ok := skipDollarQuoted(query, i); ok {
				i = end
				continue
			}
			// Not a quote opener, so `$` followed by a digit is a parameter
			// reference: the statement is already numbered, at least in part.
			if numberedAt < 0 && i+1 < len(query) && isDigit(query[i+1]) {
				numberedAt = i
			}
			i++
		case '?':
			// PostgreSQL's jsonb and geometric types spell operators with a
			// question mark: `?`, `?|`, `?&`, and `??` as the escaped form.
			// Only a lone `?` is a placeholder.
			if run := questionRun(query, i); run > 1 || isOperatorQuestion(query, i) {
				i += run
				continue
			}
			if n == 0 {
				placeholderAt = i
				b.Grow(len(query) + 8)
			}
			n++
			b.WriteString(query[copied:i])
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			i++
			copied = i
		default:
			i++
		}
	}

	if n == 0 {
		return query, nil
	}
	if numberedAt >= 0 {
		return "", &MixedPlaceholderError{Query: query, NumberedAt: numberedAt, PlaceholderAt: placeholderAt}
	}
	b.WriteString(query[copied:])
	return b.String(), nil
}

// The application's statements are a fixed set of strings, and the assembled
// ones repeat too — an `IN (…)` list of a given length spells the same text
// every time. Scanning each one once per Exec is pure repetition, so results
// are memoised on the statement text.
//
// The cache is capped because the assembled statements are not a closed set: a
// deployment that queries unusual list lengths would otherwise grow it without
// bound. Passing the cap clears the whole map rather than evicting cleverly —
// the entries are cheap to recompute, and the hit rate recovers within a few
// statements.
const reboundCacheMax = 2048

type reboundResult struct {
	sql string
	err error
}

var reboundCache = struct {
	sync.RWMutex
	m map[string]reboundResult
}{m: make(map[string]reboundResult)}

func loadRebound(query string) (reboundResult, bool) {
	reboundCache.RLock()
	defer reboundCache.RUnlock()
	r, ok := reboundCache.m[query]
	return r, ok
}

func storeRebound(query, sql string, err error) {
	reboundCache.Lock()
	defer reboundCache.Unlock()
	if len(reboundCache.m) >= reboundCacheMax {
		reboundCache.m = make(map[string]reboundResult, reboundCacheMax)
	}
	reboundCache.m[query] = reboundResult{sql: sql, err: err}
}

// skipQuoted returns the index just past a single-quoted string or a
// double-quoted identifier starting at i.
//
// Both are closed by their own quote character, and both escape that character
// by doubling it. Backslash escapes are deliberately *not* honoured:
// standard_conforming_strings has been on by default since PostgreSQL 9.1, so
// a backslash in an ordinary literal is an ordinary character, and treating it
// as an escape would run the scan past the closing quote.
func skipQuoted(query string, i int, quote byte) int {
	for j := i + 1; j < len(query); j++ {
		if query[j] != quote {
			continue
		}
		if j+1 < len(query) && query[j+1] == quote {
			j++
			continue
		}
		return j + 1
	}
	return len(query)
}

// skipLineComment returns the index just past a `-- …` comment, and whether one
// starts at i.
func skipLineComment(query string, i int) (int, bool) {
	if i+1 >= len(query) || query[i+1] != '-' {
		return i, false
	}
	if end := strings.IndexByte(query[i:], '\n'); end >= 0 {
		return i + end + 1, true
	}
	return len(query), true
}

// skipBlockComment returns the index just past a `/* … */` comment, and whether
// one starts at i. PostgreSQL nests these, so the scan counts depth rather than
// stopping at the first close.
func skipBlockComment(query string, i int) (int, bool) {
	if i+1 >= len(query) || query[i+1] != '*' {
		return i, false
	}
	depth := 0
	for j := i; j < len(query)-1; j++ {
		switch {
		case query[j] == '/' && query[j+1] == '*':
			depth++
			j++
		case query[j] == '*' && query[j+1] == '/':
			depth--
			j++
			if depth == 0 {
				return j + 1, true
			}
		}
	}
	return len(query), true
}

// skipDollarQuoted returns the index just past a `$tag$ … $tag$` body, and
// whether one starts at i.
//
// The tag is an optional identifier between two dollar signs, so `$$`, `$fn$`
// and `$_1$` all open one. A `$` that begins a parameter reference (`$1`) or
// that is not followed by a closing `$` is not a dollar quote, which is what
// keeps this from swallowing already-numbered placeholders.
func skipDollarQuoted(query string, i int) (int, bool) {
	j := i + 1
	for j < len(query) && (isIdentByte(query[j]) && !isDigit(query[j]) || j > i+1 && isDigit(query[j])) {
		j++
	}
	if j >= len(query) || query[j] != '$' {
		return i, false
	}
	tag := query[i : j+1]
	if end := strings.Index(query[j+1:], tag); end >= 0 {
		return j + 1 + end + len(tag), true
	}
	// An unterminated dollar quote is a broken statement; consuming the rest
	// keeps this function from rewriting anything inside it.
	return len(query), true
}

// questionRun counts consecutive question marks starting at i.
func questionRun(query string, i int) int {
	j := i
	for j < len(query) && query[j] == '?' {
		j++
	}
	return j - i
}

// isOperatorQuestion reports whether the `?` at i is the head of a two-character
// operator such as jsonb's `?|` and `?&`.
func isOperatorQuestion(query string, i int) bool {
	if i+1 >= len(query) {
		return false
	}
	switch query[i+1] {
	case '|', '&', '-', '#':
		return true
	}
	return false
}

func isIdentByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || isDigit(c)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

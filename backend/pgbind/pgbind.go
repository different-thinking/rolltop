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
// Already-numbered statements pass through untouched (see Rebind's fast path),
// so a file may be converted to `$n` at any time without a flag day, and this
// layer can be removed once none are left.
//
// The scanner is deliberately literal-aware. A `?` inside a string literal, a
// quoted identifier, a comment, or a dollar-quoted body is data, not a
// placeholder, and rewriting one would corrupt the statement — the trigger
// function in the baseline is a dollar-quoted body, and mail queries compare
// against strings that really do contain question marks.

package pgbind

import (
	"strconv"
	"strings"
)

// Rebind rewrites `?` placeholders as `$1..$n`.
//
// A statement that contains no `?` outside of quoted text is returned
// unchanged, without allocating, which is both the fast path for converted SQL
// and what makes mixing the two styles safe.
func Rebind(query string) string {
	if !needsRebind(query) {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	for i := 0; i < len(query); {
		switch c := query[i]; c {
		case '\'', '"':
			end := skipQuoted(query, i, c)
			b.WriteString(query[i:end])
			i = end
		case '-':
			if end, ok := skipLineComment(query, i); ok {
				b.WriteString(query[i:end])
				i = end
				continue
			}
			b.WriteByte(c)
			i++
		case '/':
			if end, ok := skipBlockComment(query, i); ok {
				b.WriteString(query[i:end])
				i = end
				continue
			}
			b.WriteByte(c)
			i++
		case '$':
			if end, ok := skipDollarQuoted(query, i); ok {
				b.WriteString(query[i:end])
				i = end
				continue
			}
			b.WriteByte(c)
			i++
		case '?':
			// PostgreSQL's jsonb and geometric types spell operators with a
			// question mark: `?`, `?|`, `?&`, and `??` as the escaped form.
			// Only a lone `?` is a placeholder.
			if run := questionRun(query, i); run > 1 || isOperatorQuestion(query, i) {
				b.WriteString(query[i : i+run])
				i += run
				continue
			}
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// needsRebind reports whether the statement carries a placeholder `?` outside
// quoted text. It is the same scan as Rebind without the writing, so the common
// case of an already-numbered or parameterless statement costs one pass and no
// allocation.
func needsRebind(query string) bool {
	if !strings.ContainsRune(query, '?') {
		return false
	}
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
			i++
		case '?':
			if run := questionRun(query, i); run > 1 || isOperatorQuestion(query, i) {
				i += run
				continue
			}
			return true
		default:
			i++
		}
	}
	return false
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

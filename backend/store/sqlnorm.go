// File overview: The one reading of SQL that decides whether two migration
// texts say the same thing. Every schema checksum — the baseline, the core
// migrations, the plugin migrations — hashes its statements through
// normalizeSQL rather than hashing them raw.
//
// The reason is a start-up refusal. Checksums used to be taken over the source
// text with only its outer whitespace trimmed, so a migration's indentation was
// part of its identity: gofmt re-wrapping a slice literal, or a hand edit that
// reflowed a CREATE TABLE, read back as "this migration was edited after it
// ran" and refused to start every install that had already applied it. Layout
// is not part of what a statement does, so it is not part of what identifies
// one.
//
// What is left byte-exact is anything whose bytes the server does read:
// string literals, quoted identifiers and dollar-quoted bodies are copied
// through untouched, so changing a DEFAULT from 'to  do' to 'to do' still
// changes the checksum. Comments are dropped outright — the server ignores
// them, so a database whose migration differs from the binary's only in a
// comment differs in nothing.

package store

import "strings"

// normalizeSQL reduces a statement to what it asks the server to do: its words
// and quoted regions, separated by single spaces, with comments removed.
//
// It is a lexer, not a parser. It knows exactly enough to tell code from data —
// where a quoted region starts and ends — and treats everything else as words.
// That is the whole safety argument: whitespace is only collapsed where the
// server would have ignored it anyway.
//
// It follows standard_conforming_strings (PostgreSQL's default): a backslash in
// a single-quoted string is an ordinary character, and a doubled quote is the only escape.
func normalizeSQL(sql string) string {
	var b strings.Builder
	b.Grow(len(sql))
	// separated tracks whether whitespace or a comment has been seen since the
	// last emitted text, so runs of either collapse to one space and trailing
	// ones vanish.
	separated := false
	emit := func(text string) {
		if separated && b.Len() > 0 {
			b.WriteByte(' ')
		}
		separated = false
		b.WriteString(text)
	}
	for i := 0; i < len(sql); {
		c := sql[i]
		switch {
		case isSQLSpace(c):
			separated = true
			i++
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			i = endOfLineComment(sql, i)
			separated = true
		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			i = endOfBlockComment(sql, i)
			separated = true
		case c == '\'' || c == '"':
			end := endOfQuoted(sql, i)
			emit(sql[i:end])
			i = end
		default:
			end, ok := endOfDollarQuoted(sql, i)
			if !ok {
				end = endOfWord(sql, i)
			}
			emit(sql[i:end])
			i = end
		}
	}
	return b.String()
}

func isSQLSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

// endOfWord returns the index after a run of ordinary characters — anything
// that is not whitespace and does not open a quoted region or a comment. Words
// are copied through verbatim, so punctuation stays attached exactly as it was
// written: "a, b" and "a , b" are different texts and keep different checksums.
func endOfWord(sql string, i int) int {
	for i < len(sql) {
		c := sql[i]
		if isSQLSpace(c) || c == '\'' || c == '"' {
			return i
		}
		if c == '-' && i+1 < len(sql) && sql[i+1] == '-' {
			return i
		}
		if c == '/' && i+1 < len(sql) && sql[i+1] == '*' {
			return i
		}
		if _, ok := endOfDollarQuoted(sql, i); ok {
			return i
		}
		i++
	}
	return i
}

func endOfLineComment(sql string, i int) int {
	if idx := strings.IndexByte(sql[i:], '\n'); idx >= 0 {
		return i + idx
	}
	return len(sql)
}

// endOfBlockComment handles PostgreSQL's nested /* */ comments. An unterminated
// one swallows the rest of the text, which is what the server does too.
func endOfBlockComment(sql string, i int) int {
	depth := 0
	for i < len(sql) {
		switch {
		case i+1 < len(sql) && sql[i] == '/' && sql[i+1] == '*':
			depth++
			i += 2
		case i+1 < len(sql) && sql[i] == '*' && sql[i+1] == '/':
			depth--
			i += 2
			if depth == 0 {
				return i
			}
		default:
			i++
		}
	}
	return len(sql)
}

// endOfQuoted returns the index after a string literal or quoted identifier
// opened at i, treating a doubled quote as an escaped one. An unterminated
// quote runs to the end of the text.
func endOfQuoted(sql string, i int) int {
	quote := sql[i]
	for j := i + 1; j < len(sql); j++ {
		if sql[j] != quote {
			continue
		}
		if j+1 < len(sql) && sql[j+1] == quote {
			j++
			continue
		}
		return j + 1
	}
	return len(sql)
}

// endOfDollarQuoted recognises a $tag$...$tag$ body starting at i and returns
// the index after its closing delimiter. The baseline's plpgsql function bodies
// arrive this way, and their layout is the function's own source, so they are
// copied through untouched.
//
// The tag rules are what keep this from firing on a parameter placeholder: a
// tag is empty or an identifier, so $1 is never an opening delimiter.
func endOfDollarQuoted(sql string, i int) (int, bool) {
	if i >= len(sql) || sql[i] != '$' {
		return 0, false
	}
	j := i + 1
	for j < len(sql) && isDollarTagByte(sql[j], j == i+1) {
		j++
	}
	if j >= len(sql) || sql[j] != '$' {
		return 0, false
	}
	tag := sql[i : j+1]
	rest := sql[j+1:]
	idx := strings.Index(rest, tag)
	if idx < 0 {
		return len(sql), true
	}
	return j + 1 + idx + len(tag), true
}

func isDollarTagByte(c byte, first bool) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		return true
	case c >= '0' && c <= '9':
		return !first
	default:
		return false
	}
}

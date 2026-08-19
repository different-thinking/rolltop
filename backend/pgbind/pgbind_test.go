package pgbind

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

func mustRebind(t *testing.T, query string) string {
	t.Helper()
	got, err := Rebind(query)
	if err != nil {
		t.Fatalf("Rebind(%s): %v", query, err)
	}
	return got
}

func TestRebindNumbersInOrder(t *testing.T) {
	got := mustRebind(t, `SELECT id FROM messages WHERE user_id = ? AND mailbox_id = ? ORDER BY id LIMIT ?`)
	want := `SELECT id FROM messages WHERE user_id = $1 AND mailbox_id = $2 ORDER BY id LIMIT $3`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// TestRebindLeavesNumberedStatementsAlone is what lets converted and
// unconverted SQL coexist: a file rewritten to $n must pass through unchanged.
func TestRebindLeavesNumberedStatementsAlone(t *testing.T) {
	query := `SELECT id FROM messages WHERE user_id = $1 AND id = ANY($2)`
	if got := mustRebind(t, query); got != query {
		t.Errorf("a numbered statement was rewritten: %s", got)
	}
}

func TestRebindKeepsQuestionMarksInsideLiterals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		want  string
	}{
		{"single quotes", `SELECT ? WHERE subject = 'why?'`, `SELECT $1 WHERE subject = 'why?'`},
		{"doubled quote escape", `SELECT ? WHERE s = 'it''s ok?' AND t = ?`, `SELECT $1 WHERE s = 'it''s ok?' AND t = $2`},
		{"quoted identifier", `SELECT "weird?col" FROM t WHERE id = ?`, `SELECT "weird?col" FROM t WHERE id = $1`},
		{"line comment", "SELECT ? -- is this ?\nAND x = ?", "SELECT $1 -- is this ?\nAND x = $2"},
		{"block comment", `SELECT ? /* what? */ AND x = ?`, `SELECT $1 /* what? */ AND x = $2`},
		{"nested block comment", `SELECT ? /* a /* ? */ b */ AND x = ?`, `SELECT $1 /* a /* ? */ b */ AND x = $2`},
		{"dollar quoted body", `DO $$ BEGIN RAISE NOTICE 'huh?'; END $$; SELECT ?`, `DO $$ BEGIN RAISE NOTICE 'huh?'; END $$; SELECT $1`},
		{"tagged dollar quote", `CREATE FUNCTION f() RETURNS void AS $fn$ SELECT '?' $fn$ LANGUAGE sql`, `CREATE FUNCTION f() RETURNS void AS $fn$ SELECT '?' $fn$ LANGUAGE sql`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mustRebind(t, tc.query); got != tc.want {
				t.Errorf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

// TestRebindKeepsJSONBOperators pins the one family of PostgreSQL operators
// spelled with a question mark. Rewriting `?|` into `$1|` would turn a working
// containment test into a syntax error.
func TestRebindKeepsJSONBOperators(t *testing.T) {
	for _, tc := range []struct{ query, want string }{
		{`SELECT * FROM t WHERE data ?| ARRAY['a'] AND id = ?`, `SELECT * FROM t WHERE data ?| ARRAY['a'] AND id = $1`},
		{`SELECT * FROM t WHERE data ?& ARRAY['a'] AND id = ?`, `SELECT * FROM t WHERE data ?& ARRAY['a'] AND id = $1`},
		{`SELECT * FROM t WHERE data ?? 'a' AND id = ?`, `SELECT * FROM t WHERE data ?? 'a' AND id = $1`},
	} {
		if got := mustRebind(t, tc.query); got != tc.want {
			t.Errorf("got  %s\nwant %s", got, tc.want)
		}
	}
}

// TestRebindNumbersAcrossAssembledFragments is the case this layer exists for:
// the statement is concatenated at run time and no fragment knows how many
// parameters the fragments before it contributed.
func TestRebindNumbersAcrossAssembledFragments(t *testing.T) {
	prefix := `SELECT id FROM messages WHERE user_id = ?`
	filter := ` AND mailbox_id = ? AND is_read = ?`
	suffix := ` ORDER BY id LIMIT ?`
	got := mustRebind(t, prefix+filter+suffix)
	want := `SELECT id FROM messages WHERE user_id = $1 AND mailbox_id = $2 AND is_read = $3 ORDER BY id LIMIT $4`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestRebindHandlesManyPlaceholders(t *testing.T) {
	query := "INSERT INTO t VALUES (?"
	want := "INSERT INTO t VALUES ($1"
	for i := 2; i <= 12; i++ {
		query += ",?"
		want += ",$" + strconv.Itoa(i)
	}
	query += ")"
	want += ")"
	if got := mustRebind(t, query); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestRebindLeavesParameterlessStatementsAlone(t *testing.T) {
	query := `SELECT count(*) FROM messages`
	if got := mustRebind(t, query); got != query {
		t.Errorf("a parameterless statement was rewritten: %s", got)
	}
}

// TestRebindRefusesMixedPlaceholderStyles is the guard on the one way this
// layer could bind arguments to the wrong slots. Numbering the `?` from $1 in a
// statement that already says `$1` reads as a working query and returns another
// tenant's rows; the mistake has to surface as an error instead.
func TestRebindRefusesMixedPlaceholderStyles(t *testing.T) {
	for _, query := range []string{
		`SELECT $1, ? FROM t`,
		`SELECT ? FROM t WHERE user_id = $1`,
		`SELECT * FROM t WHERE id = ANY($1) AND user_id = ?`,
	} {
		got, err := Rebind(query)
		var mixed *MixedPlaceholderError
		if !errors.As(err, &mixed) {
			t.Errorf("Rebind(%s) = %q, %v; want a *MixedPlaceholderError", query, got, err)
			continue
		}
		if got != "" {
			t.Errorf("Rebind(%s) returned SQL alongside the error: %q", query, got)
		}
		if !strings.Contains(mixed.Error(), query) {
			t.Errorf("the error does not name the statement: %s", mixed.Error())
		}
	}
}

// TestRebindDoesNotConfuseDollarQuoteWithParameter guards the boundary between
// `$1` (a parameter) and `$tag$` (a quote opener). Treating `$1` as a quote
// opener would swallow the rest of the statement — and would also hide the
// mixed-style refusal above, because the `?` after it would look quoted.
func TestRebindDoesNotConfuseDollarQuoteWithParameter(t *testing.T) {
	query := `SELECT $1, $2 FROM t WHERE x = $3`
	if got := mustRebind(t, query); got != query {
		t.Errorf("got  %s\nwant %s", got, query)
	}
	if _, err := Rebind(`SELECT $1, ? FROM t`); err == nil {
		t.Error("a `$1` was mistaken for a dollar-quote opener: the trailing ? was not seen")
	}
}

// TestRebindIgnoresDollarSignsInsideQuotedText keeps the mixed-style detection
// from firing on text: `$1` inside a literal or a trigger body is data, and a
// statement that only looks numbered must still be rebound.
func TestRebindIgnoresDollarSignsInsideQuotedText(t *testing.T) {
	for _, tc := range []struct{ query, want string }{
		{`SELECT ? WHERE body = 'costs $1 total'`, `SELECT $1 WHERE body = 'costs $1 total'`},
		{"SELECT ? -- $1 in a comment\n", "SELECT $1 -- $1 in a comment\n"},
		{`DO $fn$ SELECT $1 $fn$; SELECT ?`, `DO $fn$ SELECT $1 $fn$; SELECT $1`},
	} {
		if got := mustRebind(t, tc.query); got != tc.want {
			t.Errorf("got  %s\nwant %s", got, tc.want)
		}
	}
}

// TestRebindMemoisesResults pins that repeating a statement — the normal case,
// since the application's SQL is a fixed set of strings — does not re-scan it,
// and that a cached refusal is still a refusal.
func TestRebindMemoisesResults(t *testing.T) {
	const query = `SELECT id FROM memo_probe WHERE user_id = ? AND id = ?`
	first := mustRebind(t, query)
	if _, ok := loadRebound(query); !ok {
		t.Fatal("the result was not cached")
	}
	if second := mustRebind(t, query); second != first {
		t.Errorf("the cached rewrite differs: %s vs %s", second, first)
	}

	const bad = `SELECT id FROM memo_probe WHERE user_id = $1 AND id = ?`
	if _, err := Rebind(bad); err == nil {
		t.Fatal("the mixed statement was accepted")
	}
	if _, err := Rebind(bad); err == nil {
		t.Error("the cached result dropped the error")
	}
}

// TestRebindCacheStaysBounded pins the eviction: assembled `IN (…)` lists are
// not a closed set of strings, so the cache must not grow with them forever.
func TestRebindCacheStaysBounded(t *testing.T) {
	for i := 0; i < reboundCacheMax+64; i++ {
		mustRebind(t, `SELECT `+strconv.Itoa(i)+` FROM t WHERE id = ?`)
	}
	reboundCache.RLock()
	size := len(reboundCache.m)
	reboundCache.RUnlock()
	if size > reboundCacheMax {
		t.Errorf("the cache holds %d entries, above the %d cap", size, reboundCacheMax)
	}
}

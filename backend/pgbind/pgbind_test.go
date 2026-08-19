package pgbind

import "testing"

func TestRebindNumbersInOrder(t *testing.T) {
	got := Rebind(`SELECT id FROM messages WHERE user_id = ? AND mailbox_id = ? ORDER BY id LIMIT ?`)
	want := `SELECT id FROM messages WHERE user_id = $1 AND mailbox_id = $2 ORDER BY id LIMIT $3`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// TestRebindLeavesNumberedStatementsAlone is what lets converted and
// unconverted SQL coexist: a file rewritten to $n must pass through unchanged.
func TestRebindLeavesNumberedStatementsAlone(t *testing.T) {
	query := `SELECT id FROM messages WHERE user_id = $1 AND id = ANY($2)`
	if got := Rebind(query); got != query {
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
			if got := Rebind(tc.query); got != tc.want {
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
		if got := Rebind(tc.query); got != tc.want {
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
	got := Rebind(prefix + filter + suffix)
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
		want += ",$" + itoa(i)
	}
	query += ")"
	want += ")"
	if got := Rebind(query); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestRebindLeavesParameterlessStatementsAlone(t *testing.T) {
	query := `SELECT count(*) FROM messages`
	if got := Rebind(query); got != query {
		t.Errorf("a parameterless statement was rewritten: %s", got)
	}
}

// TestRebindDoesNotConfuseDollarQuoteWithParameter guards the boundary between
// `$1` (a parameter) and `$tag$` (a quote opener). Treating `$1` as a quote
// opener would swallow the rest of the statement.
func TestRebindDoesNotConfuseDollarQuoteWithParameter(t *testing.T) {
	query := `SELECT $1, ? FROM t`
	want := `SELECT $1, $1 FROM t`
	// Numbering restarts at 1 because this statement mixes the two styles,
	// which is exactly why mixing them inside one statement is not allowed.
	// The test pins the behaviour rather than endorsing the input.
	if got := Rebind(query); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

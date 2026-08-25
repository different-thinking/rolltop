package store

import "testing"

// TestNormalizeSQLIgnoresLayout covers the reformats a checksum must survive.
// Each pair is the same statement written two ways; a difference between them
// used to refuse startup on every install that had applied the migration.
func TestNormalizeSQLIgnoresLayout(t *testing.T) {
	same := []struct {
		name string
		a, b string
	}{
		{
			name: "indentation",
			a:    "CREATE TABLE t (\n\tid bigint PRIMARY KEY,\n\tname text\n)",
			b:    "CREATE TABLE t (\n\t\t\tid bigint PRIMARY KEY,\n\t\t\tname text\n\t\t)",
		},
		{
			name: "line breaks",
			a:    "CREATE INDEX i ON t (a, b)",
			b:    "CREATE INDEX i\n  ON t (a, b)",
		},
		{
			name: "trailing whitespace",
			a:    "SELECT 1",
			b:    "  SELECT 1\n\n",
		},
		{
			name: "comment text",
			a:    "CREATE TABLE t (id bigint) -- holds the ids\n",
			b:    "CREATE TABLE t (id bigint) -- holds every id we know\n",
		},
		{
			name: "block comment",
			a:    "CREATE /* one */ TABLE t (id bigint)",
			b:    "CREATE /* two\n   lines */ TABLE t (id bigint)",
		},
		{
			name: "comment on its own line",
			a:    "-- why\nCREATE TABLE t (id bigint)",
			b:    "CREATE TABLE t (id bigint)",
		},
	}
	for _, tc := range same {
		t.Run(tc.name, func(t *testing.T) {
			if normalizeSQL(tc.a) != normalizeSQL(tc.b) {
				t.Fatalf("normalizeSQL differs:\n %q\n %q", normalizeSQL(tc.a), normalizeSQL(tc.b))
			}
		})
	}
}

// TestNormalizeSQLKeepsMeaning is the other half of the contract: everything the
// server actually reads still separates two texts. A normalisation that lost any
// of these would let an edited migration pass as the one that ran.
func TestNormalizeSQLKeepsMeaning(t *testing.T) {
	differ := []struct {
		name string
		a, b string
	}{
		{
			name: "whitespace inside a string literal",
			a:    "ALTER TABLE t ADD COLUMN s text DEFAULT 'to  do'",
			b:    "ALTER TABLE t ADD COLUMN s text DEFAULT 'to do'",
		},
		{
			name: "newline inside a string literal",
			a:    "INSERT INTO t (s) VALUES ('a\nb')",
			b:    "INSERT INTO t (s) VALUES ('a b')",
		},
		{
			name: "whitespace inside a quoted identifier",
			a:    `CREATE TABLE "my  table" (id bigint)`,
			b:    `CREATE TABLE "my table" (id bigint)`,
		},
		{
			name: "layout inside a dollar-quoted body",
			a:    "CREATE FUNCTION f() RETURNS trigger AS $$\nBEGIN\n  RETURN NEW;\nEND;\n$$ LANGUAGE plpgsql",
			b:    "CREATE FUNCTION f() RETURNS trigger AS $$ BEGIN RETURN NEW; END; $$ LANGUAGE plpgsql",
		},
		{
			name: "a word runs into its neighbour",
			a:    "CREATE TABLE t (id bigint NOT NULL)",
			b:    "CREATE TABLE t (id bigint NOTNULL)",
		},
		{
			name: "a keyword changes",
			a:    "CREATE TABLE t (id bigint PRIMARY KEY)",
			b:    "CREATE TABLE t (id bigint UNIQUE)",
		},
		{
			name: "punctuation moves",
			a:    "CREATE INDEX i ON t (a, b)",
			b:    "CREATE INDEX i ON t (a , b)",
		},
		{
			name: "a comment hides a column",
			a:    "CREATE TABLE t (a int -- note\n, b int)",
			b:    "CREATE TABLE t (a int -- note , b int)",
		},
	}
	for _, tc := range differ {
		t.Run(tc.name, func(t *testing.T) {
			if normalizeSQL(tc.a) == normalizeSQL(tc.b) {
				t.Fatalf("two different statements normalised alike: %q", normalizeSQL(tc.a))
			}
		})
	}
}

// TestNormalizeSQLLexes pins the lexer's reading of the constructs it has to
// tell apart, including the ones that could run away with the rest of a
// statement if it got them wrong.
func TestNormalizeSQLLexes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "doubled quote is an escape, not a terminator",
			in:   "INSERT INTO t (s) VALUES ('it''s   here')  ",
			want: "INSERT INTO t (s) VALUES ('it''s   here')",
		},
		{
			name: "a quote inside a comment does not open a string",
			in:   "SELECT 1 -- it's fine\nFROM t",
			want: "SELECT 1 FROM t",
		},
		{
			name: "a comment marker inside a string is data",
			in:   "SELECT 'a -- b'   FROM t",
			want: "SELECT 'a -- b' FROM t",
		},
		{
			name: "block comments nest",
			in:   "SELECT /* a /* b */ c */ 1",
			want: "SELECT 1",
		},
		{
			name: "a placeholder is not a dollar quote",
			in:   "SELECT   $1,   $2",
			want: "SELECT $1, $2",
		},
		{
			name: "tagged dollar quote",
			in:   "DO $body$  BEGIN  END  $body$",
			want: "DO $body$  BEGIN  END  $body$",
		},
		{
			name: "unterminated string keeps the rest verbatim",
			in:   "SELECT 'oops",
			want: "SELECT 'oops",
		},
		{
			name: "only a comment normalises to nothing",
			in:   "-- nothing to do\n",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeSQL(tc.in); got != tc.want {
				t.Fatalf("normalizeSQL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

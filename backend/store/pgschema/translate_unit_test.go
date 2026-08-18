package pgschema

import (
	"strings"
	"testing"
)

func translateOne(t *testing.T, kind, name, ddl string) []Object {
	t.Helper()
	objects, err := Translate([]SQLiteObject{{Kind: kind, Name: name, SQL: ddl}})
	if err != nil {
		t.Fatal(err)
	}
	return objects
}

func TestTranslateColumnTypes(t *testing.T) {
	objects := translateOne(t, "table", "t", `CREATE TABLE t (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		count INTEGER NOT NULL DEFAULT 0,
		label TEXT NOT NULL DEFAULT '',
		image BLOB,
		boost REAL NOT NULL DEFAULT 0
	)`)
	got := objects[0].SQL
	for _, want := range []string{
		"id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY",
		"count bigint NOT NULL DEFAULT 0",
		`label text COLLATE "C" NOT NULL DEFAULT ''`,
		"image bytea",
		"boost double precision NOT NULL DEFAULT 0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// TestTranslateColumnNamedLikeAConstraint is the regression test for a column
// called "checksum" being mistaken for a CHECK constraint and passed through
// untranslated.
func TestTranslateColumnNamedLikeAConstraint(t *testing.T) {
	objects := translateOne(t, "table", "t", `CREATE TABLE t (
		checksum TEXT NOT NULL,
		uniqueness TEXT NOT NULL,
		constraint_note TEXT NOT NULL,
		primary_contact TEXT NOT NULL
	)`)
	got := objects[0].SQL
	for _, column := range []string{"checksum", "uniqueness", "constraint_note", "primary_contact"} {
		if !strings.Contains(got, column+` text COLLATE "C" NOT NULL`) {
			t.Errorf("column %s was not translated:\n%s", column, got)
		}
	}
}

func TestTranslateKeepsTableConstraints(t *testing.T) {
	objects := translateOne(t, "table", "t", `CREATE TABLE t (
		a INTEGER NOT NULL,
		b INTEGER NOT NULL,
		PRIMARY KEY(a, b),
		UNIQUE(b)
	)`)
	got := objects[0].SQL
	if !strings.Contains(got, "PRIMARY KEY(a, b)") || !strings.Contains(got, "UNIQUE(b)") {
		t.Errorf("table constraints lost:\n%s", got)
	}
}

// TestTranslateSplitsForeignKeys pins the phase split: no REFERENCES may
// remain in a CREATE TABLE, because the unique indexes composite keys need do
// not exist yet at that point.
func TestTranslateSplitsForeignKeys(t *testing.T) {
	objects, err := Translate([]SQLiteObject{{Kind: "table", Name: "t", SQL: `CREATE TABLE t (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		account_id INTEGER NOT NULL,
		FOREIGN KEY(user_id, account_id) REFERENCES mail_accounts(user_id, id) ON DELETE CASCADE
	)`}})
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 2 {
		t.Fatalf("got %d objects, want a table plus its foreign keys", len(objects))
	}
	table, keys := objects[0], objects[1]
	if strings.Contains(table.SQL, "REFERENCES") {
		t.Errorf("table still carries a foreign key:\n%s", table.SQL)
	}
	if !strings.Contains(table.SQL, "user_id bigint NOT NULL") {
		t.Errorf("column lost its type when the reference was split off:\n%s", table.SQL)
	}
	if keys.Kind != "foreign keys" {
		t.Fatalf("second object is %q", keys.Kind)
	}
	for _, want := range []string{
		"ALTER TABLE t ADD FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE;",
		"ALTER TABLE t ADD FOREIGN KEY(user_id, account_id) REFERENCES mail_accounts(user_id, id) ON DELETE CASCADE;",
	} {
		if !strings.Contains(keys.SQL, want) {
			t.Errorf("missing %q in:\n%s", want, keys.SQL)
		}
	}
}

// TestTranslateForeignKeysComeAfterIndexes is the ordering the composite keys
// depend on.
func TestTranslateForeignKeysComeAfterIndexes(t *testing.T) {
	objects, err := Translate([]SQLiteObject{
		{Kind: "table", Name: "t", SQL: `CREATE TABLE t (a INTEGER NOT NULL REFERENCES u(id))`},
		{Kind: "index", Name: "idx_u", SQL: `CREATE UNIQUE INDEX idx_u ON u(id)`},
	})
	if err != nil {
		t.Fatal(err)
	}
	var indexAt, keysAt = -1, -1
	for i, object := range objects {
		switch object.Kind {
		case "index":
			indexAt = i
		case "foreign keys":
			keysAt = i
		}
	}
	if indexAt < 0 || keysAt < 0 || indexAt > keysAt {
		t.Fatalf("index at %d, foreign keys at %d; keys must come last", indexAt, keysAt)
	}
}

func TestTranslateIndexes(t *testing.T) {
	cases := []struct {
		name string
		ddl  string
		want string
	}{
		{"plain", `CREATE INDEX i ON t(a, b)`, "CREATE INDEX i ON t (a, b);"},
		{"unique", `CREATE UNIQUE INDEX i ON t(a)`, "CREATE UNIQUE INDEX i ON t (a);"},
		{"partial", `CREATE UNIQUE INDEX i ON t(a, b) WHERE b <> ''`, "CREATE UNIQUE INDEX i ON t (a, b) WHERE b <> '';"},
		{"nocase", `CREATE INDEX i ON t(user_id, name COLLATE NOCASE)`, "CREATE INDEX i ON t (user_id, lower(name));"},
		{"binary", `CREATE INDEX i ON t(sha COLLATE BINARY)`, "CREATE INDEX i ON t (sha);"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			objects := translateOne(t, "index", "i", test.ddl)
			if objects[0].SQL != test.want {
				t.Errorf("got  %s\nwant %s", objects[0].SQL, test.want)
			}
		})
	}
}

// TestTranslateStripsLineComments covers the inline documentation the store's
// migrations carry: folding a comment into the following line would swallow
// the column after it.
func TestTranslateStripsLineComments(t *testing.T) {
	objects := translateOne(t, "table", "t", `CREATE TABLE t (
		-- The address is display data and can change.
		email TEXT NOT NULL,
		-- Another note, with a comma, that must not split anything.
		name TEXT NOT NULL
	)`)
	got := objects[0].SQL
	if strings.Contains(got, "--") {
		t.Errorf("comment survived translation:\n%s", got)
	}
	for _, column := range []string{"email", "name"} {
		if !strings.Contains(got, column+` text COLLATE "C" NOT NULL`) {
			t.Errorf("column %s missing:\n%s", column, got)
		}
	}
}

func TestTranslateRejectsUnknownTypeAndTrigger(t *testing.T) {
	if _, err := Translate([]SQLiteObject{{Kind: "table", Name: "t", SQL: `CREATE TABLE t (when_at TIMESTAMP NOT NULL)`}}); err == nil {
		t.Error("an unmapped column type was accepted")
	}
	if _, err := Translate([]SQLiteObject{{Kind: "trigger", Name: "something_new", SQL: `CREATE TRIGGER something_new AFTER INSERT ON t BEGIN SELECT 1; END`}}); err == nil {
		t.Error("an untranslated trigger was accepted")
	}
}

func TestTranslateRejectsUnsupportedKind(t *testing.T) {
	if _, err := Translate([]SQLiteObject{{Kind: "view", Name: "v", SQL: `CREATE VIEW v AS SELECT 1`}}); err == nil {
		t.Error("a view was accepted; views need a deliberate decision")
	}
}

func TestSplitTopLevelIgnoresCommasInParensAndStrings(t *testing.T) {
	parts, err := splitTopLevel(`a INTEGER, b TEXT DEFAULT 'x,y', PRIMARY KEY(a, b)`)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 3 {
		t.Fatalf("got %d parts: %q", len(parts), parts)
	}
}

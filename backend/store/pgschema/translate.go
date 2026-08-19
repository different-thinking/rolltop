// File overview: SQLite-to-PostgreSQL DDL translation for the migration
// baseline (docs/postgres-migration-plan.md, WP2). The baseline is not
// hand-written: it is derived from the fully migrated combined SQLite schema
// so it cannot drift from what the store actually creates, and
// TestBaselineMatchesSQLiteSchema in backend/store regenerates it to prove
// that. Everything here is pure text-in, text-out; nothing opens a database.
//
// The governing rule is **fail closed**. Anything this translator does not
// positively recognize — an unmapped column type, a SQLite-only expression, a
// foreign-key spelling the extractor cannot parse, a trigger whose body
// changed — is an error, not a pass-through. A generated schema that silently
// differs from its source is worse than a build failure, because every later
// phase of the migration trusts this file.
//
// The type mapping is deliberately conservative. Unix timestamps and 0/1
// booleans stay integers rather than becoming timestamptz/boolean, because
// converting them would mean touching every Scan in the store at the same
// time as changing the dialect. Those are separate cleanups, listed as
// non-goals in the plan.

package pgschema

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Kinds an Object can carry. ForeignKeysKind is synthetic: it has no
// sqlite_master counterpart because the references are extracted out of the
// tables that declare them.
const (
	TableKind       = "table"
	IndexKind       = "index"
	ForeignKeysKind = "foreign keys"
	TriggerKind     = "trigger"
)

// Object is one translated schema object in the order it must be applied.
type Object struct {
	Kind string
	Name string
	SQL  string
}

// SQLiteObject is one row of sqlite_master: the source material.
type SQLiteObject struct {
	Kind string
	Name string
	SQL  string
}

// Translate converts a fully migrated SQLite schema into PostgreSQL DDL, in
// four phases so the result applies top to bottom with no ordering
// assumptions: tables (without foreign keys), indexes, foreign keys, then
// triggers. Every phase is name-sorted so the generated file does not depend
// on SQLite's physical page layout, which shifts whenever a migration
// rebuilds a table.
//
// Splitting the foreign keys out is not cosmetic. PostgreSQL requires a
// referenced column list to be backed by a unique constraint or index, and
// this schema has composite keys — mailboxes(user_id, account_id, id) — whose
// uniqueness comes from a CREATE UNIQUE INDEX rather than from the table
// definition. Inline foreign keys would therefore be rejected for referencing
// an index that does not exist yet. SQLite never complains because it defers
// the check.
func Translate(objects []SQLiteObject) ([]Object, error) {
	var tables, indexes, foreignKeys, triggers []Object
	for _, object := range objects {
		switch object.Kind {
		case TableKind:
			sql, keys, err := translateTable(object.SQL)
			if err != nil {
				return nil, fmt.Errorf("table %s: %w", object.Name, err)
			}
			tables = append(tables, Object{Kind: TableKind, Name: object.Name, SQL: sql})
			if len(keys) > 0 {
				foreignKeys = append(foreignKeys, Object{
					Kind: ForeignKeysKind, Name: object.Name, SQL: strings.Join(keys, "\n"),
				})
			}
		case IndexKind:
			sql, err := translateIndex(object.SQL)
			if err != nil {
				return nil, fmt.Errorf("index %s: %w", object.Name, err)
			}
			indexes = append(indexes, Object{Kind: IndexKind, Name: object.Name, SQL: sql})
		case TriggerKind:
			sql, err := translateTrigger(object.Name, object.SQL)
			if err != nil {
				return nil, fmt.Errorf("trigger %s: %w", object.Name, err)
			}
			triggers = append(triggers, Object{Kind: TriggerKind, Name: object.Name, SQL: sql})
		default:
			return nil, fmt.Errorf("unsupported object kind %q for %s", object.Kind, object.Name)
		}
	}
	byName := func(list []Object) {
		sort.SliceStable(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	}
	byName(tables)
	byName(indexes)
	byName(foreignKeys)
	byName(triggers)
	out := make([]Object, 0, len(tables)+len(indexes)+len(foreignKeys)+len(triggers))
	out = append(out, tables...)
	out = append(out, indexes...)
	out = append(out, foreignKeys...)
	out = append(out, triggers...)
	return out, nil
}

var (
	createTableRE = regexp.MustCompile(`(?is)^\s*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z0-9_"]+)\s*(\(.*)$`)
	createIndexRE = regexp.MustCompile(`(?is)^\s*CREATE\s+(UNIQUE\s+)?INDEX\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z0-9_"]+)\s+ON\s+([A-Za-z0-9_"]+)\s*(\(.*)$`)
	collateBinary = regexp.MustCompile(`(?i)\s+COLLATE\s+BINARY\b`)
	collateNoCase = regexp.MustCompile(`(?i)([A-Za-z0-9_]+)\s+COLLATE\s+NOCASE\b`)
	// bareNoCaseRE matches COLLATE NOCASE with nothing captured, for contexts
	// where the rewrite (lower(...)) makes no sense: a column definition or a
	// table constraint. Only translateIndex may rewrite NOCASE; everywhere
	// else it is an error, because PostgreSQL has no NOCASE collation and
	// silently leaving the keyword in place produces invalid DDL rather than
	// a build failure.
	bareNoCaseRE  = regexp.MustCompile(`(?i)\bCOLLATE\s+NOCASE\b`)
	autoincrement = regexp.MustCompile(`(?i)^INTEGER\s+PRIMARY\s+KEY\s+AUTOINCREMENT$`)
	// A column-level foreign key with its optional referential actions. The
	// column list is optional because SQLite allows "REFERENCES t" against the
	// implicit primary key; anyReferencesRE is the fail-closed backstop for
	// spellings this one still misses.
	columnReferenceRE = regexp.MustCompile(`(?i)REFERENCES\s+[A-Za-z0-9_"]+\s*(\([^)]*\))?(\s+ON\s+(DELETE|UPDATE)\s+(CASCADE|RESTRICT|SET\s+NULL|SET\s+DEFAULT|NO\s+ACTION))*`)
	anyReferencesRE   = regexp.MustCompile(`(?i)\bREFERENCES\b`)
	// A table-level foreign key, named or not. The optional CONSTRAINT prefix
	// has to be recognized here, or a named foreign key matches
	// tableConstraintRE first and stays inline in the CREATE TABLE.
	tableForeignKeyRE = regexp.MustCompile(`(?i)^(CONSTRAINT\s+[A-Za-z0-9_"]+\s+)?FOREIGN\s+KEY\b`)
	// tableConstraintRE matches the start of a table-level constraint rather
	// than a column. The word boundary is load-bearing: a bare prefix test
	// treats a column named "checksum" as a CHECK constraint and passes it
	// through untranslated, which is exactly what an earlier revision did.
	tableConstraintRE = regexp.MustCompile(`(?i)^(primary\s+key|unique|check|foreign\s+key|constraint)\b`)
	// withoutRowidRE matches SQLite's storage-form suffix. PostgreSQL has no
	// equivalent — its tables are always a heap plus indexes — so dropping it
	// changes storage layout, not semantics.
	withoutRowidRE = regexp.MustCompile(`(?i)\s*WITHOUT\s+ROWID\s*;?\s*$`)
)

// sqliteOnlyRE names constructs PostgreSQL does not have. Hitting one without
// an entry in expressionTranslations is an error: a silently dropped or
// mistranslated CHECK is a constraint the database stops enforcing.
var sqliteOnlyRE = regexp.MustCompile(`(?i)\b(GLOB|REGEXP|strftime|julianday|unixepoch|sqlite_[a-z_]+)\b`)

// expressionTranslations are the SQLite-only expressions this schema uses,
// each translated deliberately. Matching is on the collapsed source text, so
// editing the SQLite side fails the build instead of silently keeping the old
// translation.
var expressionTranslations = []struct {
	sqlite   string
	postgres string
}{
	{
		// "contains no character outside 0-9a-f", i.e. lowercase hex. The
		// companion length(...) = 64 check pins the digest width.
		sqlite:   `destination_sha256 NOT GLOB '*[^0-9a-f]*'`,
		postgres: `destination_sha256 ~ '^[0-9a-f]*$'`,
	},
}

// translatePortableExpression rewrites known SQLite-only expressions and
// reports an error for any that remain.
func translatePortableExpression(part string) (string, error) {
	for _, translation := range expressionTranslations {
		part = strings.ReplaceAll(part, translation.sqlite, translation.postgres)
	}
	if match := sqliteOnlyRE.FindString(part); match != "" {
		return "", fmt.Errorf("SQLite-only construct %q needs a deliberate entry in expressionTranslations: %s", match, part)
	}
	return part, nil
}

func translateTable(ddl string) (string, []string, error) {
	match := createTableRE.FindStringSubmatch(ddl)
	if match == nil {
		return "", nil, fmt.Errorf("unrecognized CREATE TABLE: %s", collapse(ddl))
	}
	table := unquote(match[1])
	body, tail, err := splitParenthesized(collapse(match[2]))
	if err != nil {
		return "", nil, err
	}
	// WITHOUT ROWID is the only trailing clause this schema uses; anything
	// else has to be looked at rather than dropped.
	if rest := strings.TrimSpace(withoutRowidRE.ReplaceAllString(tail, "")); strings.TrimSuffix(rest, ";") != "" {
		return "", nil, fmt.Errorf("unsupported clause after the column list: %q", rest)
	}
	parts, err := splitTopLevel(body)
	if err != nil {
		return "", nil, err
	}
	rendered := make([]string, 0, len(parts))
	var foreignKeys []string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if part, err = translatePortableExpression(part); err != nil {
			return "", nil, err
		}
		if tableForeignKeyRE.MatchString(part) {
			foreignKeys = append(foreignKeys, fmt.Sprintf("ALTER TABLE %s ADD %s;", table, part))
			continue
		}
		if tableConstraintRE.MatchString(part) {
			constraint := replaceOutsideStrings(part, collateBinary, "")
			if _, _, ok := findOutsideStrings(constraint, bareNoCaseRE); ok {
				return "", nil, fmt.Errorf("COLLATE NOCASE is only supported on an index column, not a table constraint: %q", constraint)
			}
			rendered = append(rendered, "  "+constraint)
			continue
		}
		column, reference, err := translateColumn(part)
		if err != nil {
			return "", nil, err
		}
		if reference != "" {
			columnName := strings.SplitN(column, " ", 2)[0]
			foreignKeys = append(foreignKeys,
				fmt.Sprintf("ALTER TABLE %s ADD FOREIGN KEY (%s) %s;", table, columnName, reference))
		}
		// The backstop for the phase split: if any REFERENCES survived into a
		// rendered column, the extractor did not understand its spelling and
		// the four-phase guarantee no longer holds. Checked outside string
		// literals, so a DEFAULT whose text happens to contain the word does
		// not trip it.
		if _, _, ok := findOutsideStrings(column, anyReferencesRE); ok {
			return "", nil, fmt.Errorf("unrecognized foreign-key syntax left inline in column: %q", column)
		}
		// SQLite's NOCASE has a translation for an index column (lower()) but
		// not for a column definition -- PostgreSQL has no such collation
		// name, so leaving it in place would emit invalid DDL instead of
		// failing the build.
		if _, _, ok := findOutsideStrings(column, bareNoCaseRE); ok {
			return "", nil, fmt.Errorf("COLLATE NOCASE is only supported on an index column, not a column definition: %q", column)
		}
		rendered = append(rendered, "  "+column)
	}
	return fmt.Sprintf("CREATE TABLE %s (\n%s\n);", table, strings.Join(rendered, ",\n")), foreignKeys, nil
}

// translateColumn rewrites one column definition and returns any REFERENCES
// clause separately, so the caller can emit it as an ALTER TABLE after the
// indexes exist. The remainder (NOT NULL, DEFAULT, CHECK) is valid PostgreSQL
// as written and passes through untouched.
func translateColumn(definition string) (string, string, error) {
	definition = replaceOutsideStrings(definition, collateBinary, "")
	reference := ""
	// Located with a string-literal-aware search: a plain regex also matches
	// REFERENCES inside a quoted DEFAULT, which would corrupt the default and
	// invent a foreign key out of its text.
	if start, end, ok := findOutsideStrings(definition, columnReferenceRE); ok {
		reference = strings.TrimSpace(definition[start:end])
		definition = strings.TrimSpace(definition[:start] + definition[end:])
	}
	fields := strings.SplitN(definition, " ", 2)
	if len(fields) < 2 {
		return "", "", fmt.Errorf("column without a type: %q", definition)
	}
	name, rest := fields[0], strings.TrimSpace(fields[1])

	// The surrogate key. GENERATED BY DEFAULT, not GENERATED ALWAYS: SQLite's
	// AUTOINCREMENT accepts an explicit id on insert, which GENERATED ALWAYS
	// rejects with "cannot insert a non-DEFAULT value into column". The
	// permissive form matches the SQLite semantics this schema is derived
	// from and costs nothing in normal operation.
	if autoincrement.MatchString(rest) {
		return name + " bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY", reference, nil
	}
	typeName, tail := splitType(rest)
	mapped, ok := typeMapping[strings.ToUpper(typeName)]
	if !ok {
		return "", "", fmt.Errorf("unmapped column type %q in %q", typeName, definition)
	}
	out := name + " " + mapped
	if tail != "" {
		out += " " + tail
	}
	return out, reference, nil
}

// typeMapping is the whole vocabulary the store's schema uses. An unmapped
// type is an error rather than a pass-through, so a new type in a future
// SQLite migration fails the regeneration test instead of silently producing
// a column PostgreSQL interprets differently.
//
// Every text column is C-collated: SQLite compares and orders text byte-wise,
// and the measured cluster locale sorts a,ä,B,Z where byte order is B,Z,a,ä
// (plan §13), so relying on the database default would change every ORDER BY
// in the store.
var typeMapping = map[string]string{
	"INTEGER": "bigint",
	"TEXT":    `text COLLATE "C"`,
	"BLOB":    "bytea",
	"REAL":    "double precision",
}

// splitType separates the leading type name from the rest of the definition.
func splitType(rest string) (string, string) {
	index := strings.IndexByte(rest, ' ')
	if index < 0 {
		return rest, ""
	}
	return rest[:index], strings.TrimSpace(rest[index+1:])
}

func translateIndex(ddl string) (string, error) {
	match := createIndexRE.FindStringSubmatch(ddl)
	if match == nil {
		return "", fmt.Errorf("unrecognized CREATE INDEX: %s", collapse(ddl))
	}
	unique := strings.TrimSpace(match[1]) != ""
	name := unquote(match[2])
	table := unquote(match[3])
	// The column list is delimited by matching parentheses rather than by the
	// end of the statement, because a partial index carries a WHERE predicate
	// after it and that predicate may itself contain parentheses.
	columns, tail, err := splitParenthesized(collapse(match[4]))
	if err != nil {
		return "", err
	}
	// SQLite's NOCASE has no PostgreSQL equivalent as a collation name; the
	// case-insensitive intent becomes an expression index on lower(). Queries
	// must be written as lower(col) to use it — recorded as a WP3 obligation
	// in the migration plan.
	columns = replaceOutsideStrings(columns, collateNoCase, "lower($1)")
	columns = replaceOutsideStrings(columns, collateBinary, "")
	if columns, err = translatePortableExpression(columns); err != nil {
		return "", err
	}
	keyword := "CREATE INDEX"
	if unique {
		keyword = "CREATE UNIQUE INDEX"
	}
	// A partial index is portable as written: SQLite and PostgreSQL agree on
	// the syntax and on the semantics of a WHERE-restricted index.
	predicate := strings.TrimSuffix(strings.TrimSpace(tail), ";")
	if predicate != "" {
		if !strings.HasPrefix(strings.ToUpper(predicate), "WHERE") {
			return "", fmt.Errorf("unsupported index clause %q", predicate)
		}
		if predicate, err = translatePortableExpression(predicate); err != nil {
			return "", err
		}
		predicate = " " + predicate
	}
	return fmt.Sprintf("%s %s ON %s (%s)%s;", keyword, name, table, columns, predicate), nil
}

// duplicatePointerTriggerSQLite is the exact SQLite body this translation was
// derived from, collapsed. Pinning it makes the trigger fail closed like the
// type mapping does: matching on the name alone would keep emitting this
// translation after someone changed the SQLite semantics, leaving the golden
// file byte-identical while the two databases diverged.
const duplicatePointerTriggerSQLite = `CREATE TRIGGER messages_clear_duplicate_pointer ` +
	`AFTER DELETE ON messages FOR EACH ROW WHEN OLD.id IN ( ` +
	`SELECT duplicate_of_message_id FROM messages ` +
	`WHERE user_id = OLD.user_id AND duplicate_of_message_id = OLD.id ` +
	`) BEGIN UPDATE messages SET duplicate_of_message_id = 0 ` +
	`WHERE user_id = OLD.user_id AND duplicate_of_message_id = OLD.id; END`

// translateTrigger renders a PL/pgSQL function plus an AFTER DELETE trigger.
// SQLite trigger bodies are not portable in general, so each one is
// translated by hand against a pinned source rather than parsed.
func translateTrigger(name, ddl string) (string, error) {
	if name != "messages_clear_duplicate_pointer" {
		return "", fmt.Errorf("no translation for trigger %s; add one deliberately rather than guessing: %s", name, collapse(ddl))
	}
	if got := strings.TrimSuffix(collapse(ddl), ";"); got != duplicatePointerTriggerSQLite {
		return "", fmt.Errorf("trigger %s changed in SQLite; update its PostgreSQL translation and the pinned source.\n got: %s\nwant: %s",
			name, got, duplicatePointerTriggerSQLite)
	}
	// Statement-level with a transition table, not FOR EACH ROW. The SQLite
	// original guards each row with a WHEN subquery, and PostgreSQL does not
	// allow subqueries in WHEN, so a row-level port would run one UPDATE per
	// deleted row — turning a folder purge over the 200k+ corpus into that
	// many statements. The statement-level form joins the deleted rows once
	// and is semantically identical.
	return `CREATE FUNCTION messages_clear_duplicate_pointer() RETURNS trigger AS $$
BEGIN
  -- Mirrors the SQLite trigger: when a message that others point at as their
  -- duplicate original is deleted, clear those pointers instead of leaving
  -- them dangling. AGENTS.md keeps this in SQL because reconciliation, folder
  -- purges, and account deletion all delete messages by different paths.
  UPDATE messages AS m SET duplicate_of_message_id = 0
  FROM deleted AS d
  WHERE m.user_id = d.user_id AND m.duplicate_of_message_id = d.id;
  RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER messages_clear_duplicate_pointer
AFTER DELETE ON messages
REFERENCING OLD TABLE AS deleted
FOR EACH STATEMENT EXECUTE FUNCTION messages_clear_duplicate_pointer();`, nil
}

// token describes one byte of SQL in its lexical context.
type token struct {
	index    int
	char     byte
	inString bool
	inCommen bool
	depth    int
}

// scanSQL walks value byte by byte, tracking line comments, single-quoted
// string literals (with ” escapes), and parenthesis depth, and calls visit
// for each byte. visit returns false to stop the walk early.
//
// One scanner rather than a copy per caller: a quoting bug fixed in three
// hand-rolled state machines is a quoting bug fixed in two of them.
//
// Comments are tracked *before* strings, not layered on top. The store's
// migrations contain comments like "-- Google's subject claim", whose
// apostrophe opens a string literal for any scanner that looks at quotes
// first — and from there every parenthesis and comma in the rest of the table
// is misread.
func scanSQL(value string, visit func(t token) bool) error {
	depth := 0
	inString := false
	inComment := false
	for i := 0; i < len(value); i++ {
		c := value[i]
		if inComment {
			if c == '\n' {
				inComment = false
			}
			if !visit(token{i, c, false, true, depth}) {
				return nil
			}
			continue
		}
		switch c {
		case '-':
			if !inString && i+1 < len(value) && value[i+1] == '-' {
				inComment = true
				if !visit(token{i, c, false, true, depth}) {
					return nil
				}
				continue
			}
		case '\'':
			// A doubled quote escapes a quote inside a literal.
			if inString && i+1 < len(value) && value[i+1] == '\'' {
				if !visit(token{i, c, inString, false, depth}) || !visit(token{i + 1, c, inString, false, depth}) {
					return nil
				}
				i++
				continue
			}
			if !visit(token{i, c, inString, false, depth}) {
				return nil
			}
			inString = !inString
			continue
		case '(':
			if !inString {
				depth++
			}
		case ')':
			if !inString {
				depth--
				if depth < 0 {
					return fmt.Errorf("unbalanced parentheses in %q", value)
				}
			}
		}
		if !visit(token{i, c, inString, false, depth}) {
			return nil
		}
	}
	if inString || depth != 0 {
		return fmt.Errorf("unterminated string or parenthesis in %q", value)
	}
	return nil
}

// splitTopLevel splits a definition list on commas that are outside both
// parentheses and string literals.
func splitTopLevel(body string) ([]string, error) {
	var parts []string
	start := 0
	err := scanSQL(body, func(t token) bool {
		if t.char == ',' && !t.inString && !t.inCommen && t.depth == 0 {
			parts = append(parts, body[start:t.index])
			start = t.index + 1
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	return append(parts, body[start:]), nil
}

// splitParenthesized takes a string starting with "(" and returns the content
// of that parenthesized group plus whatever follows it.
func splitParenthesized(value string) (string, string, error) {
	if !strings.HasPrefix(value, "(") {
		return "", "", fmt.Errorf("expected a parenthesized list: %q", value)
	}
	closing := -1
	err := scanSQL(value, func(t token) bool {
		if t.char == ')' && !t.inString && !t.inCommen && t.depth == 0 {
			closing = t.index
			return false
		}
		return true
	})
	if err != nil {
		return "", "", err
	}
	if closing < 0 {
		return "", "", fmt.Errorf("unbalanced parentheses in %q", value)
	}
	return value[1:closing], value[closing+1:], nil
}

// collapse turns the multi-line, tab-indented DDL the migrations produce into
// one spaced line so the output formatting is ours rather than theirs.
//
// Both steps are string-literal aware. Line comments matter because the
// store's migrations document columns inline, and folding a comment into the
// line below it swallows the column that follows. Whitespace matters because
// squashing runs inside a quoted literal silently rewrites a DEFAULT value:
// 'a  b' would become 'a b', and every row inserted on PostgreSQL would carry
// a value the SQLite schema never produced.
func collapse(value string) string {
	var out strings.Builder
	out.Grow(len(value))
	pendingSpace := false
	started := false
	_ = scanSQL(value, func(t token) bool {
		if t.inCommen {
			// A comment ends the run of text before it; without this the
			// tokens on either side would be glued together.
			if started {
				pendingSpace = true
			}
			return true
		}
		if !t.inString && (t.char == ' ' || t.char == '\t' || t.char == '\n' || t.char == '\r') {
			if started {
				pendingSpace = true
			}
			return true
		}
		if pendingSpace {
			out.WriteByte(' ')
			pendingSpace = false
		}
		out.WriteByte(t.char)
		started = true
		return true
	})
	return strings.TrimSpace(out.String())
}

// replaceOutsideStrings applies pattern only to the parts of value that are
// not inside a string literal.
//
// Every rewrite this translator performs has to go through here. A plain
// ReplaceAllString also edits quoted DEFAULT values: SQLite
// DEFAULT 'literal COLLATE BINARY value' would silently become
// DEFAULT 'literal value', changing what every new row stores. The same class
// of bug hit the foreign-key extraction first; this is its collation twin.
func replaceOutsideStrings(value string, pattern *regexp.Regexp, replacement string) string {
	literal := literalPositions(value)
	var out strings.Builder
	out.Grow(len(value))
	last := 0
	for _, match := range pattern.FindAllStringIndex(value, -1) {
		if literal[match[0]] {
			continue
		}
		out.WriteString(value[last:match[0]])
		out.WriteString(pattern.ReplaceAllString(value[match[0]:match[1]], replacement))
		last = match[1]
	}
	out.WriteString(value[last:])
	return out.String()
}

// literalPositions marks the byte offsets that sit inside a string literal or
// a comment.
func literalPositions(value string) map[int]bool {
	literal := make(map[int]bool)
	_ = scanSQL(value, func(t token) bool {
		if t.inString || t.inCommen {
			literal[t.index] = true
		}
		return true
	})
	return literal
}

// findOutsideStrings locates the first match of pattern that begins outside a
// string literal, and returns its byte range.
func findOutsideStrings(value string, pattern *regexp.Regexp) (int, int, bool) {
	literal := literalPositions(value)
	for _, match := range pattern.FindAllStringIndex(value, -1) {
		if !literal[match[0]] {
			return match[0], match[1], true
		}
	}
	return 0, 0, false
}

func unquote(value string) string {
	return strings.Trim(value, `"`)
}

// Render assembles translated objects into one applyable script.
func Render(objects []Object) string {
	var out strings.Builder
	out.WriteString(`-- Generated by backend/store/pgschema from the fully migrated combined
-- SQLite schema, including every plugin's migrations. Do not edit by hand:
-- TestBaselineMatchesSQLiteSchema regenerates this file and fails when it
-- differs, which is what keeps the PostgreSQL baseline honest while SQLite
-- migrations still land.
--
-- Type choices are documented in translate.go. In short: unix timestamps and
-- 0/1 booleans stay bigint, every text column is C-collated so ordering and
-- comparison keep the byte-wise semantics the store relies on, and identity
-- columns are GENERATED BY DEFAULT so the migration can copy ids verbatim.

`)
	for i, object := range objects {
		if i > 0 {
			out.WriteString("\n")
		}
		fmt.Fprintf(&out, "-- %s %s\n%s\n", object.Kind, object.Name, object.SQL)
	}
	return out.String()
}

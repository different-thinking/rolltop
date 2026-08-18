// File overview: SQLite-to-PostgreSQL DDL translation for the migration
// baseline (docs/postgres-migration-plan.md, WP2). The baseline is not
// hand-written: it is derived from the fully migrated combined SQLite schema
// so it cannot drift from what the store actually creates, and
// TestBaselineMatchesSQLiteSchema in backend/store regenerates it to prove
// that. Everything here is pure text-in, text-out; nothing opens a database.
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

// Object is one translated schema object in the order it must be applied.
type Object struct {
	Kind string // "table", "index", or "trigger"
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
// triggers.
//
// Splitting the foreign keys out is not cosmetic. PostgreSQL requires a
// referenced column list to be backed by a unique constraint or index, and
// this schema has composite keys — mailboxes(user_id, account_id, id) — whose
// uniqueness comes from a CREATE UNIQUE INDEX rather than from the table
// definition. Inline foreign keys would therefore be rejected for referencing
// an index that does not exist yet, which is what an earlier revision did.
// SQLite never complains because it defers the check.
func Translate(objects []SQLiteObject) ([]Object, error) {
	var tables, indexes, foreignKeys, triggers []Object
	for _, object := range objects {
		switch object.Kind {
		case "table":
			sql, keys, err := translateTable(object.Name, object.SQL)
			if err != nil {
				return nil, fmt.Errorf("table %s: %w", object.Name, err)
			}
			tables = append(tables, Object{Kind: "table", Name: object.Name, SQL: sql})
			if len(keys) > 0 {
				foreignKeys = append(foreignKeys, Object{
					Kind: "foreign keys", Name: object.Name, SQL: strings.Join(keys, "\n"),
				})
			}
		case "index":
			sql, err := translateIndex(object.SQL)
			if err != nil {
				return nil, fmt.Errorf("index %s: %w", object.Name, err)
			}
			indexes = append(indexes, Object{Kind: "index", Name: object.Name, SQL: sql})
		case "trigger":
			sql, err := translateTrigger(object.Name, object.SQL)
			if err != nil {
				return nil, fmt.Errorf("trigger %s: %w", object.Name, err)
			}
			triggers = append(triggers, Object{Kind: "trigger", Name: object.Name, SQL: sql})
		default:
			return nil, fmt.Errorf("unsupported object kind %q for %s", object.Kind, object.Name)
		}
	}
	sort.SliceStable(indexes, func(i, j int) bool { return indexes[i].Name < indexes[j].Name })
	out := make([]Object, 0, len(tables)+len(indexes)+len(foreignKeys)+len(triggers))
	out = append(out, tables...)
	out = append(out, indexes...)
	out = append(out, foreignKeys...)
	out = append(out, triggers...)
	return out, nil
}

var (
	createTableRE = regexp.MustCompile(`(?is)^\s*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z0-9_"]+)\s*\((.*)\)\s*;?\s*$`)
	createIndexRE = regexp.MustCompile(`(?is)^\s*CREATE\s+(UNIQUE\s+)?INDEX\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z0-9_"]+)\s+ON\s+([A-Za-z0-9_"]+)\s*(\(.*)$`)
	collateBinary = regexp.MustCompile(`(?i)\s+COLLATE\s+BINARY\b`)
	collateNoCase = regexp.MustCompile(`(?i)([A-Za-z0-9_]+)\s+COLLATE\s+NOCASE\b`)
	autoincrement = regexp.MustCompile(`(?i)^INTEGER\s+PRIMARY\s+KEY\s+AUTOINCREMENT$`)
	// A column-level foreign key, with its optional referential actions.
	columnReferenceRE = regexp.MustCompile(`(?i)REFERENCES\s+[A-Za-z0-9_"]+\s*\([^)]*\)(\s+ON\s+(DELETE|UPDATE)\s+(CASCADE|RESTRICT|SET\s+NULL|SET\s+DEFAULT|NO\s+ACTION))*`)
	tableForeignKeyRE = regexp.MustCompile(`(?i)^FOREIGN\s+KEY\b`)
)

// tableConstraintRE matches the start of a table-level constraint rather than
// a column. The word boundary is load-bearing: a bare prefix test treats a
// column named "checksum" as a CHECK constraint and passes it through
// untranslated, which is exactly what an earlier revision did.
var tableConstraintRE = regexp.MustCompile(`(?i)^(primary\s+key|unique|check|foreign\s+key|constraint)\b`)

func translateTable(name, ddl string) (string, []string, error) {
	match := createTableRE.FindStringSubmatch(ddl)
	if match == nil {
		return "", nil, fmt.Errorf("unrecognized CREATE TABLE: %s", collapse(ddl))
	}
	table := unquote(match[1])
	parts, err := splitTopLevel(stripLineComments(match[2]))
	if err != nil {
		return "", nil, err
	}
	rendered := make([]string, 0, len(parts))
	var foreignKeys []string
	for _, part := range parts {
		part = strings.TrimSpace(collapse(part))
		if part == "" {
			continue
		}
		if tableForeignKeyRE.MatchString(part) {
			foreignKeys = append(foreignKeys, fmt.Sprintf("ALTER TABLE %s ADD %s;", table, part))
			continue
		}
		if isTableConstraint(part) {
			rendered = append(rendered, "  "+collateBinary.ReplaceAllString(part, ""))
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
		rendered = append(rendered, "  "+column)
	}
	return fmt.Sprintf("CREATE TABLE %s (\n%s\n);", table, strings.Join(rendered, ",\n")), foreignKeys, nil
}

func isTableConstraint(part string) bool {
	return tableConstraintRE.MatchString(part)
}

// translateColumn rewrites one column definition. The rest of the definition
// (NOT NULL, DEFAULT, REFERENCES ... ON DELETE ...) is valid PostgreSQL as
// written and passes through untouched.
// translateColumn rewrites one column definition and returns any REFERENCES
// clause separately, so the caller can emit it as an ALTER TABLE after the
// indexes exist. The rest of the definition (NOT NULL, DEFAULT) is valid
// PostgreSQL as written and passes through untouched.
func translateColumn(definition string) (string, string, error) {
	definition = collateBinary.ReplaceAllString(definition, "")
	reference := ""
	if match := columnReferenceRE.FindString(definition); match != "" {
		definition = strings.TrimSpace(strings.Replace(definition, match, "", 1))
		reference = strings.TrimSpace(match)
	}
	fields := strings.SplitN(definition, " ", 2)
	if len(fields) < 2 {
		return "", "", fmt.Errorf("column without a type: %q", definition)
	}
	name, rest := fields[0], strings.TrimSpace(fields[1])

	// The surrogate key. SQLite's AUTOINCREMENT and PostgreSQL's identity
	// column differ in one way that matters to the migration tool: identity
	// sequences must be advanced with setval after the rows are copied.
	if autoincrement.MatchString(rest) {
		return name + " bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY", reference, nil
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
// Every text column is C-collated: SQLite compares and orders text
// byte-wise, and the measured cluster locale sorts a,ä,B,Z where byte order
// is B,Z,a,ä (plan §13), so relying on the database default would change
// every ORDER BY in the store.
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
	// case-insensitive intent becomes an expression index on lower().
	columns = collateNoCase.ReplaceAllString(columns, "lower($1)")
	columns = collateBinary.ReplaceAllString(columns, "")
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
		predicate = " " + predicate
	}
	return fmt.Sprintf("%s %s ON %s (%s)%s;", keyword, name, table, columns, predicate), nil
}

// splitParenthesized takes a string starting with "(" and returns the content
// of that parenthesized group plus whatever follows it.
func splitParenthesized(value string) (string, string, error) {
	if !strings.HasPrefix(value, "(") {
		return "", "", fmt.Errorf("expected a parenthesized list: %q", value)
	}
	depth := 0
	inString := false
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '\'':
			if inString && i+1 < len(value) && value[i+1] == '\'' {
				i++
				continue
			}
			inString = !inString
		case '(':
			if !inString {
				depth++
			}
		case ')':
			if !inString {
				depth--
				if depth == 0 {
					return value[1:i], value[i+1:], nil
				}
			}
		}
	}
	return "", "", fmt.Errorf("unbalanced parentheses in %q", value)
}

// translateTrigger renders a PL/pgSQL function plus an AFTER DELETE trigger.
//
// Only the one trigger this schema has is supported, and it is matched by
// name rather than parsed: SQLite trigger bodies are not portable in general,
// and a silent mistranslation of a data-repair trigger is worse than a build
// failure. A second trigger must be added here deliberately.
func translateTrigger(name, ddl string) (string, error) {
	if name != "messages_clear_duplicate_pointer" {
		return "", fmt.Errorf("no translation for trigger %s; add one deliberately rather than guessing: %s", name, collapse(ddl))
	}
	return `CREATE FUNCTION messages_clear_duplicate_pointer() RETURNS trigger AS $$
BEGIN
  -- Mirrors the SQLite trigger: when a message that others point at as their
  -- duplicate original is deleted, clear those pointers instead of leaving
  -- them dangling. AGENTS.md keeps this in SQL because reconciliation, folder
  -- purges, and account deletion all delete messages by different paths.
  UPDATE messages SET duplicate_of_message_id = 0
  WHERE user_id = OLD.user_id AND duplicate_of_message_id = OLD.id;
  RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER messages_clear_duplicate_pointer
AFTER DELETE ON messages
FOR EACH ROW EXECUTE FUNCTION messages_clear_duplicate_pointer();`, nil
}

// splitTopLevel splits a column list on commas that are not inside parens or
// string literals.
func splitTopLevel(body string) ([]string, error) {
	var parts []string
	depth := 0
	inString := false
	start := 0
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '\'':
			// Doubled quotes escape a quote inside a literal.
			if inString && i+1 < len(body) && body[i+1] == '\'' {
				i++
				continue
			}
			inString = !inString
		case '(':
			if !inString {
				depth++
			}
		case ')':
			if !inString {
				depth--
				if depth < 0 {
					return nil, fmt.Errorf("unbalanced parentheses")
				}
			}
		case ',':
			if !inString && depth == 0 {
				parts = append(parts, body[start:i])
				start = i + 1
			}
		}
	}
	if inString || depth != 0 {
		return nil, fmt.Errorf("unterminated string or parenthesis")
	}
	return append(parts, body[start:]), nil
}

// collapse turns the multi-line, tab-indented DDL the migrations produce into
// one spaced line so the output formatting is ours rather than theirs. Line
// comments are removed first: the store's migrations document columns inline,
// and folding a comment into the line below it would silently swallow the
// column that follows.
func collapse(value string) string {
	return strings.Join(strings.Fields(stripLineComments(value)), " ")
}

// stripLineComments removes "--" to end of line, leaving comment markers that
// appear inside string literals alone.
func stripLineComments(value string) string {
	lines := strings.Split(value, "\n")
	for i, line := range lines {
		inString := false
		for j := 0; j < len(line); j++ {
			switch line[j] {
			case '\'':
				if inString && j+1 < len(line) && line[j+1] == '\'' {
					j++
					continue
				}
				inString = !inString
			case '-':
				if !inString && j+1 < len(line) && line[j+1] == '-' {
					line = line[:j]
					j = len(line)
				}
			}
		}
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}

func unquote(value string) string {
	return strings.Trim(value, `"`)
}

// Render assembles translated objects into one applyable script.
func Render(objects []Object) string {
	var out strings.Builder
	out.WriteString(`-- Generated by backend/store/pgschema from the fully migrated combined
-- SQLite schema. Do not edit by hand: TestBaselineMatchesSQLiteSchema
-- regenerates this file and fails when it differs, which is what keeps the
-- PostgreSQL baseline honest while SQLite migrations still land.
--
-- Type choices are documented in translate.go. In short: unix timestamps and
-- 0/1 booleans stay bigint, and every text column is C-collated so ordering
-- and comparison keep the byte-wise semantics the store relies on.

`)
	for i, object := range objects {
		if i > 0 {
			out.WriteString("\n")
		}
		fmt.Fprintf(&out, "-- %s %s\n%s\n", object.Kind, object.Name, object.SQL)
	}
	return out.String()
}

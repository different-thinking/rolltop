package pgschema_test

import (
	"strings"
	"testing"

	"rolltop/backend/store/pgschema"
)

// TestDeclaredMatchesTheRenderedSQL keeps the header parser honest against the
// SQL it claims to describe. The drop step in the admin console removes exactly
// the names Declared returns, so a table the parser misses would survive a drop
// and then block the next create.
func TestDeclaredMatchesTheRenderedSQL(t *testing.T) {
	tables := pgschema.DeclaredNames(pgschema.TableKind)
	if len(tables) == 0 {
		t.Fatal("the baseline declares no tables")
	}
	for _, name := range tables {
		if !strings.Contains(pgschema.Baseline, "\nCREATE TABLE "+name+" (") {
			t.Errorf("declared table %s has no CREATE TABLE in the baseline", name)
		}
	}
	// And the other direction: every CREATE TABLE must be declared, or the drop
	// would leave it behind.
	for _, line := range strings.Split(pgschema.Baseline, "\n") {
		if !strings.HasPrefix(line, "CREATE TABLE ") {
			continue
		}
		name := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "CREATE TABLE "), "("))
		found := false
		for _, declared := range tables {
			if declared == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("table %s is created but not declared", name)
		}
	}

	triggers := pgschema.DeclaredNames(pgschema.TriggerKind)
	if len(triggers) == 0 {
		t.Fatal("the baseline declares no triggers")
	}
	for _, name := range triggers {
		// The trigger and the function it runs share a name, and the drop needs
		// the function name because dropping the table takes only the trigger.
		if !strings.Contains(pgschema.Baseline, "CREATE FUNCTION "+name+"()") {
			t.Errorf("declared trigger %s has no function in the baseline", name)
		}
	}
}

// TestDeclaredIgnoresHeadersInsideBodies pins why the parser anchors to the
// start of a line: trigger bodies carry SQL comments of their own.
func TestDeclaredIgnoresHeadersInsideBodies(t *testing.T) {
	for _, object := range pgschema.Declared() {
		if strings.ContainsAny(object.Name, " \t;()") {
			t.Errorf("parsed a malformed object name %q", object.Name)
		}
	}
}

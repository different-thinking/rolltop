package store

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func openCalendarMigrationFixture(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	for _, statement := range []string{
		`CREATE TABLE users (id INTEGER PRIMARY KEY AUTOINCREMENT)`,
		// Two owners, so a test that switches foreign keys on still has rows for
		// the tenant column to point at.
		`INSERT INTO users (id) VALUES (1), (2)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range userGoogleCalendarMigrationSet().Statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("apply %q: %v", statement, err)
		}
	}
	return db, ctx
}

func insertFixtureCalendar(t *testing.T, ctx context.Context, db *sql.DB, userID, connectionID int64, googleID string) int64 {
	t.Helper()
	res, err := db.ExecContext(ctx,
		`INSERT INTO calendars (user_id, google_connection_id, google_calendar_id, created_at, updated_at)
			VALUES (?, ?, ?, 0, 0)`, userID, connectionID, googleID)
	if err != nil {
		t.Fatalf("insert calendar: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// The same Google calendar may legitimately be reachable through two connected
// accounts, and two tenants may both subscribe to the same public calendar. Only
// a second copy under one connection is a duplicate.
func TestCalendarMigrationScopesTheCalendarIdentity(t *testing.T) {
	db, ctx := openCalendarMigrationFixture(t)

	insertFixtureCalendar(t, ctx, db, 1, 7, "team@group.calendar.google.com")
	if _, err := db.ExecContext(ctx,
		`INSERT INTO calendars (user_id, google_connection_id, google_calendar_id, created_at, updated_at)
			VALUES (1, 7, 'team@group.calendar.google.com', 0, 0)`); err == nil {
		t.Fatal("a second row for the same calendar under one connection was accepted")
	}
	insertFixtureCalendar(t, ctx, db, 1, 8, "team@group.calendar.google.com")
	insertFixtureCalendar(t, ctx, db, 2, 7, "team@group.calendar.google.com")
}

// An event id is unique within its calendar, not across calendars: the same
// meeting appears in the organizer's and every attendee's calendar under ids
// that can collide.
func TestCalendarMigrationScopesTheEventIdentity(t *testing.T) {
	db, ctx := openCalendarMigrationFixture(t)
	first := insertFixtureCalendar(t, ctx, db, 1, 7, "primary")
	second := insertFixtureCalendar(t, ctx, db, 1, 7, "other")

	insert := func(calendarID int64, externalID string) error {
		_, err := db.ExecContext(ctx,
			`INSERT INTO calendar_events (user_id, calendar_id, external_id, created_at, updated_at)
				VALUES (1, ?, ?, 0, 0)`, calendarID, externalID)
		return err
	}
	if err := insert(first, "abc123"); err != nil {
		t.Fatal(err)
	}
	if err := insert(first, "abc123"); err == nil {
		t.Fatal("a second row for the same event was accepted")
	}
	if err := insert(second, "abc123"); err != nil {
		t.Fatalf("same event id in another calendar: %v", err)
	}
}

// A calendar the user unsubscribes from takes its events with it. Without the
// cascade they would stay in every range query, drawn under a calendar that no
// longer exists and can never be switched off again.
func TestCalendarMigrationCascadesEventsWithTheCalendar(t *testing.T) {
	db, ctx := openCalendarMigrationFixture(t)
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		t.Fatal(err)
	}
	calendarID := insertFixtureCalendar(t, ctx, db, 1, 7, "primary")
	if _, err := db.ExecContext(ctx,
		`INSERT INTO calendar_events (user_id, calendar_id, external_id, created_at, updated_at)
			VALUES (1, ?, 'abc123', 0, 0)`, calendarID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM calendars WHERE id = ?`, calendarID); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM calendar_events`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("events left behind after the calendar was removed: %d", remaining)
	}
}

// The newest migration owns the whole-list invariants: it is the one that would
// break them, and asserting them from every migration's test would mean fixing
// the same thing in a dozen places the next time one is added.
func TestUser034IsLatestRegisteredUserMigration(t *testing.T) {
	sets := currentUserMigrationSetsForUpgradeTest()
	if len(sets) < 2 {
		t.Fatalf("registered user migrations=%d, want at least 2", len(sets))
	}
	if latest := sets[len(sets)-1]; latest.Version != UserSchemaVersion034 {
		t.Fatalf("latest user migration=%q, want %q", latest.Version, UserSchemaVersion034)
	}
	if predecessor := sets[len(sets)-2]; predecessor.Version != UserSchemaVersion033 {
		t.Fatalf("user-034 predecessor=%q, want %q", predecessor.Version, UserSchemaVersion033)
	}
	// Application order is not numeric — user-011 has always run before
	// user-004 — so only duplicates are worth asserting here. A version applied
	// twice would record one checksum over two different statement lists.
	seen := map[string]bool{}
	for _, set := range sets {
		if seen[set.Version] {
			t.Fatalf("user migration %q is registered twice", set.Version)
		}
		seen[set.Version] = true
	}
	// The legacy fixture applies a frozen prefix of this list to build a v21
	// database. If it ever stops being a prefix, the upgrade tests would be
	// migrating from a schema the app never actually shipped.
	legacy := legacyUserMigrationSetsThroughV21()
	if len(legacy) > len(sets) {
		t.Fatalf("legacy prefix has %d sets, more than the %d registered", len(legacy), len(sets))
	}
	for i, set := range legacy {
		if sets[i].Version != set.Version {
			t.Fatalf("registered migration %d = %q, want the legacy prefix entry %q", i, sets[i].Version, set.Version)
		}
	}
}

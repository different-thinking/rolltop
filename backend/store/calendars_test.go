// File overview: Tests for calendar and event persistence, tenant isolation,
// and the range query the week view depends on.

package store

import (
	"context"
	"testing"
	"time"
)

func openCalendarStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	db, err := openTestStore(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, ctx
}

func mustUser(t *testing.T, db *Store, ctx context.Context, email string) int64 {
	t.Helper()
	user, err := db.CreateUser(ctx, email, "Calendar", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	return user.ID
}

func mustCalendar(t *testing.T, db *Store, ctx context.Context, userID, connectionID int64, googleID string) Calendar {
	t.Helper()
	calendar, err := db.UpsertCalendar(ctx, userID, CalendarUpsert{
		GoogleConnectionID: connectionID,
		GoogleCalendarID:   googleID,
		Summary:            googleID,
		AccessRole:         CalendarAccessRoleOwner,
		Selected:           true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return calendar
}

func mustEvent(t *testing.T, db *Store, ctx context.Context, userID int64, event CalendarEvent) CalendarEvent {
	t.Helper()
	stored, err := db.UpsertCalendarEvent(ctx, userID, event)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

// Two tenants may subscribe to calendars that share every identifier. Neither
// may read, list, or delete the other's rows.
func TestCalendarsAreScopedByUser(t *testing.T) {
	db, ctx := openCalendarStore(t)
	user := mustUser(t, db, ctx, "calendar@example.test")
	other := mustUser(t, db, ctx, "other-calendar@example.test")

	mine := mustCalendar(t, db, ctx, user, 1, "primary")
	theirs := mustCalendar(t, db, ctx, other, 1, "primary")
	if mine.ID == 0 || theirs.ID == 0 {
		t.Fatal("calendars were not stored")
	}

	if _, err := db.Calendar(ctx, other, mine.ID); !IsNotFound(err) {
		t.Fatalf("reading another tenant's calendar: err=%v, want not found", err)
	}
	if err := db.SetCalendarSelected(ctx, other, mine.ID, false); !IsNotFound(err) {
		t.Fatalf("hiding another tenant's calendar: err=%v, want not found", err)
	}
	if err := db.DeleteCalendar(ctx, other, mine.ID); !IsNotFound(err) {
		t.Fatalf("deleting another tenant's calendar: err=%v, want not found", err)
	}
	list, err := db.ListCalendars(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != theirs.ID {
		t.Fatalf("calendar list for the other tenant = %+v, want only their own", list)
	}

	start := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	mustEvent(t, db, ctx, user, CalendarEvent{
		CalendarID: mine.ID, ExternalID: "abc", Summary: "Mine",
		StartAt: start, EndAt: start.Add(time.Hour),
	})
	// The other tenant naming a calendar id they do not own must read as empty,
	// not as the owner's events.
	events, err := db.ListCalendarEventsInRange(ctx, other, []int64{mine.ID},
		start.Add(-time.Hour), start.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("cross-tenant range query returned %d events, want none", len(events))
	}
}

// The list sync runs on every poll and must not undo what the user chose. Only
// the fields Google owns are refreshed.
func TestUpsertCalendarKeepsLocalStateOnRefresh(t *testing.T) {
	db, ctx := openCalendarStore(t)
	user := mustUser(t, db, ctx, "calendar-refresh@example.test")
	calendar := mustCalendar(t, db, ctx, user, 1, "primary")

	if err := db.SetCalendarSelected(ctx, user, calendar.ID, false); err != nil {
		t.Fatal(err)
	}
	window := time.Date(2025, 8, 17, 0, 0, 0, 0, time.UTC)
	if err := db.SaveCalendarSyncState(ctx, user, calendar.ID, CalendarSyncState{
		SyncToken:     "cursor-1",
		WindowStartAt: window,
		LastSyncAt:    window,
		LastSuccessAt: window,
		Status:        CalendarSyncStatusOK,
	}); err != nil {
		t.Fatal(err)
	}

	refreshed, err := db.UpsertCalendar(ctx, user, CalendarUpsert{
		GoogleConnectionID: 1,
		GoogleCalendarID:   "primary",
		Summary:            "Renamed at Google",
		Color:              "#ff0000",
		AccessRole:         CalendarAccessRoleOwner,
		Selected:           true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Summary != "Renamed at Google" || refreshed.Color != "#ff0000" {
		t.Fatalf("calendar = %+v, want the fields Google owns refreshed", refreshed)
	}
	if refreshed.Selected {
		t.Fatal("a list sync re-enabled a calendar the user had switched off")
	}
	if refreshed.SyncToken != "cursor-1" || !refreshed.WindowStartAt.Equal(window) {
		t.Fatalf("calendar = %+v, want the event cursor and window untouched", refreshed)
	}
}

// The week view asks for one range and expects everything that overlaps it,
// including events that started before the range and end inside it.
func TestListCalendarEventsInRangeReturnsOverlaps(t *testing.T) {
	db, ctx := openCalendarStore(t)
	user := mustUser(t, db, ctx, "calendar-range@example.test")
	calendar := mustCalendar(t, db, ctx, user, 1, "primary")
	hidden := mustCalendar(t, db, ctx, user, 1, "other")

	from := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 7)
	inside := mustEvent(t, db, ctx, user, CalendarEvent{
		CalendarID: calendar.ID, ExternalID: "inside", Summary: "Inside",
		StartAt: from.Add(30 * time.Hour), EndAt: from.Add(31 * time.Hour),
	})
	straddling := mustEvent(t, db, ctx, user, CalendarEvent{
		CalendarID: calendar.ID, ExternalID: "straddling", Summary: "Started earlier",
		StartAt: from.Add(-48 * time.Hour), EndAt: from.Add(time.Hour),
	})
	mustEvent(t, db, ctx, user, CalendarEvent{
		CalendarID: calendar.ID, ExternalID: "before", Summary: "Long gone",
		StartAt: from.Add(-72 * time.Hour), EndAt: from.Add(-71 * time.Hour),
	})
	mustEvent(t, db, ctx, user, CalendarEvent{
		CalendarID: hidden.ID, ExternalID: "hidden", Summary: "Switched off",
		StartAt: from.Add(30 * time.Hour), EndAt: from.Add(31 * time.Hour),
	})

	events, err := db.ListCalendarEventsInRange(ctx, user, []int64{calendar.ID}, from, to)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, event := range events {
		got[event.ExternalID] = true
	}
	if !got["inside"] || !got["straddling"] {
		t.Fatalf("range query returned %v, want both overlapping events", got)
	}
	if got["before"] {
		t.Fatal("range query returned an event that ended before the range")
	}
	if got["hidden"] {
		t.Fatal("range query returned an event from a calendar that was not asked for")
	}
	if inside.ID == 0 || straddling.ID == 0 {
		t.Fatal("events were not stored")
	}

	// No visible calendars is a real state of the week view, and it must answer
	// with nothing rather than with everything.
	empty, err := db.ListCalendarEventsInRange(ctx, user, nil, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("range query without calendars returned %d events, want none", len(empty))
	}
}

// An all-day event is stored at midnight UTC, so for a viewer far enough east
// its bounds sit outside the local week that has to show it. The padding is
// what keeps it in the answer.
func TestListCalendarEventsInRangeKeepsEdgeAllDayEvents(t *testing.T) {
	db, ctx := openCalendarStore(t)
	user := mustUser(t, db, ctx, "calendar-allday@example.test")
	calendar := mustCalendar(t, db, ctx, user, 1, "primary")

	day := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	mustEvent(t, db, ctx, user, CalendarEvent{
		CalendarID: calendar.ID, ExternalID: "holiday", Summary: "Holiday",
		StartAt: day, EndAt: day.AddDate(0, 0, 1), AllDay: true,
	})
	// A week that starts at local midnight in UTC+12 begins twelve hours before
	// the stored UTC midnight of its first day.
	from := day.Add(-12 * time.Hour)
	events, err := db.ListCalendarEventsInRange(ctx, user, []int64{calendar.ID}, from, from.AddDate(0, 0, 7))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].ExternalID != "holiday" {
		t.Fatalf("range query returned %+v, want the all-day event on the edge", events)
	}
}

// Google is the leading system for events: an occurrence it sends again
// replaces the stored row outright rather than merging into it.
func TestUpsertCalendarEventReplacesTheMirror(t *testing.T) {
	db, ctx := openCalendarStore(t)
	user := mustUser(t, db, ctx, "calendar-upsert@example.test")
	calendar := mustCalendar(t, db, ctx, user, 1, "primary")

	start := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	first := mustEvent(t, db, ctx, user, CalendarEvent{
		CalendarID: calendar.ID, ExternalID: "abc", ETag: "v1",
		Summary: "Standup", Location: "Room 1", StartAt: start, EndAt: start.Add(time.Hour),
		Attendees: []CalendarAttendee{{Email: "someone@example.test", Response: CalendarResponseAccepted}},
	})
	second := mustEvent(t, db, ctx, user, CalendarEvent{
		CalendarID: calendar.ID, ExternalID: "abc", ETag: "v2",
		Summary: "Standup", StartAt: start, EndAt: start.Add(30 * time.Minute),
	})
	if second.ID != first.ID {
		t.Fatalf("upsert created a second row: %d then %d", first.ID, second.ID)
	}
	if second.Location != "" {
		t.Fatalf("location = %q, want the field cleared at Google to be cleared here", second.Location)
	}
	if len(second.Attendees) != 0 {
		t.Fatalf("attendees = %+v, want the removed guest list gone", second.Attendees)
	}
	if second.ETag != "v2" || !second.EndAt.Equal(start.Add(30*time.Minute)) {
		t.Fatalf("event = %+v, want the incoming copy", second)
	}
	if len(first.Attendees) != 1 || first.Attendees[0].Email != "someone@example.test" {
		t.Fatalf("attendees did not round-trip: %+v", first.Attendees)
	}
}

// Disconnecting an account takes its calendars with it. They are pure mirrors:
// left behind they could never sync again and could never be switched off.
func TestDeleteGoogleConnectionRemovesCalendars(t *testing.T) {
	db, ctx := openCalendarStore(t)
	user := mustUser(t, db, ctx, "calendar-disconnect@example.test")
	connection, err := db.UpsertGoogleConnection(ctx, user, GoogleConnectionUpsert{
		GoogleEmail:           "person@gmail.test",
		GoogleSubject:         "subject-1",
		EncryptedRefreshToken: "ciphertext",
	})
	if err != nil {
		t.Fatal(err)
	}
	calendar := mustCalendar(t, db, ctx, user, connection.ID, "primary")
	start := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	event := mustEvent(t, db, ctx, user, CalendarEvent{
		CalendarID: calendar.ID, ExternalID: "abc", StartAt: start, EndAt: start.Add(time.Hour),
	})
	if err := db.SaveGoogleCalendarSync(ctx, GoogleCalendarSync{
		UserID: user, ConnectionID: connection.ID, SyncToken: "list-cursor",
	}); err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteGoogleConnection(ctx, user, connection.ID); err != nil {
		t.Fatal(err)
	}
	calendars, err := db.ListCalendars(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if len(calendars) != 0 {
		t.Fatalf("calendars survived the disconnect: %+v", calendars)
	}
	if _, err := db.CalendarEvent(ctx, user, event.ID); !IsNotFound(err) {
		t.Fatalf("event survived the disconnect: err=%v", err)
	}
	state, err := db.GetGoogleCalendarSync(ctx, user, connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.SyncToken != "" {
		t.Fatalf("list cursor survived the disconnect: %q", state.SyncToken)
	}
}

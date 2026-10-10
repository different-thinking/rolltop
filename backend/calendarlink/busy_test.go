// File overview: Tests for the busy calendar's placeholders: what a copy made
// there carries, and that it stays a placeholder through every edit.

package calendarlink

import (
	"context"
	"testing"
	"time"

	"rolltop/backend/store"
)

const busyCalendarID = 3

func newBusyRouter() (*Router, *fakeBackend, *fakeBackend) {
	router, google, microsoft := newRouter()
	router.Calendars.(fakeCalendars)[busyCalendarID] = store.Calendar{ID: busyCalendarID, Provider: store.CalendarProviderGoogle}
	return router, google, microsoft
}

func detailedEvent(start time.Time) store.CalendarEvent {
	return store.CalendarEvent{
		Summary: "Dentist", Description: "Root canal, bring the X-ray", Location: "Hauptstraße 1",
		StartAt: start, EndAt: start.Add(time.Hour), TimeZone: "Europe/Berlin",
		Attendees:     []store.CalendarAttendee{{Email: "guest@example.test"}},
		OnlineMeeting: true,
	}
}

// assertPlaceholder fails unless event carries the time of want and nothing
// of its content.
func assertPlaceholder(t *testing.T, event, want store.CalendarEvent, title string) {
	t.Helper()
	if event.Summary != title || event.Description != "" || event.Location != "" ||
		len(event.Attendees) != 0 || event.OnlineMeeting || !event.LinkMasked || event.LinkPrimary {
		t.Fatalf("placeholder carried content: %+v", event)
	}
	if !event.StartAt.Equal(want.StartAt) || !event.EndAt.Equal(want.EndAt) || event.AllDay != want.AllDay {
		t.Fatalf("placeholder time = %v-%v, want %v-%v", event.StartAt, event.EndAt, want.StartAt, want.EndAt)
	}
}

// One event, a full copy in the second calendar and a placeholder in the busy
// calendar, all under one key.
func TestCreateWithCopiesMakesAPlaceholder(t *testing.T) {
	router, google, microsoft := newBusyRouter()
	start := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	created, problems, err := router.CreateWithCopies(context.Background(), 7, microsoftCalendarID, []Target{
		{CalendarID: googleCalendarID, Change: CopyAdd},
		{CalendarID: busyCalendarID, Masked: true, Title: "Away", Change: CopyAdd},
	}, detailedEvent(start))
	if err != nil || len(problems) != 0 {
		t.Fatalf("create: %v %v", err, problems)
	}
	if len(microsoft.creates) != 1 || len(google.creates) != 2 {
		t.Fatalf("creates: microsoft=%d google=%d", len(microsoft.creates), len(google.creates))
	}
	full, busy := google.creates[0], google.creates[1]
	if full.Summary != "Dentist" || full.Description == "" || full.LinkMasked || full.LinkKey != created.LinkKey {
		t.Fatalf("second calendar copy = %+v", full)
	}
	if busy.CalendarID != busyCalendarID || busy.LinkKey != created.LinkKey {
		t.Fatalf("busy copy link = %+v", busy)
	}
	assertPlaceholder(t, busy, created, "Away")
}

// A placeholder alone still makes the event the primary of a group, and an
// empty title falls back to the default.
func TestCreateWithOnlyAPlaceholder(t *testing.T) {
	router, google, microsoft := newBusyRouter()
	start := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	created, _, err := router.CreateWithCopies(context.Background(), 7, microsoftCalendarID, []Target{
		{CalendarID: googleCalendarID, Change: CopyKeep},
		{CalendarID: busyCalendarID, Masked: true, Change: CopyAdd},
	}, detailedEvent(start))
	if err != nil {
		t.Fatal(err)
	}
	if !microsoft.creates[0].LinkPrimary || len(google.creates) != 1 {
		t.Fatalf("primary=%+v google creates=%d", microsoft.creates[0], len(google.creates))
	}
	assertPlaceholder(t, google.creates[0], created, store.DefaultBusyLabel)
}

// Editing the event moves its placeholder and changes nothing else about it,
// and a placeholder held by a calendar that is no longer the busy one is still
// rendered as a placeholder: the role lives on the copy, not the calendar.
func TestUpdateGroupKeepsPlaceholdersEmpty(t *testing.T) {
	router, google, _ := newBusyRouter()
	start := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	existing := detailedEvent(start)
	existing.ID, existing.CalendarID, existing.LinkKey, existing.LinkPrimary = 1, microsoftCalendarID, "k", true
	busy := store.CalendarEvent{ID: 2, CalendarID: googleCalendarID, Summary: "Blocked", StartAt: start, EndAt: start.Add(time.Hour), LinkKey: "k", LinkMasked: true}
	edited := existing
	edited.Summary = "Dentist (moved)"
	edited.StartAt = start.Add(2 * time.Hour)
	edited.EndAt = start.Add(3 * time.Hour)
	// The second calendar is now googleCalendarID, the one holding the
	// placeholder; asking to keep its copy must not fill the placeholder in.
	_, problems, err := router.UpdateGroupTargets(context.Background(), 7, existing, edited, []store.CalendarEvent{busy}, []Target{
		{CalendarID: googleCalendarID, Change: CopyAdd},
	})
	if err != nil || len(problems) != 0 {
		t.Fatalf("update: %v %v", err, problems)
	}
	if len(google.updates) != 1 || len(google.creates) != 0 {
		t.Fatalf("google updates=%d creates=%d", len(google.updates), len(google.creates))
	}
	assertPlaceholder(t, google.updates[0], edited, "Blocked")
}

// The busy calendar's current title replaces the old one on the next edit.
func TestUpdateGroupRetitlesThePlaceholder(t *testing.T) {
	router, google, _ := newBusyRouter()
	start := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	existing := detailedEvent(start)
	existing.ID, existing.CalendarID, existing.LinkKey, existing.LinkPrimary = 1, microsoftCalendarID, "k", true
	busy := store.CalendarEvent{ID: 2, CalendarID: busyCalendarID, Summary: "Busy", StartAt: start, EndAt: start.Add(time.Hour), LinkKey: "k", LinkMasked: true}
	if _, _, err := router.UpdateGroupTargets(context.Background(), 7, existing, existing, []store.CalendarEvent{busy}, []Target{
		{CalendarID: busyCalendarID, Masked: true, Title: "Away", Change: CopyKeep},
	}); err != nil {
		t.Fatal(err)
	}
	assertPlaceholder(t, google.updates[0], existing, "Away")
}

// An edit made through the placeholder moves the other copies in time and
// writes nothing else into them, and never makes a copy from the placeholder.
func TestUpdateGroupThroughThePlaceholder(t *testing.T) {
	router, google, microsoft := newBusyRouter()
	start := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	primary := detailedEvent(start)
	primary.ID, primary.CalendarID, primary.LinkKey, primary.LinkPrimary = 1, microsoftCalendarID, "k", true
	existing := store.CalendarEvent{ID: 2, CalendarID: busyCalendarID, Summary: "Busy", StartAt: start, EndAt: start.Add(time.Hour), LinkKey: "k", LinkMasked: true}
	edited := existing
	edited.StartAt = start.Add(time.Hour)
	edited.EndAt = start.Add(2 * time.Hour)
	edited.Attendees = []store.CalendarAttendee{{Email: "intruder@example.test"}}
	edited.Description, edited.Location = "leaked notes", "leaked place"
	_, problems, err := router.UpdateGroupTargets(context.Background(), 7, existing, edited, []store.CalendarEvent{primary}, []Target{
		{CalendarID: googleCalendarID, Change: CopyAdd},
	})
	if err != nil || len(problems) != 0 {
		t.Fatalf("update: %v %v", err, problems)
	}
	if len(google.creates) != 0 {
		t.Fatalf("a copy was made from the placeholder: %+v", google.creates)
	}
	written := google.updates[0]
	if !written.LinkMasked || len(written.Attendees) != 0 || written.Description != "" || written.Location != "" {
		t.Fatalf("placeholder edit = %+v", written)
	}
	follow := microsoft.updates[0]
	if follow.Summary != "Dentist" || follow.Description != primary.Description || follow.Location != primary.Location ||
		len(follow.Attendees) != 1 || !follow.LinkPrimary || follow.LinkMasked {
		t.Fatalf("primary took on the placeholder's content: %+v", follow)
	}
	if !follow.StartAt.Equal(edited.StartAt) || !follow.EndAt.Equal(edited.EndAt) {
		t.Fatalf("primary time = %v-%v, want %v-%v", follow.StartAt, follow.EndAt, edited.StartAt, edited.EndAt)
	}
}

// Unticking the busy box deletes the placeholder and leaves the second
// calendar's copy alone.
func TestUpdateGroupRemovesThePlaceholder(t *testing.T) {
	router, google, _ := newBusyRouter()
	start := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	existing := detailedEvent(start)
	existing.ID, existing.CalendarID, existing.LinkKey, existing.LinkPrimary = 1, microsoftCalendarID, "k", true
	full := store.CalendarEvent{ID: 2, CalendarID: googleCalendarID, Summary: "Dentist", StartAt: start, EndAt: start.Add(time.Hour), LinkKey: "k"}
	busy := store.CalendarEvent{ID: 3, CalendarID: busyCalendarID, Summary: "Busy", StartAt: start, EndAt: start.Add(time.Hour), LinkKey: "k", LinkMasked: true}
	_, problems, err := router.UpdateGroupTargets(context.Background(), 7, existing, existing, []store.CalendarEvent{full, busy}, []Target{
		{CalendarID: googleCalendarID, Change: CopyKeep},
		{CalendarID: busyCalendarID, Masked: true, Title: "Busy", Change: CopyRemove},
	})
	if err != nil || len(problems) != 0 {
		t.Fatalf("update: %v %v", err, problems)
	}
	if len(google.deletes) != 1 || google.deletes[0].ID != busy.ID {
		t.Fatalf("deletes = %+v", google.deletes)
	}
	if len(google.updates) != 1 || google.updates[0].ID != full.ID {
		t.Fatalf("updates = %+v", google.updates)
	}
}

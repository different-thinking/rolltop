// File overview: Tests for travel time: the journey there and back kept as
// events of their own at the provider, created, moved and deleted with the
// event they belong to.

package calendarlink

import (
	"context"
	"errors"
	"testing"
	"time"

	"rolltop/backend/store"
)

func minutesPtr(value int) *int { return &value }

// A new event asking for travel is written first, under a key it did not have
// to ask a copy for, and the two blocks touch it in its own calendar.
func TestCreateWithTravelBlocksTheJourneyAroundTheEvent(t *testing.T) {
	router, google, _ := newRouter()
	start := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	created, problems, err := CreateWithTravel(context.Background(), router, 7, googleCalendarID, nil,
		TravelRequest{Before: minutesPtr(45), After: minutesPtr(30)},
		store.CalendarEvent{Summary: "Customer visit", Location: "Königstraße 12", StartAt: start, EndAt: start.Add(90 * time.Minute),
			Attendees: []store.CalendarAttendee{{Email: "guest@example.test"}}})
	if err != nil || len(problems) != 0 {
		t.Fatalf("create: %v %v", err, problems)
	}
	if len(google.creates) != 3 {
		t.Fatalf("creates = %d, want the event and two travel blocks", len(google.creates))
	}
	event, before, after := google.creates[0], google.creates[1], google.creates[2]
	if created.ID != event.ID || event.LinkKey == "" || !event.LinkPrimary || event.LinkTravel != "" {
		t.Fatalf("event = %+v, want the primary of a group of its own", event)
	}
	if before.LinkKey != event.LinkKey || before.LinkTravel != store.CalendarTravelBefore || before.CalendarID != googleCalendarID {
		t.Fatalf("before = %+v", before)
	}
	if !before.StartAt.Equal(start.Add(-45*time.Minute)) || !before.EndAt.Equal(start) {
		t.Fatalf("before runs %s-%s, want 45 minutes ending at the start", before.StartAt, before.EndAt)
	}
	if before.Location != "Königstraße 12" || before.Summary != "Travel to: Customer visit" {
		t.Fatalf("before = %q at %q", before.Summary, before.Location)
	}
	if !after.StartAt.Equal(start.Add(90*time.Minute)) || !after.EndAt.Equal(start.Add(120*time.Minute)) || after.LinkTravel != store.CalendarTravelAfter {
		t.Fatalf("after = %+v", after)
	}
	for _, block := range []store.CalendarEvent{before, after} {
		if len(block.Attendees) != 0 || block.OnlineMeeting || block.LinkPrimary || block.LinkMasked {
			t.Fatalf("travel block carries the event's guests or roles: %+v", block)
		}
	}
}

// Without travel nothing changes about how an event is created: no key is
// minted for it.
func TestCreateWithoutTravelMintsNoKey(t *testing.T) {
	router, google, _ := newRouter()
	start := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	if _, _, err := CreateWithTravel(context.Background(), router, 7, googleCalendarID, nil,
		TravelRequest{Before: minutesPtr(0)}, store.CalendarEvent{Summary: "x", StartAt: start, EndAt: start.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if len(google.creates) != 1 || google.creates[0].LinkKey != "" {
		t.Fatalf("creates = %+v", google.creates)
	}
}

// An all-day event has no instant to travel to.
func TestAllDayEventsKeepNoTravel(t *testing.T) {
	router, google, _ := newRouter()
	start := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	if _, _, err := CreateWithTravel(context.Background(), router, 7, googleCalendarID, nil,
		TravelRequest{Before: minutesPtr(30)}, store.CalendarEvent{Summary: "x", AllDay: true, StartAt: start, EndAt: start.AddDate(0, 0, 1)}); err != nil {
		t.Fatal(err)
	}
	if len(google.creates) != 1 {
		t.Fatalf("creates = %d, want only the event", len(google.creates))
	}
}

func travelFixture(start time.Time) (store.CalendarEvent, []store.CalendarEvent) {
	event := store.CalendarEvent{ID: 1, CalendarID: googleCalendarID, Summary: "Visit", StartAt: start, EndAt: start.Add(time.Hour),
		LinkKey: "k", LinkPrimary: true}
	before := TravelFor(event, store.CalendarTravelBefore, 30, googleCalendarID)
	before.ID = 2
	after := TravelFor(event, store.CalendarTravelAfter, 15, googleCalendarID)
	after.ID = 3
	return event, []store.CalendarEvent{before, after}
}

// Moving the event moves its journey, and an edit that does not mention
// travel -- a drag in the week -- keeps the minutes it had.
func TestUpdateMovesTheTravelWithTheEvent(t *testing.T) {
	router, google, _ := newRouter()
	start := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	existing, travel := travelFixture(start)
	edited := existing
	edited.StartAt, edited.EndAt = start.Add(2*time.Hour), start.Add(3*time.Hour)
	_, problems, err := UpdateGroupWithTravel(context.Background(), router, 7, existing, edited, nil, nil, travel, TravelRequest{})
	if err != nil || len(problems) != 0 {
		t.Fatalf("update: %v %v", err, problems)
	}
	if len(google.updates) != 3 || len(google.creates) != 0 || len(google.deletes) != 0 {
		t.Fatalf("updates=%d creates=%d deletes=%d", len(google.updates), len(google.creates), len(google.deletes))
	}
	before, after := google.updates[1], google.updates[2]
	if before.ID != 2 || !before.EndAt.Equal(edited.StartAt) || !before.StartAt.Equal(edited.StartAt.Add(-30*time.Minute)) {
		t.Fatalf("before = %+v", before)
	}
	if after.ID != 3 || !after.StartAt.Equal(edited.EndAt) || !after.EndAt.Equal(edited.EndAt.Add(15*time.Minute)) {
		t.Fatalf("after = %+v", after)
	}
	if before.LinkTravel != store.CalendarTravelBefore || before.LinkKey != "k" {
		t.Fatalf("before lost its role: %+v", before)
	}
}

// An edit that changes nothing about the journey writes nothing to it, and one
// that takes a side away deletes that block.
func TestUpdateChangesOnlyTheTravelThatChanged(t *testing.T) {
	router, google, _ := newRouter()
	start := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	existing, travel := travelFixture(start)
	edited := existing
	edited.Description = "agenda"
	_, problems, err := UpdateGroupWithTravel(context.Background(), router, 7, existing, edited, nil, nil, travel,
		TravelRequest{Before: minutesPtr(30), After: minutesPtr(0)})
	if err != nil || len(problems) != 0 {
		t.Fatalf("update: %v %v", err, problems)
	}
	if len(google.updates) != 1 || len(google.deletes) != 1 || google.deletes[0].ID != 3 {
		t.Fatalf("updates=%d deletes=%+v, want the event written and only the journey back removed", len(google.updates), google.deletes)
	}
}

// An event entered before it had travel gets a key in the same write that
// asks for the journey, so no block exists that its event does not know.
func TestUpdateAddsTravelToAnUnlinkedEvent(t *testing.T) {
	router, google, _ := newRouter()
	start := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	existing := store.CalendarEvent{ID: 1, CalendarID: googleCalendarID, Summary: "Visit", StartAt: start, EndAt: start.Add(time.Hour)}
	_, problems, err := UpdateGroupWithTravel(context.Background(), router, 7, existing, existing, nil, nil, nil,
		TravelRequest{Before: minutesPtr(20)})
	if err != nil || len(problems) != 0 {
		t.Fatalf("update: %v %v", err, problems)
	}
	if len(google.updates) != 1 || google.updates[0].LinkKey == "" || !google.updates[0].LinkPrimary {
		t.Fatalf("event write = %+v, want it to carry a new key", google.updates)
	}
	if len(google.creates) != 1 || google.creates[0].LinkKey != google.updates[0].LinkKey || google.creates[0].LinkTravel != store.CalendarTravelBefore {
		t.Fatalf("travel create = %+v", google.creates)
	}
}

// Moving a placeholder moves the journey and keeps its title: the stand-in
// title is not the appointment's.
func TestUpdateThroughAPlaceholderOnlyMovesTheTravel(t *testing.T) {
	router, google, _ := newRouter()
	start := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	_, travel := travelFixture(start)
	placeholder := store.CalendarEvent{ID: 9, CalendarID: googleCalendarID, Summary: "Busy", StartAt: start, EndAt: start.Add(time.Hour),
		LinkKey: "k", LinkMasked: true}
	edited := placeholder
	edited.StartAt, edited.EndAt = start.Add(time.Hour), start.Add(2*time.Hour)
	_, _, err := UpdateGroupWithTravel(context.Background(), router, 7, placeholder, edited, nil, nil, travel,
		TravelRequest{Before: minutesPtr(0), After: minutesPtr(0)})
	if err != nil {
		t.Fatal(err)
	}
	if len(google.deletes) != 0 || len(google.updates) != 3 {
		t.Fatalf("updates=%d deletes=%d, want the journey moved and kept", len(google.updates), len(google.deletes))
	}
	if before := google.updates[1]; before.Summary != "Travel to: Visit" || !before.EndAt.Equal(edited.StartAt) {
		t.Fatalf("before = %+v", before)
	}
}

// Deleting the event takes the journey with it; a block already gone at the
// provider is the end state, one the provider refuses is reported.
func TestDeleteTakesTheTravelWithIt(t *testing.T) {
	router, google, _ := newRouter()
	start := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	event, travel := travelFixture(start)
	problems, err := DeleteGroupWithTravel(context.Background(), router, 7, event, nil, travel)
	if err != nil || len(problems) != 0 {
		t.Fatalf("delete: %v %v", err, problems)
	}
	if len(google.deletes) != 3 {
		t.Fatalf("deletes = %d, want the event and both blocks", len(google.deletes))
	}
}

func TestRefusedTravelIsReportedBesideTheEvent(t *testing.T) {
	router, google, microsoft := newRouter()
	start := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	event, travel := travelFixture(start)
	for i := range travel {
		travel[i].CalendarID = microsoftCalendarID
	}
	microsoft.failOn = map[int64]error{microsoftCalendarID: ErrReadOnlyCalendar}
	problems, err := DeleteGroupWithTravel(context.Background(), router, 7, event, nil, travel)
	if err != nil {
		t.Fatal(err)
	}
	if len(google.deletes) != 1 || len(problems) != 2 || problems[0].Travel != store.CalendarTravelBefore ||
		!errors.Is(problems[0].Err, ErrReadOnlyCalendar) {
		t.Fatalf("problems = %+v", problems)
	}
}

func TestTravelMinutesReadsTheBlocks(t *testing.T) {
	start := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	event, travel := travelFixture(start)
	before, after := TravelMinutes(event, travel)
	if before != 30 || after != 15 {
		t.Fatalf("minutes = %d/%d, want 30/15", before, after)
	}
}

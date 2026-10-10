// File overview: Tests for the provider-neutral pair logic and the router that
// sends each half of a pair to its own provider.

package calendarlink

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"rolltop/backend/store"
)

// fakeBackend records what it was asked to write and answers as a provider
// would: with the event as stored, under an id of its own.
type fakeBackend struct {
	name    string
	nextID  int64
	creates []store.CalendarEvent
	updates []store.CalendarEvent
	deletes []store.CalendarEvent
	failOn  map[int64]error
	synced  []int64
}

func (f *fakeBackend) CreateRemoteEvent(_ context.Context, _ int64, calendarID int64, event store.CalendarEvent) (store.CalendarEvent, error) {
	if err := f.failOn[calendarID]; err != nil {
		return store.CalendarEvent{}, err
	}
	f.nextID++
	event.ID = f.nextID
	event.CalendarID = calendarID
	event.ExternalID = fmt.Sprintf("%s-%d", f.name, f.nextID)
	f.creates = append(f.creates, event)
	return event, nil
}

func (f *fakeBackend) UpdateRemoteEvent(_ context.Context, _ int64, existing, edited store.CalendarEvent) (store.CalendarEvent, error) {
	if err := f.failOn[existing.CalendarID]; err != nil {
		return store.CalendarEvent{}, err
	}
	edited.ID = existing.ID
	edited.CalendarID = existing.CalendarID
	f.updates = append(f.updates, edited)
	return edited, nil
}

func (f *fakeBackend) DeleteRemoteEvent(_ context.Context, _ int64, event store.CalendarEvent) error {
	if err := f.failOn[event.CalendarID]; err != nil {
		return err
	}
	f.deletes = append(f.deletes, event)
	return nil
}

func (f *fakeBackend) RespondToRemoteEvent(_ context.Context, _ int64, event store.CalendarEvent, response string) (store.CalendarEvent, error) {
	event.MyResponse = response
	return event, nil
}

func (f *fakeBackend) SyncCalendar(_ context.Context, _ int64, calendarID int64) error {
	f.synced = append(f.synced, calendarID)
	return nil
}

type fakeCalendars map[int64]store.Calendar

func (f fakeCalendars) Calendar(_ context.Context, _ int64, calendarID int64) (store.Calendar, error) {
	calendar, ok := f[calendarID]
	if !ok {
		return store.Calendar{}, store.ErrNotFound
	}
	return calendar, nil
}

const (
	googleCalendarID    = 1
	microsoftCalendarID = 2
)

func newRouter() (*Router, *fakeBackend, *fakeBackend) {
	google := &fakeBackend{name: "google", nextID: 100}
	microsoft := &fakeBackend{name: "microsoft", nextID: 200}
	return &Router{
		Calendars: fakeCalendars{
			googleCalendarID:    {ID: googleCalendarID, Provider: store.CalendarProviderGoogle},
			microsoftCalendarID: {ID: microsoftCalendarID, Provider: store.CalendarProviderMicrosoft},
		},
		Google:    google,
		Microsoft: microsoft,
	}, google, microsoft
}

// An event entered in Microsoft 365 with a copy in a Google calendar is
// written to each provider once, under one key, and the copy is neither a
// meeting of its own nor carries the guests.
func TestCreateWithCopyAcrossProviders(t *testing.T) {
	router, google, microsoft := newRouter()
	start := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	created, problems, err := router.CreateWithCopy(context.Background(), 7, microsoftCalendarID, googleCalendarID, store.CalendarEvent{
		Summary: "Review", StartAt: start, EndAt: start.Add(time.Hour),
		Attendees:     []store.CalendarAttendee{{Email: "guest@example.test"}},
		OnlineMeeting: true,
	})
	if err != nil || len(problems) != 0 {
		t.Fatalf("create: %v %v", err, problems)
	}
	if len(microsoft.creates) != 1 || len(google.creates) != 1 {
		t.Fatalf("creates: microsoft=%d google=%d", len(microsoft.creates), len(google.creates))
	}
	primary, copied := microsoft.creates[0], google.creates[0]
	if !primary.LinkPrimary || primary.LinkKey == "" || !primary.OnlineMeeting || len(primary.Attendees) != 1 {
		t.Fatalf("primary = %+v", primary)
	}
	if copied.LinkPrimary || copied.LinkKey != primary.LinkKey {
		t.Fatalf("copy link = %q primary=%v, want %q copy", copied.LinkKey, copied.LinkPrimary, primary.LinkKey)
	}
	if copied.OnlineMeeting || len(copied.Attendees) != 0 {
		t.Fatalf("copy carried the meeting or its guests: %+v", copied)
	}
	if created.ID != primary.ID {
		t.Fatalf("returned %d, want the primary %d", created.ID, primary.ID)
	}
}

// A copy the other provider refuses leaves the event in place and is reported
// beside it.
func TestCreateWithCopyReportsRefusedCopy(t *testing.T) {
	router, google, microsoft := newRouter()
	google.failOn = map[int64]error{googleCalendarID: ErrReadOnlyCalendar}
	created, problems, err := router.CreateWithCopy(context.Background(), 7, microsoftCalendarID, googleCalendarID, store.CalendarEvent{Summary: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == 0 || len(microsoft.creates) != 1 {
		t.Fatalf("event was not created: %+v", created)
	}
	if len(problems) != 1 || problems[0].Action != LinkedCreate || !errors.Is(problems[0].Err, ErrReadOnlyCalendar) {
		t.Fatalf("problems = %+v", problems)
	}
}

// An edit carried to a copy in the other provider keeps the copy's own guest
// list and online meeting: the copy is never turned into a second meeting.
func TestUpdateGroupKeepsTheCopysOwnMeeting(t *testing.T) {
	router, google, microsoft := newRouter()
	start := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	existing := store.CalendarEvent{ID: 1, CalendarID: microsoftCalendarID, Summary: "Old", StartAt: start, EndAt: start.Add(time.Hour),
		LinkKey: "k", LinkPrimary: true, OnlineMeeting: true, OnlineMeetingURL: "https://teams.example/join",
		Attendees: []store.CalendarAttendee{{Email: "guest@example.test"}}}
	copied := store.CalendarEvent{ID: 2, CalendarID: googleCalendarID, Summary: "Old", StartAt: start, EndAt: start.Add(time.Hour), LinkKey: "k"}
	edited := existing
	edited.Summary = "New"
	updated, problems, err := router.UpdateGroup(context.Background(), 7, existing, edited, []store.CalendarEvent{copied}, googleCalendarID, CopyKeep)
	if err != nil || len(problems) != 0 {
		t.Fatalf("update: %v %v", err, problems)
	}
	if updated.Summary != "New" || len(microsoft.updates) != 1 || len(google.updates) != 1 {
		t.Fatalf("updates: microsoft=%d google=%d", len(microsoft.updates), len(google.updates))
	}
	follow := google.updates[0]
	if follow.Summary != "New" || follow.OnlineMeeting || follow.OnlineMeetingURL != "" || len(follow.Attendees) != 0 || follow.LinkPrimary {
		t.Fatalf("copy follow = %+v", follow)
	}
}

// Editing through the copy never asks for a meeting or a guest list on it.
func TestUpdateGroupThroughTheCopy(t *testing.T) {
	router, google, _ := newRouter()
	primary := store.CalendarEvent{ID: 1, CalendarID: microsoftCalendarID, LinkKey: "k", LinkPrimary: true, OnlineMeeting: true}
	existing := store.CalendarEvent{ID: 2, CalendarID: googleCalendarID, LinkKey: "k"}
	edited := existing
	edited.Summary = "Renamed"
	edited.OnlineMeeting = true
	edited.Attendees = []store.CalendarAttendee{{Email: "intruder@example.test"}}
	if _, _, err := router.UpdateGroup(context.Background(), 7, existing, edited, []store.CalendarEvent{primary}, 0, CopyKeep); err != nil {
		t.Fatal(err)
	}
	written := google.updates[0]
	if written.OnlineMeeting || len(written.Attendees) != 0 {
		t.Fatalf("copy edit = %+v", written)
	}
}

func TestDeleteGroupAcrossProviders(t *testing.T) {
	router, google, microsoft := newRouter()
	google.failOn = map[int64]error{googleCalendarID: ErrRemoteDeleted}
	event := store.CalendarEvent{ID: 1, CalendarID: microsoftCalendarID, LinkKey: "k", LinkPrimary: true}
	copied := store.CalendarEvent{ID: 2, CalendarID: googleCalendarID, LinkKey: "k"}
	problems, err := router.DeleteGroup(context.Background(), 7, event, []store.CalendarEvent{copied})
	if err != nil || len(problems) != 0 {
		t.Fatalf("delete: %v %v (a copy already gone is the end state)", err, problems)
	}
	if len(microsoft.deletes) != 1 {
		t.Fatalf("microsoft deletes = %d", len(microsoft.deletes))
	}
}

func TestRouterRefusesAnUnconfiguredProvider(t *testing.T) {
	router, _, _ := newRouter()
	router.Microsoft = nil
	if _, err := router.CreateRemoteEvent(context.Background(), 7, microsoftCalendarID, store.CalendarEvent{}); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("create in an unconfigured provider: %v", err)
	}
	if err := router.SyncCalendar(context.Background(), 7, googleCalendarID); err != nil {
		t.Fatalf("google sync: %v", err)
	}
	if _, err := router.CreateRemoteEvent(context.Background(), 7, 99, store.CalendarEvent{}); !store.IsNotFound(err) {
		t.Fatalf("unknown calendar: %v", err)
	}
}

func TestProviderSentinelsUnwrapToTheSharedOutcome(t *testing.T) {
	own := Sentinel("this event was changed in Example", ErrRemoteChanged)
	wrapped := fmt.Errorf("write: %w", own)
	if !errors.Is(wrapped, own) || !errors.Is(wrapped, ErrRemoteChanged) || errors.Is(wrapped, ErrRemoteDeleted) {
		t.Fatal("a provider sentinel must match itself and its shared outcome only")
	}
	if own.Error() != "this event was changed in Example" {
		t.Fatalf("message = %q", own.Error())
	}
}

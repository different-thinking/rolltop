// File overview: Write-back against the same fake Calendar API the sync tests
// use. What matters here is the order -- Google first, the local row second --
// and what happens when Google refuses.

package googlecalendar

import (
	"context"
	"errors"
	"testing"
	"time"

	"rolltop/backend/store"
)

// syncedCalendar runs one sync so the fixture holds a stored calendar to write
// into, and returns it.
func syncedCalendar(t *testing.T, syncer *Syncer, db *store.Store, userID int64) store.Calendar {
	t.Helper()
	if _, err := syncer.SyncConnection(context.Background(), userID, fixtureConnectionID); err != nil {
		t.Fatal(err)
	}
	return onlyCalendar(t, db, userID)
}

func writeFixture(t *testing.T, fake *fakeCalendar) (*Syncer, *store.Store, store.User, store.Calendar) {
	t.Helper()
	if fake.calendarPages == nil {
		fake.calendarPages = []CalendarListPage{{
			Items: []CalendarListEntry{ownedCalendar("primary", "Work")}, NextSyncToken: "calendars-1",
		}}
	}
	if fake.eventPages == nil {
		fake.eventPages = []EventsPage{{NextSyncToken: "events-1"}}
	}
	syncer, db, user := newSyncFixture(t, fake)
	return syncer, db, user, syncedCalendar(t, syncer, db, user.ID)
}

// The stored row is built from Google's answer, not from what was submitted, so
// the id and etag Rolltop holds are the ones Google actually assigned.
func TestCreateRemoteEventStoresGooglesAnswer(t *testing.T) {
	fake := &fakeCalendar{}
	syncer, db, user, calendar := writeFixture(t, fake)
	start := fixedNow.Add(48 * time.Hour)

	created, err := syncer.CreateRemoteEvent(context.Background(), user.ID, calendar.ID, store.CalendarEvent{
		Summary: "Planning", Location: "Room 2",
		StartAt: start, EndAt: start.Add(time.Hour), TimeZone: "Europe/Berlin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.ExternalID != "created-1" || created.ETag != "etag-created" {
		t.Fatalf("event = %+v, want the identifiers Google assigned", created)
	}
	if created.Summary != "Planning" || created.Location != "Room 2" {
		t.Fatalf("event = %+v, want the submitted fields", created)
	}
	if len(fake.created) != 1 || fake.created[0].Start.DateTime == "" {
		t.Fatalf("create payload = %+v, want a timed start", fake.created)
	}
	// An event with nobody invited is a private note; mailing about it is noise.
	if len(fake.sendUpdates) != 1 || fake.sendUpdates[0] != "none" {
		t.Fatalf("sendUpdates = %v, want no notifications for an event without guests", fake.sendUpdates)
	}
	stored, err := db.CalendarEvent(context.Background(), user.ID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ExternalID != "created-1" {
		t.Fatalf("stored event = %+v, want the mirror written", stored)
	}
}

// An event with guests is a meeting: changing it silently leaves everybody
// holding an invitation that no longer matches.
func TestCreateRemoteEventNotifiesGuests(t *testing.T) {
	fake := &fakeCalendar{}
	syncer, _, user, calendar := writeFixture(t, fake)
	start := fixedNow.Add(48 * time.Hour)

	if _, err := syncer.CreateRemoteEvent(context.Background(), user.ID, calendar.ID, store.CalendarEvent{
		Summary: "Review", StartAt: start, EndAt: start.Add(time.Hour),
		Attendees: []store.CalendarAttendee{{Email: "guest@example.test", Name: "Guest"}},
	}); err != nil {
		t.Fatal(err)
	}
	if len(fake.sendUpdates) != 1 || fake.sendUpdates[0] != "all" {
		t.Fatalf("sendUpdates = %v, want the guests notified", fake.sendUpdates)
	}
	if len(fake.created) != 1 || len(fake.created[0].Attendees) != 1 {
		t.Fatalf("create payload = %+v, want the guest list forwarded", fake.created)
	}
	// The answer belongs to the invitee; sending one would answer for them.
	if fake.created[0].Attendees[0].ResponseStatus != "" {
		t.Fatalf("attendee = %+v, want no response status invented", fake.created[0].Attendees[0])
	}
}

// Google is the leading system: an edit rejected on a stale etag is discarded
// and its calendar's current version is adopted, so the user is never left
// holding a copy that exists nowhere.
func TestUpdateRemoteEventAdoptsGooglesVersionOnConflict(t *testing.T) {
	start := fixedNow.Add(24 * time.Hour)
	fake := &fakeCalendar{
		eventPages: []EventsPage{{
			Items: []Event{timedEvent("e1", "etag-1", "Standup", start)}, NextSyncToken: "events-1",
		}},
	}
	syncer, db, user, calendar := writeFixture(t, fake)
	ctx := context.Background()

	events := eventsOf(t, db, user.ID, calendar)
	if len(events) != 1 {
		t.Fatalf("events = %+v, want the synced occurrence", events)
	}
	existing := events[0]

	// Somebody else moved it at Google in the meantime.
	fake.mu.Lock()
	fake.events["e1"] = timedEvent("e1", "etag-remote", "Standup, moved by the organizer", start.Add(time.Hour))
	fake.patchConflict = true
	fake.mu.Unlock()

	edited := existing
	edited.Summary = "My local rename"
	adopted, err := syncer.UpdateRemoteEvent(ctx, user.ID, existing, edited)
	if !errors.Is(err, ErrRemoteChanged) {
		t.Fatalf("update on a stale etag: err=%v, want ErrRemoteChanged", err)
	}
	if adopted.Summary != "Standup, moved by the organizer" || adopted.ETag != "etag-remote" {
		t.Fatalf("adopted event = %+v, want Google's version", adopted)
	}
	stored, err := db.CalendarEvent(ctx, user.ID, existing.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Summary != "Standup, moved by the organizer" {
		t.Fatalf("stored event = %+v, want the local row replaced by Google's version", stored)
	}
}

// An event deleted at Google while it was being edited here has no version to
// show. The mirror goes with it rather than being resurrected by the next write.
func TestUpdateRemoteEventReportsARemoteDeletion(t *testing.T) {
	start := fixedNow.Add(24 * time.Hour)
	fake := &fakeCalendar{
		eventPages: []EventsPage{{
			Items: []Event{timedEvent("e1", "etag-1", "Standup", start)}, NextSyncToken: "events-1",
		}},
	}
	syncer, db, user, calendar := writeFixture(t, fake)
	ctx := context.Background()
	existing := eventsOf(t, db, user.ID, calendar)[0]

	fake.mu.Lock()
	delete(fake.events, "e1")
	fake.patchConflict = true
	fake.mu.Unlock()

	edited := existing
	edited.Summary = "Renamed"
	if _, err := syncer.UpdateRemoteEvent(ctx, user.ID, existing, edited); !errors.Is(err, ErrRemoteDeleted) {
		t.Fatalf("update of a deleted event: err=%v, want ErrRemoteDeleted", err)
	}
	if _, err := db.CalendarEvent(ctx, user.ID, existing.ID); !store.IsNotFound(err) {
		t.Fatalf("local mirror survived a remote deletion: err=%v", err)
	}
}

// The etag has to travel as a precondition. Without it a concurrent change at
// Google is overwritten silently instead of being reported.
func TestUpdateRemoteEventSendsThePrecondition(t *testing.T) {
	start := fixedNow.Add(24 * time.Hour)
	fake := &fakeCalendar{
		eventPages: []EventsPage{{
			Items: []Event{timedEvent("e1", "etag-1", "Standup", start)}, NextSyncToken: "events-1",
		}},
	}
	syncer, db, user, calendar := writeFixture(t, fake)
	ctx := context.Background()
	existing := eventsOf(t, db, user.ID, calendar)[0]

	edited := existing
	edited.Summary = "Renamed"
	updated, err := syncer.UpdateRemoteEvent(ctx, user.ID, existing, edited)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Summary != "Renamed" || updated.ETag != "etag-patched" {
		t.Fatalf("updated event = %+v, want Google's accepted version", updated)
	}
	if len(fake.ifMatch) != 1 || fake.ifMatch[0] != "etag-1" {
		t.Fatalf("If-Match = %v, want the stored etag", fake.ifMatch)
	}
}

// A delete that only succeeded locally comes back on the next sync, so nothing
// may remove the mirror before Google has accepted the deletion -- including a
// calendar that would have refused the write in the first place.
func TestDeleteRemoteEventRefusesAReadOnlyCalendar(t *testing.T) {
	start := fixedNow.Add(24 * time.Hour)
	fake := &fakeCalendar{
		eventPages: []EventsPage{{
			Items: []Event{timedEvent("e1", "etag-1", "Standup", start)}, NextSyncToken: "events-1",
		}},
	}
	syncer, db, user, calendar := writeFixture(t, fake)
	ctx := context.Background()
	existing := eventsOf(t, db, user.ID, calendar)[0]

	// A calendar the account may only read refuses the write before it is sent.
	if err := readOnly(ctx, db, user.ID, calendar.ID); err != nil {
		t.Fatal(err)
	}
	if err := syncer.DeleteRemoteEvent(ctx, user.ID, existing); !errors.Is(err, ErrReadOnlyCalendar) {
		t.Fatalf("delete in a read-only calendar: err=%v, want ErrReadOnlyCalendar", err)
	}
	if _, err := db.CalendarEvent(ctx, user.ID, existing.ID); err != nil {
		t.Fatalf("mirror was removed even though Google was never asked: %v", err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("delete reached Google anyway: %v", fake.deleted)
	}
}

// The successful path removes both copies, Google first.
func TestDeleteRemoteEventRemovesBothCopies(t *testing.T) {
	start := fixedNow.Add(24 * time.Hour)
	fake := &fakeCalendar{
		eventPages: []EventsPage{{
			Items: []Event{timedEvent("e1", "etag-1", "Standup", start)}, NextSyncToken: "events-1",
		}},
	}
	syncer, db, user, calendar := writeFixture(t, fake)
	ctx := context.Background()
	existing := eventsOf(t, db, user.ID, calendar)[0]

	if err := syncer.DeleteRemoteEvent(ctx, user.ID, existing); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != "e1" {
		t.Fatalf("deleted at Google = %v, want the event", fake.deleted)
	}
	if _, err := db.CalendarEvent(ctx, user.ID, existing.ID); !store.IsNotFound(err) {
		t.Fatalf("local mirror survived the delete: err=%v", err)
	}
}

// An answer carries the whole guest list because Google has no merge semantics
// for it, and the list is read back first so nobody invited since the last poll
// is dropped.
func TestRespondToRemoteEventKeepsTheWholeGuestList(t *testing.T) {
	start := fixedNow.Add(24 * time.Hour)
	invitation := timedEvent("e1", "etag-1", "Review", start)
	invitation.Attendees = []EventAttendee{
		{Email: "owner@gmail.example.test", Self: true, ResponseStatus: store.CalendarResponseNeedsAction},
		{Email: "organizer@example.test", Organizer: true, ResponseStatus: store.CalendarResponseAccepted},
	}
	fake := &fakeCalendar{
		eventPages: []EventsPage{{Items: []Event{invitation}, NextSyncToken: "events-1"}},
	}
	syncer, db, user, calendar := writeFixture(t, fake)
	ctx := context.Background()

	// Somebody was invited after the last poll; the answer must not drop them.
	fake.mu.Lock()
	current := invitation
	current.Attendees = append(append([]EventAttendee{}, invitation.Attendees...),
		EventAttendee{Email: "latecomer@example.test", ResponseStatus: store.CalendarResponseNeedsAction})
	fake.events["e1"] = current
	fake.mu.Unlock()

	existing := eventsOf(t, db, user.ID, calendar)[0]
	if existing.MyResponse != store.CalendarResponseNeedsAction {
		t.Fatalf("event = %+v, want the account's own answer lifted out of the guest list", existing)
	}

	answered, err := syncer.RespondToRemoteEvent(ctx, user.ID, existing, store.CalendarResponseAccepted)
	if err != nil {
		t.Fatal(err)
	}
	if answered.MyResponse != store.CalendarResponseAccepted {
		t.Fatalf("event = %+v, want the accepted answer", answered)
	}
	if len(fake.patched) != 1 {
		t.Fatalf("patches = %+v, want exactly one", fake.patched)
	}
	sent, _ := fake.patched[0]["attendees"].([]any)
	if len(sent) != 3 {
		t.Fatalf("patched attendees = %+v, want the whole current guest list", sent)
	}
	if len(fake.sendUpdates) != 1 || fake.sendUpdates[0] != "all" {
		t.Fatalf("sendUpdates = %v, want the organizer told about the answer", fake.sendUpdates)
	}
}

// Answering is allowed on a read-only calendar: an invitee's own response is
// the one thing Google lets them change on somebody else's event.
func TestRespondToRemoteEventWorksOnAReadOnlyCalendar(t *testing.T) {
	start := fixedNow.Add(24 * time.Hour)
	invitation := timedEvent("e1", "etag-1", "Review", start)
	invitation.Attendees = []EventAttendee{
		{Email: "owner@gmail.example.test", Self: true, ResponseStatus: store.CalendarResponseNeedsAction},
	}
	fake := &fakeCalendar{
		eventPages: []EventsPage{{Items: []Event{invitation}, NextSyncToken: "events-1"}},
	}
	syncer, db, user, calendar := writeFixture(t, fake)
	ctx := context.Background()
	fake.mu.Lock()
	fake.events["e1"] = invitation
	fake.mu.Unlock()
	existing := eventsOf(t, db, user.ID, calendar)[0]
	if err := readOnly(ctx, db, user.ID, calendar.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := syncer.RespondToRemoteEvent(ctx, user.ID, existing, store.CalendarResponseDeclined); err != nil {
		t.Fatalf("answering an invitation in a read-only calendar: %v", err)
	}
}

// An event the account was never invited to has no answer to give, and pushing
// one would add the account to somebody else's guest list.
func TestRespondToRemoteEventRefusesANonInvitation(t *testing.T) {
	start := fixedNow.Add(24 * time.Hour)
	fake := &fakeCalendar{
		eventPages: []EventsPage{{
			Items: []Event{timedEvent("e1", "etag-1", "Solo work", start)}, NextSyncToken: "events-1",
		}},
	}
	syncer, db, user, calendar := writeFixture(t, fake)
	ctx := context.Background()
	fake.mu.Lock()
	fake.events["e1"] = timedEvent("e1", "etag-1", "Solo work", start)
	fake.mu.Unlock()
	existing := eventsOf(t, db, user.ID, calendar)[0]

	if _, err := syncer.RespondToRemoteEvent(ctx, user.ID, existing, store.CalendarResponseAccepted); !errors.Is(err, ErrNotAnInvitation) {
		t.Fatalf("answering an event without a guest list: err=%v, want ErrNotAnInvitation", err)
	}
	if len(fake.patched) != 0 {
		t.Fatalf("a patch was sent anyway: %+v", fake.patched)
	}
}

// readOnly downgrades a stored calendar to a shared one the account may only
// read, which is the state Google reports for a subscribed feed.
func readOnly(ctx context.Context, db *store.Store, userID, calendarID int64) error {
	calendar, err := db.Calendar(ctx, userID, calendarID)
	if err != nil {
		return err
	}
	_, err = db.UpsertCalendar(ctx, userID, store.CalendarUpsert{
		GoogleConnectionID: calendar.GoogleConnectionID,
		GoogleCalendarID:   calendar.GoogleCalendarID,
		Summary:            calendar.Summary,
		AccessRole:         "reader",
	})
	return err
}

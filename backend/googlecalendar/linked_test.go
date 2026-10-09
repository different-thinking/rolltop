// File overview: One event kept in two calendars, against the same fake
// Calendar API the sync and write-back tests use. What matters is that Google
// holds the link on both copies, that only the entered copy carries guests, and
// that an edit or a delete reaches every copy.

package googlecalendar

import (
	"context"
	"errors"
	"testing"
	"time"

	"rolltop/backend/store"
)

// twoCalendarFixture syncs an account with a work and a family calendar and
// returns both, the family one being the reader's second calendar.
func twoCalendarFixture(t *testing.T, fake *fakeCalendar) (*Syncer, *store.Store, store.User, store.Calendar, store.Calendar) {
	t.Helper()
	fake.calendarPages = []CalendarListPage{{
		Items: []CalendarListEntry{
			ownedCalendar("primary", "Work"),
			ownedCalendar("family@group.calendar.google.com", "Family"),
		},
		NextSyncToken: "calendars-1",
	}}
	if fake.eventPages == nil {
		fake.eventPages = []EventsPage{{NextSyncToken: "events-1"}}
	}
	syncer, db, user := newSyncFixture(t, fake)
	ctx := context.Background()
	if _, err := syncer.SyncConnection(ctx, user.ID, fixtureConnectionID); err != nil {
		t.Fatal(err)
	}
	work, err := db.CalendarByGoogleID(ctx, user.ID, fixtureConnectionID, "primary")
	if err != nil {
		t.Fatal(err)
	}
	family, err := db.CalendarByGoogleID(ctx, user.ID, fixtureConnectionID, "family@group.calendar.google.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetCalendarCopyTarget(ctx, user.ID, family.ID, true); err != nil {
		t.Fatal(err)
	}
	return syncer, db, user, work, family
}

func createLinkedMeeting(t *testing.T, syncer *Syncer, userID int64, work, family store.Calendar) store.CalendarEvent {
	t.Helper()
	start := fixedNow.Add(48 * time.Hour)
	created, problems, err := syncer.CreateRemoteEventWithCopy(context.Background(), userID, work.ID, family.ID, store.CalendarEvent{
		Summary: "Parents' evening", Location: "School",
		StartAt: start, EndAt: start.Add(time.Hour), TimeZone: "Europe/Berlin",
		Attendees: []store.CalendarAttendee{{Email: "partner@example.test", Name: "Partner"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Fatalf("problems = %+v, want none", problems)
	}
	return created
}

// Both copies carry the same key at Google, so the link survives anything the
// local mirror goes through. Only the entered copy invites anybody: a guest
// list on the copy would send every guest a second invitation.
func TestCreateRemoteEventWithCopyLinksBothAtGoogle(t *testing.T) {
	fake := &fakeCalendar{}
	syncer, db, user, work, family := twoCalendarFixture(t, fake)
	ctx := context.Background()
	created := createLinkedMeeting(t, syncer, user.ID, work, family)

	if len(fake.created) != 2 {
		t.Fatalf("creates = %d, want the event and its copy", len(fake.created))
	}
	if fake.createdIn[0] != "primary" || fake.createdIn[1] != "family@group.calendar.google.com" {
		t.Fatalf("created in %v, want the chosen calendar first, then the second calendar", fake.createdIn)
	}
	primary, copied := fake.created[0], fake.created[1]
	if primary.ExtendedProperties == nil || copied.ExtendedProperties == nil {
		t.Fatalf("extended properties = %+v / %+v, want the link on both", primary.ExtendedProperties, copied.ExtendedProperties)
	}
	key := primary.ExtendedProperties.Private[linkKeyProperty]
	if key == "" || copied.ExtendedProperties.Private[linkKeyProperty] != key {
		t.Fatalf("link keys = %q / %q, want one shared key", key, copied.ExtendedProperties.Private[linkKeyProperty])
	}
	if primary.ExtendedProperties.Private[linkRoleProperty] != linkRolePrimary ||
		copied.ExtendedProperties.Private[linkRoleProperty] != linkRoleCopy {
		t.Fatalf("roles = %v / %v, want primary then copy", primary.ExtendedProperties.Private, copied.ExtendedProperties.Private)
	}
	if primary.Attendees == nil || len(*primary.Attendees) != 1 {
		t.Fatalf("primary guests = %+v, want the guest invited", primary.Attendees)
	}
	if copied.Attendees != nil && len(*copied.Attendees) != 0 {
		t.Fatalf("copy guests = %+v, want none", *copied.Attendees)
	}
	if fake.sendUpdates[0] != "all" || fake.sendUpdates[1] != "none" {
		t.Fatalf("sendUpdates = %v, want only the primary to notify", fake.sendUpdates)
	}
	if copied.Summary != "Parents' evening" || copied.Location != "School" || copied.Start != primary.Start {
		t.Fatalf("copy = %+v, want the same appointment", copied)
	}

	if !created.Linked() || !created.LinkPrimary || created.LinkKey != key {
		t.Fatalf("stored primary = %+v, want it linked as the primary", created)
	}
	copies, err := db.ListCalendarEventCopies(ctx, user.ID, created)
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) != 1 || copies[0].CalendarID != family.ID || copies[0].LinkPrimary || len(copies[0].Attendees) != 0 {
		t.Fatalf("copies = %+v, want the guestless copy in the second calendar", copies)
	}
}

// The appointment is what the reader asked for. A copy the second calendar
// refuses leaves the event in place and is reported on its own.
func TestCreateRemoteEventWithCopyKeepsTheEventWhenTheCopyFails(t *testing.T) {
	fake := &fakeCalendar{}
	syncer, db, user, work, family := twoCalendarFixture(t, fake)
	ctx := context.Background()
	if err := readOnly(ctx, db, user.ID, family.ID); err != nil {
		t.Fatal(err)
	}
	start := fixedNow.Add(48 * time.Hour)
	created, problems, err := syncer.CreateRemoteEventWithCopy(ctx, user.ID, work.ID, family.ID, store.CalendarEvent{
		Summary: "Dentist", StartAt: start, EndAt: start.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == 0 {
		t.Fatal("the event itself was not stored")
	}
	if len(problems) != 1 || problems[0].CalendarID != family.ID || problems[0].Action != LinkedCreate ||
		!errors.Is(problems[0].Err, ErrReadOnlyCalendar) {
		t.Fatalf("problems = %+v, want the refused copy reported", problems)
	}
	if len(fake.created) != 1 {
		t.Fatalf("creates = %d, want only the event to reach Google", len(fake.created))
	}
}

// An edit reaches the copy too, and the copy keeps its own (empty) guest list:
// the edit must neither invite the guests a second time from the copy nor, when
// it is made through the copy, wipe the primary's list.
func TestUpdateRemoteEventGroupCarriesTheEditToTheCopy(t *testing.T) {
	fake := &fakeCalendar{}
	syncer, db, user, work, family := twoCalendarFixture(t, fake)
	ctx := context.Background()
	primary := createLinkedMeeting(t, syncer, user.ID, work, family)
	copies, err := db.ListCalendarEventCopies(ctx, user.ID, primary)
	if err != nil {
		t.Fatal(err)
	}

	// Edited through the copy, which is what the week view draws when the work
	// calendar is switched off.
	copied := copies[0]
	others, err := db.ListCalendarEventCopies(ctx, user.ID, copied)
	if err != nil {
		t.Fatal(err)
	}
	edited := copied
	edited.Summary = "Parents' evening (moved)"
	edited.StartAt = copied.StartAt.Add(time.Hour)
	edited.EndAt = copied.EndAt.Add(time.Hour)
	updated, problems, err := syncer.UpdateRemoteEventGroup(ctx, user.ID, copied, edited, others, family.ID, CopyKeep)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Fatalf("problems = %+v, want none", problems)
	}
	if updated.Summary != "Parents' evening (moved)" {
		t.Fatalf("updated = %+v, want the edit applied", updated)
	}
	if len(fake.patched) != 2 {
		t.Fatalf("patches = %d, want the copy and the primary", len(fake.patched))
	}
	for i, patch := range fake.patched {
		if _, carries := patch["attendees"]; carries {
			t.Fatalf("patch %d = %+v, want no guest list sent", i, patch)
		}
	}
	stored, err := db.CalendarEvent(ctx, user.ID, primary.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Summary != "Parents' evening (moved)" || !stored.StartAt.Equal(edited.StartAt) {
		t.Fatalf("primary = %+v, want the edit carried over", stored)
	}
	if len(stored.Attendees) != 1 || !stored.LinkPrimary {
		t.Fatalf("primary = %+v, want its guests and its role kept", stored)
	}
	// Still one pair at the new time.
	regrouped, err := db.ListCalendarEventCopies(ctx, user.ID, stored)
	if err != nil {
		t.Fatal(err)
	}
	if len(regrouped) != 1 || regrouped[0].ID != copied.ID {
		t.Fatalf("copies after the edit = %+v, want the pair intact", regrouped)
	}
}

// Ticking the box on an existing event links it in the same patch as the edit
// and creates the copy; unticking it deletes the copy and nothing else.
func TestUpdateRemoteEventGroupAddsAndRemovesTheCopy(t *testing.T) {
	start := fixedNow.Add(24 * time.Hour)
	fake := &fakeCalendar{}
	syncer, db, user, work, family := twoCalendarFixture(t, fake)
	ctx := context.Background()
	plain, err := syncer.CreateRemoteEvent(ctx, user.ID, work.ID, store.CalendarEvent{
		Summary: "Swimming", StartAt: start, EndAt: start.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if plain.Linked() {
		t.Fatalf("plain event = %+v, want no link", plain)
	}

	updated, problems, err := syncer.UpdateRemoteEventGroup(ctx, user.ID, plain, plain, nil, family.ID, CopyAdd)
	if err != nil || len(problems) != 0 {
		t.Fatalf("add copy: err=%v problems=%+v", err, problems)
	}
	if !updated.Linked() || !updated.LinkPrimary {
		t.Fatalf("updated = %+v, want it to become the primary of a pair", updated)
	}
	if fake.createdIn[len(fake.createdIn)-1] != "family@group.calendar.google.com" {
		t.Fatalf("created in %v, want the copy in the second calendar", fake.createdIn)
	}
	copies, err := db.ListCalendarEventCopies(ctx, user.ID, updated)
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) != 1 || copies[0].LinkKey != updated.LinkKey {
		t.Fatalf("copies = %+v, want the new copy under the same key", copies)
	}

	_, problems, err = syncer.UpdateRemoteEventGroup(ctx, user.ID, updated, updated, copies, family.ID, CopyRemove)
	if err != nil || len(problems) != 0 {
		t.Fatalf("remove copy: err=%v problems=%+v", err, problems)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != copies[0].ExternalID {
		t.Fatalf("deleted = %v, want only the copy", fake.deleted)
	}
	if _, err := db.CalendarEvent(ctx, user.ID, updated.ID); err != nil {
		t.Fatalf("the event itself went with the copy: %v", err)
	}
	if _, err := db.CalendarEvent(ctx, user.ID, copies[0].ID); !store.IsNotFound(err) {
		t.Fatalf("the copy survived locally: err=%v", err)
	}
}

// The reader saw one appointment and deleted it, so every copy goes.
func TestDeleteRemoteEventGroupDeletesEveryCopy(t *testing.T) {
	fake := &fakeCalendar{}
	syncer, db, user, work, family := twoCalendarFixture(t, fake)
	ctx := context.Background()
	primary := createLinkedMeeting(t, syncer, user.ID, work, family)
	copies, err := db.ListCalendarEventCopies(ctx, user.ID, primary)
	if err != nil {
		t.Fatal(err)
	}

	problems, err := syncer.DeleteRemoteEventGroup(ctx, user.ID, primary, copies)
	if err != nil || len(problems) != 0 {
		t.Fatalf("delete: err=%v problems=%+v", err, problems)
	}
	if len(fake.deleted) != 2 {
		t.Fatalf("deleted = %v, want both copies", fake.deleted)
	}
	for _, id := range []int64{primary.ID, copies[0].ID} {
		if _, err := db.CalendarEvent(ctx, user.ID, id); !store.IsNotFound(err) {
			t.Fatalf("event %d survived: err=%v", id, err)
		}
	}
}

// The link lives at Google, so a sync rebuilds it from the events themselves.
func TestSyncReadsTheLinkFromGoogle(t *testing.T) {
	start := fixedNow.Add(24 * time.Hour)
	linked := timedEvent("e1", "etag-1", "Standup", start)
	linked.ExtendedProperties = &EventExtendedProperties{Private: map[string]string{
		linkKeyProperty: "k1", linkRoleProperty: linkRoleCopy, "otherApp": "kept",
	}}
	event := ToEvent(linked, 1)
	if event.LinkKey != "k1" || event.LinkPrimary {
		t.Fatalf("event = %+v, want the copy's link read", event)
	}
	plain := ToEvent(timedEvent("e2", "etag-2", "Lunch", start), 1)
	if plain.Linked() {
		t.Fatalf("plain = %+v, want no link", plain)
	}
	if write := ToWrite(plain); write.ExtendedProperties != nil {
		t.Fatalf("write = %+v, want no extended properties for an unlinked event", write)
	}
}

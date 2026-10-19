// File overview: Tests for calendar reminder encoding and for the pass that
// turns due reminders into durable, user-scoped feed events.

package store

import (
	"context"
	"testing"
	"time"
)

func TestCalendarRemindersEncodingRoundTrips(t *testing.T) {
	if got := EncodeCalendarReminders(true, []int{10}); got != CalendarRemindersDefault {
		t.Fatalf("default = %q", got)
	}
	if got := EncodeCalendarReminders(false, nil); got != CalendarRemindersNone {
		t.Fatalf("none = %q", got)
	}
	if got := EncodeCalendarReminders(false, []int{1440, 10, 10, -5, maxCalendarReminderMinutes + 1}); got != "10,1440" {
		t.Fatalf("minutes = %q, want deduplicated, sorted and bounded", got)
	}
	useDefault, minutes := DecodeCalendarReminders("")
	if !useDefault || minutes != nil {
		t.Fatalf("unread row = %v %v, want to follow the calendar default", useDefault, minutes)
	}
	useDefault, minutes = DecodeCalendarReminders("30,x,5")
	if useDefault || len(minutes) != 2 || minutes[0] != 5 || minutes[1] != 30 {
		t.Fatalf("decoded = %v %v", useDefault, minutes)
	}
	calendar := Calendar{DefaultReminders: "15"}
	if got := EffectiveCalendarReminders(CalendarEvent{Reminders: ""}, calendar); len(got) != 1 || got[0] != 15 {
		t.Fatalf("effective for an unread row = %v, want the calendar default", got)
	}
	if got := EffectiveCalendarReminders(CalendarEvent{Reminders: CalendarRemindersNone}, calendar); len(got) != 0 {
		t.Fatalf("effective for none = %v", got)
	}
	if got := EffectiveCalendarReminders(CalendarEvent{Reminders: "10"}, Calendar{}); len(got) != 1 || got[0] != 10 {
		t.Fatalf("effective for explicit minutes = %v", got)
	}
}

// An all-day event's reminders count back from midnight of its date in the
// zone it was entered in, not from the UTC midnight it is stored at.
func TestCalendarReminderStartReadsAllDayEventsInTheirZone(t *testing.T) {
	day := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	berlin, _ := time.LoadLocation("Europe/Berlin")
	start := CalendarReminderStart(CalendarEvent{AllDay: true, StartAt: day}, Calendar{TimeZone: "Europe/Berlin"})
	if !start.Equal(time.Date(2026, 10, 20, 0, 0, 0, 0, berlin)) {
		t.Fatalf("start = %v, want local midnight in Berlin", start)
	}
	timed := CalendarReminderStart(CalendarEvent{StartAt: day.Add(9 * time.Hour)}, Calendar{TimeZone: "Europe/Berlin"})
	if !timed.Equal(day.Add(9 * time.Hour)) {
		t.Fatalf("timed start = %v, want the instant itself", timed)
	}
}

func TestRecordDueCalendarReminderEvents(t *testing.T) {
	db, ctx := openCalendarStore(t)
	user := mustUser(t, db, ctx, "calendar-reminders@example.test")
	other := mustUser(t, db, ctx, "calendar-reminders-other@example.test")
	calendar, err := db.UpsertCalendar(ctx, user, CalendarUpsert{
		GoogleConnectionID: 1, RemoteCalendarID: "primary", Summary: "primary",
		AccessRole: CalendarAccessRoleOwner, Selected: true, DefaultReminders: "30",
	})
	if err != nil {
		t.Fatal(err)
	}
	otherCalendar := mustCalendar(t, db, ctx, other, 1, "primary")
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

	// Due now: starts in ten minutes with a ten-minute reminder.
	due := mustEvent(t, db, ctx, user, CalendarEvent{
		CalendarID: calendar.ID, ExternalID: "due", ETag: "v1", Summary: "Standup", Location: "Room 1",
		StartAt: now.Add(10 * time.Minute), EndAt: now.Add(40 * time.Minute), Reminders: "10",
	})
	// Follows the calendar default of 30 minutes; starts in an hour, so it is
	// the next one to come due.
	later := mustEvent(t, db, ctx, user, CalendarEvent{
		CalendarID: calendar.ID, ExternalID: "later", ETag: "v1", Summary: "Review",
		StartAt: now.Add(time.Hour), EndAt: now.Add(2 * time.Hour),
	})
	// Would be due, but: no reminder, declined, cancelled, a copy, a travel
	// block, or another user's.
	mustEvent(t, db, ctx, user, CalendarEvent{
		CalendarID: calendar.ID, ExternalID: "silent", ETag: "v1", Summary: "Silent",
		StartAt: now.Add(10 * time.Minute), EndAt: now.Add(time.Hour), Reminders: CalendarRemindersNone,
	})
	mustEvent(t, db, ctx, user, CalendarEvent{
		CalendarID: calendar.ID, ExternalID: "declined", ETag: "v1", Summary: "Declined",
		StartAt: now.Add(10 * time.Minute), EndAt: now.Add(time.Hour), Reminders: "10", MyResponse: CalendarResponseDeclined,
	})
	mustEvent(t, db, ctx, user, CalendarEvent{
		CalendarID: calendar.ID, ExternalID: "cancelled", ETag: "v1", Summary: "Cancelled",
		StartAt: now.Add(10 * time.Minute), EndAt: now.Add(time.Hour), Reminders: "10", Status: CalendarEventStatusCancelled,
	})
	mustEvent(t, db, ctx, user, CalendarEvent{
		CalendarID: calendar.ID, ExternalID: "copy", ETag: "v1", Summary: "Copy",
		StartAt: now.Add(10 * time.Minute), EndAt: now.Add(time.Hour), Reminders: "10", LinkKey: "k", LinkPrimary: false,
	})
	mustEvent(t, db, ctx, user, CalendarEvent{
		CalendarID: calendar.ID, ExternalID: "travel", ETag: "v1", Summary: "Travel",
		StartAt: now.Add(10 * time.Minute), EndAt: now.Add(time.Hour), Reminders: "10", LinkKey: "k", LinkTravel: CalendarTravelBefore,
	})
	// Missed by more than the late window: started an hour ago.
	mustEvent(t, db, ctx, user, CalendarEvent{
		CalendarID: calendar.ID, ExternalID: "past", ETag: "v1", Summary: "Past",
		StartAt: now.Add(-time.Hour), EndAt: now, Reminders: "10",
	})
	mustEvent(t, db, ctx, other, CalendarEvent{
		CalendarID: otherCalendar.ID, ExternalID: "theirs", ETag: "v1", Summary: "Theirs",
		StartAt: now.Add(10 * time.Minute), EndAt: now.Add(time.Hour), Reminders: "10",
	})

	recorded, next, err := db.RecordDueCalendarReminderEvents(ctx, user, now, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(recorded) != 1 || recorded[0].EventID != due.ID || recorded[0].MinutesBefore != 10 || recorded[0].Summary != "Standup" || recorded[0].Location != "Room 1" {
		t.Fatalf("recorded = %+v, want exactly the due event", recorded)
	}
	if !recorded[0].StartAt.Equal(due.StartAt) || !recorded[0].DueAt.Equal(now) {
		t.Fatalf("recorded times = start %v due %v", recorded[0].StartAt, recorded[0].DueAt)
	}
	wantNext := later.StartAt.Add(-30 * time.Minute)
	if !next.Equal(wantNext) {
		t.Fatalf("next = %v, want %v (the calendar default reminder of the later event)", next, wantNext)
	}

	// Re-running records nothing new.
	again, _, err := db.RecordDueCalendarReminderEvents(ctx, user, now.Add(time.Minute), 100)
	if err != nil || len(again) != 0 {
		t.Fatalf("second pass = %+v err=%v, want idempotent", again, err)
	}
	// Moving the event makes it remind again at its new time.
	moved := mustEvent(t, db, ctx, user, CalendarEvent{
		CalendarID: calendar.ID, ExternalID: "due", ETag: "v2", Summary: "Standup",
		StartAt: now.Add(70 * time.Minute), EndAt: now.Add(100 * time.Minute), Reminders: "10",
	})
	// An hour on, the later event's default reminder (due at 12:30, inside the
	// late window) fires too; the moved event is what this pass is about.
	afterMove, _, err := db.RecordDueCalendarReminderEvents(ctx, user, now.Add(time.Hour), 100)
	if err != nil || len(afterMove) != 2 {
		t.Fatalf("after move = %+v err=%v", afterMove, err)
	}
	movedReminder := afterMove[1]
	if movedReminder.EventID != moved.ID || !movedReminder.StartAt.Equal(moved.StartAt) || movedReminder.MinutesBefore != 10 {
		t.Fatalf("moved reminder = %+v, want the event reminding again at its new start", movedReminder)
	}

	// The feed is per user and reports the high-water mark.
	events, count, cursor, err := db.CalendarReminderEventsAfter(ctx, user, 0, 10)
	if err != nil || count != 3 || len(events) != 3 || cursor != events[2].ID || events[0].ID >= events[1].ID {
		t.Fatalf("feed = %+v count=%d cursor=%d err=%v", events, count, cursor, err)
	}
	theirs, theirCount, _, err := db.CalendarReminderEventsAfter(ctx, other, 0, 10)
	if err != nil || theirCount != 0 || len(theirs) != 0 {
		t.Fatalf("other user's feed = %+v count=%d err=%v", theirs, theirCount, err)
	}
	if _, _, err := db.RecordDueCalendarReminderEvents(ctx, other, now, 100); err != nil {
		t.Fatal(err)
	}
	theirs, theirCount, _, err = db.CalendarReminderEventsAfter(ctx, other, 0, 10)
	if err != nil || theirCount != 1 || len(theirs) != 1 || theirs[0].Summary != "Theirs" {
		t.Fatalf("other user's own feed = %+v count=%d err=%v", theirs, theirCount, err)
	}
}

// A calendar that is switched off is not drawn and is not synced, so it does
// not remind either.
func TestCalendarRemindersSkipUnselectedCalendars(t *testing.T) {
	db, ctx := openCalendarStore(t)
	user := mustUser(t, db, ctx, "calendar-reminders-off@example.test")
	calendar := mustCalendar(t, db, ctx, user, 1, "primary")
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	mustEvent(t, db, ctx, user, CalendarEvent{
		CalendarID: calendar.ID, ExternalID: "due", ETag: "v1", Summary: "Standup",
		StartAt: now.Add(10 * time.Minute), EndAt: now.Add(time.Hour), Reminders: "10",
	})
	if err := db.SetCalendarSelected(ctx, user, calendar.ID, false); err != nil {
		t.Fatal(err)
	}
	recorded, next, err := db.RecordDueCalendarReminderEvents(ctx, user, now, 100)
	if err != nil || len(recorded) != 0 || !next.IsZero() {
		t.Fatalf("recorded = %+v next=%v err=%v", recorded, next, err)
	}
}

// A new push endpoint starts at the current high-water mark, so enabling
// notifications does not replay every reminder that already fired.
func TestWebPushSubscriptionStartsAtCalendarReminderCursor(t *testing.T) {
	db, ctx := openCalendarStore(t)
	user := mustUser(t, db, ctx, "calendar-reminders-cursor@example.test")
	calendar := mustCalendar(t, db, ctx, user, 1, "primary")
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	mustEvent(t, db, ctx, user, CalendarEvent{
		CalendarID: calendar.ID, ExternalID: "due", ETag: "v1", Summary: "Standup",
		StartAt: now.Add(10 * time.Minute), EndAt: now.Add(time.Hour), Reminders: "10",
	})
	recorded, _, err := db.RecordDueCalendarReminderEvents(ctx, user, now, 100)
	if err != nil || len(recorded) != 1 {
		t.Fatalf("recorded = %+v err=%v", recorded, err)
	}
	sub := saveTestWebPushSubscription(t, db, ctx, user, "https://push.example.test/calendar-cursor")
	if sub.LastCalendarReminderEventID != recorded[0].ID {
		t.Fatalf("new subscription cursor = %d, want %d", sub.LastCalendarReminderEventID, recorded[0].ID)
	}
	advanced, err := db.AdvanceWebPushSubscriptionCalendarReminderCursor(ctx, user, sub, recorded[0].ID+5)
	if err != nil || !advanced {
		t.Fatalf("advance = %v err=%v", advanced, err)
	}
	subs, err := db.ListWebPushSubscriptions(ctx, user)
	if err != nil || len(subs) != 1 || subs[0].LastCalendarReminderEventID != recorded[0].ID+5 || subs[0].LastSnoozeReminderEventID != sub.LastSnoozeReminderEventID {
		t.Fatalf("subscriptions = %+v err=%v", subs, err)
	}
}

func saveTestWebPushSubscription(t *testing.T, db *Store, ctx context.Context, userID int64, endpoint string) WebPushSubscription {
	t.Helper()
	p256dh, auth := testWebPushKeyMaterial(byte(userID%200 + 1))
	sub, err := db.SaveWebPushSubscription(ctx, userID, WebPushSubscription{Endpoint: endpoint, P256DH: p256dh, Auth: auth})
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

// A reminder put off from the notification fires at the chosen time through
// the same feed, once, with the snooze in its key so it cannot collide with
// the provider's own reminder for that minute; a wait for an event that is
// gone is dropped unfired.
func TestSnoozedCalendarReminderFiresAtTheChosenTime(t *testing.T) {
	db, ctx := openCalendarStore(t)
	user := mustUser(t, db, ctx, "calendar-reminder-snooze@example.test")
	other := mustUser(t, db, ctx, "calendar-reminder-snooze-other@example.test")
	calendar := mustCalendar(t, db, ctx, user, 1, "primary")
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	event := mustEvent(t, db, ctx, user, CalendarEvent{
		CalendarID: calendar.ID, ExternalID: "due", ETag: "v1", Summary: "Standup",
		StartAt: now.Add(10 * time.Minute), EndAt: now.Add(40 * time.Minute), Reminders: "10",
	})
	if _, _, err := db.RecordDueCalendarReminderEvents(ctx, user, now, 100); err != nil {
		t.Fatal(err)
	}
	if err := db.SnoozeCalendarReminder(ctx, other, event.ID, now.Add(5*time.Minute)); !IsNotFound(err) {
		t.Fatalf("another user's snooze err=%v, want not found", err)
	}
	// Put off to the minute the provider reminder already fired for.
	if err := db.SnoozeCalendarReminder(ctx, user, event.ID, now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Replaced by a later choice: one wait per event.
	remindAt := now.Add(15 * time.Minute)
	if err := db.SnoozeCalendarReminder(ctx, user, event.ID, remindAt); err != nil {
		t.Fatal(err)
	}
	recorded, next, err := db.RecordDueCalendarReminderEvents(ctx, user, now.Add(time.Minute), 100)
	if err != nil || len(recorded) != 0 {
		t.Fatalf("early pass = %+v err=%v", recorded, err)
	}
	if !next.Equal(remindAt) {
		t.Fatalf("next = %v, want the snooze at %v", next, remindAt)
	}
	recorded, _, err = db.RecordDueCalendarReminderEvents(ctx, user, remindAt, 100)
	if err != nil || len(recorded) != 1 || recorded[0].SnoozeID == 0 || recorded[0].MinutesBefore != -5 || !recorded[0].DueAt.Equal(remindAt) {
		t.Fatalf("snoozed pass = %+v err=%v", recorded, err)
	}
	again, _, err := db.RecordDueCalendarReminderEvents(ctx, user, remindAt.Add(time.Minute), 100)
	if err != nil || len(again) != 0 {
		t.Fatalf("snooze fired twice: %+v err=%v", again, err)
	}
	// A wait for an event that no longer exists is dropped without firing.
	if err := db.SnoozeCalendarReminder(ctx, user, event.ID, now.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteCalendarEvent(ctx, user, event.ID); err != nil {
		t.Fatal(err)
	}
	dropped, next, err := db.RecordDueCalendarReminderEvents(ctx, user, now.Add(30*time.Minute), 100)
	if err != nil || len(dropped) != 0 || !next.IsZero() {
		t.Fatalf("orphaned snooze = %+v next=%v err=%v", dropped, next, err)
	}
}

// The feed pages oldest-first, so a client that advances to the last reminder
// it was handed and comes back for the rest skips nothing.
func TestCalendarReminderFeedPagesOldestFirst(t *testing.T) {
	db, ctx := openCalendarStore(t)
	user := mustUser(t, db, ctx, "calendar-reminder-paging@example.test")
	calendar := mustCalendar(t, db, ctx, user, 1, "primary")
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		mustEvent(t, db, ctx, user, CalendarEvent{
			CalendarID: calendar.ID, ExternalID: "e" + string(rune('a'+i)), ETag: "v1", Summary: "Event",
			StartAt: now.Add(10 * time.Minute), EndAt: now.Add(time.Hour), Reminders: "10",
		})
	}
	recorded, _, err := db.RecordDueCalendarReminderEvents(ctx, user, now, 100)
	if err != nil || len(recorded) != 4 {
		t.Fatalf("recorded = %+v err=%v", recorded, err)
	}
	first, count, cursor, err := db.CalendarReminderEventsAfter(ctx, user, 0, 3)
	if err != nil || count != 4 || len(first) != 3 || cursor != recorded[3].ID {
		t.Fatalf("first page = %+v count=%d cursor=%d err=%v", first, count, cursor, err)
	}
	if first[0].ID != recorded[0].ID || first[2].ID != recorded[2].ID {
		t.Fatalf("first page = %+v, want the three oldest in order", first)
	}
	rest, count, _, err := db.CalendarReminderEventsAfter(ctx, user, first[2].ID, 3)
	if err != nil || count != 1 || len(rest) != 1 || rest[0].ID != recorded[3].ID {
		t.Fatalf("second page = %+v count=%d err=%v", rest, count, err)
	}
}

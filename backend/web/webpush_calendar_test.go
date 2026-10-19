package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
)

func calendarReminderFixture(t *testing.T, ctx context.Context, db *store.Store, email string, now time.Time) (store.User, store.CalendarEvent) {
	t.Helper()
	user, err := db.CreateUser(ctx, email, "Reminded", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	calendar, err := db.UpsertCalendar(ctx, user.ID, store.CalendarUpsert{
		GoogleConnectionID: 1, RemoteCalendarID: "primary", Summary: "primary",
		AccessRole: store.CalendarAccessRoleOwner, Selected: true, TimeZone: "Europe/Berlin",
	})
	if err != nil {
		t.Fatal(err)
	}
	event, err := db.UpsertCalendarEvent(ctx, user.ID, store.CalendarEvent{
		CalendarID: calendar.ID, ExternalID: "standup", ETag: "v1", Summary: "Standup", Location: "Room 1",
		StartAt: now.Add(10 * time.Minute), EndAt: now.Add(40 * time.Minute), TimeZone: "Europe/Berlin", Reminders: "10",
	})
	if err != nil {
		t.Fatal(err)
	}
	return user, event
}

// A calendar reminder that came due reaches every push endpoint once, as its
// own notification, over a cursor independent of the mail and snooze ones.
func TestCalendarReminderWebPushUsesIndependentCursor(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC().Truncate(time.Second)
	user, event := calendarReminderFixture(t, ctx, db, "calendar-notifier@example.test", now)
	sub := saveNotifierTestSubscription(t, ctx, db, user.ID, "https://push.example.test/calendar-notifier")

	var delivered []webPushNotification
	server := &Server{
		store:                db,
		calendarReminderWake: make(chan struct{}, 1),
		webPushSend: func(_ context.Context, _ *http.Client, _ []byte, got store.WebPushSubscription, payload []byte, _ string) (webPushSendResult, error) {
			if got.ID != sub.ID {
				t.Fatalf("delivered subscription = %+v, want id %d", got, sub.ID)
			}
			var notification webPushNotification
			if err := json.Unmarshal(payload, &notification); err != nil {
				t.Fatal(err)
			}
			delivered = append(delivered, notification)
			return webPushSendResult{StatusCode: http.StatusCreated}, nil
		},
	}
	next, err := server.processDueCalendarReminders(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if !next.IsZero() {
		t.Fatalf("next = %v, want nothing ahead once the only reminder fired", next)
	}
	// processDueCalendarReminders hands delivery to a goroutine; the direct
	// call is what the test observes.
	if retry := server.notifyCalendarReminderWebPush(ctx, user.ID); retry {
		t.Fatal("successful reminder push requested retry")
	}
	if len(delivered) != 1 {
		t.Fatalf("delivered = %+v, want one push for one reminder", delivered)
	}
	got := delivered[0]
	if got.Kind != webPushKindCalendarReminder || got.EventID != event.ID || got.Title != "Standup" || got.Location != "Room 1" || got.AllDay {
		t.Fatalf("delivered reminder = %+v", got)
	}
	if got.Body != "Starts in 10 minutes · Room 1" || got.Icon != webPushCalendarIconURL || got.Tag == "" {
		t.Fatalf("delivered body/icon/tag = %q %q %q", got.Body, got.Icon, got.Tag)
	}
	if got.StartAt != event.StartAt.UTC().Format(time.RFC3339) || got.URL != "/calendar/"+event.StartAt.In(mustZone(t, "Europe/Berlin")).Format("2006-01-02") {
		t.Fatalf("delivered start/url = %q %q", got.StartAt, got.URL)
	}
	subs, err := db.ListWebPushSubscriptions(ctx, user.ID)
	if err != nil || len(subs) != 1 {
		t.Fatalf("subscriptions = %+v err=%v", subs, err)
	}
	if subs[0].LastCalendarReminderEventID == 0 || subs[0].LastNewMailEventID != sub.LastNewMailEventID || subs[0].LastSnoozeReminderEventID != sub.LastSnoozeReminderEventID {
		t.Fatalf("cursors after delivery = %+v", subs[0])
	}
	// Nothing new: nothing sent, no retry.
	delivered = nil
	if retry := server.notifyCalendarReminderWebPush(ctx, user.ID); retry || len(delivered) != 0 {
		t.Fatalf("second pass retry=%v delivered=%+v", retry, delivered)
	}
}

// The feed answers for the signed-in user only and starts a new client at the
// current high-water mark.
func TestCalendarReminderFeedIsScopedToTheUser(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC().Truncate(time.Second)
	owner, ownerEvent := calendarReminderFixture(t, ctx, db, "calendar-feed-owner@example.test", now)
	other, _ := calendarReminderFixture(t, ctx, db, "calendar-feed-other@example.test", now)
	server := &Server{store: db, calendarReminderWake: make(chan struct{}, 1)}
	if _, err := server.processDueCalendarReminders(ctx, now); err != nil {
		t.Fatal(err)
	}
	baseline := calendarReminderRequest(t, server, owner, "/api/notifications/calendar-reminders")
	if baseline.UserID != owner.ID || baseline.Cursor == 0 || len(baseline.Reminders) != 0 {
		t.Fatalf("baseline = %+v", baseline)
	}
	ownerFeed := calendarReminderRequest(t, server, owner, "/api/notifications/calendar-reminders?after=0")
	if ownerFeed.Count != 1 || len(ownerFeed.Reminders) != 1 || ownerFeed.Reminders[0].EventID != ownerEvent.ID || ownerFeed.Reminders[0].Title != "Standup" {
		t.Fatalf("owner feed = %+v", ownerFeed)
	}
	otherFeed := calendarReminderRequest(t, server, other, "/api/notifications/calendar-reminders?after=0")
	if otherFeed.UserID != other.ID || otherFeed.Count != 1 || otherFeed.Reminders[0].EventID == ownerEvent.ID {
		t.Fatalf("other feed = %+v", otherFeed)
	}
}

func TestCalendarReminderLeadWording(t *testing.T) {
	cases := map[int]string{0: "Starting now", 5: "Starts in 5 minutes", 60: "Starts in 1 hour", 120: "Starts in 2 hours", 1440: "Starts tomorrow", 2880: "Starts in 2 days", 90: "Starts in 90 minutes"}
	for minutes, want := range cases {
		if got := calendarReminderLead(store.CalendarReminderEvent{MinutesBefore: minutes}); got != want {
			t.Errorf("lead(%d) = %q, want %q", minutes, got, want)
		}
	}
	if got := calendarReminderLead(store.CalendarReminderEvent{AllDay: true, MinutesBefore: 900}); got != "Tomorrow" {
		t.Errorf("all-day lead = %q", got)
	}
	if got := calendarReminderWebPushNotification(store.CalendarReminderEvent{EventID: 7}).Title; got != "Upcoming event" {
		t.Errorf("untitled event = %q", got)
	}
}

func calendarReminderRequest(t *testing.T, server *Server, user store.User, target string) apiCalendarReminderNotificationsResponse {
	t.Helper()
	request := authenticatedSnoozeRequest(t, server, user, http.MethodGet, target, nil)
	response := httptest.NewRecorder()
	server.apiCalendarReminderNotifications(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%s", target, response.Code, response.Body.String())
	}
	var payload apiCalendarReminderNotificationsResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// "Remind me later" is the reader's own wait: it is accepted for their event
// only, has to be ahead, and wakes the scheduler.
func TestCalendarEventRemindRoute(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC().Truncate(time.Second)
	owner, event := calendarReminderFixture(t, ctx, db, "calendar-remind-owner@example.test", now)
	other, _ := calendarReminderFixture(t, ctx, db, "calendar-remind-other@example.test", now)
	server := &Server{store: db, calendarReminderWake: make(chan struct{}, 1), masterKey: bytes.Repeat([]byte{4}, 32), events: newEventHub()}
	remindAt := now.Add(20 * time.Minute)
	post := func(user store.User, body string) *httptest.ResponseRecorder {
		request := authenticatedSnoozeRequest(t, server, user, http.MethodPost, "/api/calendar/events/"+strconvInt64(event.ID)+"/remind", []byte(body))
		response := httptest.NewRecorder()
		server.apiCalendarPath(response, request, "events/"+strconvInt64(event.ID)+"/remind")
		return response
	}
	if response := post(other, `{"remind_at":"`+remindAt.Format(time.RFC3339)+`"}`); response.Code != http.StatusNotFound {
		t.Fatalf("other user's remind status=%d body=%s", response.Code, response.Body.String())
	}
	if response := post(owner, `{"remind_at":"`+now.Add(-time.Minute).Format(time.RFC3339)+`"}`); response.Code != http.StatusBadRequest {
		t.Fatalf("past remind status=%d body=%s", response.Code, response.Body.String())
	}
	if response := post(owner, `{"remind_at":"`+remindAt.Format(time.RFC3339)+`"}`); response.Code != http.StatusOK {
		t.Fatalf("remind status=%d body=%s", response.Code, response.Body.String())
	}
	select {
	case <-server.calendarReminderWake:
	default:
		t.Fatal("remind did not wake the reminder scheduler")
	}
	next, err := server.processDueCalendarReminders(ctx, remindAt.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !next.Equal(remindAt) {
		t.Fatalf("next = %v, want the chosen time %v", next, remindAt)
	}
	events, count, _, err := db.CalendarReminderEventsAfter(ctx, owner.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	// The provider's own reminder is past its window by then -- the event
	// started minutes ago -- and the snooze has not come due: nothing fires.
	if count != 0 {
		t.Fatalf("feed before the chosen time = %+v", events)
	}
	if _, err := server.processDueCalendarReminders(ctx, remindAt); err != nil {
		t.Fatal(err)
	}
	events, count, _, err = db.CalendarReminderEventsAfter(ctx, owner.ID, 0, 10)
	if err != nil || count != 1 || events[0].SnoozeID == 0 || events[0].MinutesBefore != -10 {
		t.Fatalf("feed at the chosen time = %+v count=%d err=%v", events, count, err)
	}
}

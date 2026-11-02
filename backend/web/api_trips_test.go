package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"rolltop/backend/mailparse"
	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
)

type tripAPIFixture struct {
	server   *Server
	db       *store.Store
	ctx      context.Context
	owner    store.User
	stranger store.User
	message  store.MessageRecord
}

func newTripAPIFixture(t *testing.T) tripAPIFixture {
	t.Helper()
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	owner, err := db.CreateUser(ctx, "trip-owner@example.test", "Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := db.CreateUser(ctx, "trip-stranger@example.test", "Stranger", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.CreateMailAccount(ctx, store.MailAccount{
		UserID: owner.ID, Email: "owner@example.test", Host: "imap.example.test", Port: 993,
		Username: "owner@example.test", EncryptedPassword: "secret", UseTLS: true, Mailbox: "*",
	})
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := db.GetOrCreateMailbox(ctx, owner.ID, account.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	blob, err := db.CreateBlob(ctx, store.BlobRecord{UserID: owner.ID, Path: "blobs/trip.eml", Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	sent := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	message, err := db.CreateMessage(ctx, store.CreateMessage{
		UserID: owner.ID, AccountID: account.ID, MailboxID: mailbox.ID, BlobID: blob.ID,
		MessageIDHeader: "<trip-api@example.test>", Subject: "Ihr Online-Ticket",
		FromAddr: "noreply@bahn.de", Date: sent, UID: 1, BlobPath: blob.Path,
	})
	if err != nil {
		t.Fatal(err)
	}
	booking := mailparse.TravelBooking{Kind: mailparse.TravelTrain, Reference: "482910375512", Provider: "Deutsche Bahn",
		Segments: []mailparse.TravelSegment{{Kind: mailparse.TravelTrain, Number: "ICE 597", Title: "ICE 597",
			From: "Berlin Hbf", To: "München Hbf", Start: "2026-11-14T08:29", End: "2026-11-14T12:32",
			StartAt: time.Date(2026, 11, 14, 7, 29, 0, 0, time.UTC), Seat: "Coach 25, seat 45"}}}
	if err := db.ReplaceMessageTrips(ctx, owner.ID, message.ID, sent.Unix(), []mailparse.TravelBooking{booking}, false); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: db, masterKey: bytes.Repeat([]byte{7}, 32), events: newEventHub()}
	return tripAPIFixture{server: server, db: db, ctx: ctx, owner: owner, stranger: stranger, message: message}
}

func (f tripAPIFixture) do(t *testing.T, user store.User, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(context.WithValue(request.Context(), userContextKey, currentUser{User: user}))
	const csrfBase = "trip-action-csrf"
	request.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrfBase})
	request.Header.Set("X-CSRF-Token", f.server.csrfForBase(csrfBase))
	response := httptest.NewRecorder()
	f.server.handleAPI(response, request)
	return response
}

func (f tripAPIFixture) list(t *testing.T, user store.User) []apiTrip {
	t.Helper()
	response := f.do(t, user, http.MethodGet, "/api/trips?day=2026-11-02", "")
	if response.Code != http.StatusOK {
		t.Fatalf("list: %d %s", response.Code, response.Body.String())
	}
	var out struct {
		Trips []apiTrip `json:"trips"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Trips
}

func TestTripRoutesListLinkAndScope(t *testing.T) {
	f := newTripAPIFixture(t)
	trips := f.list(t, f.owner)
	if len(trips) != 1 || len(trips[0].Segments) != 1 {
		t.Fatalf("trips = %+v", trips)
	}
	leg := trips[0].Segments[0]
	if leg.StartAt != "2026-11-14T07:29:00Z" || leg.Start != "2026-11-14T08:29" || leg.AllDay {
		t.Errorf("leg = %+v", leg)
	}
	if got := f.list(t, f.stranger); len(got) != 0 {
		t.Fatalf("a stranger sees %d trips", len(got))
	}
	tripPath := "/api/trips/" + strconv.FormatInt(trips[0].ID, 10)

	// A stranger's event cannot be attached, and a stranger cannot dismiss.
	calendarOther, err := f.db.UpsertCalendar(f.ctx, f.stranger.ID, store.CalendarUpsert{
		GoogleConnectionID: 1, RemoteCalendarID: "primary", Summary: "primary", AccessRole: store.CalendarAccessRoleOwner, Selected: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 11, 14, 7, 29, 0, 0, time.UTC)
	foreign, err := f.db.UpsertCalendarEvent(f.ctx, f.stranger.ID, store.CalendarEvent{CalendarID: calendarOther.ID,
		ExternalID: "x", Summary: "x", StartAt: start, EndAt: start.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"segment_ids":[` + strconv.FormatInt(leg.ID, 10) + `],"event_id":` + strconv.FormatInt(foreign.ID, 10) + `}`
	if response := f.do(t, f.owner, http.MethodPost, tripPath+"/calendar", body); response.Code != http.StatusNotFound {
		t.Fatalf("linking a stranger's event: %d", response.Code)
	}
	if response := f.do(t, f.stranger, http.MethodPost, tripPath, `{"manual_status":"dismissed"}`); response.Code != http.StatusNotFound {
		t.Fatalf("a stranger dismissing: %d", response.Code)
	}

	calendar, err := f.db.UpsertCalendar(f.ctx, f.owner.ID, store.CalendarUpsert{
		GoogleConnectionID: 1, RemoteCalendarID: "primary", Summary: "primary", AccessRole: store.CalendarAccessRoleOwner, Selected: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	own, err := f.db.UpsertCalendarEvent(f.ctx, f.owner.ID, store.CalendarEvent{CalendarID: calendar.ID,
		ExternalID: "y", Summary: "ICE 597", StartAt: start, EndAt: start.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	body = `{"segment_ids":[` + strconv.FormatInt(leg.ID, 10) + `],"event_id":` + strconv.FormatInt(own.ID, 10) + `}`
	response := f.do(t, f.owner, http.MethodPost, tripPath+"/calendar", body)
	if response.Code != http.StatusOK {
		t.Fatalf("link: %d %s", response.Code, response.Body.String())
	}
	if trips := f.list(t, f.owner); trips[0].Segments[0].CalendarEventID != own.ID {
		t.Errorf("leg after link = %+v", trips[0].Segments[0])
	}
	pending := f.do(t, f.owner, http.MethodGet, "/api/trips/pending?day=2026-11-02", "")
	if !strings.Contains(pending.Body.String(), `"count":0`) {
		t.Errorf("pending after link = %s", pending.Body.String())
	}

	if response := f.do(t, f.owner, http.MethodPost, tripPath, `{"manual_status":"dismissed"}`); response.Code != http.StatusOK {
		t.Fatalf("dismiss: %d", response.Code)
	}
	if got := f.list(t, f.owner); len(got) != 0 {
		t.Errorf("a dismissed trip is listed: %+v", got)
	}
}

func TestTripPushNamesTheJourney(t *testing.T) {
	n := tripWebPushNotification(store.TripNotice{TripID: 7, MessageID: 42, Kind: "train", Title: "ICE 597",
		From: "Berlin Hbf", To: "München Hbf", Start: "2026-11-14T08:29"})
	if n.Title != "rolltop - New train ticket" {
		t.Errorf("title = %q", n.Title)
	}
	if !strings.Contains(n.Body, "ICE 597 · Berlin Hbf → München Hbf · Sat 14 Nov, 08:29") {
		t.Errorf("body = %q", n.Body)
	}
	if n.URL != "/messages/42?back=%2Ftrips" || n.Tag != "rolltop-trip-7" {
		t.Errorf("url = %q tag = %q", n.URL, n.Tag)
	}
}

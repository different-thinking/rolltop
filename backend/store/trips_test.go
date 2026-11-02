package store

import (
	"testing"
	"time"

	"rolltop/backend/mailparse"
)

func trainLeg(number, from, to, start, end, seat string, seatOnly bool) mailparse.TravelSegment {
	return mailparse.TravelSegment{
		Kind: mailparse.TravelTrain, Number: number, Title: number, From: from, To: to,
		Start: start, End: end, Seat: seat, SeatOnly: seatOnly, Source: mailparse.TravelSourcePDF,
	}
}

func roundTrip() mailparse.TravelBooking {
	return mailparse.TravelBooking{
		Kind: mailparse.TravelTrain, Reference: "482910375512", Provider: "Deutsche Bahn",
		Segments: []mailparse.TravelSegment{
			trainLeg("ICE 597", "Berlin Hbf", "München Hbf", "2026-11-14T08:29", "2026-11-14T12:32", "", false),
			trainLeg("ICE 1006", "München Hbf", "Berlin Hbf", "2026-11-16T17:28", "2026-11-16T21:31", "", false),
		},
	}
}

// The booking and the ticket that follows it name the same reference and are one
// trip with both mails behind it; the ticket's seat joins the booking's leg.
func TestReplaceMessageTripsMergesMessagesOntoOneTrip(t *testing.T) {
	db, ctx, user, account, mailbox := shipmentTestSetup(t, "trip-merge@example.test")
	booked := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	ticketed := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)

	first := shipmentTestMessage(t, ctx, db, user, account, mailbox, 1, "Buchungsbestätigung", booked)
	if err := db.ReplaceMessageTrips(ctx, user.ID, first.ID, booked.Unix(), []mailparse.TravelBooking{roundTrip()}, false); err != nil {
		t.Fatal(err)
	}
	second := shipmentTestMessage(t, ctx, db, user, account, mailbox, 2, "Ihr Ticket", ticketed)
	ticket := roundTrip()
	ticket.Segments = ticket.Segments[:1]
	ticket.Segments[0].Seat = "Coach 25, seat 45"
	if err := db.ReplaceMessageTrips(ctx, user.ID, second.ID, ticketed.Unix(), []mailparse.TravelBooking{ticket}, false); err != nil {
		t.Fatal(err)
	}

	trips, err := db.ListTrips(ctx, user.ID, "2026-11-02")
	if err != nil {
		t.Fatal(err)
	}
	if len(trips) != 1 {
		t.Fatalf("want one trip, got %d: %+v", len(trips), trips)
	}
	trip := trips[0]
	if len(trip.Segments) != 2 {
		t.Fatalf("a message naming only the outbound leg must not drop the return: %+v", trip.Segments)
	}
	if trip.Segments[0].Seat != "Coach 25, seat 45" || trip.Segments[1].Seat != "" {
		t.Errorf("seats = %q / %q", trip.Segments[0].Seat, trip.Segments[1].Seat)
	}
	if len(trip.Messages) != 2 || trip.Messages[0].MessageID != second.ID {
		t.Errorf("messages = %+v", trip.Messages)
	}
}

// A seat reservation for a train a ticket already names is drawn inside the
// ticket's trip, whichever of the two arrived first.
func TestListTripsFoldsSeatReservationIntoTicket(t *testing.T) {
	db, ctx, user, account, mailbox := shipmentTestSetup(t, "trip-seat@example.test")
	at := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

	reservation := shipmentTestMessage(t, ctx, db, user, account, mailbox, 1, "Reservierung", at)
	if err := db.ReplaceMessageTrips(ctx, user.ID, reservation.ID, at.Unix(), []mailparse.TravelBooking{{
		Kind: mailparse.TravelTrain, Reference: "R-1",
		Segments: []mailparse.TravelSegment{trainLeg("ICE 597", "Berlin Hbf", "München Hbf", "2026-11-14T08:29", "2026-11-14T12:32", "Coach 7, seat 12", true)},
	}}, false); err != nil {
		t.Fatal(err)
	}
	trips, err := db.ListTrips(ctx, user.ID, "2026-11-02")
	if err != nil {
		t.Fatal(err)
	}
	if len(trips) != 1 || !trips[0].Segments[0].SeatOnly {
		t.Fatalf("a reservation nobody's ticket names stands on its own: %+v", trips)
	}

	ticket := shipmentTestMessage(t, ctx, db, user, account, mailbox, 2, "Ticket", at.Add(time.Hour))
	if err := db.ReplaceMessageTrips(ctx, user.ID, ticket.ID, at.Add(time.Hour).Unix(), []mailparse.TravelBooking{roundTrip()}, false); err != nil {
		t.Fatal(err)
	}
	trips, err = db.ListTrips(ctx, user.ID, "2026-11-02")
	if err != nil {
		t.Fatal(err)
	}
	if len(trips) != 1 {
		t.Fatalf("the reservation should fold into the ticket, got %d trips: %+v", len(trips), trips)
	}
	if trips[0].Segments[0].Seat != "Coach 7, seat 12" || len(trips[0].Messages) != 2 {
		t.Errorf("folded trip = %+v", trips[0])
	}
	// The reservation's mail shows the trip it was folded into.
	byMessage, err := db.TripsForMessages(ctx, user.ID, []int64{reservation.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got := byMessage[reservation.ID].Trip; got.Reference != "482910375512" {
		t.Errorf("reservation message shows trip %+v", got)
	}
}

func TestReplaceMessageTripsIgnoresOlderClaimsAndStamps(t *testing.T) {
	db, ctx, user, account, mailbox := shipmentTestSetup(t, "trip-order@example.test")
	newer := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	older := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)

	changed := roundTrip()
	changed.Segments[0].End = "2026-11-14T12:50"
	changed.Segments[0].Cancelled = true
	first := shipmentTestMessage(t, ctx, db, user, account, mailbox, 1, "Änderung", newer)
	if err := db.ReplaceMessageTrips(ctx, user.ID, first.ID, newer.Unix(), []mailparse.TravelBooking{changed}, false); err != nil {
		t.Fatal(err)
	}
	second := shipmentTestMessage(t, ctx, db, user, account, mailbox, 2, "Buchung", older)
	if err := db.ReplaceMessageTrips(ctx, user.ID, second.ID, older.Unix(), []mailparse.TravelBooking{roundTrip()}, false); err != nil {
		t.Fatal(err)
	}
	trips, err := db.ListTrips(ctx, user.ID, "2026-11-02")
	if err != nil {
		t.Fatal(err)
	}
	leg := trips[0].Segments[0]
	if leg.End != "2026-11-14T12:50" || !leg.Cancelled {
		t.Errorf("an older message overwrote a newer answer: %+v", leg)
	}
	candidates, err := db.ListMessagesNeedingTravelScan(ctx, user.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range candidates {
		if c.ID == first.ID || c.ID == second.ID {
			t.Errorf("message %d is still pending after being read", c.ID)
		}
	}
}

func TestTripsToAddAndCalendarLink(t *testing.T) {
	db, ctx, user, account, mailbox := shipmentTestSetup(t, "trip-calendar@example.test")
	at := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	message := shipmentTestMessage(t, ctx, db, user, account, mailbox, 1, "Ticket", at)
	if err := db.ReplaceMessageTrips(ctx, user.ID, message.ID, at.Unix(), []mailparse.TravelBooking{roundTrip()}, false); err != nil {
		t.Fatal(err)
	}
	pending, err := db.TripsToAdd(ctx, user.ID, "2026-11-02")
	if err != nil {
		t.Fatal(err)
	}
	if pending.Count != 1 || pending.Title != "ICE 597" {
		t.Fatalf("pending = %+v", pending)
	}

	calendar := mustCalendar(t, db, ctx, user.ID, 1, "primary")
	start := time.Date(2026, 11, 14, 7, 29, 0, 0, time.UTC)
	event := mustEvent(t, db, ctx, user.ID, CalendarEvent{CalendarID: calendar.ID, ExternalID: "trip-1",
		Summary: "ICE 597", StartAt: start, EndAt: start.Add(4 * time.Hour)})
	trips, _ := db.ListTrips(ctx, user.ID, "2026-11-02")
	legs := []int64{trips[0].Segments[0].ID, trips[0].Segments[1].ID}
	if err := db.LinkTripSegmentsToCalendar(ctx, user.ID, trips[0].ID, legs, event.ID); err != nil {
		t.Fatal(err)
	}
	pending, err = db.TripsToAdd(ctx, user.ID, "2026-11-02")
	if err != nil {
		t.Fatal(err)
	}
	if pending.Count != 0 {
		t.Errorf("a trip in the calendar is not pending: %+v", pending)
	}
	trips, _ = db.ListTrips(ctx, user.ID, "2026-11-02")
	if trips[0].Segments[0].CalendarEventID != event.ID {
		t.Errorf("leg = %+v", trips[0].Segments[0])
	}

	// Deleting the event puts the leg back on the to-add list.
	if err := db.DeleteCalendarEvent(ctx, user.ID, event.ID); err != nil {
		t.Fatal(err)
	}
	trips, _ = db.ListTrips(ctx, user.ID, "2026-11-02")
	if trips[0].Segments[0].CalendarEventID != 0 {
		t.Errorf("a deleted event still reads as added: %+v", trips[0].Segments[0])
	}
}

func TestTripsAreUserScoped(t *testing.T) {
	db, ctx, user, account, mailbox := shipmentTestSetup(t, "trip-mine@example.test")
	other := createPendingMoveTestUser(t, ctx, db, "trip-theirs@example.test")
	at := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	message := shipmentTestMessage(t, ctx, db, user, account, mailbox, 1, "Ticket", at)
	if err := db.ReplaceMessageTrips(ctx, user.ID, message.ID, at.Unix(), []mailparse.TravelBooking{roundTrip()}, false); err != nil {
		t.Fatal(err)
	}
	trips, _ := db.ListTrips(ctx, user.ID, "2026-11-02")
	theirs, err := db.ListTrips(ctx, other.ID, "2026-11-02")
	if err != nil {
		t.Fatal(err)
	}
	if len(theirs) != 0 {
		t.Fatalf("another tenant sees %d trips", len(theirs))
	}
	if _, err := db.GetTrip(ctx, other.ID, trips[0].ID); !IsNotFound(err) {
		t.Errorf("reading another tenant's trip: %v", err)
	}
	if err := db.SetTripManualStatus(ctx, other.ID, trips[0].ID, TripManualDismissed); !IsNotFound(err) {
		t.Errorf("dismissing another tenant's trip: %v", err)
	}
	if got, _ := db.TripsForMessages(ctx, other.ID, []int64{message.ID}); len(got) != 0 {
		t.Errorf("another tenant sees the message's trip: %+v", got)
	}
	if err := db.SetTripManualStatus(ctx, user.ID, trips[0].ID, TripManualDismissed); err != nil {
		t.Fatal(err)
	}
	if trips, _ := db.ListTrips(ctx, user.ID, "2026-11-02"); len(trips) != 0 {
		t.Errorf("a dismissed trip is still listed: %+v", trips)
	}
}

// Only a trip that arrived on the fetch path, recently, for a journey still to
// come is pushed; the backfill and an initial sync of old mail are not.
func TestTripNoticesOnlyForNewUpcomingTrips(t *testing.T) {
	db, ctx, user, account, mailbox := shipmentTestSetup(t, "trip-notice@example.test")
	now := time.Now().UTC()
	future := now.Add(10 * 24 * time.Hour).Format("2006-01-02T15:04")
	upcoming := mailparse.TravelBooking{Kind: mailparse.TravelEvent, Reference: "E1",
		Segments: []mailparse.TravelSegment{{Kind: mailparse.TravelEvent, Title: "Konzert", Start: future}}}

	backfilled := shipmentTestMessage(t, ctx, db, user, account, mailbox, 1, "Tickets", now)
	if err := db.ReplaceMessageTrips(ctx, user.ID, backfilled.ID, now.Unix(), []mailparse.TravelBooking{upcoming}, false); err != nil {
		t.Fatal(err)
	}
	old := shipmentTestMessage(t, ctx, db, user, account, mailbox, 2, "Tickets", now.AddDate(0, -2, 0))
	other := upcoming
	other.Reference = "E2"
	if err := db.ReplaceMessageTrips(ctx, user.ID, old.ID, now.AddDate(0, -2, 0).Unix(), []mailparse.TravelBooking{other}, true); err != nil {
		t.Fatal(err)
	}
	fresh := shipmentTestMessage(t, ctx, db, user, account, mailbox, 3, "Tickets", now)
	third := upcoming
	third.Reference = "E3"
	if err := db.ReplaceMessageTrips(ctx, user.ID, fresh.ID, now.Unix(), []mailparse.TravelBooking{third}, true); err != nil {
		t.Fatal(err)
	}
	notices, cursor, err := db.TripNoticesAfter(ctx, user.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(notices) != 1 || notices[0].MessageID != fresh.ID || notices[0].Title != "Konzert" {
		t.Fatalf("notices = %+v", notices)
	}
	if again, _, err := db.TripNoticesAfter(ctx, user.ID, cursor); err != nil || len(again) != 0 {
		t.Fatalf("the cursor did not advance: %+v %v", again, err)
	}
}

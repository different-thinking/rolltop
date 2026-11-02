// File overview: The trip list -- the tickets the mail carried, their legs, the
// messages that named them, and the reader's two acts on one: putting a leg in
// the calendar, and saying a trip is none of their business.

package web

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"rolltop/backend/store"
)

// apiTrip is one trip as the browser reads it.
type apiTrip struct {
	ID        int64            `json:"id"`
	Kind      string           `json:"kind"`
	Reference string           `json:"reference"`
	Provider  string           `json:"provider"`
	Segments  []apiTripSegment `json:"segments"`
	Messages  []apiTripMessage `json:"messages"`
}

// apiTripSegment is one leg. start and end are the wall-clock times the ticket
// printed; start_at and end_at are the same moments as instants, empty when the
// mail did not say which zone it meant -- the browser then reads the wall clock
// in the reader's own zone.
type apiTripSegment struct {
	ID       int64  `json:"id"`
	Kind     string `json:"kind"`
	Number   string `json:"number"`
	Title    string `json:"title"`
	Provider string `json:"provider"`
	From     string `json:"from"`
	FromCode string `json:"from_code"`
	To       string `json:"to"`
	ToCode   string `json:"to_code"`
	Venue    string `json:"venue"`
	Address  string `json:"address"`
	Start    string `json:"start"`
	StartAt  string `json:"start_at"`
	End      string `json:"end"`
	EndAt    string `json:"end_at"`
	// AllDay marks a leg that names a day and no time.
	AllDay    bool   `json:"all_day"`
	Seat      string `json:"seat"`
	SeatOnly  bool   `json:"seat_only"`
	Cancelled bool   `json:"cancelled"`
	// CalendarEventID is the event the reader created from this leg, 0 while
	// there is none; CalendarStart is its start, for the way to it.
	CalendarEventID int64  `json:"calendar_event_id"`
	CalendarStart   string `json:"calendar_start"`
	CalendarAllDay  bool   `json:"calendar_all_day"`
}

type apiTripMessage struct {
	ID        int64  `json:"id"`
	MailboxID int64  `json:"mailbox_id"`
	Subject   string `json:"subject"`
	From      string `json:"from"`
	Date      string `json:"date"`
}

// apiMessageTrip is the trip one message carries, and how many it named.
type apiMessageTrip struct {
	apiTrip
	Count int `json:"count"`
}

func (s *Server) apiTrips(w http.ResponseWriter, r *http.Request) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	day, ok := requestDay(w, r)
	if !ok {
		return
	}
	trips, err := s.store.ListTrips(r.Context(), cu.User.ID, day)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	out := make([]apiTrip, 0, len(trips))
	for _, trip := range trips {
		out = append(out, apiTripFromStore(trip))
	}
	writeJSON(w, map[string]any{"trips": out})
}

// apiTripsPending is the header chip's own read: how many trips still to come
// have a leg nobody put in the calendar.
func (s *Server) apiTripsPending(w http.ResponseWriter, r *http.Request) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	day, ok := requestDay(w, r)
	if !ok {
		return
	}
	pending, err := s.store.TripsToAdd(r.Context(), cu.User.ID, day)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"count": pending.Count, "title": pending.Title})
}

// requestDay reads the reader's day from the request, for the parcel list's
// reason: the server has no zone for a reader.
func requestDay(w http.ResponseWriter, r *http.Request) (string, bool) {
	day := r.URL.Query().Get("day")
	if day == "" {
		day = time.Now().Format("2006-01-02")
	}
	if _, err := time.Parse("2006-01-02", day); err != nil {
		writeAPIError(w, http.StatusBadRequest, "A day has to be written YYYY-MM-DD.")
		return "", false
	}
	return day, true
}

// apiTripByID handles POST /api/trips/:id (dismiss or restore) and
// POST /api/trips/:id/calendar (record the event created from some legs).
func (s *Server) apiTripByID(w http.ResponseWriter, r *http.Request, rest string) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !s.verifyCSRF(w, r) {
		return
	}
	idPart, action, _ := strings.Cut(rest, "/")
	tripID, ok := parsePositiveID(w, idPart)
	if !ok {
		return
	}
	switch action {
	case "":
		var in struct {
			ManualStatus string `json:"manual_status"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		if !store.ValidTripManualStatus(in.ManualStatus) {
			writeAPIError(w, http.StatusBadRequest, "A trip is either dismissed or shown.")
			return
		}
		if err := s.store.SetTripManualStatus(r.Context(), cu.User.ID, tripID, in.ManualStatus); err != nil {
			if store.IsNotFound(err) {
				http.NotFound(w, r)
				return
			}
			s.serverError(w, r, err)
			return
		}
		s.notifySyncProgress(cu.User.ID)
		writeJSON(w, map[string]any{"ok": true})
	case "calendar":
		var in struct {
			SegmentIDs []int64 `json:"segment_ids"`
			EventID    int64   `json:"event_id"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		if in.EventID <= 0 || len(in.SegmentIDs) == 0 || len(in.SegmentIDs) > 32 {
			writeAPIError(w, http.StatusBadRequest, "Name the event and the legs it was created from.")
			return
		}
		if err := s.store.LinkTripSegmentsToCalendar(r.Context(), cu.User.ID, tripID, in.SegmentIDs, in.EventID); err != nil {
			if store.IsNotFound(err) {
				http.NotFound(w, r)
				return
			}
			s.serverError(w, r, err)
			return
		}
		trip, err := s.store.GetTrip(r.Context(), cu.User.ID, tripID)
		if err != nil {
			if store.IsNotFound(err) {
				http.NotFound(w, r)
				return
			}
			s.serverError(w, r, err)
			return
		}
		s.notifySyncProgress(cu.User.ID)
		writeJSON(w, map[string]any{"trip": apiTripFromStore(trip)})
	default:
		http.NotFound(w, r)
	}
}

func apiTripFromStore(trip store.Trip) apiTrip {
	out := apiTrip{
		ID:        trip.ID,
		Kind:      trip.Kind,
		Reference: trip.Reference,
		Provider:  trip.Provider,
		Segments:  make([]apiTripSegment, 0, len(trip.Segments)),
		Messages:  make([]apiTripMessage, 0, len(trip.Messages)),
	}
	for _, seg := range trip.Segments {
		item := apiTripSegment{
			ID: seg.ID, Kind: seg.Kind, Number: seg.Number, Title: seg.Title, Provider: seg.Provider,
			From: seg.From, FromCode: seg.FromCode, To: seg.To, ToCode: seg.ToCode,
			Venue: seg.Venue, Address: seg.Address,
			Start: seg.Start, End: seg.End, AllDay: len(seg.Start) == len("2006-01-02"),
			Seat: seg.Seat, SeatOnly: seg.SeatOnly, Cancelled: seg.Cancelled,
			CalendarEventID: seg.CalendarEventID, CalendarAllDay: seg.CalendarAllDay,
		}
		if seg.StartAt > 0 {
			item.StartAt = timeString(time.Unix(seg.StartAt, 0).UTC())
		}
		if seg.EndAt > 0 {
			item.EndAt = timeString(time.Unix(seg.EndAt, 0).UTC())
		}
		if seg.CalendarEventID > 0 && seg.CalendarStart > 0 {
			item.CalendarStart = timeString(time.Unix(seg.CalendarStart, 0).UTC())
		}
		out.Segments = append(out.Segments, item)
	}
	for _, message := range trip.Messages {
		out.Messages = append(out.Messages, apiTripMessage{
			ID: message.MessageID, MailboxID: message.MailboxID, Subject: message.Subject,
			From: message.FromAddr, Date: timeString(time.Unix(message.Date, 0).UTC()),
		})
	}
	return out
}

// messageTrips looks up the trips a batch of messages carried. A failure is
// logged and dropped rather than failing the list, as for parcels.
func (s *Server) messageTrips(ctx context.Context, userID int64, messageIDs []int64) map[int64]*apiMessageTrip {
	if s == nil || userID <= 0 || len(messageIDs) == 0 {
		return nil
	}
	found, err := s.store.TripsForMessages(ctx, userID, messageIDs)
	if err != nil {
		log.Printf("message trips lookup failed user_id=%d messages=%d: %v", userID, len(messageIDs), err)
		return nil
	}
	if len(found) == 0 {
		return nil
	}
	out := make(map[int64]*apiMessageTrip, len(found))
	for messageID, item := range found {
		out[messageID] = &apiMessageTrip{apiTrip: apiTripFromStore(item.Trip), Count: item.Count}
	}
	return out
}

// tripMessageURL opens the mail a trip came from, with the trip list as the
// way back.
func tripMessageURL(messageID int64) string {
	if messageID <= 0 {
		return "/trips"
	}
	return "/messages/" + strconv.FormatInt(messageID, 10) + "?back=%2Ftrips"
}

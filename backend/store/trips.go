// File overview: Tickets the mail carried -- one row per trip, its legs, the
// messages that named it, the event the reader created from it, and the
// backfill's work queue.
//
// The shape is the shipments shape. "What am I travelling to next" is a question
// about journeys, and the booking, the ticket and the seat reservation are three
// messages of evidence for one of them. So a trip is the row, keyed by what the
// provider issued, its legs hang off it, and every message that named it is
// linked to it.

package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"rolltop/backend/mailparse"
)

// TravelVersion is the extraction generation a message was read for tickets by.
// Bump it whenever a change to mailparse's ticket readers should reach stored
// mail; the backfill re-reads what an older generation read, as far as blob
// retention still holds it.
//
// 2: a DB calendar object is read as the booking's train legs rather than as
// an event ticket, and a DB mail's subject decides seat reservation or ticket.
// Mail read under 1 carries an "event" trip beside the train for the same
// journey; re-reading it detaches that trip, which then has no mail left and
// drops out of every list.
//
// 3: DB's calendar export is read train by train from its description (one
// event per journey, every change listed), zoneless times are Berlin's, and
// the same departure minute joins the readings of one train.
//
// 4: a DB mail's order number ("Auftrag: 916319593981" in the subject) is the
// booking's reference, which changes the trip's identity.
//
// 5: a vehicle leg is keyed by its departure minute alone, and every train of
// a DB journey carries the calendar object's UID. The re-read is clean because
// a leg only this message named is dropped when it no longer names it.
const TravelVersion = 5

// TravelBackfillLimit bounds one backfill pass. It is a quarter of the parcel
// pass's because a ticket read opens attachment bodies -- a pass, a PDF run
// through a text extractor -- where the parcel read opens none.
const TravelBackfillLimit = 50

const (
	// tripListLimit bounds what one read of the trip list returns.
	tripListLimit = 200
	// tripHistoryDays is how long a trip stays on the list after its last leg.
	// Long enough to find last weekend's ticket again, short enough that the
	// list is about what is coming.
	tripHistoryDays = 14
	// tripNoticeWindow is how recent a message has to be for its trip to be
	// pushed. An initial sync goes through the fetch path too, and an old
	// booking for a journey still months away must not announce itself as new.
	tripNoticeWindow = 3 * 24 * time.Hour
	// MaxTripNoticesPerPass bounds the push feed one read returns.
	MaxTripNoticesPerPass = 20
)

// Manual statuses a reader can put on a trip.
const (
	TripManualNone      = ""
	TripManualDismissed = "dismissed"
)

// ValidTripManualStatus keeps anything the column would refuse out of the
// query. It arrives from a request, so it is checked rather than trusted.
func ValidTripManualStatus(value string) bool {
	return value == TripManualNone || value == TripManualDismissed
}

// Trip is one booking, as everything the mail has said about it so far.
type Trip struct {
	ID           int64
	Kind         string
	Reference    string
	Provider     string
	ManualStatus string
	ReportedAt   int64
	Segments     []TripSegment
	// Messages are the mails that named this trip, newest first.
	Messages []TripMessage
}

// TripSegment is one leg of a trip.
type TripSegment struct {
	ID        int64
	TripID    int64
	Kind      string
	Number    string
	Title     string
	Provider  string
	From      string
	FromCode  string
	To        string
	ToCode    string
	Venue     string
	Address   string
	Start     string
	StartAt   int64
	End       string
	EndAt     int64
	Seat      string
	SeatOnly  bool
	Cancelled bool
	// CalendarEventID is the event the reader created from this leg, zero
	// when there is none or when that event no longer exists.
	CalendarEventID int64
	// CalendarStart is that event's start, for the way to it.
	CalendarStart  int64
	CalendarAllDay bool
	// FromCalendarObject marks a leg read from a calendar object the message
	// carries, which the trip card offers in place of the invitation card.
	FromCalendarObject bool
}

// TripMessage is one mail that named a trip.
type TripMessage struct {
	MessageID int64
	MailboxID int64
	Subject   string
	FromAddr  string
	Date      int64
}

// TripCandidate is one message the ticket backfill still has to read.
type TripCandidate struct {
	ID       int64
	BlobPath string
	Date     int64
}

// ReplaceMessageTrips records what one message said about tickets, and stamps
// the message as read by this generation either way.
//
// The upserts are what make several messages one trip. A leg's fields are only
// overwritten by a message at least as recent as the one they came from, and a
// leg is never removed by a message that does not name it: a check-in mail for
// the outbound flight says nothing about the return, and that silence is not a
// cancellation. A seat a newer message leaves out is kept for the same reason.
//
// notify says the message arrived on the fetch path. A trip it creates for a
// journey still to come, from a message sent within tripNoticeWindow, is marked
// for a push.
func (s *Store) ReplaceMessageTrips(ctx context.Context, userID, messageID, messageDate int64, bookings []mailparse.TravelBooking, notify bool) error {
	if userID <= 0 || messageID <= 0 {
		return fmt.Errorf("trips: user_id and message_id are required")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := nowUnix()
	if _, err := tx.ExecContext(ctx, `DELETE FROM trip_messages WHERE user_id = ? AND message_id = ?`, userID, messageID); err != nil {
		return err
	}
	// The legs this message named before are re-linked below as it names them
	// now; whatever it alone named and no longer does goes at the end.
	prior, err := unlinkMessageSegments(ctx, tx, userID, []any{messageID})
	if err != nil {
		return err
	}
	recent := messageDate > 0 && time.Since(time.Unix(messageDate, 0)) <= tripNoticeWindow
	for _, booking := range bookings {
		key := booking.Key()
		if key == "" || !mailparse.ValidTravelKind(booking.Kind) || len(booking.Segments) == 0 {
			continue
		}
		var tripID int64
		var inserted bool
		err := tx.QueryRowContext(ctx, `INSERT INTO trips
				(user_id, trip_key, kind, reference, provider, reported_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (user_id, trip_key) DO UPDATE SET
				reference = CASE WHEN trips.reference = '' THEN EXCLUDED.reference ELSE trips.reference END,
				provider = CASE WHEN trips.provider = '' THEN EXCLUDED.provider ELSE trips.provider END,
				reported_at = GREATEST(EXCLUDED.reported_at, trips.reported_at),
				updated_at = EXCLUDED.updated_at
			RETURNING id, (xmax = 0)`,
			userID, key, booking.Kind, booking.Reference, booking.Provider, messageDate, now, now).Scan(&tripID, &inserted)
		if err != nil {
			return err
		}
		upcoming := false
		for _, seg := range booking.Segments {
			segmentID, err := upsertTripSegment(ctx, tx, userID, tripID, messageDate, now, seg)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO trip_segment_messages (segment_id, message_id, user_id)
				VALUES (?, ?, ?) ON CONFLICT (segment_id, message_id) DO NOTHING`, segmentID, messageID, userID); err != nil {
				return err
			}
			if segmentStillToCome(seg) && !seg.Cancelled {
				upcoming = true
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO trip_messages (trip_id, message_id, user_id, created_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (trip_id, message_id) DO NOTHING`, tripID, messageID, userID, now); err != nil {
			return err
		}
		if inserted && notify && recent && upcoming {
			if _, err := tx.ExecContext(ctx, `UPDATE trips SET notify_at = ? WHERE user_id = ? AND id = ?`, now, userID, tripID); err != nil {
				return err
			}
		}
	}
	if err := dropUnnamedSegments(ctx, tx, userID, prior); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET travel_version = ? WHERE user_id = ? AND id = ?`,
		TravelVersion, userID, messageID); err != nil {
		return err
	}
	return tx.Commit()
}

// unlinkMessageSegments removes the messages' links to the legs they named and
// returns those legs, for dropUnnamedSegments to look at once the messages
// have said what they name now.
func unlinkMessageSegments(ctx context.Context, tx *sql.Tx, userID int64, messageIDs []any) ([]int64, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(messageIDs)), ", ")
	rows, err := tx.QueryContext(ctx, `DELETE FROM trip_segment_messages
		WHERE user_id = ? AND message_id IN (`+marks+`)
		RETURNING segment_id`, append([]any{userID}, messageIDs...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// dropUnnamedSegments deletes those of the given legs no message names any
// more. A calendar event the reader created from one moves to the leg that
// now stands for it -- the same departure on the same trip -- so a re-read
// that only respelled a leg does not put the journey back on the to-add list.
func dropUnnamedSegments(ctx context.Context, tx *sql.Tx, userID int64, segmentIDs []int64) error {
	if len(segmentIDs) == 0 {
		return nil
	}
	args := make([]any, 0, len(segmentIDs)+1)
	args = append(args, userID)
	for _, id := range segmentIDs {
		args = append(args, id)
	}
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(segmentIDs)), ", ")
	unnamed := `SELECT gone.id FROM trip_segments gone
		WHERE gone.user_id = ? AND gone.id IN (` + marks + `)
			AND NOT EXISTS (SELECT 1 FROM trip_segment_messages l WHERE l.segment_id = gone.id)`
	if _, err := tx.ExecContext(ctx, `UPDATE trip_segments s
		SET calendar_event_id = o.calendar_event_id, calendar_added_at = o.calendar_added_at
		FROM trip_segments o
		WHERE o.id IN (`+unnamed+`) AND o.calendar_event_id <> 0
			AND s.user_id = o.user_id AND s.trip_id = o.trip_id AND s.id <> o.id
			AND s.start_local = o.start_local AND s.calendar_event_id = 0`, args...); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM trip_segments WHERE id IN (`+unnamed+`)`, args...)
	return err
}

// segmentStillToCome reports a leg whose day has not passed. It is read on the
// server's clock, which is close enough for a decision measured in days.
func segmentStillToCome(seg mailparse.TravelSegment) bool {
	if !seg.StartAt.IsZero() {
		return seg.StartAt.After(time.Now())
	}
	day := seg.Start
	if len(day) > len(plainDateLayout) {
		day = day[:len(plainDateLayout)]
	}
	return day >= time.Now().UTC().AddDate(0, 0, -1).Format(plainDateLayout)
}

func upsertTripSegment(ctx context.Context, tx *sql.Tx, userID, tripID, reportedAt, now int64, seg mailparse.TravelSegment) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `INSERT INTO trip_segments
			(trip_id, user_id, segment_key, kind, number, title, provider, from_name, from_code, to_name, to_code,
			 venue, address, start_local, start_at, end_local, end_at, seat, seat_only, cancelled, source,
			 ical_uid, reported_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (trip_id, segment_key) DO UPDATE SET
			number = CASE WHEN EXCLUDED.reported_at >= trip_segments.reported_at AND EXCLUDED.number <> '' THEN EXCLUDED.number ELSE trip_segments.number END,
			title = CASE WHEN EXCLUDED.reported_at >= trip_segments.reported_at AND EXCLUDED.title <> '' THEN EXCLUDED.title ELSE trip_segments.title END,
			provider = CASE WHEN trip_segments.provider = '' THEN EXCLUDED.provider ELSE trip_segments.provider END,
			from_name = CASE WHEN EXCLUDED.reported_at >= trip_segments.reported_at AND EXCLUDED.from_name <> '' THEN EXCLUDED.from_name ELSE trip_segments.from_name END,
			from_code = CASE WHEN trip_segments.from_code = '' THEN EXCLUDED.from_code ELSE trip_segments.from_code END,
			to_name = CASE WHEN EXCLUDED.reported_at >= trip_segments.reported_at AND EXCLUDED.to_name <> '' THEN EXCLUDED.to_name ELSE trip_segments.to_name END,
			to_code = CASE WHEN trip_segments.to_code = '' THEN EXCLUDED.to_code ELSE trip_segments.to_code END,
			venue = CASE WHEN EXCLUDED.reported_at >= trip_segments.reported_at AND EXCLUDED.venue <> '' THEN EXCLUDED.venue ELSE trip_segments.venue END,
			address = CASE WHEN EXCLUDED.reported_at >= trip_segments.reported_at AND EXCLUDED.address <> '' THEN EXCLUDED.address ELSE trip_segments.address END,
			start_at = CASE WHEN trip_segments.start_at = 0 THEN EXCLUDED.start_at ELSE trip_segments.start_at END,
			end_local = CASE WHEN EXCLUDED.reported_at >= trip_segments.reported_at AND EXCLUDED.end_local <> '' THEN EXCLUDED.end_local ELSE trip_segments.end_local END,
			end_at = CASE WHEN EXCLUDED.reported_at >= trip_segments.reported_at AND EXCLUDED.end_at <> 0 THEN EXCLUDED.end_at ELSE trip_segments.end_at END,
			seat = CASE WHEN EXCLUDED.seat <> '' AND (EXCLUDED.reported_at >= trip_segments.reported_at OR trip_segments.seat = '') THEN EXCLUDED.seat ELSE trip_segments.seat END,
			seat_only = CASE WHEN EXCLUDED.seat_only = 0 THEN 0 ELSE trip_segments.seat_only END,
			cancelled = CASE WHEN EXCLUDED.reported_at >= trip_segments.reported_at THEN EXCLUDED.cancelled ELSE trip_segments.cancelled END,
			ical_uid = CASE WHEN trip_segments.ical_uid = '' THEN EXCLUDED.ical_uid ELSE trip_segments.ical_uid END,
			reported_at = GREATEST(EXCLUDED.reported_at, trip_segments.reported_at),
			updated_at = EXCLUDED.updated_at
		RETURNING id`,
		tripID, userID, seg.Key(), seg.Kind, seg.Number, seg.Title, seg.Provider, seg.From, seg.FromCode, seg.To, seg.ToCode,
		seg.Venue, seg.Address, seg.Start, unixOrZero(seg.StartAt), seg.End, unixOrZero(seg.EndAt), seg.Seat,
		boolInt(seg.SeatOnly), boolInt(seg.Cancelled), seg.Source, seg.ICalUID, reportedAt, now, now).Scan(&id)
	return id, err
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// ListTrips returns the trips worth showing on the given day: everything with a
// leg still to come, and what ended within tripHistoryDays. today is the
// reader's own day, "YYYY-MM-DD", for the reason ListShipments takes one.
//
// A seat reservation for a train some ticket of the reader's already names is
// folded into that ticket's leg here rather than stored there: the two arrive
// in either order and from different bookings, and a fold at read time is the
// one answer that does not depend on which came first.
func (s *Store) ListTrips(ctx context.Context, userID int64, today string) ([]Trip, error) {
	day, err := parsePlainDate(today)
	if err != nil {
		return nil, err
	}
	trips, _, err := s.listTrips(ctx, userID, day.AddDate(0, 0, -tripHistoryDays).Format(plainDateLayout), nil)
	return trips, err
}

// GetTrip returns one trip with its legs and messages, folded the same way the
// list folds it. A seat reservation folded into a ticket answers with the
// ticket's trip, which is where the list draws it.
func (s *Store) GetTrip(ctx context.Context, userID, tripID int64) (Trip, error) {
	trips, absorbed, err := s.listTrips(ctx, userID, "", []int64{tripID})
	if err != nil {
		return Trip{}, err
	}
	if host, ok := absorbed[tripID]; ok {
		tripID = host
	}
	for _, trip := range trips {
		if trip.ID == tripID {
			return trip, nil
		}
	}
	return Trip{}, ErrNotFound
}

func (s *Store) listTrips(ctx context.Context, userID int64, since string, onlyTrips []int64) ([]Trip, map[int64]int64, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	// Every trip of the reader's with a leg on or after `since` is read, the
	// dismissed ones included: a dismissed ticket's seat reservation still
	// belongs to it, and folding has to see both halves to know that.
	query := `SELECT t.id, t.kind, t.reference, t.provider, t.manual_status, t.reported_at
		FROM trips t
		WHERE t.user_id = ? AND EXISTS (
			SELECT 1 FROM trip_segments ts WHERE ts.user_id = t.user_id AND ts.trip_id = t.id
				AND (ts.start_local >= ? OR ts.end_local >= ?))`
	args := []any{userID, since, since}
	if len(onlyTrips) > 0 {
		// Named trips are read with every trip that could fold into them or
		// they into it: the trains on the same days.
		marks := make([]string, len(onlyTrips))
		ids := make([]any, len(onlyTrips))
		for i, id := range onlyTrips {
			marks[i] = "?"
			ids[i] = id
		}
		list := strings.Join(marks, ", ")
		query = `SELECT t.id, t.kind, t.reference, t.provider, t.manual_status, t.reported_at
			FROM trips t
			WHERE t.user_id = ? AND (t.id IN (` + list + `) OR EXISTS (
				SELECT 1 FROM trip_segments mine
				JOIN trip_segments other ON other.user_id = mine.user_id AND other.kind = mine.kind
					AND other.number = mine.number AND LEFT(other.start_local, 10) = LEFT(mine.start_local, 10)
				WHERE mine.user_id = t.user_id AND mine.trip_id IN (` + list + `) AND mine.number <> '' AND other.trip_id = t.id))`
		args = append(append([]any{userID}, ids...), ids...)
	}
	rows, err := db.QueryContext(ctx, query+` ORDER BY t.id DESC LIMIT ?`, append(args, tripListLimit*2)...)
	if err != nil {
		return nil, nil, err
	}
	var trips []Trip
	index := map[int64]int{}
	for rows.Next() {
		var trip Trip
		if err := rows.Scan(&trip.ID, &trip.Kind, &trip.Reference, &trip.Provider, &trip.ManualStatus, &trip.ReportedAt); err != nil {
			rows.Close()
			return nil, nil, err
		}
		index[trip.ID] = len(trips)
		trips = append(trips, trip)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(trips) == 0 {
		return nil, nil, nil
	}
	if err := s.attachTripSegments(ctx, userID, trips, index); err != nil {
		return nil, nil, err
	}
	if err := s.attachTripMessages(ctx, userID, trips, index); err != nil {
		return nil, nil, err
	}
	trips, absorbed := foldSeatReservations(trips)
	kept := trips[:0]
	for _, trip := range trips {
		// A trip with no message left has no evidence left, and one the reader
		// dismissed is gone from every read.
		if len(trip.Messages) == 0 || len(trip.Segments) == 0 || trip.ManualStatus == TripManualDismissed {
			continue
		}
		kept = append(kept, trip)
	}
	sort.SliceStable(kept, func(a, b int) bool {
		return kept[a].Segments[0].Start < kept[b].Segments[0].Start
	})
	if len(kept) > tripListLimit {
		kept = kept[:tripListLimit]
	}
	return kept, absorbed, nil
}

func idPlaceholders(userID int64, trips []Trip) ([]any, string) {
	args := make([]any, 0, len(trips)+1)
	args = append(args, userID)
	placeholders := make([]string, 0, len(trips))
	for _, trip := range trips {
		args = append(args, trip.ID)
		placeholders = append(placeholders, "?")
	}
	return args, strings.Join(placeholders, ", ")
}

func (s *Store) attachTripSegments(ctx context.Context, userID int64, trips []Trip, index map[int64]int) error {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return err
	}
	args, list := idPlaceholders(userID, trips)
	rows, err := db.QueryContext(ctx, `SELECT ts.id, ts.trip_id, ts.kind, ts.number, ts.title, ts.provider,
			ts.from_name, ts.from_code, ts.to_name, ts.to_code, ts.venue, ts.address,
			ts.start_local, ts.start_at, ts.end_local, ts.end_at, ts.seat, ts.seat_only, ts.cancelled,
			COALESCE(ce.id, imported.id, 0), COALESCE(ce.start_at, imported.start_at, 0), COALESCE(ce.all_day, imported.all_day, 0),
			ts.ical_uid <> ''
		FROM trip_segments ts
		LEFT JOIN calendar_events ce ON ce.id = ts.calendar_event_id AND ce.user_id = ts.user_id
		-- The event the reader made from the leg wins; failing that, one the
		-- provider imported from the very calendar object the leg was read
		-- from (Gmail does this with a DB booking's .ics).
		LEFT JOIN LATERAL (SELECT id, start_at, all_day FROM calendar_events
			WHERE user_id = ts.user_id AND ts.ical_uid <> '' AND ical_uid = ts.ical_uid
			ORDER BY id LIMIT 1) imported ON true
		-- A leg no message names any more -- its only mail deleted -- is not
		-- drawn: it has no evidence left.
		WHERE ts.user_id = ? AND ts.trip_id IN (`+list+`)
			AND EXISTS (SELECT 1 FROM trip_segment_messages tsm WHERE tsm.segment_id = ts.id)
		ORDER BY ts.start_local, ts.id`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var seg TripSegment
		var seatOnly, cancelled, allDay int64
		if err := rows.Scan(&seg.ID, &seg.TripID, &seg.Kind, &seg.Number, &seg.Title, &seg.Provider,
			&seg.From, &seg.FromCode, &seg.To, &seg.ToCode, &seg.Venue, &seg.Address,
			&seg.Start, &seg.StartAt, &seg.End, &seg.EndAt, &seg.Seat, &seatOnly, &cancelled,
			&seg.CalendarEventID, &seg.CalendarStart, &allDay, &seg.FromCalendarObject); err != nil {
			return err
		}
		seg.SeatOnly = seatOnly != 0
		seg.Cancelled = cancelled != 0
		seg.CalendarAllDay = allDay != 0
		if at, ok := index[seg.TripID]; ok {
			trips[at].Segments = append(trips[at].Segments, seg)
		}
	}
	return rows.Err()
}

func (s *Store) attachTripMessages(ctx context.Context, userID int64, trips []Trip, index map[int64]int) error {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return err
	}
	args, list := idPlaceholders(userID, trips)
	rows, err := db.QueryContext(ctx, `SELECT tm.trip_id, m.id, m.mailbox_id, m.subject, m.from_addr, m.date_unix
		FROM trip_messages tm
		JOIN messages m ON m.id = tm.message_id AND m.user_id = tm.user_id
		WHERE tm.user_id = ? AND tm.trip_id IN (`+list+`)
		ORDER BY m.date_unix DESC, m.id DESC`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var tripID int64
		var item TripMessage
		if err := rows.Scan(&tripID, &item.MessageID, &item.MailboxID, &item.Subject, &item.FromAddr, &item.Date); err != nil {
			return err
		}
		if at, ok := index[tripID]; ok {
			trips[at].Messages = append(trips[at].Messages, item)
		}
	}
	return rows.Err()
}

// foldSeatReservations joins a seat-only leg to a ticketed leg of the same
// train on the same day in another trip: the seat moves onto the ticket's leg
// when it has none, the reservation's messages join the ticket's trip, and a
// reservation trip left with no legs of its own disappears into it. A
// reservation no ticket names stays a trip of its own -- the reader may have a
// season ticket and only booked the seat.
func foldSeatReservations(trips []Trip) ([]Trip, map[int64]int64) {
	type ticketed struct{ trip, seg int }
	byTrain := map[string]ticketed{}
	for ti, trip := range trips {
		// A ticket the reader dismissed hosts nothing: a reservation folded
		// into it would disappear with it.
		if trip.ManualStatus == TripManualDismissed {
			continue
		}
		for si, seg := range trip.Segments {
			if seg.SeatOnly || seg.Number == "" || seg.Kind != mailparse.TravelTrain {
				continue
			}
			key := trainDayKey(seg)
			if _, ok := byTrain[key]; !ok {
				byTrain[key] = ticketed{ti, si}
			}
		}
	}
	gone := map[int]bool{}
	absorbed := map[int64]int64{}
	for ti := range trips {
		var kept []TripSegment
		hosts := map[int]bool{}
		for _, seg := range trips[ti].Segments {
			target, ok := byTrain[trainDayKey(seg)]
			if !seg.SeatOnly || seg.Kind != mailparse.TravelTrain || !ok || target.trip == ti {
				kept = append(kept, seg)
				continue
			}
			host := &trips[target.trip].Segments[target.seg]
			if host.Seat == "" {
				host.Seat = seg.Seat
			}
			if host.CalendarEventID == 0 && seg.CalendarEventID != 0 {
				host.CalendarEventID, host.CalendarStart, host.CalendarAllDay = seg.CalendarEventID, seg.CalendarStart, seg.CalendarAllDay
			}
			hosts[target.trip] = true
		}
		if len(hosts) == 0 {
			continue
		}
		trips[ti].Segments = kept
		for host := range hosts {
			trips[host].Messages = mergeTripMessages(trips[host].Messages, trips[ti].Messages)
		}
		if len(kept) == 0 {
			gone[ti] = true
			for host := range hosts {
				absorbed[trips[ti].ID] = trips[host].ID
				break
			}
		}
	}
	out := make([]Trip, 0, len(trips))
	for ti, trip := range trips {
		if !gone[ti] {
			out = append(out, trip)
		}
	}
	return out, absorbed
}

func trainDayKey(seg TripSegment) string {
	day := seg.Start
	if len(day) > len(plainDateLayout) {
		day = day[:len(plainDateLayout)]
	}
	return strings.ToUpper(strings.ReplaceAll(seg.Number, " ", "")) + "|" + day
}

func mergeTripMessages(into, from []TripMessage) []TripMessage {
	seen := map[int64]bool{}
	for _, m := range into {
		seen[m.MessageID] = true
	}
	for _, m := range from {
		if !seen[m.MessageID] {
			into = append(into, m)
			seen[m.MessageID] = true
		}
	}
	sort.SliceStable(into, func(a, b int) bool { return into[a].Date > into[b].Date })
	return into
}

// PendingTrips is the header's whole answer: how many trips still to come have
// a leg nobody has put in the calendar, and the first one's title when there
// is exactly one.
type PendingTrips struct {
	Count int
	Title string
}

// TripsToAdd answers "is there a ticket I have not put in my calendar yet". It
// reads the list and counts, because the fold decides what a trip is and a
// second definition in SQL would drift from it; the list is bounded.
func (s *Store) TripsToAdd(ctx context.Context, userID int64, today string) (PendingTrips, error) {
	day, err := parsePlainDate(today)
	if err != nil {
		return PendingTrips{}, err
	}
	trips, _, err := s.listTrips(ctx, userID, day.Format(plainDateLayout), nil)
	if err != nil {
		return PendingTrips{}, err
	}
	out := PendingTrips{}
	for _, trip := range trips {
		if tripHasLegToAdd(trip, today) {
			out.Count++
			if out.Count == 1 {
				out.Title = trip.Segments[0].Title
			}
		}
	}
	if out.Count != 1 {
		out.Title = ""
	}
	return out, nil
}

func tripHasLegToAdd(trip Trip, today string) bool {
	for _, seg := range trip.Segments {
		if seg.Cancelled || seg.CalendarEventID != 0 {
			continue
		}
		if seg.Start >= today {
			return true
		}
	}
	return false
}

// MessageTrip is the trip one message is about, as a message carries it.
type MessageTrip struct {
	Trip Trip
	// Count is how many trips this message named.
	Count int
}

// TripsForMessages answers, for a batch of messages, which trip each one is
// about, with the trip's legs, so an open message can offer them for the
// calendar without a second request. Where a message names several, the one
// carried is the one whose first leg comes first.
func (s *Store) TripsForMessages(ctx context.Context, userID int64, messageIDs []int64) (map[int64]MessageTrip, error) {
	out := map[int64]MessageTrip{}
	if userID <= 0 || len(messageIDs) == 0 {
		return out, nil
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	args := []any{userID}
	placeholders := make([]string, 0, len(messageIDs))
	seen := map[int64]bool{}
	for _, id := range messageIDs {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		args = append(args, id)
		placeholders = append(placeholders, "?")
	}
	if len(placeholders) == 0 {
		return out, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT tm.message_id, tm.trip_id
		FROM trip_messages tm
		JOIN trips t ON t.id = tm.trip_id AND t.user_id = tm.user_id
		WHERE tm.user_id = ? AND t.manual_status = ''
			AND tm.message_id IN (`+strings.Join(placeholders, ", ")+`)
		ORDER BY tm.message_id, tm.trip_id`, args...)
	if err != nil {
		return nil, err
	}
	byMessage := map[int64][]int64{}
	var order []int64
	for rows.Next() {
		var messageID, tripID int64
		if err := rows.Scan(&messageID, &tripID); err != nil {
			rows.Close()
			return nil, err
		}
		if _, ok := byMessage[messageID]; !ok {
			order = append(order, messageID)
		}
		byMessage[messageID] = append(byMessage[messageID], tripID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Every trip the batch names is read in one pass, folded the way the list
	// folds it; a reservation folded into a ticket answers with the ticket.
	wanted := make([]int64, 0, len(byMessage))
	seenTrip := map[int64]bool{}
	for _, messageID := range order {
		for _, tripID := range byMessage[messageID] {
			if !seenTrip[tripID] {
				seenTrip[tripID] = true
				wanted = append(wanted, tripID)
			}
		}
	}
	loaded, absorbed, err := s.listTrips(ctx, userID, "", wanted)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]*Trip, len(loaded))
	for i := range loaded {
		byID[loaded[i].ID] = &loaded[i]
	}
	for _, messageID := range order {
		var best *Trip
		for _, tripID := range byMessage[messageID] {
			if host, ok := absorbed[tripID]; ok {
				tripID = host
			}
			trip := byID[tripID]
			if trip == nil {
				continue
			}
			if best == nil || trip.Segments[0].Start < best.Segments[0].Start {
				best = trip
			}
		}
		if best != nil {
			out[messageID] = MessageTrip{Trip: *best, Count: len(byMessage[messageID])}
		}
	}
	return out, nil
}

// SetTripManualStatus dismisses a trip or takes the dismissal back.
func (s *Store) SetTripManualStatus(ctx context.Context, userID, tripID int64, value string) error {
	if !ValidTripManualStatus(value) {
		return fmt.Errorf("trips: unsupported manual status %q", value)
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return err
	}
	at := nowUnix()
	if value == TripManualNone {
		at = 0
	}
	result, err := db.ExecContext(ctx, `UPDATE trips SET manual_status = ?, manual_status_at = ?, updated_at = ?
		WHERE user_id = ? AND id = ?`, value, at, nowUnix(), userID, tripID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// LinkTripSegmentsToCalendar records that the reader created eventID from the
// given legs of one trip. The event must be the reader's own, and every leg
// must belong to the trip; anything else is refused rather than half-written.
func (s *Store) LinkTripSegmentsToCalendar(ctx context.Context, userID, tripID int64, segmentIDs []int64, eventID int64) error {
	if userID <= 0 || tripID <= 0 || eventID <= 0 || len(segmentIDs) == 0 {
		return fmt.Errorf("trips: user_id, trip, legs and event are required")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var owned bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM calendar_events WHERE user_id = ? AND id = ?)`,
		userID, eventID).Scan(&owned); err != nil {
		return err
	}
	if !owned {
		return ErrNotFound
	}
	now := nowUnix()
	for _, segmentID := range segmentIDs {
		// A folded seat reservation is a leg of another trip that the list
		// drew inside this one, so a leg is accepted from any trip of the
		// reader's whose train is one this trip names that day.
		result, err := tx.ExecContext(ctx, `UPDATE trip_segments SET calendar_event_id = ?, calendar_added_at = ?, updated_at = ?
			WHERE user_id = ? AND id = ? AND (trip_id = ? OR EXISTS (
				SELECT 1 FROM trip_segments host WHERE host.user_id = trip_segments.user_id AND host.trip_id = ?
					AND trip_segments.seat_only <> 0 AND trip_segments.number <> ''
					AND host.kind = trip_segments.kind AND host.number = trip_segments.number
					AND LEFT(host.start_local, 10) = LEFT(trip_segments.start_local, 10)))`,
			eventID, now, now, userID, segmentID, tripID, tripID)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return ErrNotFound
		}
	}
	return tx.Commit()
}

// TripNotice is one trip worth a push.
type TripNotice struct {
	TripID    int64
	MessageID int64
	Kind      string
	Title     string
	From      string
	To        string
	Venue     string
	Start     string
	StartAt   int64
}

// TripNoticesAfter returns the trips marked for a push past an endpoint's
// cursor, with the cursor to store once they were delivered.
func (s *Store) TripNoticesAfter(ctx context.Context, userID, after int64) ([]TripNotice, int64, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, after, err
	}
	rows, err := db.QueryContext(ctx, `SELECT t.id, t.kind,
			COALESCE((SELECT tm.message_id FROM trip_messages tm WHERE tm.user_id = t.user_id AND tm.trip_id = t.id ORDER BY tm.message_id DESC LIMIT 1), 0),
			ts.title, ts.from_name, ts.to_name, ts.venue, ts.start_local, ts.start_at
		FROM trips t
		JOIN LATERAL (SELECT title, from_name, to_name, venue, start_local, start_at FROM trip_segments
			WHERE user_id = t.user_id AND trip_id = t.id ORDER BY start_local, id LIMIT 1) ts ON true
		WHERE t.user_id = ? AND t.notify_at > 0 AND t.id > ? AND t.manual_status = ''
		ORDER BY t.id
		LIMIT ?`, userID, after, MaxTripNoticesPerPass)
	if err != nil {
		return nil, after, err
	}
	defer rows.Close()
	cursor := after
	var out []TripNotice
	for rows.Next() {
		var n TripNotice
		if err := rows.Scan(&n.TripID, &n.Kind, &n.MessageID, &n.Title, &n.From, &n.To, &n.Venue, &n.Start, &n.StartAt); err != nil {
			return nil, after, err
		}
		out = append(out, n)
		cursor = n.TripID
	}
	return out, cursor, rows.Err()
}

// ListMessagesNeedingTravelScan returns the next batch of messages to read for
// tickets, newest first: the journeys still to come are in the newest mail.
// Only messages whose raw copy is still on disk are selected.
func (s *Store) ListMessagesNeedingTravelScan(ctx context.Context, userID int64, limit int) ([]TripCandidate, error) {
	if limit <= 0 || limit > TravelBackfillLimit {
		limit = TravelBackfillLimit
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT id, blob_path, date_unix
		FROM messages
		WHERE user_id = ? AND travel_version < ? AND blob_path <> ''
		ORDER BY date_unix DESC, id DESC
		LIMIT ?`, userID, TravelVersion, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TripCandidate, 0, limit)
	for rows.Next() {
		var item TripCandidate
		if err := rows.Scan(&item.ID, &item.BlobPath, &item.Date); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// MarkMessagesTravelScanned stamps messages the pass could not read. Their trip
// links are left alone: a message whose stored copy has gone has not stopped
// naming its ticket.
func (s *Store) MarkMessagesTravelScanned(ctx context.Context, userID int64, ids []int64) error {
	return s.stampMessagesTravelScanned(ctx, userID, ids, false)
}

// ClearMessageTrips stamps messages that were read and named no ticket, and
// detaches whatever an older generation attached to them.
func (s *Store) ClearMessageTrips(ctx context.Context, userID int64, ids []int64) error {
	return s.stampMessagesTravelScanned(ctx, userID, ids, true)
}

func (s *Store) stampMessagesTravelScanned(ctx context.Context, userID int64, ids []int64, detach bool) error {
	if len(ids) == 0 {
		return nil
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return err
	}
	placeholders := make([]string, 0, len(ids))
	idArgs := make([]any, 0, len(ids))
	for _, id := range ids {
		idArgs = append(idArgs, id)
		placeholders = append(placeholders, "?")
	}
	list := strings.Join(placeholders, ", ")
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if detach {
		if _, err := tx.ExecContext(ctx, `DELETE FROM trip_messages WHERE user_id = ? AND message_id IN (`+list+`)`,
			append([]any{userID}, idArgs...)...); err != nil {
			return err
		}
		prior, err := unlinkMessageSegments(ctx, tx, userID, idArgs)
		if err != nil {
			return err
		}
		if err := dropUnnamedSegments(ctx, tx, userID, prior); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET travel_version = ? WHERE user_id = ? AND id IN (`+list+`)`,
		append([]any{TravelVersion, userID}, idArgs...)...); err != nil {
		return err
	}
	return tx.Commit()
}

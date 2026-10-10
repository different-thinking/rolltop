// File overview: Tenant-scoped persistence for the event instances mirrored
// from Google Calendar and Microsoft 365. Events are stored expanded -- one row per occurrence --
// so nothing here has to evaluate a recurrence rule.

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"strings"
	"time"
)

const (
	// CalendarEventStatusConfirmed is an event that is happening.
	CalendarEventStatusConfirmed = "confirmed"
	// CalendarEventStatusTentative is an event whose organizer has not settled it.
	CalendarEventStatusTentative = "tentative"
	// CalendarEventStatusCancelled is Google's tombstone. A delta reports a
	// deleted occurrence this way rather than omitting it, so it is the signal
	// that removes the local row.
	CalendarEventStatusCancelled = "cancelled"
)

const (
	// CalendarResponseNeedsAction is an invitation nobody has answered.
	CalendarResponseNeedsAction = "needsAction"
	// CalendarResponseAccepted is a yes.
	CalendarResponseAccepted = "accepted"
	// CalendarResponseDeclined is a no.
	CalendarResponseDeclined = "declined"
	// CalendarResponseTentative is a maybe.
	CalendarResponseTentative = "tentative"
)

// allDayRangePadding widens a range query so an all-day event cannot fall out
// of the week that shows it. All-day rows are stored at midnight UTC because
// the date they name has no instant, so for a viewer far enough east or west
// the row's bounds sit up to a day outside the range they belong in. Padding
// over-fetches at most two days of all-day rows; the caller draws by date and
// discards the rest.
const allDayRangePadding = 24 * time.Hour

const calendarEventSelectColumns = `id, user_id, calendar_id, external_id, etag, ical_uid,
	summary, description, location, status, start_at, end_at, all_day, time_zone,
	recurring_event_id, organizer_email, organizer_name, attendees_json, my_response,
	html_link, remote_updated_at, link_key, link_primary, online_meeting,
	online_meeting_provider, online_meeting_url`

// CalendarAttendee is one invitee as Google reports them. It is display data:
// nothing queries against it, so the list is stored as JSON on the event.
type CalendarAttendee struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
	// Response is one of the CalendarResponse constants.
	Response string `json:"response,omitempty"`
	Optional bool   `json:"optional,omitempty"`
	// Organizer marks the person who owns the event, Self the connected account
	// itself, and Resource a room or piece of equipment rather than a person.
	Organizer bool `json:"organizer,omitempty"`
	Self      bool `json:"self,omitempty"`
	Resource  bool `json:"resource,omitempty"`
}

// CalendarEvent is one occurrence in one calendar.
type CalendarEvent struct {
	ID         int64
	UserID     int64
	CalendarID int64
	// ExternalID is Google's event id, unique within its calendar.
	ExternalID string
	ETag       string
	// ICalUID identifies the same meeting across the calendars of everyone
	// invited to it, which is how a mailed invitation is matched to it.
	ICalUID     string
	Summary     string
	Description string
	Location    string
	Status      string
	// StartAt and EndAt are half-open. For an all-day event they are midnight
	// UTC of Google's plain dates and must be formatted in UTC; see AllDay.
	StartAt time.Time
	EndAt   time.Time
	AllDay  bool
	// TimeZone is the zone the event was entered in. It is kept for display and
	// for writing an edit back in the same zone; the stored bounds are absolute.
	TimeZone         string
	RecurringEventID string
	OrganizerEmail   string
	OrganizerName    string
	Attendees        []CalendarAttendee
	// MyResponse is the connected account's own answer, lifted out of the
	// attendee list so the invitation card does not have to search it.
	MyResponse      string
	HTMLLink        string
	RemoteUpdatedAt time.Time
	// LinkKey ties together the copies of one event entered once and kept in
	// two calendars. It is read from the event at Google, never invented here,
	// so it means the same thing after a resync. Empty for an ordinary event.
	LinkKey string
	// LinkPrimary marks the copy that was entered, as opposed to the one made
	// in the second calendar. Only it carries the guest list.
	LinkPrimary bool
	// OnlineMeeting marks an event that is held online -- a Teams meeting for
	// a Microsoft calendar. On a write it asks for one to be created; the
	// provider and the join link are only ever read back from the provider.
	OnlineMeeting         bool
	OnlineMeetingProvider string
	// OnlineMeetingURL is the link a participant follows to join, for an
	// online meeting of either provider.
	OnlineMeetingURL string
}

// Linked reports whether this event is one copy of a linked pair.
func (e CalendarEvent) Linked() bool {
	return e.LinkKey != ""
}

// CalendarEventRef is the minimal row a full resync needs to decide whether an
// event it just received is new, changed, or already current.
type CalendarEventRef struct {
	EventID    int64
	ExternalID string
	ETag       string
}

// UpsertCalendarEvent stores one occurrence, replacing the mirror if it exists.
// Google is the leading system for events, so the incoming copy is the whole
// truth: folding the old row back in would make a field cleared at Google
// unrepresentable here.
func (s *Store) UpsertCalendarEvent(ctx context.Context, userID int64, event CalendarEvent) (CalendarEvent, error) {
	externalID := strings.TrimSpace(event.ExternalID)
	if userID <= 0 || event.CalendarID <= 0 || externalID == "" {
		return CalendarEvent{}, ErrNotFound
	}
	ts := nowUnix()
	_, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `INSERT INTO calendar_events
			(user_id, calendar_id, external_id, etag, ical_uid, summary, description,
			 location, status, start_at, end_at, all_day, time_zone, recurring_event_id,
			 organizer_email, organizer_name, attendees_json, my_response, html_link,
			 remote_updated_at, link_key, link_primary, online_meeting, online_meeting_provider,
			 online_meeting_url, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, calendar_id, external_id) DO UPDATE SET
			etag = excluded.etag,
			ical_uid = excluded.ical_uid,
			summary = excluded.summary,
			description = excluded.description,
			location = excluded.location,
			status = excluded.status,
			start_at = excluded.start_at,
			end_at = excluded.end_at,
			all_day = excluded.all_day,
			time_zone = excluded.time_zone,
			recurring_event_id = excluded.recurring_event_id,
			organizer_email = excluded.organizer_email,
			organizer_name = excluded.organizer_name,
			attendees_json = excluded.attendees_json,
			my_response = excluded.my_response,
			html_link = excluded.html_link,
			remote_updated_at = excluded.remote_updated_at,
			link_key = excluded.link_key,
			link_primary = excluded.link_primary,
			online_meeting = excluded.online_meeting,
			online_meeting_provider = excluded.online_meeting_provider,
			online_meeting_url = excluded.online_meeting_url,
			updated_at = excluded.updated_at`,
		userID, event.CalendarID, trimLimit(externalID, 300), trimLimit(event.ETag, 200),
		trimLimit(event.ICalUID, 300), trimLimit(event.Summary, 500),
		trimLimit(event.Description, 20000), trimLimit(event.Location, 1000),
		trimLimit(strings.TrimSpace(event.Status), 40), timeUnix(event.StartAt), timeUnix(event.EndAt),
		boolInt(event.AllDay), trimLimit(event.TimeZone, 100), trimLimit(event.RecurringEventID, 300),
		cleanEmail(event.OrganizerEmail), trimLimit(event.OrganizerName, 300),
		encodeAttendees(event.Attendees), trimLimit(strings.TrimSpace(event.MyResponse), 40),
		trimLimit(event.HTMLLink, 1000), timeUnix(event.RemoteUpdatedAt),
		trimLimit(strings.TrimSpace(event.LinkKey), 100), boolInt(event.LinkPrimary),
		boolInt(event.OnlineMeeting), trimLimit(strings.TrimSpace(event.OnlineMeetingProvider), 60),
		trimLimit(strings.TrimSpace(event.OnlineMeetingURL), 2000), ts, ts)
	if err != nil {
		return CalendarEvent{}, err
	}
	return s.CalendarEventByExternalID(ctx, userID, event.CalendarID, externalID)
}

// ListCalendarEventsInRange returns the events of the given calendars that
// overlap [from, to). Passing no calendar ids returns nothing rather than
// everything: an empty visible set is a legitimate state of the week view, and
// answering it with every event would draw calendars the user switched off.
func (s *Store) ListCalendarEventsInRange(ctx context.Context, userID int64, calendarIDs []int64, from, to time.Time) ([]CalendarEvent, error) {
	if userID <= 0 || len(calendarIDs) == 0 || !to.After(from) {
		return []CalendarEvent{}, nil
	}
	args := make([]any, 0, len(calendarIDs)+3)
	args = append(args, userID)
	for _, id := range calendarIDs {
		args = append(args, id)
	}
	args = append(args, timeUnix(to.Add(allDayRangePadding)), timeUnix(from.Add(-allDayRangePadding)))
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT `+calendarEventSelectColumns+`
		FROM calendar_events
		WHERE user_id = ? AND calendar_id IN (`+sqlPlaceholders(len(calendarIDs))+`)
			AND start_at < ? AND end_at > ?
		ORDER BY start_at ASC, id ASC`, args...)
	if err != nil {
		return nil, err
	}
	return collectCalendarEvents(rows)
}

// ListLinkedCalendarEventsInRange returns every linked event that overlaps
// [from, to), from every calendar the user has -- switched on or not. The week
// view folds each linked pair into one entry, and the entry has to be able to
// say which other calendar holds the event even when that calendar is hidden.
func (s *Store) ListLinkedCalendarEventsInRange(ctx context.Context, userID int64, from, to time.Time) ([]CalendarEvent, error) {
	if userID <= 0 || !to.After(from) {
		return []CalendarEvent{}, nil
	}
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT `+calendarEventSelectColumns+`
		FROM calendar_events
		WHERE user_id = ? AND link_key <> '' AND start_at < ? AND end_at > ?
		ORDER BY start_at ASC, id ASC`,
		userID, timeUnix(to.Add(allDayRangePadding)), timeUnix(from.Add(-allDayRangePadding)))
	if err != nil {
		return nil, err
	}
	return collectCalendarEvents(rows)
}

// ListCalendarEventCopies returns the other copies of one linked event: the
// rows carrying its link key at its start. The start is part of the identity
// because Google hands an extended property down to every occurrence of a
// series, so the key alone names a whole series of appointments; and a copy
// somebody moved at Google is no longer the same appointment and is left out
// rather than dragged along by an edit made to the other one. A row in the
// event's own calendar is never a copy of it either: the pair lives in two
// calendars, and an event duplicated at Google carries its private properties
// along without being the same appointment.
func (s *Store) ListCalendarEventCopies(ctx context.Context, userID int64, event CalendarEvent) ([]CalendarEvent, error) {
	key := strings.TrimSpace(event.LinkKey)
	if userID <= 0 || key == "" {
		return []CalendarEvent{}, nil
	}
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT `+calendarEventSelectColumns+`
		FROM calendar_events
		WHERE user_id = ? AND link_key = ? AND start_at = ? AND calendar_id <> ?
		ORDER BY link_primary DESC, id ASC`,
		userID, key, timeUnix(event.StartAt), event.CalendarID)
	if err != nil {
		return nil, err
	}
	return collectCalendarEvents(rows)
}

// collectCalendarEvents scans every row of an event query and closes it.
func collectCalendarEvents(rows *sql.Rows) ([]CalendarEvent, error) {
	defer rows.Close()
	events := []CalendarEvent{}
	for rows.Next() {
		event, err := scanCalendarEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

// CalendarEvent loads one event, scoped to its owner.
func (s *Store) CalendarEvent(ctx context.Context, userID, eventID int64) (CalendarEvent, error) {
	if userID <= 0 || eventID <= 0 {
		return CalendarEvent{}, ErrNotFound
	}
	return scanCalendarEvent(s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT `+calendarEventSelectColumns+`
		FROM calendar_events WHERE user_id = ? AND id = ?`, userID, eventID))
}

// CalendarEventByExternalID loads the mirror of one Google event.
func (s *Store) CalendarEventByExternalID(ctx context.Context, userID, calendarID int64, externalID string) (CalendarEvent, error) {
	externalID = strings.TrimSpace(externalID)
	if userID <= 0 || calendarID <= 0 || externalID == "" {
		return CalendarEvent{}, ErrNotFound
	}
	return scanCalendarEvent(s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT `+calendarEventSelectColumns+`
		FROM calendar_events WHERE user_id = ? AND calendar_id = ? AND external_id = ?`,
		userID, calendarID, externalID))
}

// DeleteCalendarEvent removes one mirrored event by its local id.
func (s *Store) DeleteCalendarEvent(ctx context.Context, userID, eventID int64) error {
	if userID <= 0 || eventID <= 0 {
		return ErrNotFound
	}
	res, err := s.mustDataDB(ctx, userID).ExecContext(ctx,
		`DELETE FROM calendar_events WHERE user_id = ? AND id = ?`, userID, eventID)
	if err != nil {
		return err
	}
	return requireCalendarRow(res)
}

// DeleteCalendarEventByExternalID removes the mirror of one Google event and
// reports whether there was one. A tombstone for an event Rolltop never had is
// normal on a delta and is not an error.
func (s *Store) DeleteCalendarEventByExternalID(ctx context.Context, userID, calendarID int64, externalID string) (bool, error) {
	externalID = strings.TrimSpace(externalID)
	if userID <= 0 || calendarID <= 0 || externalID == "" {
		return false, nil
	}
	res, err := s.mustDataDB(ctx, userID).ExecContext(ctx,
		`DELETE FROM calendar_events WHERE user_id = ? AND calendar_id = ? AND external_id = ?`,
		userID, calendarID, externalID)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// ListCalendarEventRefs returns every mirrored event of one calendar keyed by
// Google's event id, which is how a full resync tells the events Google still
// returns apart from the ones it has dropped.
func (s *Store) ListCalendarEventRefs(ctx context.Context, userID, calendarID int64) (map[string]CalendarEventRef, error) {
	out := map[string]CalendarEventRef{}
	if userID <= 0 || calendarID <= 0 {
		return out, nil
	}
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx,
		`SELECT id, external_id, etag FROM calendar_events WHERE user_id = ? AND calendar_id = ?`,
		userID, calendarID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ref CalendarEventRef
		if err := rows.Scan(&ref.EventID, &ref.ExternalID, &ref.ETag); err != nil {
			return nil, err
		}
		out[ref.ExternalID] = ref
	}
	return out, rows.Err()
}

// ListCalendarEventRefsStartingIn returns the mirrored events of one calendar
// whose start lies in [from, to), keyed by external id. A read of a window
// that cannot report deletions -- Microsoft's calendarView, which only ever
// says what is there -- concludes that a row it did not return is gone, and
// this is the set it may conclude that about: rows the read would have
// returned had they still existed.
func (s *Store) ListCalendarEventRefsStartingIn(ctx context.Context, userID, calendarID int64, from, to time.Time) (map[string]CalendarEventRef, error) {
	out := map[string]CalendarEventRef{}
	if userID <= 0 || calendarID <= 0 || !to.After(from) {
		return out, nil
	}
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx,
		`SELECT id, external_id, etag FROM calendar_events
			WHERE user_id = ? AND calendar_id = ? AND start_at >= ? AND start_at < ?`,
		userID, calendarID, timeUnix(from), timeUnix(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ref CalendarEventRef
		if err := rows.Scan(&ref.EventID, &ref.ExternalID, &ref.ETag); err != nil {
			return nil, err
		}
		out[ref.ExternalID] = ref
	}
	return out, rows.Err()
}

// DeleteCalendarEventsEndingBefore removes the rows of one calendar that ended
// before a point, which is what falling out of the mirrored window means.
func (s *Store) DeleteCalendarEventsEndingBefore(ctx context.Context, userID, calendarID int64, before time.Time) (int64, error) {
	if userID <= 0 || calendarID <= 0 || before.IsZero() {
		return 0, nil
	}
	res, err := s.mustDataDB(ctx, userID).ExecContext(ctx,
		`DELETE FROM calendar_events WHERE user_id = ? AND calendar_id = ? AND end_at <= ?`,
		userID, calendarID, timeUnix(before))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// encodeAttendees renders the attendee list for storage. An unencodable list
// is stored as none rather than failing the event: the attendees are display
// data, and losing them is a smaller fault than losing the appointment.
func encodeAttendees(attendees []CalendarAttendee) string {
	if len(attendees) == 0 {
		return ""
	}
	encoded, err := json.Marshal(attendees)
	if err != nil {
		log.Printf("calendar event attendees: %v", err)
		return ""
	}
	return string(encoded)
}

func decodeAttendees(raw string) []CalendarAttendee {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var attendees []CalendarAttendee
	if err := json.Unmarshal([]byte(raw), &attendees); err != nil {
		// A row written by an older or corrupted encoder should still render as
		// an event; only its guest list is lost.
		log.Printf("calendar event attendees: %v", err)
		return nil
	}
	return attendees
}

func scanCalendarEvent(dest scanDest) (CalendarEvent, error) {
	var event CalendarEvent
	var attendees string
	var allDay, startAt, endAt, remoteUpdated, linkPrimary, onlineMeeting int64
	if err := dest.Scan(&event.ID, &event.UserID, &event.CalendarID, &event.ExternalID,
		&event.ETag, &event.ICalUID, &event.Summary, &event.Description, &event.Location,
		&event.Status, &startAt, &endAt, &allDay, &event.TimeZone, &event.RecurringEventID,
		&event.OrganizerEmail, &event.OrganizerName, &attendees, &event.MyResponse,
		&event.HTMLLink, &remoteUpdated, &event.LinkKey, &linkPrimary, &onlineMeeting,
		&event.OnlineMeetingProvider, &event.OnlineMeetingURL); err != nil {
		return CalendarEvent{}, err
	}
	event.OnlineMeeting = onlineMeeting != 0
	event.LinkPrimary = linkPrimary != 0
	event.StartAt = unixTime(startAt)
	event.EndAt = unixTime(endAt)
	event.AllDay = allDay != 0
	event.Attendees = decodeAttendees(attendees)
	event.RemoteUpdatedAt = unixTime(remoteUpdated)
	return event, nil
}

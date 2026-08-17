// File overview: Translation between Calendar API resources and Rolltop's own
// rows. The awkward part is time: Google expresses an all-day event as plain
// dates with no instant behind them, and a timed one as an offset timestamp.

package googlecalendar

import (
	"strings"
	"time"

	"rolltop/backend/store"
)

// dateLayout is Google's plain-date format for all-day events.
const dateLayout = "2006-01-02"

// ToCalendarUpsert turns one calendar-list entry into the row the store keeps.
func ToCalendarUpsert(entry CalendarListEntry, connectionID int64) store.CalendarUpsert {
	return store.CalendarUpsert{
		GoogleConnectionID: connectionID,
		GoogleCalendarID:   strings.TrimSpace(entry.ID),
		Summary:            strings.TrimSpace(entry.Name()),
		Description:        strings.TrimSpace(entry.Description),
		TimeZone:           strings.TrimSpace(entry.TimeZone),
		Color:              strings.TrimSpace(entry.BackgroundColor),
		AccessRole:         strings.TrimSpace(entry.AccessRole),
		IsPrimary:          entry.Primary,
		// Only used the first time a calendar is seen. A calendar the account
		// already switched off at Google arrives switched off here rather than
		// dumping a subscribed holiday feed into the first week the user opens.
		Selected: entry.Selected,
	}
}

// ToEvent turns one occurrence into the row the store keeps. calendarID is the
// local calendar the event belongs to; Google's own event id travels along as
// the external id.
func ToEvent(event Event, calendarID int64) store.CalendarEvent {
	start, end, allDay := eventBounds(event)
	out := store.CalendarEvent{
		CalendarID:       calendarID,
		ExternalID:       strings.TrimSpace(event.ID),
		ETag:             strings.TrimSpace(event.ETag),
		ICalUID:          strings.TrimSpace(event.ICalUID),
		Summary:          strings.TrimSpace(event.Summary),
		Description:      event.Description,
		Location:         strings.TrimSpace(event.Location),
		Status:           strings.TrimSpace(event.Status),
		StartAt:          start,
		EndAt:            end,
		AllDay:           allDay,
		TimeZone:         strings.TrimSpace(firstNonEmptyZone(event.Start.TimeZone, event.End.TimeZone)),
		RecurringEventID: strings.TrimSpace(event.RecurringEventID),
		OrganizerEmail:   strings.TrimSpace(event.Organizer.Email),
		OrganizerName:    strings.TrimSpace(event.Organizer.DisplayName),
		HTMLLink:         strings.TrimSpace(event.HTMLLink),
		RemoteUpdatedAt:  parseTimestamp(event.Updated),
	}
	out.Attendees, out.MyResponse = attendees(event.Attendees)
	return out
}

// eventBounds resolves Google's start/end pair into a half-open interval.
//
// An all-day event carries plain dates, which name a day rather than a moment.
// They are anchored at midnight UTC because any other anchor would be a guess
// about which zone the day belongs to, and that guess moves a public holiday
// onto the wrong date for everyone in a different one. Google's end date is
// already exclusive, so the half-open interval needs no adjustment.
//
// An event whose end is missing or not after its start is stored as a single
// point in time rather than dropped: the week view can draw it at a minimum
// height, and losing an appointment because Google sent an odd pair would be
// the worse failure.
func eventBounds(event Event) (time.Time, time.Time, bool) {
	if date := strings.TrimSpace(event.Start.Date); date != "" {
		start := parseDate(date)
		end := parseDate(strings.TrimSpace(event.End.Date))
		if end.IsZero() || !end.After(start) {
			end = start.AddDate(0, 0, 1)
		}
		return start, end, true
	}
	start := parseTimestamp(event.Start.DateTime)
	end := parseTimestamp(event.End.DateTime)
	if end.IsZero() || end.Before(start) {
		end = start
	}
	return start, end, false
}

// attendees converts the guest list and lifts the connected account's own
// answer out of it, which is what the invitation card needs without searching
// the list again on every render.
func attendees(list []EventAttendee) ([]store.CalendarAttendee, string) {
	if len(list) == 0 {
		return nil, ""
	}
	out := make([]store.CalendarAttendee, 0, len(list))
	mine := ""
	for _, attendee := range list {
		email := strings.TrimSpace(attendee.Email)
		name := strings.TrimSpace(attendee.DisplayName)
		if email == "" && name == "" {
			continue
		}
		response := strings.TrimSpace(attendee.ResponseStatus)
		if attendee.Self {
			mine = response
		}
		out = append(out, store.CalendarAttendee{
			Email:     email,
			Name:      name,
			Response:  response,
			Optional:  attendee.Optional,
			Organizer: attendee.Organizer,
			Self:      attendee.Self,
			Resource:  attendee.Resource,
		})
	}
	if len(out) == 0 {
		return nil, ""
	}
	return out, mine
}

// ToWrite renders a stored event as the body of an insert or an update. The
// guest list is deliberately left out: whether a write may touch it is a
// decision only the caller can make, and getting it wrong resets every RSVP.
func ToWrite(event store.CalendarEvent) EventWrite {
	return EventWrite{
		Summary:     event.Summary,
		Description: event.Description,
		Location:    event.Location,
		Start:       toEventDateTime(event.StartAt, event.AllDay, event.TimeZone),
		End:         toEventDateTime(event.EndAt, event.AllDay, event.TimeZone),
	}
}

// MergeAttendees renders a guest list for a write. current is what Google holds
// right now and wanted is what the dialog submitted.
//
// Everybody who stays on the list keeps the answer they already gave. Google
// replaces the whole list on a write, so restating a guest without their
// responseStatus is what would silently reset their invitation to unanswered --
// and, because a guest list means the write notifies, mail them about it.
func MergeAttendees(current []EventAttendee, wanted []store.CalendarAttendee) []EventAttendee {
	existing := make(map[string]EventAttendee, len(current))
	for _, attendee := range current {
		existing[guestKey(attendee.Email)] = attendee
	}
	out := make([]EventAttendee, 0, len(wanted))
	for _, attendee := range wanted {
		email := strings.TrimSpace(attendee.Email)
		if email == "" {
			// Google addresses an attendee by mail address; one without is a
			// row it would reject for the whole request.
			continue
		}
		merged := EventAttendee{
			Email:       email,
			DisplayName: strings.TrimSpace(attendee.Name),
			Optional:    attendee.Optional,
			Resource:    attendee.Resource,
		}
		if previous, ok := existing[guestKey(email)]; ok {
			merged.ResponseStatus = previous.ResponseStatus
			merged.Organizer = previous.Organizer
			merged.Self = previous.Self
			if merged.DisplayName == "" {
				merged.DisplayName = previous.DisplayName
			}
		}
		out = append(out, merged)
	}
	return out
}

// SameGuestList reports whether two lists name the same people. It is what
// decides whether a write has to carry the guest list at all: an edit that
// leaves the guests alone is a strictly safer request without it.
func SameGuestList(a, b []store.CalendarAttendee) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, attendee := range a {
		key := guestKey(attendee.Email)
		if key == "" {
			continue
		}
		seen[key]++
	}
	for _, attendee := range b {
		key := guestKey(attendee.Email)
		if key == "" {
			continue
		}
		seen[key]--
		if seen[key] == 0 {
			delete(seen, key)
		}
	}
	return len(seen) == 0
}

// guestKey normalizes an address for comparison. Google treats an invitee's
// address case-insensitively, and a list differing only in case is the same
// list.
func guestKey(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func toEventDateTime(at time.Time, allDay bool, zone string) EventDateTime {
	if allDay {
		// Written back in UTC because that is how it was read: the stored value
		// is midnight UTC of the date Google named, so formatting it in any
		// other zone would shift the date by a day.
		return EventDateTime{Date: at.UTC().Format(dateLayout)}
	}
	out := EventDateTime{DateTime: at.UTC().Format(time.RFC3339)}
	if zone = strings.TrimSpace(zone); zone != "" {
		// The timestamp already carries the instant; the zone only tells Google
		// which one to display it in, so a recurring edit keeps its wall time.
		out.TimeZone = zone
	}
	return out
}

// AnswerAttendees returns the guest list with the connected account's own
// answer changed. Google has no merge semantics here, so the whole list travels
// on every RSVP and everybody else's answer has to survive the round trip.
func AnswerAttendees(event Event, response string) ([]EventAttendee, bool) {
	response = strings.TrimSpace(response)
	if response == "" {
		return nil, false
	}
	out := make([]EventAttendee, 0, len(event.Attendees))
	answered := false
	for _, attendee := range event.Attendees {
		if attendee.Self {
			attendee.ResponseStatus = response
			answered = true
		}
		out = append(out, attendee)
	}
	if !answered {
		return nil, false
	}
	return out, true
}

func parseDate(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.ParseInLocation(dateLayout, value, time.UTC)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

// parseTimestamp reads Google's RFC 3339 timestamps. A value it cannot read
// becomes the zero time rather than an error: one unreadable field should cost
// an event its position in the week, not abort the page it arrived on.
func parseTimestamp(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

func firstNonEmptyZone(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

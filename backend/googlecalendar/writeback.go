// File overview: Local calendar changes travelling to Google. Google is the
// leading system, so every write goes there first and the local row is then
// made to match what Google actually accepted -- never the other way round,
// which would let the two copies disagree the moment a write is rejected.

package googlecalendar

import (
	"context"
	"errors"
	"fmt"
	"time"

	"rolltop/backend/store"
)

// writeTimeout bounds one write-back. It runs inside a request the user is
// waiting on, so it is far shorter than a sync.
const writeTimeout = 30 * time.Second

// ErrRemoteChanged reports that the event was changed at Google since Rolltop
// last read it. The local row has been refreshed to Google's version by the
// time this is returned, so the caller can show what the event looks like now
// instead of leaving the user with a copy that no longer exists anywhere.
var ErrRemoteChanged = errors.New("this event was changed in Google")

// ErrRemoteDeleted reports that the event was removed at Google while it was
// being edited here. The local mirror is gone by the time this is returned;
// there is no version left to show, which is what separates it from
// ErrRemoteChanged.
var ErrRemoteDeleted = errors.New("this event was deleted in Google")

// ErrReadOnlyCalendar reports a write to a calendar the account may only read.
// Google would refuse it, and refusing here means the user is told why rather
// than watching a save fail with a generic upstream error.
var ErrReadOnlyCalendar = errors.New("this calendar is shared read-only")

// ErrNotAnInvitation reports an answer to an event the account was not invited
// to. Only an attendee has a response to give.
var ErrNotAnInvitation = errors.New("this event has no invitation to answer")

// CreateRemoteEvent adds an event to a Google calendar and stores the result
// locally. The local row is built from Google's response rather than from the
// submitted values, so the id, etag and any normalization Google applied are
// what Rolltop ends up holding.
func (s *Syncer) CreateRemoteEvent(ctx context.Context, userID, calendarID int64, event store.CalendarEvent) (store.CalendarEvent, error) {
	calendar, err := s.writableCalendar(ctx, userID, calendarID)
	if err != nil {
		return store.CalendarEvent{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	write := ToWrite(event)
	// A create has no existing list to preserve, so the guest list always
	// travels: an event with nobody invited has to be expressible too.
	guests := MergeAttendees(nil, event.Attendees)
	write.Attendees = &guests

	var created Event
	if err := s.withToken(ctx, userID, calendar.GoogleConnectionID, func(token string) error {
		var callErr error
		created, callErr = s.client().CreateEvent(ctx, token, calendar.GoogleCalendarID, write, len(guests) > 0)
		return callErr
	}); err != nil {
		return store.CalendarEvent{}, err
	}
	return s.Store.UpsertCalendarEvent(ctx, userID, ToEvent(created, calendar.ID))
}

// UpdateRemoteEvent pushes an edit of a mirrored event and then applies what
// Google accepted to the local row.
//
// On a conflict the local row is replaced with Google's current version and
// ErrRemoteChanged is returned. Discarding the submitted edit is the direct
// consequence of Google being the leading system: keeping it locally would
// produce an appointment that disagrees with the calendar it lives in, and the
// next sync would silently undo it anyway.
func (s *Syncer) UpdateRemoteEvent(ctx context.Context, userID int64, existing store.CalendarEvent, edited store.CalendarEvent) (store.CalendarEvent, error) {
	calendar, err := s.writableCalendar(ctx, userID, existing.CalendarID)
	if err != nil {
		return store.CalendarEvent{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	write := ToWrite(edited)
	// The guests are told about any change to a meeting, including one that
	// does not touch the list -- a moved start time is exactly what they need
	// to hear about.
	notify := len(existing.Attendees) > 0 || len(edited.Attendees) > 0
	if !SameGuestList(existing.Attendees, edited.Attendees) {
		// Only now does the list travel, and only after reading back what
		// Google currently holds. Google replaces the list wholesale, so
		// sending the mirror's copy would drop anybody invited since the last
		// poll and reset the answer of everybody who stays.
		current, err := s.readRemote(ctx, userID, calendar, existing)
		if err != nil {
			return store.CalendarEvent{}, err
		}
		if current.ETag != existing.ETag {
			// The event moved on at Google since the mirror was written, so the
			// PATCH below would be rejected on that same etag. Resolving the
			// conflict here saves a round trip that can only fail.
			//
			// Substituting the etag just read is what must not happen instead:
			// it would make the write succeed and quietly overwrite whatever
			// changed remotely. Google is the leading system, so the submitted
			// edit loses and the user is shown the version that won.
			return s.applyAdopted(ctx, userID, calendar, existing, current)
		}
		guests := MergeAttendees(current.Attendees, edited.Attendees)
		write.Attendees = &guests
	}

	var updated Event
	err = s.withToken(ctx, userID, calendar.GoogleConnectionID, func(token string) error {
		var callErr error
		updated, callErr = s.client().UpdateEvent(ctx, token, calendar.GoogleCalendarID,
			existing.ExternalID, existing.ETag, write, notify)
		return callErr
	})
	if errors.Is(err, ErrConflict) {
		return s.adoptRemote(ctx, userID, calendar, existing)
	}
	if errors.Is(err, ErrNotFound) {
		if delErr := s.Store.DeleteCalendarEvent(ctx, userID, existing.ID); delErr != nil && !store.IsNotFound(delErr) {
			return store.CalendarEvent{}, delErr
		}
		return store.CalendarEvent{}, ErrRemoteDeleted
	}
	if err != nil {
		return store.CalendarEvent{}, err
	}
	return s.Store.UpsertCalendarEvent(ctx, userID, ToEvent(updated, calendar.ID))
}

// DeleteRemoteEvent removes an event at Google and then locally.
//
// The order is the point: a delete that only succeeded locally would come back
// on the next sync, and a user who confirmed the deletion would watch the
// appointment reappear. A Google failure therefore leaves both copies intact.
func (s *Syncer) DeleteRemoteEvent(ctx context.Context, userID int64, event store.CalendarEvent) error {
	calendar, err := s.writableCalendar(ctx, userID, event.CalendarID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	if err := s.withToken(ctx, userID, calendar.GoogleConnectionID, func(token string) error {
		return s.client().DeleteEvent(ctx, token, calendar.GoogleCalendarID,
			event.ExternalID, event.ETag, len(event.Attendees) > 0)
	}); err != nil {
		return err
	}
	if err := s.Store.DeleteCalendarEvent(ctx, userID, event.ID); err != nil && !store.IsNotFound(err) {
		return err
	}
	return nil
}

// RespondToRemoteEvent answers an invitation.
//
// The guest list is read back from Google first rather than taken from the
// local mirror: Google has no merge semantics for attendees, so the whole list
// travels on every answer, and sending a list that is a poll interval old would
// silently drop everybody invited since. Answering is allowed on a read-only
// calendar, because an invitee's own response is the one thing Google lets them
// change on somebody else's event.
func (s *Syncer) RespondToRemoteEvent(ctx context.Context, userID int64, event store.CalendarEvent, response string) (store.CalendarEvent, error) {
	if err := s.ready(); err != nil {
		return store.CalendarEvent{}, err
	}
	if !validResponse(response) {
		return store.CalendarEvent{}, fmt.Errorf("%w: unknown response %q", ErrNotAnInvitation, response)
	}
	calendar, err := s.usableCalendar(ctx, userID, event.CalendarID)
	if err != nil {
		return store.CalendarEvent{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	current, err := s.readRemote(ctx, userID, calendar, event)
	if err != nil {
		return store.CalendarEvent{}, err
	}
	attendees, ok := AnswerAttendees(current, response)
	if !ok {
		return store.CalendarEvent{}, ErrNotAnInvitation
	}

	var updated Event
	err = s.withToken(ctx, userID, calendar.GoogleConnectionID, func(token string) error {
		var callErr error
		updated, callErr = s.client().RespondToEvent(ctx, token, calendar.GoogleCalendarID,
			event.ExternalID, current.ETag, attendees)
		return callErr
	})
	if errors.Is(err, ErrConflict) {
		return s.adoptRemote(ctx, userID, calendar, event)
	}
	if err != nil {
		return store.CalendarEvent{}, err
	}
	return s.Store.UpsertCalendarEvent(ctx, userID, ToEvent(updated, calendar.ID))
}

// readRemote reads Google's current version of one mirrored event. An event
// Google no longer has takes the local mirror with it and is reported as such,
// because there is no version left for the caller to work from.
func (s *Syncer) readRemote(ctx context.Context, userID int64, calendar store.Calendar, event store.CalendarEvent) (Event, error) {
	var current Event
	err := s.withToken(ctx, userID, calendar.GoogleConnectionID, func(token string) error {
		var callErr error
		current, callErr = s.client().GetEvent(ctx, token, calendar.GoogleCalendarID, event.ExternalID)
		return callErr
	})
	if errors.Is(err, ErrNotFound) {
		if delErr := s.Store.DeleteCalendarEvent(ctx, userID, event.ID); delErr != nil && !store.IsNotFound(delErr) {
			return Event{}, delErr
		}
		return Event{}, ErrRemoteDeleted
	}
	if err != nil {
		return Event{}, err
	}
	return current, nil
}

// adoptRemote replaces the local row with Google's current version of the event
// and returns it alongside ErrRemoteChanged. An event Google no longer has is
// deleted here too: it is gone, and leaving a mirror of it behind would
// resurrect it on the next write.
func (s *Syncer) adoptRemote(ctx context.Context, userID int64, calendar store.Calendar, existing store.CalendarEvent) (store.CalendarEvent, error) {
	current, err := s.readRemote(ctx, userID, calendar, existing)
	if err != nil {
		return store.CalendarEvent{}, err
	}
	return s.applyAdopted(ctx, userID, calendar, existing, current)
}

// applyAdopted writes a version already read from Google over the local row. It
// is separate from adoptRemote so a caller holding a fresh copy -- the guest
// list read that just found a diverged etag -- does not fetch it a second time.
func (s *Syncer) applyAdopted(ctx context.Context, userID int64, calendar store.Calendar, existing store.CalendarEvent, current Event) (store.CalendarEvent, error) {
	if current.IsCancelled() {
		// A cancelled occurrence is Google's tombstone, not a version to show.
		if delErr := s.Store.DeleteCalendarEvent(ctx, userID, existing.ID); delErr != nil && !store.IsNotFound(delErr) {
			return store.CalendarEvent{}, delErr
		}
		return store.CalendarEvent{}, ErrRemoteDeleted
	}
	refreshed, err := s.Store.UpsertCalendarEvent(ctx, userID, ToEvent(current, calendar.ID))
	if err != nil {
		return store.CalendarEvent{}, err
	}
	return refreshed, ErrRemoteChanged
}

// usableCalendar loads a calendar and checks that the connection behind it can
// still talk to Google at all.
func (s *Syncer) usableCalendar(ctx context.Context, userID, calendarID int64) (store.Calendar, error) {
	if err := s.ready(); err != nil {
		return store.Calendar{}, err
	}
	calendar, err := s.Store.Calendar(ctx, userID, calendarID)
	if err != nil {
		return store.Calendar{}, err
	}
	connection, err := s.Connections.Get(ctx, userID, calendar.GoogleConnectionID)
	if err != nil {
		return store.Calendar{}, err
	}
	if !s.scopeGranted(connection) {
		return store.Calendar{}, ErrScopeMissing
	}
	if connection.NeedsReauth() {
		return store.Calendar{}, ErrUnauthorized
	}
	return calendar, nil
}

// writableCalendar additionally refuses a calendar the account may only read.
func (s *Syncer) writableCalendar(ctx context.Context, userID, calendarID int64) (store.Calendar, error) {
	calendar, err := s.usableCalendar(ctx, userID, calendarID)
	if err != nil {
		return store.Calendar{}, err
	}
	if !calendar.CanWrite() {
		return store.Calendar{}, ErrReadOnlyCalendar
	}
	return calendar, nil
}

func validResponse(response string) bool {
	switch response {
	case store.CalendarResponseAccepted, store.CalendarResponseDeclined,
		store.CalendarResponseTentative, store.CalendarResponseNeedsAction:
		return true
	}
	return false
}

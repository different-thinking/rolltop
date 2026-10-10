// File overview: Local calendar changes travelling to Microsoft 365. Microsoft
// is the leading system, so every write goes there first and the local row is
// then made to match what Graph accepted -- never the other way round.

package m365calendar

import (
	"context"
	"errors"
	"fmt"
	"time"

	"rolltop/backend/calendarlink"
	"rolltop/backend/store"
)

// writeTimeout bounds one write-back; it runs inside a request the reader is
// waiting on.
const writeTimeout = 30 * time.Second

var (
	// ErrRemoteChanged reports that the event changed in Outlook since
	// Rolltop last read it. The local row holds Microsoft's version by the time
	// this is returned.
	ErrRemoteChanged = calendarlink.Sentinel("this event was changed in Microsoft 365", calendarlink.ErrRemoteChanged)
	// ErrRemoteDeleted reports that the event was removed in Microsoft 365
	// while it was being edited here; the local mirror is gone with it.
	ErrRemoteDeleted = calendarlink.Sentinel("this event was deleted in Microsoft 365", calendarlink.ErrRemoteDeleted)
	// ErrReadOnlyCalendar reports a write to a calendar shared read-only.
	ErrReadOnlyCalendar = calendarlink.Sentinel("this calendar is shared read-only", calendarlink.ErrReadOnlyCalendar)
	// ErrNotAnInvitation reports an answer to an event nobody invited the
	// reader to -- including one they organize themselves.
	ErrNotAnInvitation = calendarlink.Sentinel("this event has no invitation to answer", calendarlink.ErrNotAnInvitation)
	// ErrNoOnlineMeeting reports a Teams meeting asked for in a calendar that
	// offers none.
	ErrNoOnlineMeeting = calendarlink.Sentinel("this calendar cannot hold Teams meetings", calendarlink.ErrNoOnlineMeeting)
)

// CreateRemoteEvent adds an event to a Microsoft calendar and stores what
// Graph answered. Graph sends the invitations of an event with attendees, and
// creates the Teams meeting of one that asks for it, in the same call.
func (s *Syncer) CreateRemoteEvent(ctx context.Context, userID, calendarID int64, event store.CalendarEvent) (store.CalendarEvent, error) {
	calendar, connection, err := s.writableCalendar(ctx, userID, calendarID)
	if err != nil {
		return store.CalendarEvent{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	payload := basePayload(event)
	payload["body"] = bodyPayload(event.Description)
	// The reader creating the event is its organizer, never one of its guests.
	payload["attendees"] = graphAttendees(event.Attendees, connection.Email)
	if event.OnlineMeeting {
		provider, ok := chooseMeetingProvider(calendar.OnlineMeetingProviders)
		if !ok {
			return store.CalendarEvent{}, ErrNoOnlineMeeting
		}
		payload["isOnlineMeeting"] = true
		payload["onlineMeetingProvider"] = provider
	}

	var created Event
	if err := s.withToken(ctx, userID, connection.ID, func(token string) error {
		var callErr error
		created, callErr = s.client().CreateEvent(ctx, token, calendar.RemoteCalendarID, payload)
		return callErr
	}); err != nil {
		return store.CalendarEvent{}, err
	}
	return s.storeWritten(ctx, userID, calendar, connection, created, event)
}

// UpdateRemoteEvent pushes an edit and applies what Graph accepted.
//
// Graph merges a PATCH property by property, so only what the edit changed
// beyond the dialog's own fields travels: the notes only when they were
// edited (rewriting them would turn an organizer's formatted agenda into the
// text it was read as), the guest list only when it changed (Graph sends every
// guest an update for it), and a Teams meeting only when one is newly asked
// for -- Outlook keeps a meeting online once it is, whatever a later write
// says.
//
// On a conflict the local row is replaced with Microsoft's version and
// ErrRemoteChanged is returned: the leading system's version wins.
func (s *Syncer) UpdateRemoteEvent(ctx context.Context, userID int64, existing, edited store.CalendarEvent) (store.CalendarEvent, error) {
	calendar, connection, err := s.writableCalendar(ctx, userID, existing.CalendarID)
	if err != nil {
		return store.CalendarEvent{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	payload := basePayload(edited)
	if edited.Description != existing.Description {
		payload["body"] = bodyPayload(edited.Description)
	}
	if edited.OnlineMeeting && !existing.OnlineMeeting {
		provider, ok := chooseMeetingProvider(calendar.OnlineMeetingProviders)
		if !ok {
			return store.CalendarEvent{}, ErrNoOnlineMeeting
		}
		payload["isOnlineMeeting"] = true
		payload["onlineMeetingProvider"] = provider
	}
	organizer := existing.OrganizerEmail
	if organizer == "" {
		organizer = connection.Email
	}
	if !sameGuests(existing.Attendees, edited.Attendees, organizer) {
		// Read Microsoft's current version first. A list sent against a
		// mirror a poll interval old would drop anybody invited since, and an
		// event that moved on in Outlook is a conflict to resolve now rather
		// than after a write that can only fail.
		current, err := s.readRemote(ctx, userID, connection, existing)
		if err != nil {
			return store.CalendarEvent{}, err
		}
		if current.version() != existing.ETag {
			return s.applyAdopted(ctx, userID, calendar, connection, current)
		}
		payload["attendees"] = graphAttendees(edited.Attendees, organizer)
	}

	var updated Event
	err = s.withToken(ctx, userID, connection.ID, func(token string) error {
		var callErr error
		updated, callErr = s.client().UpdateEvent(ctx, token, existing.ExternalID, existing.ETag, payload)
		return callErr
	})
	if errors.Is(err, ErrConflict) {
		return s.adoptRemote(ctx, userID, calendar, connection, existing)
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
	return s.storeWritten(ctx, userID, calendar, connection, updated, edited)
}

// DeleteRemoteEvent removes an event in Microsoft 365 and then locally. A
// meeting the reader organizes is cancelled for its guests by the same call.
// A refusal leaves both copies, so a confirmed delete never comes back on the
// next sync.
func (s *Syncer) DeleteRemoteEvent(ctx context.Context, userID int64, event store.CalendarEvent) error {
	_, connection, err := s.writableCalendar(ctx, userID, event.CalendarID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	if err := s.withToken(ctx, userID, connection.ID, func(token string) error {
		return s.client().DeleteEvent(ctx, token, event.ExternalID)
	}); err != nil {
		return err
	}
	if err := s.Store.DeleteCalendarEvent(ctx, userID, event.ID); err != nil && !store.IsNotFound(err) {
		return err
	}
	return nil
}

// RespondToRemoteEvent answers an invitation and tells the organizer. It works
// in a calendar shared read-only too: an invitee's answer is the one thing
// they may change on somebody else's meeting.
func (s *Syncer) RespondToRemoteEvent(ctx context.Context, userID int64, event store.CalendarEvent, response string) (store.CalendarEvent, error) {
	if event.MyResponse == "" {
		return store.CalendarEvent{}, ErrNotAnInvitation
	}
	action, ok := responseAction(response)
	if !ok {
		// Graph has no way to take an answer back to "not answered".
		return store.CalendarEvent{}, fmt.Errorf("%w: unknown response %q", ErrNotAnInvitation, response)
	}
	calendar, connection, err := s.usableCalendar(ctx, userID, event.CalendarID)
	if err != nil {
		return store.CalendarEvent{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	err = s.withToken(ctx, userID, connection.ID, func(token string) error {
		return s.client().RespondToEvent(ctx, token, event.ExternalID, action)
	})
	if errors.Is(err, ErrNotFound) {
		if delErr := s.Store.DeleteCalendarEvent(ctx, userID, event.ID); delErr != nil && !store.IsNotFound(delErr) {
			return store.CalendarEvent{}, delErr
		}
		return store.CalendarEvent{}, ErrRemoteDeleted
	}
	if err != nil {
		return store.CalendarEvent{}, err
	}
	// The answer returns no event; reading it back is what puts the answer,
	// and anything else that changed, into the mirror.
	current, err := s.readRemote(ctx, userID, connection, event)
	if err != nil {
		return store.CalendarEvent{}, err
	}
	return s.Store.UpsertCalendarEvent(ctx, userID, ToEvent(current, calendar.ID, connection.Email))
}

// storeWritten stores the event a create or update answered with. The answer
// does not carry the link property -- Graph returns extended properties only
// when asked for by $expand, which a write cannot do -- so the link written is
// the link stored.
func (s *Syncer) storeWritten(ctx context.Context, userID int64, calendar store.Calendar, connection store.MicrosoftConnection, written Event, submitted store.CalendarEvent) (store.CalendarEvent, error) {
	row := ToEvent(written, calendar.ID, connection.Email)
	if row.LinkKey == "" {
		row.LinkKey, row.LinkPrimary = submitted.LinkKey, submitted.LinkPrimary
	}
	return s.Store.UpsertCalendarEvent(ctx, userID, row)
}

// readRemote reads Microsoft's current version of one mirrored event. An
// event Graph no longer has takes the local mirror with it.
func (s *Syncer) readRemote(ctx context.Context, userID int64, connection store.MicrosoftConnection, event store.CalendarEvent) (Event, error) {
	var current Event
	err := s.withToken(ctx, userID, connection.ID, func(token string) error {
		var callErr error
		current, callErr = s.client().GetEvent(ctx, token, event.ExternalID)
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

func (s *Syncer) adoptRemote(ctx context.Context, userID int64, calendar store.Calendar, connection store.MicrosoftConnection, existing store.CalendarEvent) (store.CalendarEvent, error) {
	current, err := s.readRemote(ctx, userID, connection, existing)
	if err != nil {
		return store.CalendarEvent{}, err
	}
	return s.applyAdopted(ctx, userID, calendar, connection, current)
}

// applyAdopted writes a version already read from Microsoft over the local row
// and reports that the reader's edit lost.
func (s *Syncer) applyAdopted(ctx context.Context, userID int64, calendar store.Calendar, connection store.MicrosoftConnection, current Event) (store.CalendarEvent, error) {
	refreshed, err := s.Store.UpsertCalendarEvent(ctx, userID, ToEvent(current, calendar.ID, connection.Email))
	if err != nil {
		return store.CalendarEvent{}, err
	}
	return refreshed, ErrRemoteChanged
}

// usableCalendar loads a Microsoft calendar and the connection behind it, and
// checks that connection can still talk to Graph.
func (s *Syncer) usableCalendar(ctx context.Context, userID, calendarID int64) (store.Calendar, store.MicrosoftConnection, error) {
	if err := s.ready(); err != nil {
		return store.Calendar{}, store.MicrosoftConnection{}, err
	}
	calendar, err := s.Store.Calendar(ctx, userID, calendarID)
	if err != nil {
		return store.Calendar{}, store.MicrosoftConnection{}, err
	}
	if !calendar.IsMicrosoft() {
		return store.Calendar{}, store.MicrosoftConnection{}, fmt.Errorf("%w: calendar %d is not a Microsoft calendar", ErrNotFound, calendarID)
	}
	connection, err := s.usableConnection(ctx, userID, calendar.MicrosoftConnectionID)
	if err != nil {
		return store.Calendar{}, store.MicrosoftConnection{}, err
	}
	return calendar, connection, nil
}

// writableCalendar additionally refuses a calendar the account may only read.
func (s *Syncer) writableCalendar(ctx context.Context, userID, calendarID int64) (store.Calendar, store.MicrosoftConnection, error) {
	calendar, connection, err := s.usableCalendar(ctx, userID, calendarID)
	if err != nil {
		return store.Calendar{}, store.MicrosoftConnection{}, err
	}
	if !calendar.CanWrite() {
		return store.Calendar{}, store.MicrosoftConnection{}, ErrReadOnlyCalendar
	}
	return calendar, connection, nil
}

// File overview: The dispatcher between the calendar surface and the provider
// a calendar belongs to. The web layer acts on calendars, not on providers: a
// week mixes Google and Microsoft 365 calendars, and an event entered in one
// can be copied into the other. The router looks up which provider a calendar
// is mirrored from and hands each write to that provider's backend, which is
// also what lets one linked pair span both.

package calendarlink

import (
	"context"

	"rolltop/backend/store"
)

// Backend is one provider's calendar implementation.
type Backend interface {
	Writer
	RespondToRemoteEvent(ctx context.Context, userID int64, event store.CalendarEvent, response string) (store.CalendarEvent, error)
	SyncCalendar(ctx context.Context, userID, calendarID int64) error
}

// CalendarLoader is the slice of the store the router reads.
type CalendarLoader interface {
	Calendar(ctx context.Context, userID, calendarID int64) (store.Calendar, error)
}

// Router hands each calendar operation to the backend of the calendar's
// provider. A nil backend means the provider is not configured on this server.
type Router struct {
	Calendars CalendarLoader
	Google    Backend
	Microsoft Backend
}

// Available reports whether any provider can be written to at all.
func (r *Router) Available() bool {
	return r != nil && (r.Google != nil || r.Microsoft != nil)
}

func (r *Router) backendFor(ctx context.Context, userID, calendarID int64) (Backend, error) {
	if r == nil || r.Calendars == nil {
		return nil, ErrProviderUnavailable
	}
	calendar, err := r.Calendars.Calendar(ctx, userID, calendarID)
	if err != nil {
		return nil, err
	}
	return r.backendForCalendar(calendar)
}

func (r *Router) backendForCalendar(calendar store.Calendar) (Backend, error) {
	var backend Backend
	if calendar.IsMicrosoft() {
		backend = r.Microsoft
	} else {
		backend = r.Google
	}
	if backend == nil {
		return nil, ErrProviderUnavailable
	}
	return backend, nil
}

// CreateRemoteEvent creates an event in whichever provider holds the calendar.
func (r *Router) CreateRemoteEvent(ctx context.Context, userID, calendarID int64, event store.CalendarEvent) (store.CalendarEvent, error) {
	backend, err := r.backendFor(ctx, userID, calendarID)
	if err != nil {
		return store.CalendarEvent{}, err
	}
	return backend.CreateRemoteEvent(ctx, userID, calendarID, event)
}

// UpdateRemoteEvent edits an event in the provider of its own calendar.
func (r *Router) UpdateRemoteEvent(ctx context.Context, userID int64, existing, edited store.CalendarEvent) (store.CalendarEvent, error) {
	backend, err := r.backendFor(ctx, userID, existing.CalendarID)
	if err != nil {
		return store.CalendarEvent{}, err
	}
	return backend.UpdateRemoteEvent(ctx, userID, existing, edited)
}

// DeleteRemoteEvent deletes an event in the provider of its own calendar.
func (r *Router) DeleteRemoteEvent(ctx context.Context, userID int64, event store.CalendarEvent) error {
	backend, err := r.backendFor(ctx, userID, event.CalendarID)
	if err != nil {
		return err
	}
	return backend.DeleteRemoteEvent(ctx, userID, event)
}

// RespondToRemoteEvent answers an invitation in the provider of the event's
// calendar.
func (r *Router) RespondToRemoteEvent(ctx context.Context, userID int64, event store.CalendarEvent, response string) (store.CalendarEvent, error) {
	backend, err := r.backendFor(ctx, userID, event.CalendarID)
	if err != nil {
		return store.CalendarEvent{}, err
	}
	return backend.RespondToRemoteEvent(ctx, userID, event, response)
}

// SyncCalendar reads one calendar's events from its provider.
func (r *Router) SyncCalendar(ctx context.Context, userID, calendarID int64) error {
	backend, err := r.backendFor(ctx, userID, calendarID)
	if err != nil {
		return err
	}
	return backend.SyncCalendar(ctx, userID, calendarID)
}

// CreateWithCopy creates an event and, when asked, its copy in the second
// calendar -- each in its own provider.
func (r *Router) CreateWithCopy(ctx context.Context, userID, calendarID, copyCalendarID int64, event store.CalendarEvent) (store.CalendarEvent, []LinkedProblem, error) {
	return CreateWithCopy(ctx, r, userID, calendarID, copyCalendarID, event)
}

// CreateWithCopies creates an event and a copy in every target that asks for
// one, each in its own provider.
func (r *Router) CreateWithCopies(ctx context.Context, userID, calendarID int64, targets []Target, event store.CalendarEvent) (store.CalendarEvent, []LinkedProblem, error) {
	return CreateWithCopies(ctx, r, userID, calendarID, targets, event)
}

// UpdateGroupTargets edits an event and carries the edit to its copies, adding
// or removing the copy each target asks for.
func (r *Router) UpdateGroupTargets(ctx context.Context, userID int64, existing, edited store.CalendarEvent, copies []store.CalendarEvent, targets []Target) (store.CalendarEvent, []LinkedProblem, error) {
	return UpdateGroupTargets(ctx, r, userID, existing, edited, copies, targets)
}

// UpdateGroup edits an event and carries the edit to its copies.
func (r *Router) UpdateGroup(ctx context.Context, userID int64, existing, edited store.CalendarEvent, copies []store.CalendarEvent, copyCalendarID int64, change CopyChange) (store.CalendarEvent, []LinkedProblem, error) {
	return UpdateGroup(ctx, r, userID, existing, edited, copies, copyCalendarID, change)
}

// DeleteGroup deletes an event and every copy of it.
func (r *Router) DeleteGroup(ctx context.Context, userID int64, event store.CalendarEvent, copies []store.CalendarEvent) ([]LinkedProblem, error) {
	return DeleteGroup(ctx, r, userID, event, copies)
}

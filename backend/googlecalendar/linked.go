// File overview: How Google holds the link between the two copies of one event
// kept in two calendars: a private extended property both carry. The pair
// logic itself -- which copy is written when, and what a refused copy means --
// is provider-neutral and lives in calendarlink, because the second calendar
// may just as well be a Microsoft 365 one.

package googlecalendar

import (
	"context"
	"strings"

	"rolltop/backend/calendarlink"
	"rolltop/backend/store"
)

const (
	// linkKeyProperty is the private extended property naming the pair an
	// event belongs to. It is the only place the link lives: the local
	// link_key column mirrors it on every sync.
	linkKeyProperty = "rolltopLink"
	// linkRoleProperty says which side of the pair an event is. The copy that
	// was entered is the primary and is the one carrying the guest list; a
	// busy copy is a placeholder in the reader's busy calendar, whose edits
	// carry the time and nothing else.
	linkRoleProperty = "rolltopLinkRole"
	linkRolePrimary  = "primary"
	linkRoleCopy     = "copy"
	linkRoleBusy     = "busy"
)

// The pair logic is shared with Microsoft 365 and lives in calendarlink; these
// names keep the Google package's callers and tests reading as they did.
type (
	LinkedAction  = calendarlink.LinkedAction
	LinkedProblem = calendarlink.LinkedProblem
	CopyChange    = calendarlink.CopyChange
)

const (
	LinkedCreate = calendarlink.LinkedCreate
	LinkedUpdate = calendarlink.LinkedUpdate
	LinkedDelete = calendarlink.LinkedDelete
	CopyKeep     = calendarlink.CopyKeep
	CopyAdd      = calendarlink.CopyAdd
	CopyRemove   = calendarlink.CopyRemove
)

// NewLinkKey returns a fresh key for a pair.
func NewLinkKey() string {
	return calendarlink.NewLinkKey()
}

// eventLink reads the pair an event belongs to off its extended properties:
// the key, whether this is the copy that was entered, and whether it is a
// placeholder.
func eventLink(event Event) (key string, primary, masked bool) {
	if event.ExtendedProperties == nil {
		return "", false, false
	}
	key = strings.TrimSpace(event.ExtendedProperties.Private[linkKeyProperty])
	if key == "" {
		return "", false, false
	}
	role := event.ExtendedProperties.Private[linkRoleProperty]
	return key, role == linkRolePrimary, role == linkRoleBusy
}

// linkProperties renders an event's link for a write, or nothing for an event
// that is not linked.
func linkProperties(event store.CalendarEvent) *EventExtendedProperties {
	key := strings.TrimSpace(event.LinkKey)
	if key == "" {
		return nil
	}
	role := linkRoleCopy
	switch {
	case event.LinkPrimary:
		role = linkRolePrimary
	case event.LinkMasked:
		role = linkRoleBusy
	}
	return &EventExtendedProperties{Private: map[string]string{
		linkKeyProperty:  key,
		linkRoleProperty: role,
	}}
}

// CreateRemoteEventWithCopy creates an event and its copy in the second
// calendar, both in Google. See calendarlink.CreateWithCopy.
func (s *Syncer) CreateRemoteEventWithCopy(ctx context.Context, userID, calendarID, copyCalendarID int64, event store.CalendarEvent) (store.CalendarEvent, []LinkedProblem, error) {
	return calendarlink.CreateWithCopy(ctx, s, userID, calendarID, copyCalendarID, event)
}

// UpdateRemoteEventGroup applies an edit to an event and carries it to the
// event's copies. See calendarlink.UpdateGroup.
func (s *Syncer) UpdateRemoteEventGroup(ctx context.Context, userID int64, existing, edited store.CalendarEvent, copies []store.CalendarEvent, copyCalendarID int64, change CopyChange) (store.CalendarEvent, []LinkedProblem, error) {
	return calendarlink.UpdateGroup(ctx, s, userID, existing, edited, copies, copyCalendarID, change)
}

// DeleteRemoteEventGroup deletes an event and every copy of it. See
// calendarlink.DeleteGroup.
func (s *Syncer) DeleteRemoteEventGroup(ctx context.Context, userID int64, event store.CalendarEvent, copies []store.CalendarEvent) ([]LinkedProblem, error) {
	return calendarlink.DeleteGroup(ctx, s, userID, event, copies)
}

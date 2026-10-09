// File overview: One event kept in two calendars. The reader enters an event
// once and asks for it to appear in their second calendar as well; Google ends
// up holding two independent events, and what makes them one appointment is a
// private extended property both carry. Every write here goes to Google first
// and per copy, exactly as a write to a single event does, so a copy Google
// refuses is reported on its own instead of failing the copy that succeeded.

package googlecalendar

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"

	"rolltop/backend/store"
)

const (
	// linkKeyProperty is the private extended property naming the pair an
	// event belongs to. It is the only place the link lives: the local
	// link_key column mirrors it on every sync.
	linkKeyProperty = "rolltopLink"
	// linkRoleProperty says which side of the pair an event is. The copy that
	// was entered is the primary and is the one carrying the guest list.
	linkRoleProperty = "rolltopLinkRole"
	linkRolePrimary  = "primary"
	linkRoleCopy     = "copy"
)

// LinkedAction names what was being done to a copy when it failed.
type LinkedAction string

const (
	LinkedCreate LinkedAction = "create"
	LinkedUpdate LinkedAction = "update"
	LinkedDelete LinkedAction = "delete"
)

// LinkedProblem is a copy that could not be brought along with the event the
// reader acted on. The event itself succeeded; this is what the caller has to
// tell the reader is out of step.
type LinkedProblem struct {
	CalendarID int64
	Action     LinkedAction
	Err        error
}

// CopyChange is what an edit asks of the copy in the reader's second calendar.
type CopyChange int

const (
	// CopyKeep leaves the copies as they are, apart from carrying the edit.
	CopyKeep CopyChange = iota
	// CopyAdd makes sure the second calendar holds a copy.
	CopyAdd
	// CopyRemove deletes the second calendar's copy and keeps the rest.
	CopyRemove
)

// NewLinkKey returns a fresh key for a pair. It only has to be unique among
// one reader's events, and 128 random bits are that with room to spare.
func NewLinkKey() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand does not fail on any platform Rolltop runs on; if it
		// ever did, refusing to link is better than linking two unrelated
		// events under a predictable key.
		panic("googlecalendar: no randomness for a link key: " + err.Error())
	}
	return hex.EncodeToString(raw[:])
}

// eventLink reads the pair an event belongs to off its extended properties.
func eventLink(event Event) (string, bool) {
	if event.ExtendedProperties == nil {
		return "", false
	}
	key := strings.TrimSpace(event.ExtendedProperties.Private[linkKeyProperty])
	if key == "" {
		return "", false
	}
	return key, event.ExtendedProperties.Private[linkRoleProperty] == linkRolePrimary
}

// linkProperties renders an event's link for a write, or nothing for an event
// that is not linked.
func linkProperties(event store.CalendarEvent) *EventExtendedProperties {
	key := strings.TrimSpace(event.LinkKey)
	if key == "" {
		return nil
	}
	role := linkRoleCopy
	if event.LinkPrimary {
		role = linkRolePrimary
	}
	return &EventExtendedProperties{Private: map[string]string{
		linkKeyProperty:  key,
		linkRoleProperty: role,
	}}
}

// copyFor renders the second calendar's copy of an event. The copy carries no
// guests: they were invited by the primary, and a guest list on the copy would
// send every one of them a second invitation to the same meeting.
func copyFor(event store.CalendarEvent, calendarID int64) store.CalendarEvent {
	return store.CalendarEvent{
		CalendarID:  calendarID,
		Summary:     event.Summary,
		Description: event.Description,
		Location:    event.Location,
		StartAt:     event.StartAt,
		EndAt:       event.EndAt,
		AllDay:      event.AllDay,
		TimeZone:    event.TimeZone,
		LinkKey:     event.LinkKey,
		LinkPrimary: false,
	}
}

// CreateRemoteEventWithCopy creates an event and its copy in the second
// calendar. The event comes first and decides the outcome: a refused copy
// leaves the event in place and is returned as a problem, because the reader
// asked for the appointment above all and it now exists.
func (s *Syncer) CreateRemoteEventWithCopy(ctx context.Context, userID, calendarID, copyCalendarID int64, event store.CalendarEvent) (store.CalendarEvent, []LinkedProblem, error) {
	if copyCalendarID <= 0 || copyCalendarID == calendarID {
		created, err := s.CreateRemoteEvent(ctx, userID, calendarID, event)
		return created, nil, err
	}
	event.LinkKey = NewLinkKey()
	event.LinkPrimary = true
	created, err := s.CreateRemoteEvent(ctx, userID, calendarID, event)
	if err != nil {
		return store.CalendarEvent{}, nil, err
	}
	if _, err := s.CreateRemoteEvent(ctx, userID, copyCalendarID, copyFor(event, copyCalendarID)); err != nil {
		return created, []LinkedProblem{{CalendarID: copyCalendarID, Action: LinkedCreate, Err: err}}, nil
	}
	return created, nil, nil
}

// UpdateRemoteEventGroup applies an edit to an event and carries it to the
// event's copies. copies are the other members of its pair as they stood
// before the edit (store.ListCalendarEventCopies); copyCalendarID is the
// reader's second calendar and change says what the edit wants of it.
//
// The edited event is written first and alone decides the error: a conflict or
// a deletion there is reported exactly as it is for an unlinked event, and
// nothing is carried to a copy of an edit Google refused. Each copy keeps its
// own guest list -- the copy has none, and an edit made through the copy must
// not wipe the primary's.
func (s *Syncer) UpdateRemoteEventGroup(ctx context.Context, userID int64, existing, edited store.CalendarEvent, copies []store.CalendarEvent, copyCalendarID int64, change CopyChange) (store.CalendarEvent, []LinkedProblem, error) {
	var inTarget *store.CalendarEvent
	for i := range copies {
		if copyCalendarID > 0 && copies[i].CalendarID == copyCalendarID {
			inTarget = &copies[i]
			break
		}
	}
	addCopy := change == CopyAdd && copyCalendarID > 0 && copyCalendarID != existing.CalendarID && inTarget == nil
	removeCopy := change == CopyRemove && inTarget != nil

	edited.LinkKey = existing.LinkKey
	edited.LinkPrimary = existing.LinkPrimary
	if addCopy && edited.LinkKey == "" {
		// An event entered before it had a copy becomes the primary of a new
		// pair. The link travels in the same patch as the edit, so there is no
		// state in which the copy exists and the event does not know it.
		edited.LinkKey = NewLinkKey()
		edited.LinkPrimary = true
	}
	updated, err := s.UpdateRemoteEvent(ctx, userID, existing, edited)
	if err != nil {
		return updated, nil, err
	}

	var problems []LinkedProblem
	for _, copied := range copies {
		if removeCopy && copied.ID == inTarget.ID {
			continue
		}
		follow := edited
		follow.ID = copied.ID
		follow.CalendarID = copied.CalendarID
		follow.ExternalID = copied.ExternalID
		follow.ETag = copied.ETag
		follow.Attendees = copied.Attendees
		follow.LinkKey = copied.LinkKey
		follow.LinkPrimary = copied.LinkPrimary
		if _, err := s.UpdateRemoteEvent(ctx, userID, copied, follow); err != nil {
			problems = append(problems, LinkedProblem{CalendarID: copied.CalendarID, Action: LinkedUpdate, Err: err})
		}
	}
	if removeCopy {
		if err := s.DeleteRemoteEvent(ctx, userID, *inTarget); err != nil {
			problems = append(problems, LinkedProblem{CalendarID: inTarget.CalendarID, Action: LinkedDelete, Err: err})
		}
	}
	if addCopy {
		if _, err := s.CreateRemoteEvent(ctx, userID, copyCalendarID, copyFor(edited, copyCalendarID)); err != nil {
			problems = append(problems, LinkedProblem{CalendarID: copyCalendarID, Action: LinkedCreate, Err: err})
		}
	}
	return updated, problems, nil
}

// DeleteRemoteEventGroup deletes an event and every copy of it. The reader saw
// one appointment and deleted it; leaving its copy behind would bring it back
// the moment the other calendar is switched on. The event goes first, so a
// refusal there leaves the whole pair as it was.
func (s *Syncer) DeleteRemoteEventGroup(ctx context.Context, userID int64, event store.CalendarEvent, copies []store.CalendarEvent) ([]LinkedProblem, error) {
	if err := s.DeleteRemoteEvent(ctx, userID, event); err != nil && !errors.Is(err, ErrRemoteDeleted) {
		return nil, err
	}
	var problems []LinkedProblem
	for _, copied := range copies {
		if err := s.DeleteRemoteEvent(ctx, userID, copied); err != nil && !errors.Is(err, ErrRemoteDeleted) {
			problems = append(problems, LinkedProblem{CalendarID: copied.CalendarID, Action: LinkedDelete, Err: err})
		}
	}
	return problems, nil
}

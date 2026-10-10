// File overview: One event kept in two calendars. The reader enters an event
// once and asks for it to appear in their second calendar as well; the
// provider (or the two providers -- the second calendar may be a Google
// calendar for an event entered in Microsoft 365, or the other way round) ends
// up holding two independent events, and what makes them one appointment is a
// link key both carry in the provider's own private storage. Every write here
// goes to the provider first and per copy, exactly as a write to a single
// event does, so a copy the provider refuses is reported on its own instead of
// failing the copy that succeeded.
//
// Nothing here knows how a provider stores the link. A Writer does: it writes
// event.LinkKey and event.LinkPrimary along with everything else and reads
// them back on every sync.

package calendarlink

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"

	"rolltop/backend/store"
)

// Writer is what a provider offers for writing one event. Each call goes to
// the provider first and makes the local row match what the provider
// accepted.
type Writer interface {
	CreateRemoteEvent(ctx context.Context, userID, calendarID int64, event store.CalendarEvent) (store.CalendarEvent, error)
	UpdateRemoteEvent(ctx context.Context, userID int64, existing, edited store.CalendarEvent) (store.CalendarEvent, error)
	DeleteRemoteEvent(ctx context.Context, userID int64, event store.CalendarEvent) error
}

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
		panic("calendarlink: no randomness for a link key: " + err.Error())
	}
	return hex.EncodeToString(raw[:])
}

// CopyFor renders the second calendar's copy of an event. The copy carries no
// guests: they were invited by the primary, and a guest list on the copy would
// send every one of them a second invitation to the same meeting. For the same
// reason it is never an online meeting of its own -- a copy of a Teams meeting
// that asked for one would create a second meeting nobody was invited to; the
// join details reach the copy in the notes, where the provider wrote them.
func CopyFor(event store.CalendarEvent, calendarID int64) store.CalendarEvent {
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

// CreateWithCopy creates an event and its copy in the second calendar. The
// event comes first and decides the outcome: a refused copy leaves the event in
// place and is returned as a problem, because the reader asked for the
// appointment above all and it now exists.
func CreateWithCopy(ctx context.Context, w Writer, userID, calendarID, copyCalendarID int64, event store.CalendarEvent) (store.CalendarEvent, []LinkedProblem, error) {
	if copyCalendarID <= 0 || copyCalendarID == calendarID {
		created, err := w.CreateRemoteEvent(ctx, userID, calendarID, event)
		return created, nil, err
	}
	event.LinkKey = NewLinkKey()
	event.LinkPrimary = true
	created, err := w.CreateRemoteEvent(ctx, userID, calendarID, event)
	if err != nil {
		return store.CalendarEvent{}, nil, err
	}
	// The copy is rendered from what the provider accepted, not from what was
	// submitted: a pair is its key and its start, and a start the provider
	// normalized on the event would otherwise leave the copy unmatched.
	copied := CopyFor(created, copyCalendarID)
	copied.LinkKey = event.LinkKey
	if _, err := w.CreateRemoteEvent(ctx, userID, copyCalendarID, copied); err != nil {
		return created, []LinkedProblem{{CalendarID: copyCalendarID, Action: LinkedCreate, Err: err}}, nil
	}
	return created, nil, nil
}

// UpdateGroup applies an edit to an event and carries it to the event's
// copies. copies are the other members of its pair as they stood before the
// edit (store.ListCalendarEventCopies); copyCalendarID is the reader's second
// calendar and change says what the edit wants of it.
//
// The edited event is written first and alone decides the error: a conflict or
// a deletion there is reported exactly as it is for an unlinked event, and
// nothing is carried to a copy of an edit the provider refused. Each copy keeps
// its own guest list -- the copy has none, and an edit made through the copy
// must not wipe the primary's -- and its own online meeting, for the reason
// CopyFor gives.
func UpdateGroup(ctx context.Context, w Writer, userID int64, existing, edited store.CalendarEvent, copies []store.CalendarEvent, copyCalendarID int64, change CopyChange) (store.CalendarEvent, []LinkedProblem, error) {
	var inTarget *store.CalendarEvent
	for i := range copies {
		if copyCalendarID > 0 && copies[i].CalendarID == copyCalendarID {
			inTarget = &copies[i]
			break
		}
	}
	addCopy := change == CopyAdd && copyCalendarID > 0 && copyCalendarID != existing.CalendarID && inTarget == nil
	// Only a copy is ever removed. The second calendar can be changed after a
	// pair was made, so the member it holds may be the event that was entered
	// -- the one carrying the guest list -- and deleting that would cancel the
	// meeting for every guest when the reader only unticked a box on the copy.
	removeCopy := change == CopyRemove && inTarget != nil && !inTarget.LinkPrimary

	edited.LinkKey = existing.LinkKey
	edited.LinkPrimary = existing.LinkPrimary
	if existing.Linked() && !existing.LinkPrimary {
		for _, copied := range copies {
			if copied.LinkPrimary {
				// The guests were invited by the entered event; a list written
				// through the copy would invite each of them a second time,
				// and a meeting asked for here would be a second meeting.
				edited.Attendees = existing.Attendees
				edited.OnlineMeeting = existing.OnlineMeeting
				break
			}
		}
	}
	if addCopy && edited.LinkKey == "" {
		// An event entered before it had a copy becomes the primary of a new
		// pair. The link travels in the same write as the edit, so there is no
		// state in which the copy exists and the event does not know it.
		edited.LinkKey = NewLinkKey()
		edited.LinkPrimary = true
	}
	updated, err := w.UpdateRemoteEvent(ctx, userID, existing, edited)
	if err != nil {
		return updated, nil, err
	}

	// The copies follow what the provider accepted, not what was submitted,
	// for the reason CreateWithCopy gives.
	var problems []LinkedProblem
	for _, copied := range copies {
		if removeCopy && copied.ID == inTarget.ID {
			continue
		}
		follow := updated
		follow.ID = copied.ID
		follow.CalendarID = copied.CalendarID
		follow.ExternalID = copied.ExternalID
		follow.ETag = copied.ETag
		follow.Attendees = copied.Attendees
		follow.LinkKey = copied.LinkKey
		follow.LinkPrimary = copied.LinkPrimary
		follow.OnlineMeeting = copied.OnlineMeeting
		follow.OnlineMeetingProvider = copied.OnlineMeetingProvider
		follow.OnlineMeetingURL = copied.OnlineMeetingURL
		if _, err := w.UpdateRemoteEvent(ctx, userID, copied, follow); err != nil {
			problems = append(problems, LinkedProblem{CalendarID: copied.CalendarID, Action: LinkedUpdate, Err: err})
		}
	}
	if removeCopy {
		if err := w.DeleteRemoteEvent(ctx, userID, *inTarget); err != nil {
			problems = append(problems, LinkedProblem{CalendarID: inTarget.CalendarID, Action: LinkedDelete, Err: err})
		}
	}
	if addCopy {
		copied := CopyFor(updated, copyCalendarID)
		copied.LinkKey = edited.LinkKey
		if _, err := w.CreateRemoteEvent(ctx, userID, copyCalendarID, copied); err != nil {
			problems = append(problems, LinkedProblem{CalendarID: copyCalendarID, Action: LinkedCreate, Err: err})
		}
	}
	return updated, problems, nil
}

// DeleteGroup deletes an event and every copy of it. The reader saw one
// appointment and deleted it; leaving its copy behind would bring it back the
// moment the other calendar is switched on. The event goes first, so a refusal
// there leaves the whole pair as it was.
func DeleteGroup(ctx context.Context, w Writer, userID int64, event store.CalendarEvent, copies []store.CalendarEvent) ([]LinkedProblem, error) {
	if err := w.DeleteRemoteEvent(ctx, userID, event); err != nil && !errors.Is(err, ErrRemoteDeleted) {
		return nil, err
	}
	var problems []LinkedProblem
	for _, copied := range copies {
		if err := w.DeleteRemoteEvent(ctx, userID, copied); err != nil && !errors.Is(err, ErrRemoteDeleted) {
			problems = append(problems, LinkedProblem{CalendarID: copied.CalendarID, Action: LinkedDelete, Err: err})
		}
	}
	return problems, nil
}

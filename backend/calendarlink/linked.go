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
// A reader may also keep a busy calendar, and an event copied there arrives as
// a placeholder: its time under a stand-in title such as "Busy" or "Away", and
// none of its content. A placeholder is a member of the same group and moves
// with every edit, but never takes on anything but the time.
//
// Nothing here knows how a provider stores the link. A Writer does: it writes
// event.LinkKey, event.LinkPrimary and event.LinkMasked along with everything
// else and reads them back on every sync.

package calendarlink

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"

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

// CopyChange is what an edit asks of the copy in one of the reader's target
// calendars -- the second calendar or the busy calendar.
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

// MaskedCopyFor renders the busy calendar's placeholder of an event: its time
// under the given title, and nothing else. No notes, no place, no guests and
// no meeting -- the busy calendar is the one other people read, and what it is
// for is saying that the time is taken without saying by what.
func MaskedCopyFor(event store.CalendarEvent, calendarID int64, title string) store.CalendarEvent {
	if title == "" {
		title = store.DefaultBusyLabel
	}
	return store.CalendarEvent{
		CalendarID:  calendarID,
		Summary:     title,
		StartAt:     event.StartAt,
		EndAt:       event.EndAt,
		AllDay:      event.AllDay,
		TimeZone:    event.TimeZone,
		LinkKey:     event.LinkKey,
		LinkPrimary: false,
		LinkMasked:  true,
	}
}

// Target is one calendar an event may be copied into, and what a write asks of
// the copy there.
type Target struct {
	CalendarID int64
	// Masked makes the copy a placeholder (MaskedCopyFor) titled Title.
	Masked bool
	Title  string
	// Change is what the write wants of the copy. A create only acts on
	// CopyAdd.
	Change CopyChange
}

// render is the copy a target receives of an event.
func (t Target) render(event store.CalendarEvent) store.CalendarEvent {
	if t.Masked {
		return MaskedCopyFor(event, t.CalendarID, t.Title)
	}
	return CopyFor(event, t.CalendarID)
}

// CreateWithCopy creates an event and its copy in the second calendar. See
// CreateWithCopies.
func CreateWithCopy(ctx context.Context, w Writer, userID, calendarID, copyCalendarID int64, event store.CalendarEvent) (store.CalendarEvent, []LinkedProblem, error) {
	return CreateWithCopies(ctx, w, userID, calendarID, []Target{{CalendarID: copyCalendarID, Change: CopyAdd}}, event)
}

// CreateWithCopies creates an event and a copy of it in every target that asks
// for one. The event comes first and decides the outcome: a refused copy leaves
// the event in place and is returned as a problem, because the reader asked for
// the appointment above all and it now exists.
func CreateWithCopies(ctx context.Context, w Writer, userID, calendarID int64, targets []Target, event store.CalendarEvent) (store.CalendarEvent, []LinkedProblem, error) {
	wanted := make([]Target, 0, len(targets))
	seen := map[int64]bool{calendarID: true}
	for _, target := range targets {
		if target.Change != CopyAdd || target.CalendarID <= 0 || seen[target.CalendarID] {
			continue
		}
		seen[target.CalendarID] = true
		wanted = append(wanted, target)
	}
	if len(wanted) == 0 {
		created, err := w.CreateRemoteEvent(ctx, userID, calendarID, event)
		return created, nil, err
	}
	event.LinkKey = NewLinkKey()
	event.LinkPrimary = true
	event.LinkMasked = false
	created, err := w.CreateRemoteEvent(ctx, userID, calendarID, event)
	if err != nil {
		return store.CalendarEvent{}, nil, err
	}
	// The copies are rendered from what the provider accepted, not from what
	// was submitted: a pair is its key and its start, and a start the provider
	// normalized on the event would otherwise leave the copy unmatched.
	var problems []LinkedProblem
	for _, target := range wanted {
		copied := target.render(created)
		copied.LinkKey = event.LinkKey
		if _, err := w.CreateRemoteEvent(ctx, userID, target.CalendarID, copied); err != nil {
			problems = append(problems, LinkedProblem{CalendarID: target.CalendarID, Action: LinkedCreate, Err: err})
		}
	}
	return created, problems, nil
}

// UpdateGroup applies an edit and carries it to the event's copies, with one
// target: the reader's second calendar. See UpdateGroupTargets.
func UpdateGroup(ctx context.Context, w Writer, userID int64, existing, edited store.CalendarEvent, copies []store.CalendarEvent, copyCalendarID int64, change CopyChange) (store.CalendarEvent, []LinkedProblem, error) {
	var targets []Target
	if copyCalendarID > 0 {
		targets = []Target{{CalendarID: copyCalendarID, Change: change}}
	}
	return UpdateGroupTargets(ctx, w, userID, existing, edited, copies, targets)
}

// UpdateGroupTargets applies an edit to an event and carries it to the event's
// copies. copies are the other members of its group as they stood before the
// edit (store.ListCalendarEventCopies); targets are the reader's second and
// busy calendars and what the edit wants of each.
//
// The edited event is written first and alone decides the error: a conflict or
// a deletion there is reported exactly as it is for an unlinked event, and
// nothing is carried to a copy of an edit the provider refused. Each copy keeps
// its own guest list -- a copy has none, and an edit made through a copy must
// not wipe the primary's -- and its own online meeting, for the reason CopyFor
// gives. A placeholder follows as a placeholder, whatever its calendar is now,
// and an edit made through a placeholder moves the other copies' time and
// nothing else: its title is not the event's.
func UpdateGroupTargets(ctx context.Context, w Writer, userID int64, existing, edited store.CalendarEvent, copies []store.CalendarEvent, targets []Target) (store.CalendarEvent, []LinkedProblem, error) {
	throughPlaceholder := existing.Linked() && existing.LinkMasked
	type plan struct {
		target Target
		add    bool
		remove *store.CalendarEvent
	}
	var plans []plan
	seen := map[int64]bool{existing.CalendarID: true}
	for _, target := range targets {
		if target.CalendarID <= 0 || seen[target.CalendarID] {
			continue
		}
		seen[target.CalendarID] = true
		var held *store.CalendarEvent
		for i := range copies {
			if copies[i].CalendarID == target.CalendarID {
				held = &copies[i]
				break
			}
		}
		change := target.Change
		if throughPlaceholder {
			// A placeholder has nothing to copy from: a copy made from it
			// would carry its stand-in title as the event's.
			change = CopyKeep
		}
		p := plan{target: target, add: change == CopyAdd && held == nil}
		// Only a copy is ever removed. A calendar's role can be changed after
		// a group was made, so the member it holds may be the event that was
		// entered -- the one carrying the guest list -- and deleting that
		// would cancel the meeting for every guest when the reader only
		// unticked a box on the copy.
		if change == CopyRemove && held != nil && !held.LinkPrimary {
			p.remove = held
		}
		plans = append(plans, p)
	}

	edited.LinkKey = existing.LinkKey
	edited.LinkPrimary = existing.LinkPrimary
	edited.LinkMasked = existing.LinkMasked
	if throughPlaceholder {
		// A placeholder never takes on content, whoever asks: the dialog
		// greys these fields out, but the route accepts whatever is sent, and
		// the busy calendar is the one other people read.
		edited.Description = existing.Description
		edited.Location = existing.Location
		edited.Attendees = existing.Attendees
		edited.OnlineMeeting = existing.OnlineMeeting
	} else if existing.Linked() && !existing.LinkPrimary {
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
	adding := false
	for _, p := range plans {
		adding = adding || p.add
	}
	if adding && edited.LinkKey == "" {
		// An event entered before it had a copy becomes the primary of a new
		// group. The link travels in the same write as the edit, so there is
		// no state in which the copy exists and the event does not know it.
		edited.LinkKey = NewLinkKey()
		edited.LinkPrimary = true
	}
	updated, err := w.UpdateRemoteEvent(ctx, userID, existing, edited)
	if err != nil {
		return updated, nil, err
	}

	removing := map[int64]bool{}
	for _, p := range plans {
		if p.remove != nil {
			removing[p.remove.ID] = true
		}
	}
	// The copies follow what the provider accepted, not what was submitted,
	// for the reason CreateWithCopies gives.
	var problems []LinkedProblem
	for _, copied := range copies {
		if removing[copied.ID] {
			continue
		}
		follow := followCopy(updated, copied, throughPlaceholder, placeholderTitle(copied, targets))
		if _, err := w.UpdateRemoteEvent(ctx, userID, copied, follow); err != nil {
			problems = append(problems, LinkedProblem{CalendarID: copied.CalendarID, Action: LinkedUpdate, Err: err})
		}
	}
	for _, p := range plans {
		if p.remove == nil {
			continue
		}
		if err := w.DeleteRemoteEvent(ctx, userID, *p.remove); err != nil {
			problems = append(problems, LinkedProblem{CalendarID: p.remove.CalendarID, Action: LinkedDelete, Err: err})
		}
	}
	for _, p := range plans {
		if !p.add {
			continue
		}
		copied := p.target.render(updated)
		copied.LinkKey = edited.LinkKey
		if _, err := w.CreateRemoteEvent(ctx, userID, p.target.CalendarID, copied); err != nil {
			problems = append(problems, LinkedProblem{CalendarID: p.target.CalendarID, Action: LinkedCreate, Err: err})
		}
	}
	return updated, problems, nil
}

// followCopy renders what one copy becomes after its group's event was edited
// to updated. A placeholder is rendered afresh from the new time; a full copy
// follows a placeholder's edit in its time only; any other copy takes the edit
// whole, keeping its own guests and meeting.
func followCopy(updated, copied store.CalendarEvent, throughPlaceholder bool, title string) store.CalendarEvent {
	var follow store.CalendarEvent
	switch {
	case copied.LinkMasked:
		follow = MaskedCopyFor(updated, copied.CalendarID, title)
	case throughPlaceholder:
		follow = copied
		follow.StartAt = updated.StartAt
		follow.EndAt = updated.EndAt
		follow.AllDay = updated.AllDay
		follow.TimeZone = updated.TimeZone
	default:
		follow = updated
	}
	follow.ID = copied.ID
	follow.CalendarID = copied.CalendarID
	follow.ExternalID = copied.ExternalID
	follow.ETag = copied.ETag
	follow.Attendees = copied.Attendees
	follow.LinkKey = copied.LinkKey
	follow.LinkPrimary = copied.LinkPrimary
	follow.LinkMasked = copied.LinkMasked
	follow.OnlineMeeting = copied.OnlineMeeting
	follow.OnlineMeetingProvider = copied.OnlineMeetingProvider
	follow.OnlineMeetingURL = copied.OnlineMeetingURL
	return follow
}

// placeholderTitle is the title a placeholder keeps through an edit: its busy
// calendar's current one while that calendar is still a busy target, and the
// one it already carries otherwise.
func placeholderTitle(copied store.CalendarEvent, targets []Target) string {
	for _, target := range targets {
		if target.Masked && target.CalendarID == copied.CalendarID && target.Title != "" {
			return target.Title
		}
	}
	if title := strings.TrimSpace(copied.Summary); title != "" {
		return title
	}
	return store.DefaultBusyLabel
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

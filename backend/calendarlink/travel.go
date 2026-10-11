// File overview: Travel time around an event. The journey there and the
// journey back are kept at the provider as events of their own, so the time
// reads as taken in every client that shows the calendar -- a colleague
// booking a meeting, the phone, Outlook -- and not only in Rolltop. They are
// part of the event all the same: they carry its link key under a travel role,
// touch it (the journey there ends where it starts, the journey back starts
// where it ends), move with every edit and go when it is deleted. Rolltop draws
// them as part of the event rather than as appointments of their own.
//
// As with copies, the event is written first and decides the outcome; a travel
// block the provider refuses afterwards is reported beside a successful answer.

package calendarlink

import (
	"context"
	"errors"
	"strings"
	"time"

	"rolltop/backend/store"
)

// MaxTravelMinutes bounds one journey. Half a day is past any trip somebody
// blocks around an appointment; anything longer is a typo, and a block that
// long would swallow the day around it.
const MaxTravelMinutes = 12 * 60

// TravelRequest is what a write asks of an event's travel time, in minutes on
// either side. Nil keeps whatever the event has -- a drag in the week sends
// neither -- and zero takes that side away.
type TravelRequest struct {
	Before *int
	After  *int
}

// travelPlan is the travel time an event ends up with, in minutes.
type travelPlan struct {
	before, after int
}

func (p travelPlan) any() bool {
	return p.before > 0 || p.after > 0
}

func (p travelPlan) minutes(side string) int {
	if side == store.CalendarTravelBefore {
		return p.before
	}
	return p.after
}

// TravelMinutes reads how long the travel blocks of an event are on either
// side, from the blocks themselves. It is what the dialog is filled from and
// what an edit that does not mention travel keeps.
func TravelMinutes(travel []store.CalendarEvent) (before, after int) {
	for _, block := range travel {
		switch block.LinkTravel {
		case store.CalendarTravelBefore:
			if before == 0 && !block.EndAt.Before(block.StartAt) {
				before = int(block.EndAt.Sub(block.StartAt) / time.Minute)
			}
		case store.CalendarTravelAfter:
			if after == 0 && !block.EndAt.Before(block.StartAt) {
				after = int(block.EndAt.Sub(block.StartAt) / time.Minute)
			}
		}
	}
	return before, after
}

// resolve turns a request into the plan for an event that already has the
// given blocks. An all-day event has no instant to travel to, so it keeps none,
// and a travel block has no journey of its own.
func (r TravelRequest) resolve(event store.CalendarEvent, travel []store.CalendarEvent) travelPlan {
	before, after := TravelMinutes(travel)
	if r.Before != nil {
		before = clampTravel(*r.Before)
	}
	if r.After != nil {
		after = clampTravel(*r.After)
	}
	if event.AllDay || event.LinkTravel != "" {
		return travelPlan{}
	}
	return travelPlan{before: before, after: after}
}

func clampTravel(minutes int) int {
	switch {
	case minutes < 0:
		return 0
	case minutes > MaxTravelMinutes:
		return MaxTravelMinutes
	}
	return minutes
}

// TravelFor renders one travel block of an event: its journey there
// (store.CalendarTravelBefore) or back (store.CalendarTravelAfter), lasting the
// given minutes, in the given calendar. It carries the event's title so it
// reads for what it is in another client -- in German ("Anfahrt",
// "Abfahrt"), which is how the people using this name it in their calendars -- and the journey there carries the
// place, which is where a phone offers directions from; neither carries guests,
// notes of the event or a meeting -- it is the reader's time, not the
// appointment's.
func TravelFor(event store.CalendarEvent, side string, minutes int, calendarID int64) store.CalendarEvent {
	title := strings.TrimSpace(event.Summary)
	block := store.CalendarEvent{
		CalendarID: calendarID,
		AllDay:     false,
		TimeZone:   event.TimeZone,
		LinkKey:    event.LinkKey,
		LinkTravel: side,
	}
	length := time.Duration(minutes) * time.Minute
	if side == store.CalendarTravelBefore {
		block.Summary = "Anfahrt: " + title
		block.Location = event.Location
		block.StartAt = event.StartAt.Add(-length)
		block.EndAt = event.StartAt
		block.Description = "Fahrzeit zu „" + title + "“. Rolltop verschiebt und löscht sie zusammen mit diesem Termin."
	} else {
		block.Summary = "Abfahrt: " + title
		block.StartAt = event.EndAt
		block.EndAt = event.EndAt.Add(length)
		block.Description = "Fahrzeit nach „" + title + "“. Rolltop verschiebt und löscht sie zusammen mit diesem Termin."
	}
	return block
}

// sameTravelBlock reports whether a stored block already says what a rendered
// one would, so an edit that changes nothing about the journey writes nothing.
func sameTravelBlock(held, wanted store.CalendarEvent) bool {
	return held.Summary == wanted.Summary &&
		held.Location == wanted.Location &&
		held.Description == wanted.Description &&
		held.StartAt.Equal(wanted.StartAt) &&
		held.EndAt.Equal(wanted.EndAt) &&
		!held.AllDay
}

// CreateWithTravel creates an event, its copies and its travel blocks. An event
// that is to have travel needs a link key before it is written, because the
// key is what the blocks are found by; one the copies did not already ask for
// is minted here, making the event the primary of a group of its own.
func CreateWithTravel(ctx context.Context, w Writer, userID, calendarID int64, targets []Target, travel TravelRequest, event store.CalendarEvent) (store.CalendarEvent, []LinkedProblem, error) {
	plan := travel.resolve(event, nil)
	if plan.any() && event.LinkKey == "" {
		event.LinkKey = NewLinkKey()
		event.LinkPrimary = true
		event.LinkMasked = false
	}
	created, problems, err := CreateWithCopies(ctx, w, userID, calendarID, targets, event)
	if err != nil {
		return created, nil, err
	}
	problems = append(problems, syncTravel(ctx, w, userID, created, nil, plan, false)...)
	return created, problems, nil
}

// UpdateGroupWithTravel applies an edit to an event, carries it to the event's
// copies (UpdateGroupTargets) and brings the travel blocks along: they follow
// the event's new bounds, take the minutes the request asks for, and are added
// or removed. travel is the blocks as they stood before the edit
// (store.ListCalendarEventTravel). An edit made through a placeholder moves
// the blocks and changes nothing else about them: there is no journey in a
// placeholder to edit.
func UpdateGroupWithTravel(ctx context.Context, w Writer, userID int64, existing, edited store.CalendarEvent, copies []store.CalendarEvent, targets []Target, travel []store.CalendarEvent, request TravelRequest) (store.CalendarEvent, []LinkedProblem, error) {
	timesOnly := existing.Linked() && existing.LinkMasked
	if timesOnly {
		request = TravelRequest{}
	}
	if existing.LinkTravel != "" {
		// A travel block is never a member of a copy group, so it is never
		// copied either: a copy rendered from it would drop the travel role
		// and stand in the other calendar as an appointment of its own.
		targets = nil
	}
	resolveFor := edited
	resolveFor.LinkTravel = existing.LinkTravel
	plan := request.resolve(resolveFor, travel)
	if plan.any() && !existing.Linked() {
		// An event entered before it had travel becomes the primary of a
		// group of its own. The key travels in the same write as the edit,
		// so a block never exists that its event does not know about.
		existing.LinkKey = NewLinkKey()
		existing.LinkPrimary = true
	}
	updated, problems, err := UpdateGroupTargets(ctx, w, userID, existing, edited, copies, targets)
	if err != nil {
		return updated, nil, err
	}
	problems = append(problems, syncTravel(ctx, w, userID, updated, travel, plan, timesOnly)...)
	return updated, problems, nil
}

// syncTravel makes an event's travel blocks match a plan: the side the plan
// has minutes for is created or moved, and the side it has none for removed.
// A block that already matches is left alone. New blocks go into the event's
// own calendar; existing ones stay wherever they were made. timesOnly moves
// the blocks and keeps everything else they say, for an event that is a
// placeholder: its stand-in title is not the appointment's.
func syncTravel(ctx context.Context, w Writer, userID int64, event store.CalendarEvent, held []store.CalendarEvent, plan travelPlan, timesOnly bool) []LinkedProblem {
	var problems []LinkedProblem
	for _, side := range []string{store.CalendarTravelBefore, store.CalendarTravelAfter} {
		var current *store.CalendarEvent
		for i := range held {
			if held[i].LinkTravel == side {
				current = &held[i]
				break
			}
		}
		minutes := plan.minutes(side)
		switch {
		case minutes <= 0 && current != nil:
			if err := w.DeleteRemoteEvent(ctx, userID, *current); err != nil && !errors.Is(err, ErrRemoteDeleted) {
				problems = append(problems, LinkedProblem{CalendarID: current.CalendarID, Action: LinkedDelete, Err: err, Travel: side})
			}
		case minutes <= 0:
		case current != nil:
			wanted := TravelFor(event, side, minutes, current.CalendarID)
			if timesOnly {
				moved := *current
				moved.StartAt, moved.EndAt = wanted.StartAt, wanted.EndAt
				moved.TimeZone = wanted.TimeZone
				wanted = moved
			}
			if sameTravelBlock(*current, wanted) {
				continue
			}
			wanted.ID = current.ID
			wanted.ExternalID = current.ExternalID
			wanted.ETag = current.ETag
			if _, err := w.UpdateRemoteEvent(ctx, userID, *current, wanted); err != nil {
				problems = append(problems, LinkedProblem{CalendarID: current.CalendarID, Action: LinkedUpdate, Err: err, Travel: side})
			}
		case event.LinkKey == "" || timesOnly:
			// Nothing to tie a block to. CreateWithTravel and
			// UpdateGroupWithTravel mint a key before getting here, so this is
			// a provider that dropped it; a block nothing can find again would
			// be travel nobody can move or delete with its event. And a
			// placeholder never grows travel of its own.
		default:
			wanted := TravelFor(event, side, minutes, event.CalendarID)
			if _, err := w.CreateRemoteEvent(ctx, userID, event.CalendarID, wanted); err != nil {
				problems = append(problems, LinkedProblem{CalendarID: event.CalendarID, Action: LinkedCreate, Err: err, Travel: side})
			}
		}
	}
	return problems
}

// DeleteGroupWithTravel deletes an event, every copy of it and its travel
// blocks. The event goes first and decides the outcome, as DeleteGroup's does.
func DeleteGroupWithTravel(ctx context.Context, w Writer, userID int64, event store.CalendarEvent, copies, travel []store.CalendarEvent) ([]LinkedProblem, error) {
	problems, err := DeleteGroup(ctx, w, userID, event, copies)
	if err != nil {
		return nil, err
	}
	for _, block := range travel {
		if err := w.DeleteRemoteEvent(ctx, userID, block); err != nil && !errors.Is(err, ErrRemoteDeleted) {
			problems = append(problems, LinkedProblem{CalendarID: block.CalendarID, Action: LinkedDelete, Err: err, Travel: block.LinkTravel})
		}
	}
	return problems, nil
}

// File overview: Precise wake scheduling for calendar reminders. The pass
// itself lives in the store (RecordDueCalendarReminderEvents); this loop only
// decides when to run it and tells the delivery paths when it recorded
// something.

package web

import (
	"context"
	"errors"
	"log"
	"time"

	"rolltop/backend/store"
)

const (
	// calendarReminderIdleInterval bounds how long the loop sleeps when no
	// reminder is ahead. A sync that brings a new event wakes it earlier.
	calendarReminderIdleInterval = time.Hour
	calendarReminderErrorBackoff = 30 * time.Second
	calendarReminderMinimumDelay = 100 * time.Millisecond
	calendarReminderProcessLimit = 100
)

func (s *Server) startCalendarReminderScheduler() {
	if s == nil || s.store == nil {
		return
	}
	if s.calendarReminderWake == nil {
		s.calendarReminderWake = make(chan struct{}, 1)
	}
	go s.runCalendarReminderScheduler()
}

// runCalendarReminderScheduler runs the pass at two paces. The timer runs it
// for every user and is what decides the next sleep; a wake runs it for the
// users whose calendars changed and nobody else, and can only pull the timer
// earlier. The distinction is the cost: both calendar pollers sync every
// connection of every user every few minutes and each sync is a wake, so a
// wake that re-read everybody made one poll round cost a pass per connection
// over the whole user table.
func (s *Server) runCalendarReminderScheduler() {
	timer := time.NewTimer(0)
	defer timer.Stop()
	// next is the earliest reminder the last full pass left ahead; a wake's
	// partial pass moves it earlier, and only the full pass moves it later.
	var next time.Time
	for {
		full := false
		select {
		case <-timer.C:
			full = true
		case <-s.calendarReminderWake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		now := time.Now().UTC()
		var err error
		if full {
			next, err = s.processDueCalendarReminders(context.Background(), now)
		} else {
			var changedNext time.Time
			changedNext, err = s.processCalendarRemindersFor(context.Background(), now, s.takeCalendarReminderDirty())
			if !changedNext.IsZero() && (next.IsZero() || changedNext.Before(next)) {
				next = changedNext
			}
		}
		delay := calendarReminderIdleInterval
		if store.IsClosed(err) {
			return
		}
		if err != nil {
			log.Printf("calendar reminder scheduler: %v", err)
			delay = calendarReminderErrorBackoff
		} else if !next.IsZero() {
			// A next already behind us -- the timer fired together with a
			// wake and the wake was taken -- is the minimum delay, which is
			// the full pass straight away rather than a lost reminder.
			delay = time.Until(next)
			if delay < calendarReminderMinimumDelay {
				delay = calendarReminderMinimumDelay
			}
			if delay > calendarReminderIdleInterval {
				delay = calendarReminderIdleInterval
			}
		}
		timer.Reset(delay)
	}
}

// processDueCalendarReminders records every reminder that has come due for
// every user and returns the earliest one still ahead, so the caller can sleep
// precisely. A user whose pass fails does not stop the others.
func (s *Server) processDueCalendarReminders(ctx context.Context, now time.Time) (time.Time, error) {
	users, err := s.store.ServiceableUsers(ctx)
	if err != nil {
		return time.Time{}, err
	}
	ids := make([]int64, 0, len(users))
	for _, user := range users {
		ids = append(ids, user.ID)
	}
	return s.processCalendarRemindersFor(ctx, now, ids)
}

// processCalendarRemindersFor is the pass over the users named, with the same
// contract as processDueCalendarReminders.
func (s *Server) processCalendarRemindersFor(ctx context.Context, now time.Time, userIDs []int64) (time.Time, error) {
	var next time.Time
	var firstErr error
	for _, userID := range userIDs {
		events, userNext, err := s.store.RecordDueCalendarReminderEvents(ctx, userID, now, calendarReminderProcessLimit)
		if len(events) > 0 {
			s.notifyCalendarReminderWebPushAsync(userID)
			if s.events != nil {
				s.events.Notify(userID)
			}
		}
		if err != nil {
			if !errors.Is(err, context.Canceled) && firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !userNext.IsZero() && (next.IsZero() || userNext.Before(next)) {
			next = userNext
		}
	}
	return next, firstErr
}

// notifyCalendarChanged is told whenever a user's mirrored calendars changed --
// a sync landed, an event was entered, moved or deleted here, a calendar was
// switched on or off -- so the next reminder is computed from what is there
// now rather than from what was there when the loop last slept.
func (s *Server) notifyCalendarChanged(userID int64) {
	if s == nil || userID <= 0 {
		return
	}
	s.calendarReminderDirtyMu.Lock()
	if s.calendarReminderDirty == nil {
		s.calendarReminderDirty = map[int64]bool{}
	}
	s.calendarReminderDirty[userID] = true
	s.calendarReminderDirtyMu.Unlock()
	if s.calendarReminderWake != nil {
		select {
		case s.calendarReminderWake <- struct{}{}:
		default:
		}
	}
}

// takeCalendarReminderDirty hands the loop the users whose calendars changed
// since it last asked, and forgets them.
func (s *Server) takeCalendarReminderDirty() []int64 {
	s.calendarReminderDirtyMu.Lock()
	defer s.calendarReminderDirtyMu.Unlock()
	ids := make([]int64, 0, len(s.calendarReminderDirty))
	for id := range s.calendarReminderDirty {
		ids = append(ids, id)
	}
	s.calendarReminderDirty = map[int64]bool{}
	return ids
}

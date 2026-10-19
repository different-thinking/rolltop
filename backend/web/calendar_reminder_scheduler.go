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

func (s *Server) runCalendarReminderScheduler() {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
		case <-s.calendarReminderWake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		now := time.Now().UTC()
		next, err := s.processDueCalendarReminders(context.Background(), now)
		delay := calendarReminderIdleInterval
		if store.IsClosed(err) {
			return
		}
		if err != nil {
			log.Printf("calendar reminder scheduler: %v", err)
			delay = calendarReminderErrorBackoff
		} else if !next.IsZero() {
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
	var next time.Time
	var firstErr error
	for _, user := range users {
		events, userNext, err := s.store.RecordDueCalendarReminderEvents(ctx, user.ID, now, calendarReminderProcessLimit)
		if len(events) > 0 {
			s.notifyCalendarReminderWebPushAsync(user.ID)
			if s.events != nil {
				s.events.Notify(user.ID)
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
	if s.calendarReminderWake != nil {
		select {
		case s.calendarReminderWake <- struct{}{}:
		default:
		}
	}
}

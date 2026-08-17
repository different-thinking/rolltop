// File overview: Startup recovery and precise wake scheduling for local snooze reminders.

package web

import (
	"context"
	"errors"
	"log"
	"time"

	"rolltop/backend/store"
)

const (
	snoozeSchedulerIdleInterval = time.Hour
	snoozeSchedulerErrorBackoff = 30 * time.Second
	snoozeSchedulerMinimumDelay = 100 * time.Millisecond
	snoozeSchedulerProcessLimit = 100
)

func (s *Server) startSnoozeScheduler() {
	if s == nil || s.store == nil {
		return
	}
	if s.snoozeSchedulerWake == nil {
		s.snoozeSchedulerWake = make(chan struct{}, 1)
	}
	go s.runSnoozeScheduler()
}

func (s *Server) runSnoozeScheduler() {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
		case <-s.snoozeSchedulerWake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		now := time.Now().UTC()
		next, err := s.processDueSnoozes(context.Background(), now)
		delay := snoozeSchedulerIdleInterval
		if err != nil {
			log.Printf("snooze scheduler: %v", err)
			delay = snoozeSchedulerErrorBackoff
		} else if !next.IsZero() {
			delay = time.Until(next)
			if delay < snoozeSchedulerMinimumDelay {
				delay = snoozeSchedulerMinimumDelay
			}
		}
		timer.Reset(delay)
	}
}

// processDueSnoozes recovers durable pending state for every local user. It
// returns the earliest remaining due time so the caller can sleep precisely.
func (s *Server) processDueSnoozes(ctx context.Context, now time.Time) (time.Time, error) {
	users, err := s.store.ServiceableUsers(ctx)
	if err != nil {
		return time.Time{}, err
	}
	var next time.Time
	var firstErr error
	for _, user := range users {
		// A tenant whose database is already latched cannot be processed, and
		// repeating that failure every cycle only buries the one log line that
		// tells the operator how to repair it.
		if s.store.DatabaseCorrupt(user.ID) {
			continue
		}
		events, err := s.store.RecordDueSnoozeReminderEvents(ctx, user.ID, now, snoozeSchedulerProcessLimit)
		if err != nil {
			if s.noteSnoozeTenantFailure(user.ID, err) {
				continue
			}
			if !errors.Is(err, context.Canceled) && firstErr == nil {
				firstErr = err
			}
			continue
		}
		if len(events) > 0 {
			s.noteMailListChanged(user.ID)
			s.warmAllMailFirstPageAsync(user.ID)
			s.notifySnoozeReminderWebPushAsync(user.ID)
			if s.events != nil {
				s.events.Notify(user.ID)
			}
		}
		userNext, err := s.store.NextPendingSnoozeDue(ctx, user.ID)
		if err != nil {
			if s.noteSnoozeTenantFailure(user.ID, err) {
				continue
			}
			if firstErr == nil {
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

// noteSnoozeTenantFailure latches a tenant whose database has stopped answering
// and reports whether the scheduler should carry on without it.
//
// Corruption is deliberately not returned as the sweep's error. Returning it put
// the scheduler into its 30-second error backoff on every cycle -- degrading
// wake accuracy for every healthy tenant -- and re-logged a bare driver message
// forever, because nothing on this path ever latched the tenant to take it out
// of the serviceable set.
func (s *Server) noteSnoozeTenantFailure(userID int64, err error) bool {
	noted := s.store.NoteError(userID, err)
	if !store.IsCorrupt(noted) {
		return false
	}
	log.Printf("snooze scheduler user_id=%d: %v", userID, noted)
	return true
}

func (s *Server) notifySnoozeStateChanged(userID int64) {
	if s == nil || userID <= 0 {
		return
	}
	s.noteMailListChanged(userID)
	s.warmAllMailFirstPageAsync(userID)
	if s.events != nil {
		s.events.Notify(userID)
	}
	if s.snoozeSchedulerWake != nil {
		select {
		case s.snoozeSchedulerWake <- struct{}{}:
		default:
		}
	}
}

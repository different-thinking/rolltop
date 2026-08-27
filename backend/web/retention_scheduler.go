// File overview: The background pass that enforces the retention policy. It throws
// away mail a category has kept long enough, and then tells each account's server
// to delete what its Trash has kept long enough.

package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"rolltop/backend/store"
	"rolltop/backend/syncer"
)

const (
	// retentionSchedulerIdleInterval is how often the loop looks for readers
	// whose policy is due. Retention is measured in days, so nothing is gained
	// by looking more often, and every pass reads the user list. It must stay at
	// or below retentionCatchUpInterval, or a pass that was cut short would ask
	// to come back sooner than the loop ever looks.
	retentionSchedulerIdleInterval = 30 * time.Minute
	// retentionSchedulerErrorBackoff paces a pass that could not read the user
	// list at all, which is a database problem rather than a policy one.
	retentionSchedulerErrorBackoff = 5 * time.Minute
	// retentionSweepInterval is how long one reader's policy waits between
	// passes. A rule saved in the settings clears the marks, so a new rule does
	// not wait this out before it first runs.
	retentionSweepInterval = 6 * time.Hour
	// retentionCatchUpInterval is what a pass that was cut short waits instead.
	// A first pass over years of mail moves what one scope resolution holds and
	// leaves the rest; waiting the full interval for each of those rounds would
	// spread the first clear-out over days. It is deliberately not shorter than
	// this: a pass resolves the same scope again, so the moves the last one
	// queued have to have landed, or the round would name the same messages a
	// second time.
	retentionCatchUpInterval = time.Hour
	// retentionPassBudget bounds the wall-clock one reader's Trash purges may
	// take before the rest are left to the next pass. The purges are waited on
	// one folder at a time -- they hold the tenant's foreground reservation, so
	// they cannot overlap -- and one reader's very full Trash must not hold up
	// everybody else's policy behind it.
	retentionPassBudget = 20 * time.Minute
	// retentionReservationWait is how long a scheduled purge waits for the
	// tenant's foreground slot. Scheduled work yields to whatever the reader is
	// doing: a folder skipped here is purged on the next pass.
	retentionReservationWait = 15 * time.Second
)

func (s *Server) startRetentionScheduler() {
	if s == nil || s.store == nil {
		return
	}
	if s.retentionSchedulerWake == nil {
		s.retentionSchedulerWake = make(chan struct{}, 1)
	}
	go s.runRetentionScheduler()
}

// wakeRetentionScheduler asks for a pass now. Saving a policy clears the sweep
// marks, so the pass this wakes is the one that first applies it.
func (s *Server) wakeRetentionScheduler() {
	if s == nil || s.retentionSchedulerWake == nil {
		return
	}
	select {
	case s.retentionSchedulerWake <- struct{}{}:
	default:
	}
}

func (s *Server) runRetentionScheduler() {
	// The first wait is a full interval rather than zero: a restart is not a
	// reason to purge anybody's Trash, and the stored sweep marks mean a pass
	// missed while the process was down is picked up by the next one anyway.
	timer := time.NewTimer(retentionSchedulerIdleInterval)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
		case <-s.retentionSchedulerWake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-s.retentionSchedulerDone():
			return
		}
		delay := retentionSchedulerIdleInterval
		if err := s.sweepRetention(s.backgroundContext(), time.Now().UTC()); err != nil {
			if store.IsClosed(err) {
				// The store this loop reads from is gone; there is no policy
				// left to enforce and no backoff that would bring one back.
				return
			}
			log.Printf("retention scheduler: %v", err)
			delay = retentionSchedulerErrorBackoff
		}
		timer.Reset(delay)
	}
}

// retentionSchedulerDone reports the server's own shutdown, so a pass that is
// only waiting on the timer stops with the process rather than after it.
func (s *Server) retentionSchedulerDone() <-chan struct{} {
	if s == nil || s.lifetime == nil {
		return nil
	}
	return s.lifetime.Done()
}

func (s *Server) backgroundContext() context.Context {
	if s == nil || s.lifetime == nil {
		return context.Background()
	}
	return s.lifetime
}

// sweepRetention runs one pass over every reader whose policy is due. A reader
// whose own pass fails is logged and skipped; only a failure to read the user
// list at all is returned, because that is the one that says nothing can be
// swept rather than that one thing could not be.
func (s *Server) sweepRetention(ctx context.Context, now time.Time) error {
	users, err := s.store.ServiceableUsers(ctx)
	if err != nil {
		return err
	}
	for _, user := range users {
		if ctx.Err() != nil {
			return nil
		}
		if err := s.sweepUserRetention(ctx, user, now); err != nil {
			if store.IsClosed(err) {
				return err
			}
			log.Printf("retention sweep user_id=%d: %v", user.ID, err)
		}
	}
	return nil
}

// sweepUserRetention enforces one reader's policy. The Trash half runs first
// and the categories second, which is about the tenant's foreground slot rather
// than about the mail: the Trash half waits for each purge it starts and gives
// the slot back before returning, while the category half hands its moves to
// the background and leaves the slot held until they land. In the other order
// the categories' moves would still be holding it when the Trash half asked,
// and the Trash half would be skipped on every pass a category rule fired.
//
// The mail itself does not care which way round they go: the two halves measure
// different clocks -- a category counts from the date the message was sent, the
// Trash from the moment the message arrived in it -- so mail one pass throws
// away is never also deleted by it.
func (s *Server) sweepUserRetention(ctx context.Context, user store.User, now time.Time) error {
	settings, err := s.store.GetRetentionSettings(ctx, user.ID)
	if err != nil {
		return err
	}
	categoriesDue := len(settings.Categories) > 0 && retentionDue(settings.CategoriesSweptAt, now)
	trashDue := settings.TrashEnabled && retentionDue(settings.TrashSweptAt, now)
	if !categoriesDue && !trashDue {
		return nil
	}
	var firstErr error
	// A half that did not run this pass leaves a zero mark, which the store
	// reads as "leave it as it is". A half that ran but did not finish leaves a
	// mark far enough back to come round again on the catch-up interval.
	var categoriesMark, trashMark time.Time
	if trashDue {
		trashMark = now
		complete, err := s.sweepUserTrashRetention(ctx, user, settings, now)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if !complete {
			trashMark = retentionCatchUpMark(now)
		}
	}
	if categoriesDue {
		categoriesMark = now
		complete, err := s.sweepUserCategoryRetention(ctx, user, settings, now)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if !complete {
			categoriesMark = retentionCatchUpMark(now)
		}
	}
	if err := s.store.MarkRetentionSwept(ctx, user.ID, categoriesMark, trashMark); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// retentionDue reports whether a half of the policy has waited out its interval.
// A zero mark is due: it is what saving a policy leaves behind.
func retentionDue(sweptAt, now time.Time) bool {
	if sweptAt.IsZero() {
		return true
	}
	return !now.Before(sweptAt.Add(retentionSweepInterval))
}

// retentionCatchUpMark is the mark a pass that was cut short leaves: far enough
// back that the next pass comes in retentionCatchUpInterval rather than in a
// full one.
func retentionCatchUpMark(now time.Time) time.Time {
	return now.Add(retentionCatchUpInterval - retentionSweepInterval)
}

// categoryRetentionMessages resolves every category rule into the one selection
// they ask for between them: the mail those categories hold, each older than its
// own rule's cutoff. The bool reports that the rules matched more than one pass
// can take.
//
// The scope of each rule is its category across every folder it reaches, the
// archived mail behind the list included — a rule about Newsletters is a
// statement about newsletters, not about the list. Sent, Drafts, Trash and Junk
// stay out, because they are out of the whole-account scope the category lists
// are built from, which is also what stops a pass from throwing away what the
// last one threw away.
func (s *Server) categoryRetentionMessages(ctx context.Context, user store.User, rules []store.CategoryRetention,
	now time.Time) ([]store.ScopeMessage, bool, error) {
	messages := make([]store.ScopeMessage, 0, 256)
	seen := map[int64]bool{}
	complete := true
	var firstErr error
	for _, rule := range rules {
		if err := ctx.Err(); err != nil {
			return messages, false, firstErr
		}
		cutoff, ok := rule.Cutoff(now)
		if !ok {
			continue
		}
		// The limit is what is left of one pass rather than a fresh allowance
		// per rule, because everything collected here goes into a single move
		// plan and that plan is what the limit bounds.
		remaining := scopeTrashMessageLimit - len(messages)
		if remaining <= 0 {
			return messages, false, firstErr
		}
		found, err := s.store.ListCategoryRetentionScopeMessagesForUser(ctx, user.ID, rule.Category,
			store.ScopeFilter{Before: cutoff}, remaining+1)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			complete = false
			continue
		}
		if len(found) > remaining {
			found = found[:remaining]
			complete = false
		}
		for _, message := range found {
			if seen[message.ID] {
				continue
			}
			seen[message.ID] = true
			messages = append(messages, message)
		}
	}
	return messages, complete, firstErr
}

// sweepUserCategoryRetention throws away the mail each category has kept longer
// than its rule allows. It reuses the whole-filter delete the reader presses by
// hand, so scheduled deleting means exactly what pressing Delete means: the mail
// moves into its own account's Trash, and nothing is removed from a server here.
//
// Every rule's mail goes into **one** move plan, not one plan per rule. A plan
// takes the tenant's exclusive foreground reservation and holds it until its
// runs land, so a plan per rule meant the second rule waiting out the whole
// reservation timeout and then failing: at most one rule applied per pass, and
// the sweep stalled for minutes on each of the others. One plan also gives each
// account a single move for all of its categories, which is what the move batch
// was built to want.
//
// The bool reports whether the pass covered everything the rules matched.
func (s *Server) sweepUserCategoryRetention(ctx context.Context, user store.User, settings store.RetentionSettings, now time.Time) (bool, error) {
	if s.syncer == nil {
		return true, nil
	}
	messages, complete, firstErr := s.categoryRetentionMessages(ctx, user, settings.Categories, now)
	if len(messages) == 0 {
		return complete, firstErr
	}
	plan, err := s.trashPlanForMessages(ctx, user, messages)
	if err != nil {
		// A missing Trash folder arrives here too. It is a setup question
		// rather than a failure, but it is still worth saying: a rule that says
		// it deletes mail is not deleting any.
		if firstErr == nil {
			firstErr = err
		}
		return false, firstErr
	}
	if len(plan.Groups) == 0 {
		return complete, firstErr
	}
	runs, queued, err := s.startMovePlan(ctx, user.ID, plan)
	if err != nil {
		// The reader is doing something that holds the tenant. Scheduled work
		// yields to that: it is a postponed pass rather than a failed one, and
		// logging it as an error would say a policy is broken when it is
		// waiting.
		if !errors.Is(err, errForegroundBusy) && firstErr == nil {
			firstErr = err
		}
		complete = false
	}
	if len(runs) > 0 {
		log.Printf("retention: user_id=%d queued %d messages for Trash in %d runs across %d category rules",
			user.ID, queued, len(runs), len(settings.Categories))
		s.noteMailListChanged(user.ID)
		if s.events != nil {
			s.events.Notify(user.ID)
		}
	}
	return complete, firstErr
}

// errRetentionSlotBusy reports that the reader holds the tenant's foreground
// slot. Scheduled work yields to whatever they are doing, so this is a
// postponed folder rather than a failed one.
var errRetentionSlotBusy = errors.New("the reader is using this account")

// sweepUserTrashRetention tells each account's server to delete the mail its
// Trash folder has held for longer than the policy keeps it.
//
// The folders are purged one at a time and waited on, because each holds the
// tenant's foreground reservation for as long as its remote half runs and two
// of them would simply queue behind one another anyway. A folder whose slot is
// busy, or that the budget does not reach, is left to the next pass.
func (s *Server) sweepUserTrashRetention(ctx context.Context, user store.User, settings store.RetentionSettings, now time.Time) (bool, error) {
	if s.syncer == nil {
		return true, nil
	}
	cutoff, ok := settings.TrashCutoff(now)
	if !ok {
		return true, nil
	}
	mailboxes, err := s.store.ListMailboxesForUser(ctx, user.ID)
	if err != nil {
		return false, err
	}
	// The budget belongs to this reader and starts now, off the clock rather
	// than off the timestamp the whole pass was stamped with: a deadline
	// measured from the start of the pass would already have run out for
	// everybody queued behind a reader with a very full Trash, and would keep
	// running out for them on every pass.
	deadline := time.Now().UTC().Add(retentionPassBudget)
	complete := true
	var firstErr error
	for _, mailbox := range mailboxes {
		if mailbox.Role != "trash" {
			continue
		}
		if ctx.Err() != nil {
			return false, firstErr
		}
		if !time.Now().UTC().Before(deadline) {
			complete = false
			break
		}
		done, err := s.purgeTrashRetention(ctx, user.ID, mailbox, cutoff)
		if err != nil {
			if errors.Is(err, syncer.ErrEmptyTrashUnsupported) {
				// This deployment's IMAP client cannot delete remote mail at
				// all. Nothing about the next pass will be different.
				continue
			}
			if !errors.Is(err, errRetentionSlotBusy) && firstErr == nil {
				firstErr = err
			}
			complete = false
			continue
		}
		s.awaitRetentionPurge(ctx, done, deadline)
		// "The pass ran" and "the folder is clear" are two different answers: a
		// purge takes a bounded number of messages, and the server may refuse
		// some of what it takes. Asking the folder is what turns a partly
		// cleared one into a pass that comes round again on the catch-up
		// interval rather than in six hours.
		stillDue, err := s.store.TrashRetentionStillDue(ctx, user.ID, mailbox.ID, cutoff)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			complete = false
			continue
		}
		if stillDue {
			complete = false
		}
	}
	return complete, firstErr
}

// purgeTrashRetention reserves the tenant's foreground slot and starts one
// folder's purge, returning the channel that closes when it has finished.
func (s *Server) purgeTrashRetention(ctx context.Context, userID int64, mailbox store.MailboxSummary, cutoff time.Time) (<-chan struct{}, error) {
	release := func() {}
	if s.syncRunner != nil {
		reservationCtx, cancel := context.WithTimeout(ctx, retentionReservationWait)
		reserved, err := s.syncRunner.BeginForegroundOperation(reservationCtx, userID)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errRetentionSlotBusy, err)
		}
		release = reserved
	}
	done := make(chan struct{})
	// The syncer calls the first callback once the remote half is settled and
	// the second after the local mirror has caught up. Releasing on the first
	// is what keeps a large purge from blocking the reader's own moves and
	// sends for its whole duration; the pass waits for the second, because the
	// folder is not really purged until its rows are gone.
	_, err := s.syncer.StartTrashRetentionPurge(ctx, userID, mailbox.ID, cutoff, release, func() {
		s.startMoveRefresh(userID, mailbox.AccountID, []string{mailbox.Name})
		s.noteMailListChanged(userID)
		if s.events != nil {
			s.events.Notify(userID)
		}
		close(done)
	})
	if err != nil {
		release()
		return nil, err
	}
	return done, nil
}

// awaitRetentionPurge waits for one folder's purge, giving up on the pass
// budget rather than on the purge: the run carries on in the background, and
// what this pass stops doing is starting the next folder.
func (s *Server) awaitRetentionPurge(ctx context.Context, done <-chan struct{}, deadline time.Time) {
	wait := time.NewTimer(time.Until(deadline))
	defer wait.Stop()
	select {
	case <-done:
	case <-wait.C:
	case <-ctx.Done():
	}
}

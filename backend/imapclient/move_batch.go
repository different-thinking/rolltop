// File overview: One SELECT, one UID SEARCH and one UID MOVE for a whole batch of messages.

package imapclient

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/commands"

	"rolltop/backend/syncer"
)

// moveBatchLimit bounds how many UIDs one UID MOVE command names. A whole-filter
// delete resolves into thousands, and one command naming all of them builds a
// command line some servers refuse outright — the same ceiling emptying Trash
// works under.
const moveBatchLimit = 500

var _ syncer.BatchMoveSession = (*MoveSession)(nil)

// MoveMessagesWithReceipts moves a set of UIDs out of one source mailbox on the
// held connection. It proves the source generation and confirms the UIDs are
// still there exactly as the single-message form does; the only thing it drops
// is repeating that proof, that confirmation and the login for every message.
func (s *MoveSession) MoveMessagesWithReceipts(ctx context.Context, sourceMailbox, destMailbox string,
	uids []uint32, expectedSourceUIDValidity uint32) ([]syncer.MoveOutcome, error) {
	if err := ctx.Err(); err != nil {
		// Nothing was sent yet: a cancelled batch must not be recorded as a
		// failed move for every message it never attempted.
		return nil, syncer.MoveNotAttempted(err)
	}
	if s == nil || s.client == nil {
		return nil, errors.New("move session is closed")
	}
	return moveMessagesWithReceipts(ctx, s.client, sourceMailbox, destMailbox, uids, expectedSourceUIDValidity)
}

func moveMessagesWithReceipts(ctx context.Context, c moveCommandClient, sourceMailbox, destMailbox string,
	uids []uint32, expectedSourceUIDValidity uint32) ([]syncer.MoveOutcome, error) {
	if err := validateMoveBatchRequest(ctx, sourceMailbox, destMailbox, uids, expectedSourceUIDValidity); err != nil {
		return nil, err
	}
	sourceMailbox = strings.TrimSpace(sourceMailbox)
	destMailbox = strings.TrimSpace(destMailbox)
	selected, err := c.Select(sourceMailbox, false)
	if err != nil {
		return nil, fmt.Errorf("select mailbox %q read-write for move: %w", sourceMailbox, err)
	}
	if selected == nil || selected.UidValidity == 0 || selected.UidValidity != expectedSourceUIDValidity {
		selectedUIDValidity := uint32(0)
		if selected != nil {
			selectedUIDValidity = selected.UidValidity
		}
		return nil, fmt.Errorf("source mailbox %q UIDVALIDITY is %d, expected %d; refresh before moving",
			sourceMailbox, selectedUIDValidity, expectedSourceUIDValidity)
	}
	criteria := imap.NewSearchCriteria()
	criteria.Uid = new(imap.SeqSet)
	criteria.Uid.AddNum(uids...)
	foundUIDs, err := c.UidSearch(criteria)
	if err != nil {
		return nil, fmt.Errorf("search source mailbox %q for %d UIDs before move: %w", sourceMailbox, len(uids), err)
	}
	if err := ctx.Err(); err != nil {
		// The MOVE has not been issued: this abort leaves every message exactly
		// where it was, and must not read as a failed or unknown move.
		return nil, syncer.MoveNotAttempted(err)
	}
	ok, err := c.Support("MOVE")
	if err != nil {
		return nil, fmt.Errorf("check IMAP MOVE support: %w", err)
	}
	if !ok {
		return nil, errors.New("IMAP server does not support MOVE; rolltop will not emulate move with copy/delete")
	}
	present := make(map[uint32]struct{}, len(foundUIDs))
	for _, found := range foundUIDs {
		present[found] = struct{}{}
	}
	// A UID the server no longer has belongs to one message, not to the batch:
	// it is reported against that message so the rest of the batch still moves.
	outcomes := make([]syncer.MoveOutcome, 0, len(uids))
	movable := new(imap.SeqSet)
	movableUIDs := make([]uint32, 0, len(uids))
	for _, uid := range uids {
		if _, found := present[uid]; !found {
			outcomes = append(outcomes, syncer.MoveOutcome{
				UID: uid,
				Err: syncer.SourceUIDGone(fmt.Errorf("source mailbox %q no longer contains UID %d; refresh before moving",
					sourceMailbox, uid)),
			})
			continue
		}
		movable.AddNum(uid)
		movableUIDs = append(movableUIDs, uid)
	}
	if len(movableUIDs) == 0 {
		return outcomes, nil
	}

	command := &commands.Uid{Cmd: &commands.Move{SeqSet: movable, Mailbox: destMailbox}}
	status, err := c.Execute(command, nil)
	if err != nil {
		return nil, syncer.MoveOutcomeUnknown(fmt.Errorf("move mailbox %q %d UIDs to %q: %w",
			sourceMailbox, len(movableUIDs), destMailbox, err))
	}
	if status == nil {
		return nil, syncer.MoveOutcomeUnknown(fmt.Errorf("move mailbox %q %d UIDs to %q: IMAP connection closed before MOVE completed",
			sourceMailbox, len(movableUIDs), destMailbox))
	}
	if err := status.Err(); err != nil {
		// RFC 6851 says a server that cannot move every message SHOULD move none,
		// which is not a promise. Ask what actually happened rather than recording
		// one answer for the whole batch: a UID the server no longer has under the
		// generation this batch proved was moved, and one it still has was not.
		moveErr := fmt.Errorf("move mailbox %q %d UIDs to %q: %w", sourceMailbox, len(movableUIDs), destMailbox, err)
		return refusedMoveOutcomes(ctx, c, outcomes, sourceMailbox, movableUIDs, moveErr)
	}
	receipts := parseBatchMoveReceipts(status, movableUIDs)
	for _, uid := range movableUIDs {
		outcomes = append(outcomes, syncer.MoveOutcome{UID: uid, Receipt: receipts[uid]})
	}
	return outcomes, nil
}

// refusedMoveOutcomes settles a batch the server refused by asking which of its
// UIDs are still in the source mailbox. That costs one UID SEARCH and answers
// per message, where the refusal alone answers only for the command.
func refusedMoveOutcomes(ctx context.Context, c moveCommandClient, outcomes []syncer.MoveOutcome,
	sourceMailbox string, movableUIDs []uint32, moveErr error) ([]syncer.MoveOutcome, error) {
	criteria := imap.NewSearchCriteria()
	criteria.Uid = new(imap.SeqSet)
	criteria.Uid.AddNum(movableUIDs...)
	remaining, err := c.UidSearch(criteria)
	if err != nil {
		// Nothing is known about any of them now, so the whole batch is left for
		// reconciliation rather than recorded as moved or as left behind.
		return nil, syncer.MoveOutcomeUnknown(errors.Join(moveErr, err))
	}
	// The verification search answered, so the outcomes are built from it even
	// when the context has since been cancelled. Discarding the answer here
	// returned a bare cancellation, which the caller recorded as a failed move
	// for every message — including the ones the server demonstrably applied —
	// and threw their destination receipts away.
	stillThere := make(map[uint32]struct{}, len(remaining))
	for _, uid := range remaining {
		stillThere[uid] = struct{}{}
	}
	for _, uid := range movableUIDs {
		if _, present := stillThere[uid]; present {
			outcomes = append(outcomes, syncer.MoveOutcome{UID: uid, Err: moveErr})
			continue
		}
		// The server moved this one before refusing the rest. Its destination UID
		// is unavailable, which is an outcome a move already has.
		outcomes = append(outcomes, syncer.MoveOutcome{UID: uid})
	}
	return outcomes, nil
}

func validateMoveBatchRequest(ctx context.Context, sourceMailbox, destMailbox string, uids []uint32, expectedSourceUIDValidity uint32) error {
	if err := ctx.Err(); err != nil {
		return syncer.MoveNotAttempted(err)
	}
	if strings.TrimSpace(sourceMailbox) == "" || strings.TrimSpace(destMailbox) == "" || expectedSourceUIDValidity == 0 {
		return errors.New("move messages requires source mailbox, destination mailbox, and source UIDVALIDITY")
	}
	if len(uids) == 0 {
		return errors.New("move messages requires at least one UID")
	}
	if len(uids) > moveBatchLimit {
		return fmt.Errorf("move takes at most %d UIDs per request", moveBatchLimit)
	}
	seen := make(map[uint32]struct{}, len(uids))
	for _, uid := range uids {
		if uid == 0 {
			return errors.New("move messages requires non-zero UIDs")
		}
		if _, duplicate := seen[uid]; duplicate {
			return fmt.Errorf("move names UID %d twice in one request", uid)
		}
		seen[uid] = struct{}{}
	}
	return nil
}

// parseBatchMoveReceipts maps each moved source UID to the destination UID the
// server assigned it. RFC 4315 states the correspondence positionally — the nth
// UID of the source set is the nth of the destination set — so both sets are
// expanded and zipped rather than read as ranges. Anything that does not line
// up returns no receipts at all: a move without COPYUID metadata is an outcome
// the caller already handles, and a guessed destination UID is not.
func parseBatchMoveReceipts(status *imap.StatusResp, requestedUIDs []uint32) map[uint32]*syncer.MoveReceipt {
	if status == nil || !strings.EqualFold(string(status.Code), string(copyUIDResponseCode)) || len(status.Arguments) != 3 {
		return nil
	}
	uidValidityValue, ok := responseAtom(status.Arguments[0])
	if !ok {
		return nil
	}
	uidValidity, err := strconv.ParseUint(uidValidityValue, 10, 32)
	if err != nil || uidValidity == 0 {
		return nil
	}
	sourceValue, ok := responseAtom(status.Arguments[1])
	if !ok {
		return nil
	}
	destinationValue, ok := responseAtom(status.Arguments[2])
	if !ok {
		return nil
	}
	sourceUIDs, ok := expandStaticUIDSet(sourceValue, len(requestedUIDs))
	if !ok {
		return nil
	}
	destinationUIDs, ok := expandStaticUIDSet(destinationValue, len(requestedUIDs))
	if !ok || len(sourceUIDs) != len(destinationUIDs) {
		return nil
	}
	requested := make(map[uint32]struct{}, len(requestedUIDs))
	for _, uid := range requestedUIDs {
		requested[uid] = struct{}{}
	}
	receipts := make(map[uint32]*syncer.MoveReceipt, len(sourceUIDs))
	for i, sourceUID := range sourceUIDs {
		if _, asked := requested[sourceUID]; !asked {
			return nil
		}
		if _, duplicate := receipts[sourceUID]; duplicate {
			return nil
		}
		receipts[sourceUID] = &syncer.MoveReceipt{
			DestinationUIDValidity: uint32(uidValidity),
			DestinationUID:         destinationUIDs[i],
		}
	}
	return receipts
}

// expandStaticUIDSet lists the individual UIDs a set names. It refuses the
// open-ended "*" forms, which have no fixed length, and any set longer than the
// batch it is supposed to describe.
func expandStaticUIDSet(value string, limit int) ([]uint32, bool) {
	set, err := imap.ParseSeqSet(value)
	if err != nil || len(set.Set) == 0 || limit <= 0 {
		return nil, false
	}
	uids := make([]uint32, 0, limit)
	for _, item := range set.Set {
		if item.Start == 0 || item.Stop == 0 || item.Stop < item.Start {
			return nil, false
		}
		for uid := item.Start; ; uid++ {
			if len(uids) >= limit {
				return nil, false
			}
			uids = append(uids, uid)
			if uid == item.Stop {
				break
			}
		}
	}
	return uids, true
}

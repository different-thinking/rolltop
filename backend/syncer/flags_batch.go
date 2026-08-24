// File overview: Batched upload of locally queued read/star flag changes.

package syncer

import (
	"context"
	"errors"

	"rolltop/backend/store"
)

// MailboxFlagChanges names the UID sets one selected mailbox receives in one
// push: at most four UID STORE commands instead of one login per message.
type MailboxFlagChanges struct {
	SetSeen      []uint32
	ClearSeen    []uint32
	SetFlagged   []uint32
	ClearFlagged []uint32
}

// Empty reports whether there is nothing to push.
func (c MailboxFlagChanges) Empty() bool {
	return len(c.SetSeen) == 0 && len(c.ClearSeen) == 0 && len(c.SetFlagged) == 0 && len(c.ClearFlagged) == 0
}

// BatchFlagFetcher is the optional capability behind batched flag pushes. The
// implementation must SELECT the mailbox, prove expectedUIDValidity on the
// same session, and answer (false, nil) — not applied, flags stay pending —
// when the generation does not match.
type BatchFlagFetcher interface {
	ApplyFlagChangesWithUIDValidity(ctx context.Context, account store.MailAccount, mailbox string,
		expectedUIDValidity uint32, changes MailboxFlagChanges) (bool, error)
}

type pendingFlagKind int

const (
	pendingReadFlag pendingFlagKind = iota
	pendingStarFlag
)

// pushPendingFlagState uploads queued flag changes grouped by folder. Five
// hundred queued changes used to cost five hundred logins, one per message;
// grouped they cost one SELECT and at most four UID STORE commands per folder,
// on the turn's shared connection. A fetcher without the batch capability
// keeps the per-message path.
func (s *Service) pushPendingFlagState(ctx context.Context, userID int64, messages []store.MessageRecord, kind pendingFlagKind) error {
	if len(messages) == 0 {
		return nil
	}
	batcher, canBatch := s.Fetcher.(BatchFlagFetcher)
	if !canBatch {
		for _, msg := range messages {
			var err error
			if kind == pendingReadFlag {
				err = s.SyncReadStateForMessage(ctx, userID, msg.ID)
			} else {
				err = s.SyncStarStateForMessage(ctx, userID, msg.ID)
			}
			if err != nil {
				return err
			}
		}
		return nil
	}
	type flagGroupKey struct {
		accountID int64
		mailboxID int64
	}
	var order []flagGroupKey
	groups := map[flagGroupKey][]store.MessageRecord{}
	for _, msg := range messages {
		key := flagGroupKey{accountID: msg.AccountID, mailboxID: msg.MailboxID}
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], msg)
	}
	var errs []error
	for _, key := range order {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.pushPendingFlagGroup(ctx, userID, key.accountID, key.mailboxID, groups[key], kind, batcher); err != nil {
			// One folder's push failing is that folder's problem; the flags stay
			// queued and the remaining folders still get theirs.
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Service) pushPendingFlagGroup(ctx context.Context, userID, accountID, mailboxID int64,
	group []store.MessageRecord, kind pendingFlagKind, batcher BatchFlagFetcher) error {
	account, err := s.Store.GetMailAccountForUser(ctx, userID, accountID)
	if err != nil {
		if store.IsNotFound(err) {
			return nil
		}
		return err
	}
	mailbox, err := s.Store.GetMailboxForUser(ctx, userID, mailboxID)
	if err != nil {
		if store.IsNotFound(err) {
			return nil
		}
		return err
	}
	if mailbox.UIDValidity <= 0 || mailbox.UIDValidity > int64(^uint32(0)) {
		// Leave the mutations pending. The next mailbox sync will refresh the
		// stale generation without issuing STORE against a reused UID.
		return nil
	}
	changes := MailboxFlagChanges{}
	confirmed := make([]store.MessageRecord, 0, len(group))
	for _, msg := range group {
		if msg.UID == 0 {
			continue
		}
		expected, err := s.Store.GetMessageUIDValidityForUser(ctx, userID, msg.ID)
		if err != nil {
			if store.IsNotFound(err) {
				continue
			}
			return err
		}
		if expected <= 0 || expected != mailbox.UIDValidity {
			continue
		}
		switch kind {
		case pendingReadFlag:
			if msg.IsRead {
				changes.SetSeen = append(changes.SetSeen, msg.UID)
			} else {
				changes.ClearSeen = append(changes.ClearSeen, msg.UID)
			}
		case pendingStarFlag:
			if msg.IsStarred {
				changes.SetFlagged = append(changes.SetFlagged, msg.UID)
			} else {
				changes.ClearFlagged = append(changes.ClearFlagged, msg.UID)
			}
		}
		confirmed = append(confirmed, msg)
	}
	if changes.Empty() {
		return nil
	}
	applied, err := batcher.ApplyFlagChangesWithUIDValidity(ctx, account, mailbox.Name,
		uint32(mailbox.UIDValidity), changes)
	if err != nil {
		return err
	}
	if !applied {
		return nil
	}
	for _, msg := range confirmed {
		if kind == pendingReadFlag {
			if err := s.Store.ClearReadSyncPending(ctx, userID, msg.ID); err != nil {
				return err
			}
			msg.ReadSyncPending = false
		} else {
			if err := s.Store.ClearStarSyncPending(ctx, userID, msg.ID); err != nil {
				return err
			}
			msg.StarSyncPending = false
		}
		if err := s.IndexAttachmentsForMessage(ctx, msg); err != nil {
			return err
		}
	}
	return nil
}

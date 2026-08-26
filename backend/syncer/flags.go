// File overview: IMAP flag reconciliation helpers for read and starred state.

package syncer

import (
	"context"
	"errors"
	"strings"

	"rolltop/backend/store"
)

func (s *Service) syncMailboxReadFlags(ctx context.Context, userID int64, account store.MailAccount, mailbox store.Mailbox) error {
	var seenUIDs []uint32
	if flagFetcher, ok := s.Fetcher.(UIDValidityFlagReader); ok {
		if mailbox.UIDValidity <= 0 {
			return nil
		}
		var matched bool
		var err error
		seenUIDs, matched, err = flagFetcher.SeenUIDsWithUIDValidity(ctx, account, mailbox.Name, uint32(mailbox.UIDValidity))
		if err != nil {
			return err
		}
		if !matched {
			return nil
		}
	} else {
		var err error
		seenUIDs, err = s.Fetcher.SeenUIDs(ctx, account, mailbox.Name)
		if err != nil {
			return err
		}
	}
	changedIDs, err := s.Store.UpdateMailboxReadFlags(ctx, userID, account.ID, mailbox.ID, seenUIDs)
	if err != nil {
		return err
	}
	for _, id := range changedIDs {
		msg, err := s.Store.GetMessageForUser(ctx, userID, id)
		if err != nil {
			return err
		}
		if err := s.IndexAttachmentsForMessage(ctx, msg); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) syncMailboxStarFlags(ctx context.Context, userID int64, account store.MailAccount, mailbox store.Mailbox) error {
	var flaggedUIDs []uint32
	if flagFetcher, ok := s.Fetcher.(UIDValidityFlagReader); ok {
		if mailbox.UIDValidity <= 0 {
			return nil
		}
		var matched bool
		var err error
		flaggedUIDs, matched, err = flagFetcher.FlaggedUIDsWithUIDValidity(ctx, account, mailbox.Name, uint32(mailbox.UIDValidity))
		if err != nil {
			return err
		}
		if !matched {
			return nil
		}
	} else {
		var err error
		flaggedUIDs, err = s.Fetcher.FlaggedUIDs(ctx, account, mailbox.Name)
		if err != nil {
			return err
		}
	}
	changedIDs, err := s.Store.UpdateMailboxStarFlags(ctx, userID, account.ID, mailbox.ID, flaggedUIDs)
	if err != nil {
		return err
	}
	for _, id := range changedIDs {
		msg, err := s.Store.GetMessageForUser(ctx, userID, id)
		if err != nil {
			return err
		}
		if err := s.IndexAttachmentsForMessage(ctx, msg); err != nil {
			return err
		}
	}
	return nil
}

// PushPendingReadState sends locally queued read-state changes to IMAP in bounded batches.
func (s *Service) PushPendingReadState(ctx context.Context, userID int64, limit int) error {
	messages, err := s.Store.ListMessagesWithReadSyncPending(ctx, userID, limit)
	if err != nil {
		return err
	}
	return s.pushPendingFlagState(ctx, userID, messages, pendingReadFlag)
}

// SyncReadStateForMessage pushes the read state for one message UID to IMAP.
func (s *Service) SyncReadStateForMessage(ctx context.Context, userID, messageID int64) error {
	_, err := s.PushReadStateForMessage(ctx, userID, messageID)
	return err
}

// PushReadStateForMessage is SyncReadStateForMessage with the one answer its
// callers used to throw away: whether the change actually reached the server.
// A mailbox generation that cannot be proved leaves the change queued for
// PushPendingReadState rather than issuing STORE against a reused UID, which is
// the correct outcome and not an error -- but it is not the same as done, and a
// caller that records what it did to a message must be able to tell them apart.
func (s *Service) PushReadStateForMessage(ctx context.Context, userID, messageID int64) (bool, error) {
	msg, err := s.Store.GetMessageForUser(ctx, userID, messageID)
	if err != nil {
		return false, err
	}
	account, err := s.Store.GetMailAccountForUser(ctx, userID, msg.AccountID)
	if err != nil {
		return false, err
	}
	mailbox, err := s.Store.GetMailboxForUser(ctx, userID, msg.MailboxID)
	if err != nil {
		return false, err
	}
	expectedUIDValidity, err := s.Store.GetMessageUIDValidityForUser(ctx, userID, msg.ID)
	if err != nil {
		return false, err
	}
	if expectedUIDValidity <= 0 || mailbox.UIDValidity <= 0 || expectedUIDValidity != mailbox.UIDValidity {
		// Leave the mutation pending. The next mailbox sync will reset or refresh
		// the stale generation without issuing STORE against a reused UID.
		return false, nil
	}
	flagFetcher, ok := s.Fetcher.(UIDValidityFlagFetcher)
	if !ok {
		return false, errors.New("IMAP fetcher cannot prove mailbox generation for read-state sync")
	}
	applied, err := flagFetcher.SetSeenWithUIDValidity(ctx, account, mailbox.Name, msg.UID,
		msg.IsRead, uint32(expectedUIDValidity))
	if err != nil {
		return false, err
	}
	if !applied {
		return false, nil
	}
	if err := s.Store.ClearReadSyncPending(ctx, userID, msg.ID); err != nil {
		return false, err
	}
	msg.ReadSyncPending = false
	return true, s.IndexAttachmentsForMessage(ctx, msg)
}

// SetReadForMessage updates local read state and queues the matching IMAP flag
// change, the way SetStarredForMessage does for the star. The push itself is
// SyncReadStateForMessage: leaving the change queued is the correct outcome for
// a mailbox generation that cannot be proved, not a failure.
func (s *Service) SetReadForMessage(ctx context.Context, userID, messageID int64, read bool) (store.MessageRecord, error) {
	if err := s.Store.MarkMessageReadForUser(ctx, userID, messageID, read, true); err != nil {
		return store.MessageRecord{}, err
	}
	msg, err := s.Store.GetMessageForUser(ctx, userID, messageID)
	if err != nil {
		return store.MessageRecord{}, err
	}
	if err := s.IndexAttachmentsForMessage(ctx, msg); err != nil {
		return store.MessageRecord{}, err
	}
	return msg, nil
}

// PushPendingStarState sends locally queued star-state changes to IMAP in bounded batches.
func (s *Service) PushPendingStarState(ctx context.Context, userID int64, limit int) error {
	messages, err := s.Store.ListMessagesWithStarSyncPending(ctx, userID, limit)
	if err != nil {
		return err
	}
	return s.pushPendingFlagState(ctx, userID, messages, pendingStarFlag)
}

// SetStarredForMessage updates local star state and queues or performs the matching IMAP flag change.
func (s *Service) SetStarredForMessage(ctx context.Context, userID, messageID int64, starred bool) (store.MessageRecord, error) {
	if err := s.Store.MarkMessageStarredForUser(ctx, userID, messageID, starred, true); err != nil {
		return store.MessageRecord{}, err
	}
	msg, err := s.Store.GetMessageForUser(ctx, userID, messageID)
	if err != nil {
		return store.MessageRecord{}, err
	}
	if err := s.IndexAttachmentsForMessage(ctx, msg); err != nil {
		return store.MessageRecord{}, err
	}
	return msg, nil
}

// SyncStarStateForMessage pushes the star state for one message UID to IMAP.
func (s *Service) SyncStarStateForMessage(ctx context.Context, userID, messageID int64) error {
	if s.Fetcher == nil {
		return errors.New("sync fetcher is not configured")
	}
	msg, err := s.Store.GetMessageForUser(ctx, userID, messageID)
	if err != nil {
		return err
	}
	account, err := s.Store.GetMailAccountForUser(ctx, userID, msg.AccountID)
	if err != nil {
		return err
	}
	mailbox, err := s.Store.GetMailboxForUser(ctx, userID, msg.MailboxID)
	if err != nil {
		return err
	}
	expectedUIDValidity, err := s.Store.GetMessageUIDValidityForUser(ctx, userID, msg.ID)
	if err != nil {
		return err
	}
	if expectedUIDValidity <= 0 || mailbox.UIDValidity <= 0 || expectedUIDValidity != mailbox.UIDValidity {
		return nil
	}
	flagFetcher, ok := s.Fetcher.(UIDValidityFlagFetcher)
	if !ok {
		return errors.New("IMAP fetcher cannot prove mailbox generation for star-state sync")
	}
	applied, err := flagFetcher.SetFlaggedWithUIDValidity(ctx, account, mailbox.Name, msg.UID,
		msg.IsStarred, uint32(expectedUIDValidity))
	if err != nil {
		return err
	}
	if !applied {
		return nil
	}
	if err := s.Store.ClearStarSyncPending(ctx, userID, msg.ID); err != nil {
		return err
	}
	msg.StarSyncPending = false
	return s.IndexAttachmentsForMessage(ctx, msg)
}
func hasSeen(flags []string) bool {
	for _, flag := range flags {
		if strings.EqualFold(flag, "\\Seen") {
			return true
		}
	}
	return false
}

func hasFlagged(flags []string) bool {
	for _, flag := range flags {
		switch {
		case strings.EqualFold(flag, "\\Flagged"):
			return true
		case strings.EqualFold(flag, "$Flagged"):
			return true
		case strings.EqualFold(flag, "$Starred"):
			return true
		}
	}
	return false
}

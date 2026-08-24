// File overview: One SELECT and up to four UID STORE commands for a folder's
// queued flag changes.

package imapclient

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/emersion/go-imap"

	"rolltop/backend/store"
	"rolltop/backend/syncer"
)

// flagStoreBatchLimit bounds how many UIDs one UID STORE names, matching the
// ceiling the batched move and expunge commands work under.
const flagStoreBatchLimit = 500

var _ syncer.BatchFlagFetcher = (*Fetcher)(nil)

// ApplyFlagChangesWithUIDValidity pushes a folder's queued read/star changes
// over one selected session. It proves the mailbox generation exactly like the
// single-message writes do and answers (false, nil) on a mismatch, leaving the
// local mutations pending. Five hundred queued changes used to cost five
// hundred logins; this costs one SELECT and at most four STORE commands per
// five hundred UIDs.
func (f *Fetcher) ApplyFlagChangesWithUIDValidity(ctx context.Context, account store.MailAccount, mailbox string,
	expectedUIDValidity uint32, changes syncer.MailboxFlagChanges) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	mailbox = strings.TrimSpace(mailbox)
	if mailbox == "" || expectedUIDValidity == 0 {
		return false, errors.New("batched flag update requires a mailbox and UIDVALIDITY")
	}
	if changes.Empty() {
		return false, errors.New("batched flag update requires at least one UID")
	}
	c, release, err := f.sessionClient(ctx, account)
	if err != nil {
		return false, err
	}
	defer release()
	status, err := c.Select(mailbox, false)
	if err != nil {
		return false, fmt.Errorf("select mailbox %q read-write for batched flag update: %w", mailbox, err)
	}
	if status == nil || status.UidValidity == 0 {
		return false, fmt.Errorf("select mailbox %q returned no UIDVALIDITY for batched flag update", mailbox)
	}
	if status.UidValidity != expectedUIDValidity {
		return false, nil
	}
	apply := func(uids []uint32, op imap.FlagsOp, flag, label string) error {
		uids = normalizeUIDList(uids)
		for start := 0; start < len(uids); start += flagStoreBatchLimit {
			if err := ctx.Err(); err != nil {
				return err
			}
			end := min(start+flagStoreBatchLimit, len(uids))
			seqset := new(imap.SeqSet)
			seqset.AddNum(uids[start:end]...)
			item := imap.FormatFlagsOp(op, true)
			if err := c.UidStore(seqset, item, []interface{}{flag}, nil); err != nil {
				return fmt.Errorf("batched %s update mailbox %q (%d UIDs): %w", label, mailbox, end-start, err)
			}
		}
		return nil
	}
	if err := apply(changes.SetSeen, imap.AddFlags, imap.SeenFlag, "seen"); err != nil {
		return false, err
	}
	if err := apply(changes.ClearSeen, imap.RemoveFlags, imap.SeenFlag, "unseen"); err != nil {
		return false, err
	}
	if err := apply(changes.SetFlagged, imap.AddFlags, imap.FlaggedFlag, "flagged"); err != nil {
		return false, err
	}
	if err := apply(changes.ClearFlagged, imap.RemoveFlags, imap.FlaggedFlag, "unflagged"); err != nil {
		return false, err
	}
	return true, nil
}

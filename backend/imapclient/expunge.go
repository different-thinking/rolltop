// File overview: Permanent removal of messages from a remote IMAP mailbox. This is
// the one place Rolltop deletes mail on the server rather than moving it, and it
// exists for emptying a Trash folder.

package imapclient

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-imap/commands"
	"github.com/emersion/go-imap/responses"

	"rolltop/backend/store"
	"rolltop/backend/syncer"
)

var _ syncer.ExpungeFetcher = (*Fetcher)(nil)

// expungeBatchLimit bounds one STORE/EXPUNGE pair. A whole Trash folder can hold
// tens of thousands of messages, and one command naming all of them builds a
// command line some servers refuse outright.
const expungeBatchLimit = 500

// uidExpungeCommand renders the sequence set that UID EXPUNGE takes. go-imap's
// own EXPUNGE command carries no arguments, so the UIDPLUS form (RFC 4315) is
// spelled out here and wrapped in commands.Uid to become "UID EXPUNGE <set>".
type uidExpungeCommand struct {
	SeqSet *imap.SeqSet
}

func (cmd *uidExpungeCommand) Command() *imap.Command {
	return &imap.Command{
		Name:      "EXPUNGE",
		Arguments: []any{imap.RawString(cmd.SeqSet.String())},
	}
}

type expungeCommandClient interface {
	Select(mailbox string, readOnly bool) (*imap.MailboxStatus, error)
	UidSearch(criteria *imap.SearchCriteria) ([]uint32, error)
	UidStore(seqset *imap.SeqSet, item imap.StoreItem, value any, ch chan *imap.Message) error
	Expunge(ch chan uint32) error
	Support(capability string) (bool, error)
	Execute(command imap.Commander, handler responses.Handler) (*imap.StatusResp, error)
}

// ExpungeMessages permanently deletes the given UIDs from a mailbox and reports
// which of them the server no longer has. The caller proves which mailbox
// generation it means: a UID belongs to one UIDVALIDITY, so a folder that was
// recreated since the caller listed it is refused rather than deleted from.
//
// The returned list is the evidence local rows are removed on. It is read back
// from the server after the expunge, so a partial or silently ignored delete
// never presents itself as a finished one.
func (f *Fetcher) ExpungeMessages(ctx context.Context, account store.MailAccount, mailbox string, uids []uint32, expectedUIDValidity uint32, scope syncer.ExpungeScope) ([]uint32, error) {
	if err := validateExpungeRequest(ctx, mailbox, uids, expectedUIDValidity); err != nil {
		return nil, err
	}
	if f == nil {
		return nil, errors.New("expunge requires a fetcher")
	}
	c, err := f.loginWithinContext(ctx, account)
	if err != nil {
		return nil, err
	}
	defer terminateClientOnContext(ctx, c)()
	return expungeMessages(ctx, c, mailbox, uids, expectedUIDValidity, scope)
}

func expungeMessages(ctx context.Context, c expungeCommandClient, mailbox string, uids []uint32, expectedUIDValidity uint32, scope syncer.ExpungeScope) ([]uint32, error) {
	if err := validateExpungeRequest(ctx, mailbox, uids, expectedUIDValidity); err != nil {
		return nil, err
	}
	mailbox = strings.TrimSpace(mailbox)
	selected, err := c.Select(mailbox, false)
	if err != nil {
		return nil, fmt.Errorf("select mailbox %q read-write for expunge: %w", mailbox, err)
	}
	selectedUIDValidity := uint32(0)
	if selected != nil {
		selectedUIDValidity = selected.UidValidity
	}
	if selectedUIDValidity == 0 || selectedUIDValidity != expectedUIDValidity {
		return nil, fmt.Errorf("mailbox %q UIDVALIDITY is %d, expected %d; refresh before deleting",
			mailbox, selectedUIDValidity, expectedUIDValidity)
	}
	seqset := new(imap.SeqSet)
	for _, uid := range uids {
		seqset.AddNum(uid)
	}
	// Ask which expunge is available before flagging anything, for two reasons.
	// A message left carrying \Deleted with nothing to remove it is a message
	// the next plain EXPUNGE from any client deletes without anyone confirming
	// it. And a caller that named its UIDs because it means only those has to
	// be refused here, while refusing still costs nothing: flagging first and
	// finding out afterwards would leave exactly that residue behind.
	uidPlus, err := c.Support("UIDPLUS")
	if err != nil {
		return nil, fmt.Errorf("check IMAP UIDPLUS support: %w", err)
	}
	if !uidPlus && scope != syncer.ExpungeWholeFolder {
		return nil, fmt.Errorf("mailbox %q: %w", mailbox, syncer.ErrSelectiveExpungeUnsupported)
	}
	if err := c.UidStore(seqset, imap.FormatFlagsOp(imap.AddFlags, true), []any{imap.DeletedFlag}, nil); err != nil {
		return nil, fmt.Errorf("flag mailbox %q messages deleted: %w", mailbox, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if uidPlus {
		// UID EXPUNGE removes exactly the named messages. Without UIDPLUS the
		// only expunge available removes everything currently flagged \Deleted in
		// the folder, which is why the fallback below is reachable only under
		// ExpungeWholeFolder: there, the whole folder is what the user asked to
		// delete anyway.
		command := &commands.Uid{Cmd: &uidExpungeCommand{SeqSet: seqset}}
		status, execErr := c.Execute(command, nil)
		if execErr != nil {
			return nil, fmt.Errorf("expunge mailbox %q: %w", mailbox, execErr)
		}
		if status == nil {
			return nil, fmt.Errorf("expunge mailbox %q: IMAP connection closed before UID EXPUNGE completed", mailbox)
		}
		if err := status.Err(); err != nil {
			return nil, fmt.Errorf("expunge mailbox %q: %w", mailbox, err)
		}
	} else if err := c.Expunge(nil); err != nil {
		return nil, fmt.Errorf("expunge mailbox %q: %w", mailbox, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Ask the server what actually went. A message another client had open, a
	// server that refuses to delete some flag combination, or a folder that
	// silently ignores the command all show up here rather than as local rows
	// deleted for mail that is still there.
	remaining, err := c.UidSearch(uidSearchCriteria(seqset))
	if err != nil {
		return nil, fmt.Errorf("verify mailbox %q after expunge: %w", mailbox, err)
	}
	stillPresent := make(map[uint32]struct{}, len(remaining))
	for _, uid := range remaining {
		stillPresent[uid] = struct{}{}
	}
	gone := make([]uint32, 0, len(uids))
	for _, uid := range uids {
		if _, present := stillPresent[uid]; !present {
			gone = append(gone, uid)
		}
	}
	return gone, nil
}

func uidSearchCriteria(seqset *imap.SeqSet) *imap.SearchCriteria {
	criteria := imap.NewSearchCriteria()
	criteria.Uid = seqset
	return criteria
}

func validateExpungeRequest(ctx context.Context, mailbox string, uids []uint32, expectedUIDValidity uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(mailbox) == "" || len(uids) == 0 || expectedUIDValidity == 0 {
		return errors.New("expunge requires a mailbox, UIDs, and the mailbox UIDVALIDITY")
	}
	if len(uids) > expungeBatchLimit {
		return fmt.Errorf("expunge takes at most %d UIDs per request", expungeBatchLimit)
	}
	for _, uid := range uids {
		if uid == 0 {
			return errors.New("expunge requires non-zero UIDs")
		}
	}
	return nil
}

// compile-time proof that the real client satisfies the narrow command surface
// the expunge path uses, so tests can substitute a fake without drifting from it.
var _ expungeCommandClient = (*client.Client)(nil)

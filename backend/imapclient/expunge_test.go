package imapclient

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/responses"

	"rolltop/backend/syncer"
)

type fakeExpungeClient struct {
	uids          []uint32
	uidValidity   uint32
	uidPlus       bool
	selected      string
	readOnly      bool
	storedFlags   []any
	storedSeqSet  string
	storeItem     imap.StoreItem
	command       imap.Commander
	plainExpunges int
	searches      int
	supportCalls  []string
	selectErr     error
	storeErr      error
	status        *imap.StatusResp
}

func (c *fakeExpungeClient) Select(mailbox string, readOnly bool) (*imap.MailboxStatus, error) {
	c.selected = mailbox
	c.readOnly = readOnly
	if c.selectErr != nil {
		return nil, c.selectErr
	}
	return &imap.MailboxStatus{UidValidity: c.uidValidity}, nil
}

func (c *fakeExpungeClient) UidSearch(criteria *imap.SearchCriteria) ([]uint32, error) {
	c.searches++
	requested := criteria.Uid
	found := make([]uint32, 0, len(c.uids))
	for _, uid := range c.uids {
		if requested == nil || requested.Contains(uid) {
			found = append(found, uid)
		}
	}
	return found, nil
}

func (c *fakeExpungeClient) UidStore(seqset *imap.SeqSet, item imap.StoreItem, value any, _ chan *imap.Message) error {
	if c.storeErr != nil {
		return c.storeErr
	}
	c.storedSeqSet = seqset.String()
	c.storeItem = item
	flags, _ := value.([]any)
	c.storedFlags = flags
	return nil
}

// removeFlagged is what an expunge does on this fake server: the messages the
// test marked deleted stop existing.
func (c *fakeExpungeClient) removeFlagged(set *imap.SeqSet) {
	c.uids = slices.DeleteFunc(c.uids, func(uid uint32) bool { return set == nil || set.Contains(uid) })
}

func (c *fakeExpungeClient) Expunge(ch chan uint32) error {
	if ch != nil {
		close(ch)
	}
	c.plainExpunges++
	marked, err := imap.ParseSeqSet(c.storedSeqSet)
	if err != nil {
		return err
	}
	c.removeFlagged(marked)
	return nil
}

func (c *fakeExpungeClient) Support(capability string) (bool, error) {
	c.supportCalls = append(c.supportCalls, capability)
	return c.uidPlus && capability == "UIDPLUS", nil
}

func (c *fakeExpungeClient) Execute(command imap.Commander, _ responses.Handler) (*imap.StatusResp, error) {
	c.command = command
	rendered := command.Command()
	if len(rendered.Arguments) > 1 {
		if raw, ok := rendered.Arguments[1].(imap.RawString); ok {
			if set, err := imap.ParseSeqSet(string(raw)); err == nil {
				c.removeFlagged(set)
			}
		}
	}
	if c.status != nil {
		return c.status, nil
	}
	return &imap.StatusResp{Type: imap.StatusRespOk}, nil
}

func TestExpungeMessagesUsesUIDExpungeAndReportsWhatWent(t *testing.T) {
	client := &fakeExpungeClient{uids: []uint32{7, 8, 9}, uidValidity: 4321, uidPlus: true}

	gone, err := expungeMessages(context.Background(), client, " Trash ", []uint32{7, 9}, 4321, syncer.ExpungeWholeFolder)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(gone, []uint32{7, 9}) {
		t.Fatalf("gone = %v, want the two requested UIDs", gone)
	}
	if client.selected != "Trash" || client.readOnly {
		t.Fatalf("selected = %q readOnly=%t, want Trash read-write", client.selected, client.readOnly)
	}
	if client.storedSeqSet != "7,9" {
		t.Fatalf("stored sequence set = %q, want 7,9", client.storedSeqSet)
	}
	if !reflect.DeepEqual(client.storedFlags, []any{imap.DeletedFlag}) {
		t.Fatalf("stored flags = %#v, want the deleted flag", client.storedFlags)
	}
	if client.storeItem != imap.FormatFlagsOp(imap.AddFlags, true) {
		t.Fatalf("store item = %v, want a silent flag addition", client.storeItem)
	}
	if client.command == nil {
		t.Fatal("UID EXPUNGE was not executed")
	}
	rendered := client.command.Command()
	if rendered.Name != "UID" || len(rendered.Arguments) != 2 {
		t.Fatalf("command = %+v, want UID EXPUNGE with a sequence set", rendered)
	}
	if name, ok := rendered.Arguments[0].(imap.RawString); !ok || string(name) != "EXPUNGE" {
		t.Fatalf("UID command name = %#v, want EXPUNGE", rendered.Arguments[0])
	}
	if set, ok := rendered.Arguments[1].(imap.RawString); !ok || string(set) != "7,9" {
		t.Fatalf("UID EXPUNGE set = %#v, want 7,9", rendered.Arguments[1])
	}
	if client.plainExpunges != 0 {
		t.Fatalf("plain EXPUNGE calls = %d, want none while UIDPLUS is available", client.plainExpunges)
	}
	if !slices.Equal(client.uids, []uint32{8}) {
		t.Fatalf("server still holds %v, want only the untouched UID 8", client.uids)
	}
}

func TestExpungeMessagesFallsBackToPlainExpunge(t *testing.T) {
	client := &fakeExpungeClient{uids: []uint32{4, 5}, uidValidity: 12, uidPlus: false}

	gone, err := expungeMessages(context.Background(), client, "Trash", []uint32{4, 5}, 12, syncer.ExpungeWholeFolder)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(gone, []uint32{4, 5}) {
		t.Fatalf("gone = %v, want both UIDs", gone)
	}
	if client.plainExpunges != 1 || client.command != nil {
		t.Fatalf("plain expunges = %d command = %v, want one plain EXPUNGE", client.plainExpunges, client.command)
	}
}

// The plain-EXPUNGE fallback removes every message in the folder carrying
// \Deleted, which is only what was asked for when the whole folder is going. A
// caller that named its UIDs is refused instead, and refused before anything is
// flagged: flagging first would leave exactly the residue the next plain
// EXPUNGE from any client deletes.
func TestExpungeMessagesRefusesToWidenASelectiveDeleteWithoutUIDPlus(t *testing.T) {
	client := &fakeExpungeClient{uids: []uint32{4, 5, 6}, uidValidity: 12, uidPlus: false}

	_, err := expungeMessages(context.Background(), client, "Trash", []uint32{4}, 12, syncer.ExpungeNamedUIDsOnly)
	if !errors.Is(err, syncer.ErrSelectiveExpungeUnsupported) {
		t.Fatalf("error = %v, want ErrSelectiveExpungeUnsupported", err)
	}
	if client.storedSeqSet != "" || client.plainExpunges != 0 || client.command != nil {
		t.Fatalf("the refusal still touched the folder: stored=%q plain=%d command=%v",
			client.storedSeqSet, client.plainExpunges, client.command)
	}
	if !slices.Equal(client.uids, []uint32{4, 5, 6}) {
		t.Fatalf("server holds %v, want the folder untouched", client.uids)
	}
}

// A server that does have UID EXPUNGE removes exactly the named UIDs, so a
// selective delete is served normally.
func TestExpungeMessagesServesASelectiveDeleteWithUIDPlus(t *testing.T) {
	client := &fakeExpungeClient{uids: []uint32{4, 5, 6}, uidValidity: 12, uidPlus: true}

	gone, err := expungeMessages(context.Background(), client, "Trash", []uint32{5}, 12, syncer.ExpungeNamedUIDsOnly)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(gone, []uint32{5}) {
		t.Fatalf("gone = %v, want only the named UID", gone)
	}
	if !slices.Equal(client.uids, []uint32{4, 6}) {
		t.Fatalf("server holds %v, want the unnamed mail kept", client.uids)
	}
}

func TestExpungeMessagesReportsUIDsTheServerKept(t *testing.T) {
	client := &fakeExpungeClient{uids: []uint32{1, 2}, uidValidity: 77, uidPlus: true}
	// A server that acknowledges the command without removing anything must not
	// be mistaken for a finished delete.
	client.status = &imap.StatusResp{Type: imap.StatusRespOk}
	client.storedSeqSet = ""
	stubborn := &stubbornExpungeClient{fakeExpungeClient: client}

	gone, err := expungeMessages(context.Background(), stubborn, "Trash", []uint32{1, 2}, 77, syncer.ExpungeWholeFolder)
	if err != nil {
		t.Fatal(err)
	}
	if len(gone) != 0 {
		t.Fatalf("gone = %v, want nothing reported as deleted", gone)
	}
}

// stubbornExpungeClient acknowledges every command but never removes a message.
type stubbornExpungeClient struct {
	*fakeExpungeClient
}

func (c *stubbornExpungeClient) Execute(command imap.Commander, _ responses.Handler) (*imap.StatusResp, error) {
	c.command = command
	return &imap.StatusResp{Type: imap.StatusRespOk}, nil
}

func TestExpungeMessagesRefusesAnotherMailboxGeneration(t *testing.T) {
	client := &fakeExpungeClient{uids: []uint32{3}, uidValidity: 99, uidPlus: true}

	_, err := expungeMessages(context.Background(), client, "Trash", []uint32{3}, 98, syncer.ExpungeWholeFolder)
	if err == nil || !strings.Contains(err.Error(), "UIDVALIDITY") {
		t.Fatalf("error = %v, want a refusal naming UIDVALIDITY", err)
	}
	if client.storedSeqSet != "" || client.plainExpunges != 0 || client.command != nil {
		t.Fatal("a mailbox generation mismatch still ran the delete")
	}
}

func TestExpungeMessagesValidatesItsRequest(t *testing.T) {
	ctx := context.Background()
	if err := validateExpungeRequest(ctx, "", []uint32{1}, 5); err == nil {
		t.Fatal("an empty mailbox name was accepted")
	}
	if err := validateExpungeRequest(ctx, "Trash", nil, 5); err == nil {
		t.Fatal("an empty UID set was accepted")
	}
	if err := validateExpungeRequest(ctx, "Trash", []uint32{1}, 0); err == nil {
		t.Fatal("a missing UIDVALIDITY was accepted")
	}
	if err := validateExpungeRequest(ctx, "Trash", []uint32{0}, 5); err == nil {
		t.Fatal("a zero UID was accepted")
	}
	oversized := make([]uint32, expungeBatchLimit+1)
	for i := range oversized {
		oversized[i] = uint32(i + 1)
	}
	if err := validateExpungeRequest(ctx, "Trash", oversized, 5); err == nil {
		t.Fatal("an oversized batch was accepted")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := validateExpungeRequest(cancelled, "Trash", []uint32{1}, 5); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the cancelled context", err)
	}
}

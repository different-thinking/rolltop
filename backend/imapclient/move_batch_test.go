package imapclient

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/responses"

	"rolltop/backend/syncer"
)

// fakeBatchMoveClient answers a batched move the way a UIDPLUS server does: the
// UIDs it still holds come back from the search, and the ones it moves come back
// paired in a COPYUID response code.
type fakeBatchMoveClient struct {
	moveSupported bool
	present       []uint32
	searchResults [][]uint32
	searchErr     error
	status        *imap.StatusResp
	executeErr    error

	selected     string
	readOnly     bool
	selects      int
	searches     int
	executes     int
	searchedSets []string
	command      imap.Commander
}

func (f *fakeBatchMoveClient) Select(mailbox string, readOnly bool) (*imap.MailboxStatus, error) {
	f.selected = mailbox
	f.readOnly = readOnly
	f.selects++
	return &imap.MailboxStatus{UidValidity: 830}, nil
}

func (f *fakeBatchMoveClient) UidSearch(criteria *imap.SearchCriteria) ([]uint32, error) {
	f.searches++
	if criteria != nil && criteria.Uid != nil {
		f.searchedSets = append(f.searchedSets, criteria.Uid.String())
	}
	if len(f.searchResults) > 0 {
		result := f.searchResults[0]
		f.searchResults = f.searchResults[1:]
		return result, f.searchErr
	}
	return f.present, f.searchErr
}

func (f *fakeBatchMoveClient) Support(capability string) (bool, error) {
	return f.moveSupported && capability == "MOVE", nil
}

func (f *fakeBatchMoveClient) Execute(command imap.Commander, _ responses.Handler) (*imap.StatusResp, error) {
	f.command = command
	f.executes++
	return f.status, f.executeErr
}

func outcomesByUID(t *testing.T, outcomes []syncer.MoveOutcome) map[uint32]syncer.MoveOutcome {
	t.Helper()
	byUID := make(map[uint32]syncer.MoveOutcome, len(outcomes))
	for _, outcome := range outcomes {
		if _, duplicate := byUID[outcome.UID]; duplicate {
			t.Fatalf("UID %d was reported twice", outcome.UID)
		}
		byUID[outcome.UID] = outcome
	}
	return byUID
}

// A batch costs one SELECT, one UID SEARCH and one UID MOVE however many
// messages it carries. That is the whole point of it: those three used to be
// paid per message.
func TestMoveMessagesWithReceiptsUsesOneCommandPerBatch(t *testing.T) {
	client := &fakeBatchMoveClient{
		moveSupported: true,
		present:       []uint32{42, 43, 44},
		status: &imap.StatusResp{
			Type:      imap.StatusRespOk,
			Code:      copyUIDResponseCode,
			Arguments: []interface{}{"830", "42:44", "109:111"},
		},
	}

	outcomes, err := moveMessagesWithReceipts(context.Background(), client, " Spam ", " Inbox ", []uint32{42, 43, 44}, 830)
	if err != nil {
		t.Fatal(err)
	}

	if client.selects != 1 || client.searches != 1 || client.executes != 1 {
		t.Fatalf("selects=%d searches=%d executes=%d, want one of each for the batch",
			client.selects, client.searches, client.executes)
	}
	if client.selected != "Spam" || client.readOnly {
		t.Fatalf("selected mailbox = %q readOnly=%t, want Spam read-write", client.selected, client.readOnly)
	}
	command := client.command.Command()
	if command.Name != "UID" || len(command.Arguments) != 3 {
		t.Fatalf("command = %+v, want UID MOVE with two arguments", command)
	}
	seqset, ok := command.Arguments[1].(*imap.SeqSet)
	if !ok || seqset.String() != "42:44" {
		t.Fatalf("UID sequence set = %#v, want the whole batch as one set", command.Arguments[1])
	}
	if got, ok := command.Arguments[2].(string); !ok || got != "Inbox" {
		t.Fatalf("destination = %#v, want Inbox", command.Arguments[2])
	}
	// COPYUID pairs the two sets positionally, so each source UID has to come
	// back with the destination UID that sits at its own place in the pair.
	byUID := outcomesByUID(t, outcomes)
	for uid, wantDestination := range map[uint32]uint32{42: 109, 43: 110, 44: 111} {
		outcome := byUID[uid]
		if outcome.Err != nil {
			t.Fatalf("UID %d reported %v, want it moved", uid, outcome.Err)
		}
		if outcome.Receipt == nil || outcome.Receipt.DestinationUID != wantDestination ||
			outcome.Receipt.DestinationUIDValidity != 830 {
			t.Fatalf("UID %d receipt = %+v, want destination UID %d", uid, outcome.Receipt, wantDestination)
		}
	}
}

// A UID the server no longer has belongs to one message. It is reported against
// that message and left out of the command, so the rest of the batch still moves.
func TestMoveMessagesWithReceiptsReportsMissingUIDsPerMessage(t *testing.T) {
	client := &fakeBatchMoveClient{
		moveSupported: true,
		present:       []uint32{42, 44},
		status: &imap.StatusResp{
			Type:      imap.StatusRespOk,
			Code:      copyUIDResponseCode,
			Arguments: []interface{}{"830", "42,44", "109,110"},
		},
	}

	outcomes, err := moveMessagesWithReceipts(context.Background(), client, "Spam", "Inbox", []uint32{42, 43, 44}, 830)
	if err != nil {
		t.Fatal(err)
	}

	byUID := outcomesByUID(t, outcomes)
	if len(byUID) != 3 {
		t.Fatalf("outcomes = %d, want one per requested UID", len(byUID))
	}
	if byUID[43].Err == nil || !strings.Contains(byUID[43].Err.Error(), "no longer contains UID 43") {
		t.Fatalf("UID 43 outcome = %+v, want it reported as gone from the source", byUID[43])
	}
	if byUID[42].Err != nil || byUID[44].Err != nil {
		t.Fatalf("the rest of the batch did not move: %+v", outcomes)
	}
	seqset, ok := client.command.Command().Arguments[1].(*imap.SeqSet)
	if !ok || seqset.String() != "42,44" {
		t.Fatalf("UID sequence set = %#v, want the missing UID left out", client.command.Command().Arguments[1])
	}
}

// A refused batch is one answer for messages that each need their own, so the
// UIDs the server no longer holds are recorded as moved and the rest as failed.
func TestMoveMessagesWithReceiptsSettlesARefusedBatchPerMessage(t *testing.T) {
	client := &fakeBatchMoveClient{
		moveSupported: true,
		// The pre-move search sees all three; the search after the refusal shows
		// the server had already moved 42 before it gave up on the rest.
		searchResults: [][]uint32{{42, 43, 44}, {43, 44}},
		status:        &imap.StatusResp{Type: imap.StatusRespNo, Info: "over quota"},
	}

	outcomes, err := moveMessagesWithReceipts(context.Background(), client, "Spam", "Inbox", []uint32{42, 43, 44}, 830)
	if err != nil {
		t.Fatal(err)
	}

	byUID := outcomesByUID(t, outcomes)
	if byUID[42].Err != nil {
		t.Fatalf("UID 42 = %+v, want the one the server moved before refusing recorded as moved", byUID[42])
	}
	for _, uid := range []uint32{43, 44} {
		if byUID[uid].Err == nil || !strings.Contains(byUID[uid].Err.Error(), "over quota") {
			t.Fatalf("UID %d = %+v, want the server's refusal", uid, byUID[uid])
		}
	}
	if syncer.IsMoveOutcomeUnknown(byUID[43].Err) {
		t.Fatal("a refusal the source mailbox confirmed is a definite outcome, not an unknown one")
	}
}

// A transport failure leaves nothing known about any message in the batch, so
// none of them may be recorded either way.
func TestMoveMessagesWithReceiptsLeavesATransportFailureUnknown(t *testing.T) {
	client := &fakeBatchMoveClient{
		moveSupported: true,
		present:       []uint32{42, 43},
		executeErr:    errors.New("connection reset"),
	}

	outcomes, err := moveMessagesWithReceipts(context.Background(), client, "Spam", "Inbox", []uint32{42, 43}, 830)
	if outcomes != nil {
		t.Fatalf("outcomes = %+v, want none for a batch whose fate is unknown", outcomes)
	}
	if !syncer.IsMoveOutcomeUnknown(err) {
		t.Fatalf("error = %v, want it marked as an unknown move outcome", err)
	}
}

// The source generation is proved for a batch exactly as it is for one message.
func TestMoveMessagesWithReceiptsRefusesAChangedSourceGeneration(t *testing.T) {
	client := &fakeBatchMoveClient{moveSupported: true, present: []uint32{42}}
	_, err := moveMessagesWithReceipts(context.Background(), client, "Spam", "Inbox", []uint32{42}, 831)
	if err == nil || !strings.Contains(err.Error(), "UIDVALIDITY is 830, expected 831") {
		t.Fatalf("error = %v, want the batch refused before it moves anything", err)
	}
	if client.executes != 0 {
		t.Fatalf("executed %d commands under a generation it could not prove", client.executes)
	}
}

func TestValidateMoveBatchRequestRefusesUnusableSets(t *testing.T) {
	for name, uids := range map[string][]uint32{
		"empty":     {},
		"zero UID":  {42, 0},
		"duplicate": {42, 43, 42},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateMoveBatchRequest(context.Background(), "Spam", "Inbox", uids, 830); err == nil {
				t.Fatalf("UIDs %v were accepted", uids)
			}
		})
	}
	oversized := make([]uint32, moveBatchLimit+1)
	for i := range oversized {
		oversized[i] = uint32(i + 1)
	}
	if err := validateMoveBatchRequest(context.Background(), "Spam", "Inbox", oversized, 830); err == nil {
		t.Fatal("a batch above the per-request ceiling was accepted")
	}
}

// A COPYUID pair that cannot be read as a one-to-one mapping yields no receipts
// at all. A move without destination UIDs is an outcome the caller handles; a
// guessed destination UID is not.
func TestParseBatchMoveReceiptsRefusesAnUnusablePairing(t *testing.T) {
	for name, arguments := range map[string][]interface{}{
		"uneven sets":        {"830", "42:44", "109:110"},
		"open-ended set":     {"830", "42:*", "109:111"},
		"unrequested source": {"830", "42,43,99", "109,110,111"},
		"zero UIDVALIDITY":   {"0", "42,43", "109,110"},
	} {
		t.Run(name, func(t *testing.T) {
			requested := []uint32{42, 43, 44}
			status := &imap.StatusResp{Type: imap.StatusRespOk, Code: copyUIDResponseCode, Arguments: arguments}
			if receipts := parseBatchMoveReceipts(status, requested); receipts != nil {
				t.Fatalf("receipts = %+v, want none", receipts)
			}
		})
	}
}

func TestExpandStaticUIDSetListsRangesAndRefusesDynamicOnes(t *testing.T) {
	uids, ok := expandStaticUIDSet("4,7:9", 5)
	if !ok {
		t.Fatal("a static set was refused")
	}
	want := []uint32{4, 7, 8, 9}
	if len(uids) != len(want) {
		t.Fatalf("expanded = %v, want %v", uids, want)
	}
	for i, uid := range want {
		if uids[i] != uid {
			t.Fatalf("expanded = %v, want %v", uids, want)
		}
	}
	for _, value := range []string{"*", "7:*", "", "nonsense"} {
		if _, ok := expandStaticUIDSet(value, 5); ok {
			t.Fatalf("set %q was expanded", value)
		}
	}
	// A set longer than the batch it should describe is not the batch's answer.
	if _, ok := expandStaticUIDSet("1:100", 5); ok {
		t.Fatal("a set larger than the batch was expanded")
	}
}

package imapclient

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"

	"rolltop/backend/syncer"
	"rolltop/internal/testlog"
)

func TestRawBodySectionUsesPeek(t *testing.T) {
	section := rawBodySection()
	if !section.Peek {
		t.Fatal("raw body section does not use PEEK")
	}
	if got, want := section.FetchItem(), imap.FetchItem("BODY.PEEK[]"); got != want {
		t.Fatalf("raw body fetch item = %q, want %q", got, want)
	}
}

func TestTrainingBodySectionUsesBoundedPeek(t *testing.T) {
	section := trainingBodySection()
	if !section.Peek {
		t.Fatal("training body section does not use PEEK")
	}
	if got, want := section.Partial, []int{0, syncer.MaxTrainingCandidateBodyBytes}; !reflect.DeepEqual(got, want) {
		t.Fatalf("training body partial = %#v, want %#v", got, want)
	}
	if got, want := section.FetchItem(), imap.FetchItem("BODY.PEEK[]<0.524288>"); got != want {
		t.Fatalf("training body fetch item = %q, want %q", got, want)
	}
}

func TestMailboxDiscoveryInfoPreservesAttributesAndSkipsNoSelect(t *testing.T) {
	remote := &imap.MailboxInfo{Name: "Bulk", Attributes: []string{imap.HasNoChildrenAttr, imap.JunkAttr}}
	got, ok := mailboxDiscoveryInfo(remote)
	if !ok || got.Name != "Bulk" || !reflect.DeepEqual(got.Attributes, remote.Attributes) {
		t.Fatalf("mailbox discovery info = %+v, %t", got, ok)
	}
	remote.Attributes[1] = imap.TrashAttr
	if got.Attributes[1] != imap.JunkAttr {
		t.Fatal("mailbox discovery attributes alias the IMAP response")
	}
	if got, ok := mailboxDiscoveryInfo(&imap.MailboxInfo{Name: "Group", Attributes: []string{imap.NoSelectAttr}}); ok {
		t.Fatalf("NoSelect mailbox accepted: %+v", got)
	}
}

func TestSearchTrainingCandidatesIsReadOnlyBoundedAndNewestFirst(t *testing.T) {
	since := time.Date(2026, time.January, 1, 9, 30, 0, 0, time.UTC)
	before := since.Add(30 * 24 * time.Hour)
	fake := &fakeTrainingCandidateClient{
		status:     &imap.MailboxStatus{Messages: 4},
		searchUIDs: []uint32{2, 3, 1, 3, 0},
		messages: map[uint32]*imap.Message{
			1: trainingMetadataMessage(1, "one@example.test"),
			2: trainingMetadataMessage(2, "two@example.test"),
			3: trainingMetadataMessage(3, "three@example.test"),
		},
	}
	got, err := (&Fetcher{BatchSize: 2}).searchTrainingCandidates(context.Background(), fake, "INBOX", syncer.TrainingCandidateQuery{
		Since: since, Before: before, SeenOnly: true, Limit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !fake.readOnly || fake.selected != "INBOX" {
		t.Fatalf("SELECT mailbox=%q readOnly=%t, want INBOX/true", fake.selected, fake.readOnly)
	}
	if fake.criteria == nil || !fake.criteria.Since.Equal(since) || !fake.criteria.Before.Equal(before) || !reflect.DeepEqual(fake.criteria.WithFlags, []string{imap.SeenFlag}) {
		t.Fatalf("search criteria = %+v", fake.criteria)
	}
	if got.Matched != 3 {
		t.Fatalf("matched = %d, want 3 unique UIDs", got.Matched)
	}
	if len(got.Candidates) != 2 || got.Candidates[0].UID != 3 || got.Candidates[1].UID != 2 {
		t.Fatalf("candidates = %+v, want UIDs 3,2", got.Candidates)
	}
	if !reflect.DeepEqual(got.Candidates[0].From, []string{"three@example.test"}) || got.Candidates[0].Subject != "Subject 3" {
		t.Fatalf("candidate envelope = %+v", got.Candidates[0])
	}
	for _, item := range fake.fetchItems {
		if strings.HasPrefix(string(item), "BODY") || item == imap.FetchRFC822 || item == imap.FetchRFC822Header || item == imap.FetchRFC822Text {
			t.Fatalf("metadata search fetched body item %q", item)
		}
	}
}

func TestFetchTrainingCandidatesUsesReadOnlyPeekAndCapsPayload(t *testing.T) {
	raw := bytes.Repeat([]byte("x"), syncer.MaxTrainingCandidateBodyBytes+37)
	fake := &fakeTrainingCandidateClient{
		status: &imap.MailboxStatus{Messages: 1},
		messages: map[uint32]*imap.Message{
			7: {
				Uid:          7,
				InternalDate: time.Date(2026, time.February, 2, 3, 4, 5, 0, time.UTC),
				Size:         uint32(len(raw)),
				Flags:        []string{imap.SeenFlag},
				Body: map[*imap.BodySectionName]imap.Literal{
					{Partial: []int{0}}: bytes.NewReader(raw),
				},
			},
		},
	}
	var got []syncer.TrainingCandidate
	err := (&Fetcher{}).fetchTrainingCandidates(context.Background(), fake, "INBOX", []uint32{7}, func(candidate syncer.TrainingCandidate) error {
		got = append(got, candidate)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !fake.readOnly {
		t.Fatal("training body fetch selected mailbox read-write")
	}
	if len(got) != 1 || got[0].UID != 7 || len(got[0].Raw) != syncer.MaxTrainingCandidateBodyBytes || !got[0].Truncated {
		t.Fatalf("candidate = UID %d bytes %d truncated %t", got[0].UID, len(got[0].Raw), got[0].Truncated)
	}
	wantItem := trainingBodySection().FetchItem()
	if !containsFetchItem(fake.fetchItems, wantItem) {
		t.Fatalf("fetch items = %#v, want %q", fake.fetchItems, wantItem)
	}
}

type fakeTrainingCandidateClient struct {
	status     *imap.MailboxStatus
	searchUIDs []uint32
	messages   map[uint32]*imap.Message
	selected   string
	readOnly   bool
	criteria   *imap.SearchCriteria
	fetchItems []imap.FetchItem
}

func (f *fakeTrainingCandidateClient) Select(name string, readOnly bool) (*imap.MailboxStatus, error) {
	f.selected = name
	f.readOnly = readOnly
	return f.status, nil
}

func (f *fakeTrainingCandidateClient) UidSearch(criteria *imap.SearchCriteria) ([]uint32, error) {
	f.criteria = criteria
	return append([]uint32(nil), f.searchUIDs...), nil
}

func (f *fakeTrainingCandidateClient) UidFetch(seqset *imap.SeqSet, items []imap.FetchItem, ch chan *imap.Message) error {
	defer close(ch)
	f.fetchItems = append(f.fetchItems, items...)
	for uid, message := range f.messages {
		if seqset.Contains(uid) {
			ch <- message
		}
	}
	return nil
}

func trainingMetadataMessage(uid uint32, from string) *imap.Message {
	return &imap.Message{
		Uid:          uid,
		InternalDate: time.Date(2026, time.January, int(uid), 12, 0, 0, 0, time.UTC),
		Size:         100 + uid,
		Flags:        []string{imap.SeenFlag},
		Envelope: &imap.Envelope{
			Date:      time.Date(2026, time.January, int(uid), 11, 0, 0, 0, time.UTC),
			Subject:   fmt.Sprintf("Subject %d", uid),
			From:      []*imap.Address{trainingAddress(from)},
			To:        []*imap.Address{trainingAddress("owner@example.test")},
			MessageId: fmt.Sprintf("<%d@example.test>", uid),
		},
	}
}

func trainingAddress(value string) *imap.Address {
	parts := strings.SplitN(value, "@", 2)
	return &imap.Address{MailboxName: parts[0], HostName: parts[1]}
}

func containsFetchItem(items []imap.FetchItem, want imap.FetchItem) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func TestFetcherCommandTimeoutUsesBoundedDefault(t *testing.T) {
	if got := (*Fetcher)(nil).commandTimeout(); got != 60*time.Second {
		t.Fatalf("nil fetcher timeout = %s", got)
	}
	if got := (&Fetcher{}).commandTimeout(); got != 60*time.Second {
		t.Fatalf("default fetcher timeout = %s", got)
	}
	if got := (&Fetcher{Timeout: 17 * time.Second}).commandTimeout(); got != 17*time.Second {
		t.Fatalf("configured fetcher timeout = %s", got)
	}
}

func TestFetchUIDsDoesNotApplyCommandDeadlineToMessageHandler(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	serverDone := make(chan error, 1)
	serverRelease := make(chan struct{})
	defer close(serverRelease)
	go func() {
		defer serverConn.Close()
		if _, err := io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev1] test server ready\r\n"); err != nil {
			serverDone <- err
			return
		}
		reader := bufio.NewReader(serverConn)
		for i, uid := range []uint32{1, 2} {
			line, err := reader.ReadString('\n')
			if err != nil {
				serverDone <- err
				return
			}
			fields := strings.Fields(line)
			if len(fields) < 4 || !strings.EqualFold(fields[1], "UID") || !strings.EqualFold(fields[2], "FETCH") || fields[3] != fmt.Sprint(uid) {
				serverDone <- fmt.Errorf("unexpected command %q", strings.TrimSpace(line))
				return
			}
			raw := []byte(fmt.Sprintf("Subject: UID %d\r\n\r\nbody", uid))
			if _, err := fmt.Fprintf(serverConn,
				"* %d FETCH (UID %d INTERNALDATE \"16-Jul-2026 00:00:00 +0000\" RFC822.SIZE %d FLAGS () BODY[] {%d}\r\n",
				i+1, uid, len(raw), len(raw)); err != nil {
				serverDone <- err
				return
			}
			if _, err := serverConn.Write(raw); err != nil {
				serverDone <- err
				return
			}
			if _, err := fmt.Fprintf(serverConn, ")\r\n%s OK UID FETCH complete\r\n", fields[0]); err != nil {
				serverDone <- err
				return
			}
		}
		serverDone <- nil
		<-serverRelease
	}()

	c, err := client.New(clientConn)
	if err != nil {
		t.Fatal(err)
	}
	c.ErrorLog = log.New(io.Discard, "", 0)
	defer c.Terminate()
	c.SetState(imap.SelectedState, &imap.MailboxStatus{Name: "INBOX", Messages: 2, UidNext: 3, UidValidity: 1})
	c.Timeout = 20 * time.Millisecond

	var fetched []uint32
	err = (&Fetcher{BatchSize: 1}).fetchUIDs(context.Background(), c, "INBOX", []uint32{1, 2}, func(message syncer.FetchedMessage) error {
		fetched = append(fetched, message.UID)
		if message.UID == 1 {
			time.Sleep(60 * time.Millisecond)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("fetchUIDs() error = %v", err)
	}
	if want := []uint32{1, 2}; !reflect.DeepEqual(fetched, want) {
		t.Fatalf("fetched UIDs = %v, want %v", fetched, want)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestGuardedUIDFetchHonorsConfiguredCommandTimeout(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	serverDone := make(chan error, 1)
	commandReceived := make(chan struct{})
	go func() {
		defer serverConn.Close()
		if _, err := io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev1] test server ready\r\n"); err != nil {
			serverDone <- err
			return
		}
		reader := bufio.NewReader(serverConn)
		if _, err := reader.ReadString('\n'); err != nil {
			serverDone <- err
			return
		}
		close(commandReceived)
		_, err := io.Copy(io.Discard, reader)
		serverDone <- err
	}()

	c, err := client.New(clientConn)
	if err != nil {
		t.Fatal(err)
	}
	c.ErrorLog = log.New(io.Discard, "", 0)
	defer c.Terminate()
	c.SetState(imap.SelectedState, &imap.MailboxStatus{Name: "INBOX", Messages: 1, UidNext: 2, UidValidity: 1})
	c.Timeout = 20 * time.Millisecond
	seqset := new(imap.SeqSet)
	seqset.AddNum(1)
	messages := make(chan *imap.Message, 1)
	// The parent deadline is deliberately much longer. The configured client
	// timeout must still bound the active command after its socket deadline is
	// cleared to avoid leaking that deadline into message handling.
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	started := time.Now()
	err = guardedUIDFetch(ctx, c, seqset, []imap.FetchItem{imap.FetchUid, rawBodySection().FetchItem()}, messages)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("guardedUIDFetch() error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 150*time.Millisecond {
		t.Fatalf("guardedUIDFetch() took %s, want configured timeout before parent deadline", elapsed)
	}
	select {
	case <-commandReceived:
	default:
		t.Fatal("server did not receive UID FETCH")
	}
	if _, ok := <-messages; ok {
		t.Fatal("UID FETCH message channel remained open")
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestTerminateClientOnContextUnblocksStalledCommand(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	serverDone := make(chan error, 1)
	commandReceived := make(chan struct{})
	go func() {
		defer serverConn.Close()
		if _, err := io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev1] test server ready\r\n"); err != nil {
			serverDone <- err
			return
		}
		reader := bufio.NewReader(serverConn)
		if _, err := reader.ReadString('\n'); err != nil {
			serverDone <- err
			return
		}
		close(commandReceived)
		_, err := io.Copy(io.Discard, reader)
		serverDone <- err
	}()

	c, err := client.New(clientConn)
	if err != nil {
		t.Fatal(err)
	}
	c.ErrorLog = log.New(io.Discard, "", 0)
	c.SetState(imap.AuthenticatedState, nil)
	c.Timeout = time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	cleanup := terminateClientOnContext(ctx, c)
	defer cleanup()

	started := time.Now()
	_, err = c.Status("INBOX", []imap.StatusItem{imap.StatusUidNext})
	if err == nil {
		t.Fatal("stalled STATUS unexpectedly succeeded")
	}
	if elapsed := time.Since(started); elapsed > 150*time.Millisecond {
		t.Fatalf("stalled STATUS took %s after context deadline", elapsed)
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("context error = %v, want deadline exceeded", ctx.Err())
	}
	select {
	case <-commandReceived:
	default:
		t.Fatal("server did not receive STATUS")
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestTerminateClientSuppressesIntentionalCloseLog(t *testing.T) {
	c, serverDone := newCleanupTestClient(t)
	var errorLog bytes.Buffer
	c.ErrorLog = log.New(&errorLog, "", 0)

	if err := terminateClient(c); err != nil {
		t.Fatal(err)
	}
	<-c.LoggedOut()
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(errorLog.String()); got != "" {
		t.Fatalf("intentional client close logged an error: %q", got)
	}
}

func TestUnmarkedClientCloseStillLogsTransportError(t *testing.T) {
	c, serverDone := newCleanupTestClient(t)
	var errorLog bytes.Buffer
	c.ErrorLog = log.New(&errorLog, "", 0)

	if err := c.Terminate(); err != nil {
		t.Fatal(err)
	}
	<-c.LoggedOut()
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(errorLog.String()); got == "" {
		t.Fatal("unmarked transport close was unexpectedly suppressed")
	}
}

func newCleanupTestClient(t *testing.T) (*client.Client, <-chan error) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	serverDone := make(chan error, 1)
	go func() {
		defer serverConn.Close()
		if _, err := io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev1] test server ready\r\n"); err != nil {
			serverDone <- err
			return
		}
		_, err := io.Copy(io.Discard, serverConn)
		serverDone <- err
	}()
	c, err := client.New(clientConn)
	if err != nil {
		t.Fatal(err)
	}
	return c, serverDone
}

func TestProbeCapabilitiesReportsAuthenticatedServerSupport(t *testing.T) {
	supporter := &fakeCapabilitySupporter{supported: map[string]bool{"IDLE": true}}
	got, err := probeCapabilities(supporter)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IDLE || got.UIDPlus {
		t.Fatalf("capabilities = %+v, want IDLE only", got)
	}
	if !reflect.DeepEqual(supporter.calls, []string{"IDLE", "UIDPLUS"}) {
		t.Fatalf("Support calls = %#v", supporter.calls)
	}
}

func TestProbeCapabilitiesReturnsSupportError(t *testing.T) {
	want := errors.New("capability failed")
	_, err := probeCapabilities(&fakeCapabilitySupporter{errFor: "UIDPLUS", err: want})
	if !errors.Is(err, want) || !strings.Contains(err.Error(), "UIDPLUS") {
		t.Fatalf("probeCapabilities error = %v", err)
	}
}

type fakeCapabilitySupporter struct {
	supported map[string]bool
	errFor    string
	err       error
	calls     []string
}

func (f *fakeCapabilitySupporter) Support(capability string) (bool, error) {
	f.calls = append(f.calls, capability)
	if capability == f.errFor {
		return false, f.err
	}
	return f.supported[capability], nil
}

func TestOrderFetchedUIDBatchSortsServerResponses(t *testing.T) {
	fetched := []syncer.FetchedMessage{{UID: 9}, {UID: 3}, {UID: 7}}
	got, err := orderFetchedUIDBatch([]uint32{3, 7, 9}, fetched)
	if err != nil {
		t.Fatal(err)
	}
	if gotUIDs := []uint32{got[0].UID, got[1].UID, got[2].UID}; !reflect.DeepEqual(gotUIDs, []uint32{3, 7, 9}) {
		t.Fatalf("ordered UIDs = %#v", gotUIDs)
	}
}

func TestOrderFetchedUIDBatchRejectsMissingUIDBeforeDelivery(t *testing.T) {
	got, err := orderFetchedUIDBatch([]uint32{3, 7, 9}, []syncer.FetchedMessage{{UID: 9}, {UID: 3}})
	if err == nil || !strings.Contains(err.Error(), "UID batch 7") {
		t.Fatalf("missing UID error = %v", err)
	}
	if got != nil {
		t.Fatalf("partial ordered batch = %#v, want nil", got)
	}
}

func TestOrderFetchedUIDBatchIgnoresUnsolicitedUIDAndRejectsDuplicates(t *testing.T) {
	got, err := orderFetchedUIDBatch([]uint32{3}, []syncer.FetchedMessage{{UID: 99}, {UID: 3}})
	if err != nil || len(got) != 1 || got[0].UID != 3 {
		t.Fatalf("unsolicited UID result = %#v, %v", got, err)
	}
	if _, err := orderFetchedUIDBatch([]uint32{3}, []syncer.FetchedMessage{{UID: 3}, {UID: 3}}); err == nil {
		t.Fatal("duplicate requested UID was accepted")
	}
}

func TestStopIdleSessionStopsCleanly(t *testing.T) {
	stop := make(chan struct{})
	done := make(chan error, 1)
	terminated := false
	go func() {
		<-stop
		done <- nil
	}()

	if err := stopIdleSession(stop, done, func() error {
		terminated = true
		return nil
	}, time.Second); err != nil {
		t.Fatalf("stopIdleSession error = %v", err)
	}
	if terminated {
		t.Fatalf("terminate called for clean IDLE stop")
	}
}

func TestStopIdleSessionTerminatesStuckIdle(t *testing.T) {
	stop := make(chan struct{})
	done := make(chan error)
	terminated := false

	err := stopIdleSession(stop, done, func() error {
		terminated = true
		return nil
	}, 10*time.Millisecond)
	if !errors.Is(err, errIdleStopTimeout) {
		t.Fatalf("stopIdleSession error = %v, want errIdleStopTimeout", err)
	}
	if !terminated {
		t.Fatalf("terminate was not called for stuck IDLE stop")
	}
	select {
	case <-stop:
	default:
		t.Fatalf("stop channel was not closed")
	}
}

func TestMailboxUIDSearchCriteriaCombinesUIDAndSince(t *testing.T) {
	since := time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC)
	criteria, ok := mailboxUIDSearchCriteria(41, since)
	if !ok || criteria == nil {
		t.Fatal("mailboxUIDSearchCriteria returned no criteria")
	}
	if criteria.Uid == nil || !criteria.Uid.Contains(42) || !criteria.Uid.Contains(900) || criteria.Uid.Contains(41) {
		t.Fatalf("UID criteria = %v, want 42:*", criteria.Uid)
	}
	if !criteria.Since.Equal(since) {
		t.Fatalf("Since = %s, want %s", criteria.Since, since)
	}
	if criteria, ok := mailboxUIDSearchCriteria(^uint32(0), since); ok || criteria != nil {
		t.Fatalf("maximum UID criteria = %#v, %t, want nil, false", criteria, ok)
	}
}

func TestMessageSyncMarkerIsDeterministicAndDelimited(t *testing.T) {
	got, err := MessageSyncMarker("task_abc-123", 7, 42)
	if err != nil {
		t.Fatal(err)
	}
	if want := "v1.task_abc-123.0000000007.0000000042"; got != want {
		t.Fatalf("marker = %q, want %q", got, want)
	}
	other, err := MessageSyncMarker("task_abc-123", 7, 420)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(other, got) || strings.Contains(got, other) {
		t.Fatalf("markers are ambiguous substrings: %q and %q", got, other)
	}
	for _, tc := range []struct {
		token       string
		uidValidity uint32
		uid         uint32
	}{
		{"", 1, 1},
		{"bad token", 1, 1},
		{"bad\r\ntoken", 1, 1},
		{"task", 0, 1},
		{"task", 1, 0},
	} {
		if _, err := MessageSyncMarker(tc.token, tc.uidValidity, tc.uid); err == nil {
			t.Fatalf("MessageSyncMarker(%q, %d, %d) succeeded", tc.token, tc.uidValidity, tc.uid)
		}
	}
}

func TestAddSyncMarkerHeaderPreservesRawMessageAndIsIdempotent(t *testing.T) {
	marker, err := MessageSyncMarker("task", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: sender@example.test\r\nSubject: Test\r\n\r\nbody\r\n")
	marked, err := AddSyncMarkerHeader(raw, marker)
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := []byte(SyncMarkerHeader + ": " + marker + "\r\n")
	if !bytes.HasPrefix(marked, wantPrefix) {
		t.Fatalf("marked message prefix = %q, want %q", marked[:len(wantPrefix)], wantPrefix)
	}
	if !bytes.Equal(marked[len(wantPrefix):], raw) {
		t.Fatal("marker insertion changed the original raw message")
	}
	again, err := AddSyncMarkerHeader(marked, marker)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, marked) {
		t.Fatal("adding the same marker twice was not idempotent")
	}
	if _, err := AddSyncMarkerHeader(raw, "bad\r\nInjected: value"); err == nil {
		t.Fatal("header-injection marker was accepted")
	}
}

func TestAddSyncMarkerHeaderUsesSourceLineEndings(t *testing.T) {
	raw := []byte("From: sender@example.test\nSubject: Test\n\nbody\n")
	marked, err := AddSyncMarkerHeader(raw, "v1.task.0000000001.0000000002")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(marked, []byte(SyncMarkerHeader+": v1.task.0000000001.0000000002\n")) {
		t.Fatalf("marked LF message = %q", marked)
	}
}

func TestAddSyncHeadersRecordsUTCTransferTimeAndPreservesRaw(t *testing.T) {
	marker := "v1.task.0000000001.0000000002"
	zone := time.FixedZone("source", -6*60*60)
	syncedAt := time.Date(2026, time.July, 14, 13, 42, 9, 987654321, zone)
	raw := []byte("From: sender@example.test\r\nSubject: Test\r\n\r\nbody\r\n")
	original := append([]byte(nil), raw...)

	marked, err := AddSyncHeaders(raw, marker, syncedAt)
	if err != nil {
		t.Fatal(err)
	}
	value := syncedAt.UTC().Truncate(time.Second).Format(time.RFC3339)
	wantPrefix := []byte(SyncTimestampHeader + ": " + value + "\r\n" + SyncMarkerHeader + ": " + marker + "\r\n")
	if !bytes.HasPrefix(marked, wantPrefix) {
		t.Fatalf("sync headers prefix = %q, want %q", marked[:min(len(marked), len(wantPrefix))], wantPrefix)
	}
	if !bytes.Equal(marked[len(wantPrefix):], original) {
		t.Fatal("sync header insertion changed the original raw message")
	}
	if !bytes.Equal(raw, original) {
		t.Fatal("sync header insertion mutated the caller's raw message")
	}
	gotTime, ok := SyncTimestampForMarker(marked, marker)
	if !ok || !gotTime.Equal(syncedAt.UTC().Truncate(time.Second)) {
		t.Fatalf("SyncTimestampForMarker() = %s, %t, want %s, true", gotTime, ok, syncedAt.UTC().Truncate(time.Second))
	}
}

func TestAddSyncHeadersPreservesFirstTimestampForExistingMarker(t *testing.T) {
	marker := "v1.task.0000000001.0000000002"
	firstTime := time.Date(2026, time.July, 14, 19, 42, 9, 0, time.UTC)
	first, err := AddSyncHeaders([]byte("Subject: Test\r\n\r\nbody"), marker, firstTime)
	if err != nil {
		t.Fatal(err)
	}

	again, err := AddSyncHeaders(first, marker, firstTime.Add(8*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, first) {
		t.Fatal("reapplying an existing marker changed its original transfer timestamp")
	}
	if got := bytes.Count(again, []byte(SyncMarkerHeader+":")); got != 1 {
		t.Fatalf("marker header count = %d, want 1", got)
	}
	if got := bytes.Count(again, []byte(SyncTimestampHeader+":")); got != 1 {
		t.Fatalf("timestamp header count = %d, want 1", got)
	}
}

func TestAddSyncHeadersDoesNotReuseTimestampWithoutMarker(t *testing.T) {
	marker := "v1.task.0000000001.0000000002"
	syncedAt := time.Date(2026, time.July, 14, 19, 42, 9, 0, time.UTC)
	value := syncedAt.Format(time.RFC3339)
	raw := []byte(SyncTimestampHeader + ": " + value + "\r\nSubject: Unrelated header\r\n\r\nbody")

	marked, err := AddSyncHeaders(raw, marker, syncedAt)
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := []byte(SyncTimestampHeader + ": " + value + "\r\n" + SyncMarkerHeader + ": " + marker + "\r\n")
	if !bytes.HasPrefix(marked, wantPrefix) {
		t.Fatalf("sync headers prefix = %q, want %q", marked[:min(len(marked), len(wantPrefix))], wantPrefix)
	}
	if got := bytes.Count(marked, []byte(SyncTimestampHeader+":")); got != 2 {
		t.Fatalf("timestamp header count = %d, want the new and unrelated headers", got)
	}
	gotTime, ok := SyncTimestampForMarker(marked, marker)
	if !ok || !gotTime.Equal(syncedAt) {
		t.Fatalf("SyncTimestampForMarker() = %s, %t, want %s, true", gotTime, ok, syncedAt)
	}

	again, err := AddSyncHeaders(marked, marker, syncedAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, marked) {
		t.Fatal("reapplying the new marker changed its timestamp")
	}
}

func TestAddSyncHeadersRepairsMarkerWithoutValidTimestamp(t *testing.T) {
	marker := "v1.task.0000000001.0000000002"
	syncedAt := time.Date(2026, time.July, 14, 19, 42, 9, 0, time.UTC)
	raw := []byte(SyncMarkerHeader + ": " + marker + "\n" + SyncTimestampHeader + ": not-a-date\nSubject: Legacy\n\nbody")

	marked, err := AddSyncHeaders(raw, marker, syncedAt)
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := []byte(SyncTimestampHeader + ": " + syncedAt.Format(time.RFC3339) + "\n")
	if !bytes.HasPrefix(marked, wantPrefix) {
		t.Fatalf("repaired LF message = %q", marked)
	}
	if got := bytes.Count(marked, []byte(SyncMarkerHeader+":")); got != 1 {
		t.Fatalf("marker header count = %d, want 1", got)
	}
	gotTime, ok := SyncTimestampForMarker(marked, marker)
	if !ok || !gotTime.Equal(syncedAt) {
		t.Fatalf("SyncTimestampForMarker() = %s, %t, want %s, true", gotTime, ok, syncedAt)
	}
}

func TestAddSyncHeadersRejectsInvalidInputs(t *testing.T) {
	raw := []byte("Subject: Test\r\n\r\nbody")
	marker := "v1.task.0000000001.0000000002"
	for _, syncedAt := range []time.Time{
		time.Time{},
		time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC),
		time.Date(1, time.January, 1, 0, 0, 0, 0, time.FixedZone("+14", 14*60*60)),
		time.Date(9999, time.December, 31, 23, 59, 59, 0, time.FixedZone("-12", -12*60*60)),
	} {
		if _, err := AddSyncHeaders(raw, marker, syncedAt); err == nil {
			t.Fatalf("AddSyncHeaders accepted invalid timestamp %v", syncedAt)
		}
	}
	if _, err := AddSyncHeaders(raw, "bad\r\nInjected: value", time.Now()); err == nil {
		t.Fatal("AddSyncHeaders accepted a header-injection marker")
	}
}

func TestAddSyncHeadersAcceptsRFC3339UTCYearBoundaries(t *testing.T) {
	marker := "v1.task.0000000001.0000000002"
	for _, syncedAt := range []time.Time{
		time.Date(1, time.January, 1, 14, 0, 1, 0, time.FixedZone("+14", 14*60*60)),
		time.Date(9999, time.December, 31, 11, 59, 59, 0, time.FixedZone("-12", -12*60*60)),
	} {
		marked, err := AddSyncHeaders([]byte("Subject: Boundary\r\n\r\nbody"), marker, syncedAt)
		if err != nil {
			t.Fatalf("AddSyncHeaders(%v): %v", syncedAt, err)
		}
		got, ok := SyncTimestampForMarker(marked, marker)
		if !ok || !got.Equal(syncedAt.UTC()) {
			t.Fatalf("SyncTimestampForMarker() = %s, %t, want %s, true", got, ok, syncedAt.UTC())
		}
	}
}

func TestSyncTimestampForMarkerRequiresExactAdjacentPair(t *testing.T) {
	marker := "v1.task.0000000001.0000000002"
	want := time.Date(2026, time.July, 14, 19, 42, 9, 0, time.UTC)
	raw := []byte(SyncTimestampHeader + ": 2025-01-01T00:00:00Z\r\n" +
		"Subject: unrelated timestamp\r\n" +
		strings.ToLower(SyncTimestampHeader) + ": 2026-07-14T13:42:09-06:00\r\n" +
		strings.ToLower(SyncMarkerHeader) + ": " + marker + "\r\n\r\nbody")
	got, ok := SyncTimestampForMarker(raw, marker)
	if !ok || !got.Equal(want) {
		t.Fatalf("SyncTimestampForMarker() = %s, %t, want %s, true", got, ok, want)
	}
}

func TestSyncTimestampForMarkerRejectsSpoofedAndLegacyLayouts(t *testing.T) {
	marker := "v1.task.0000000001.0000000002"
	otherMarker := "v1.other.0000000001.0000000002"
	timestamp := SyncTimestampHeader + ": 2026-07-14T19:42:09Z\r\n"
	markerLine := SyncMarkerHeader + ": " + marker + "\r\n"
	otherMarkerLine := SyncMarkerHeader + ": " + otherMarker + "\r\n"
	for name, raw := range map[string][]byte{
		"orphan timestamp":       []byte(timestamp + "Subject: Test\r\n\r\nbody"),
		"legacy marker only":     []byte(markerLine + "Subject: Test\r\n\r\nbody"),
		"wrong marker pair":      []byte(timestamp + otherMarkerLine + markerLine + "\r\nbody"),
		"separated pair":         []byte(timestamp + "Subject: gap\r\n" + markerLine + "\r\nbody"),
		"reversed pair":          []byte(markerLine + timestamp + "\r\nbody"),
		"malformed timestamp":    []byte(SyncTimestampHeader + ": no\r\n" + markerLine + "\r\nbody"),
		"out-of-range timestamp": []byte(SyncTimestampHeader + ": 0000-01-01T00:00:00Z\r\n" + markerLine + "\r\nbody"),
		"body-only pair":         []byte("Subject: Test\r\n\r\n" + timestamp + markerLine),
	} {
		t.Run(name, func(t *testing.T) {
			if got, ok := SyncTimestampForMarker(raw, marker); ok || !got.IsZero() {
				t.Fatalf("SyncTimestampForMarker() = %s, %t, want zero, false", got, ok)
			}
		})
	}
	if got, ok := SyncTimestampForMarker([]byte(timestamp+markerLine+"\r\nbody"), "bad\r\nInjected: value"); ok || !got.IsZero() {
		t.Fatalf("invalid marker SyncTimestampForMarker() = %s, %t, want zero, false", got, ok)
	}
}

func TestHasSyncMarkerForTaskMatchesOnlyValidHeaderMarkers(t *testing.T) {
	raw := []byte("Subject: Test\r\nX-Rolltop-Sync-ID: v1.task_abc.0000000007.0000000042\r\n\r\nbody\r\n")
	if !HasSyncMarkerForTask(raw, "task_abc") {
		t.Fatal("valid task marker was not detected")
	}
	if HasSyncMarkerForTask(raw, "task") {
		t.Fatal("a marker for another task was accepted")
	}
	invalid := []byte("X-Rolltop-Sync-ID: v1.task_abc.not-a-uid.0000000042\r\n\r\nbody")
	if HasSyncMarkerForTask(invalid, "task_abc") {
		t.Fatal("an invalid marker was accepted")
	}
	bodyOnly := []byte("Subject: Test\r\n\r\nX-Rolltop-Sync-ID: v1.task_abc.0000000007.0000000042")
	if HasSyncMarkerForTask(bodyOnly, "task_abc") {
		t.Fatal("a marker in the message body was accepted")
	}
}

func TestSafeAppendFlagsKeepsOnlyPortableNonDestructiveFlags(t *testing.T) {
	got := SafeAppendFlags([]string{
		imap.SeenFlag,
		"\\seen",
		imap.AnsweredFlag,
		imap.FlaggedFlag,
		imap.DraftFlag,
		imap.DeletedFlag,
		imap.RecentFlag,
		"custom-keyword",
		"",
	})
	want := []string{imap.SeenFlag, imap.AnsweredFlag, imap.FlaggedFlag, imap.DraftFlag}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SafeAppendFlags() = %#v, want %#v", got, want)
	}
}

func TestSyncDestinationSessionUIDValidityIsNilSafeAndPersistent(t *testing.T) {
	var nilSession *SyncDestinationSession
	if got := nilSession.UIDValidity(); got != 0 {
		t.Fatalf("nil session UIDValidity() = %d, want 0", got)
	}

	session := &SyncDestinationSession{uidValidity: 987654321}
	if got := session.UIDValidity(); got != 987654321 {
		t.Fatalf("UIDValidity() = %d, want 987654321", got)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if got := session.UIDValidity(); got != 987654321 {
		t.Fatalf("closed session UIDValidity() = %d, want 987654321", got)
	}
}

func TestClosedSyncDestinationSessionRejectsOperations(t *testing.T) {
	session := &SyncDestinationSession{}
	if _, _, err := session.FindMessageBySyncMarker(context.Background(), "v1.task.0000000001.0000000002"); err == nil {
		t.Fatal("closed destination session searched for a marker")
	}
	if _, err := session.AppendMessageWithSyncMarker(context.Background(), []byte("Subject: Test\r\n\r\nbody"), "v1.task.0000000001.0000000002", time.Time{}, nil); err == nil {
		t.Fatal("closed destination session appended a message")
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close() on closed destination session = %v", err)
	}
	if err := (*SyncDestinationSession)(nil).Close(); err != nil {
		t.Fatalf("Close() on nil destination session = %v", err)
	}
}

func TestSyncDestinationSessionHonorsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	session := &SyncDestinationSession{}
	if _, _, err := session.FindMessageBySyncMarker(ctx, "v1.task.0000000001.0000000002"); !errors.Is(err, context.Canceled) {
		t.Fatalf("FindMessageBySyncMarker() error = %v, want context.Canceled", err)
	}
}

func TestPlanUIDFetchBatchesRespectsBothBudgets(t *testing.T) {
	uids := []uint32{1, 2, 3, 4, 5, 6}
	sizes := map[uint32]int64{1: 1000, 2: 1000, 3: 1000, 4: 1000, 5: 1000, 6: 1000}

	got := planUIDFetchBatches(uids, sizes, 2, 1<<20)
	if want := [][]uint32{{1, 2}, {3, 4}, {5, 6}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("count-bounded batches = %v, want %v", got, want)
	}

	got = planUIDFetchBatches(uids, sizes, 10, 2500)
	if want := [][]uint32{{1, 2}, {3, 4}, {5, 6}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("byte-bounded batches = %v, want %v", got, want)
	}

	// A message larger than the whole budget still has to be fetched. It gets a
	// batch of its own so nothing else is downloaded next to it.
	sizes[3] = 64 << 20
	got = planUIDFetchBatches(uids, sizes, 10, 4000)
	if want := [][]uint32{{1, 2}, {3}, {4, 5, 6}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("oversized message batches = %v, want %v", got, want)
	}
}

func TestPlanUIDFetchBatchesFallsBackToCountWithoutServerSizes(t *testing.T) {
	uids := make([]uint32, 25)
	for i := range uids {
		uids[i] = uint32(i + 1)
	}
	got := planUIDFetchBatches(uids, nil, defaultFetchBatchSize, defaultFetchBatchBytes)
	if len(got) != 3 {
		t.Fatalf("unsized batches = %d, want 3", len(got))
	}
	for i, batch := range got {
		if i < 2 && len(batch) != defaultFetchBatchSize {
			t.Fatalf("unsized batch %d holds %d UIDs, want %d", i, len(batch), defaultFetchBatchSize)
		}
	}
	if got := planUIDFetchBatches(nil, nil, 10, 1<<20); got != nil {
		t.Fatalf("empty batch plan = %v, want nil", got)
	}
}

// A fetcher that downloads one message at a time has no batch to plan, so it
// must not pay for a size probe either.
func TestUIDProbeWindowsSkipPlanningForSingleMessageFetches(t *testing.T) {
	uids := make([]uint32, uidSizeProbeWindow+5)
	for i := range uids {
		uids[i] = uint32(i + 1)
	}
	if got := uidProbeWindows(uids, 1); len(got) != 1 || len(got[0]) != len(uids) {
		t.Fatalf("single-message windows = %d", len(got))
	}
	windows := uidProbeWindows(uids, 10)
	if len(windows) != 2 || len(windows[0]) != uidSizeProbeWindow || len(windows[1]) != 5 {
		t.Fatalf("probe windows = %d, %v", len(windows), windows)
	}
	if sizes, err := probeUIDSizes(context.Background(), nil, "INBOX", uids, 1); sizes != nil || err != nil {
		t.Fatalf("skipped probe returned %v, %v", sizes, err)
	}
}

// The whole point of the probe: a folder holding a few large messages must not
// put them in one batch, because the batch stays resident until its last
// message has been stored.
func TestFetchUIDsPlansBodyBatchesFromProbedSizes(t *testing.T) {
	const largeBody = 6 << 20
	sizes := map[uint32]int{1: 1024, 2: largeBody, 3: largeBody, 4: 1024}
	clientConn, serverConn := net.Pipe()
	serverDone := make(chan error, 1)
	commands := make(chan string, 8)
	go func() {
		defer serverConn.Close()
		if _, err := io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev1] test server ready\r\n"); err != nil {
			serverDone <- err
			return
		}
		reader := bufio.NewReader(serverConn)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				serverDone <- nil
				return
			}
			fields := strings.Fields(line)
			if len(fields) < 4 || !strings.EqualFold(fields[1], "UID") || !strings.EqualFold(fields[2], "FETCH") {
				serverDone <- fmt.Errorf("unexpected command %q", strings.TrimSpace(line))
				return
			}
			requested, err := parseTestUIDSet(fields[3])
			if err != nil {
				serverDone <- err
				return
			}
			commands <- fmt.Sprintf("%s %s", strings.ToUpper(strings.Join(fields[4:], " ")), fields[3])
			sizesOnly := strings.Contains(strings.ToUpper(line), "RFC822.SIZE)")
			for i, uid := range requested {
				if sizesOnly {
					if _, err := fmt.Fprintf(serverConn, "* %d FETCH (UID %d RFC822.SIZE %d)\r\n", i+1, uid, sizes[uid]); err != nil {
						serverDone <- err
						return
					}
					continue
				}
				raw := append([]byte(fmt.Sprintf("Subject: UID %d\r\n\r\n", uid)), bytes.Repeat([]byte("x"), sizes[uid])...)
				if _, err := fmt.Fprintf(serverConn,
					"* %d FETCH (UID %d INTERNALDATE \"16-Jul-2026 00:00:00 +0000\" RFC822.SIZE %d FLAGS () BODY[] {%d}\r\n",
					i+1, uid, len(raw), len(raw)); err != nil {
					serverDone <- err
					return
				}
				if _, err := serverConn.Write(raw); err != nil {
					serverDone <- err
					return
				}
				if _, err := io.WriteString(serverConn, ")\r\n"); err != nil {
					serverDone <- err
					return
				}
			}
			if _, err := fmt.Fprintf(serverConn, "%s OK UID FETCH complete\r\n", fields[0]); err != nil {
				serverDone <- err
				return
			}
		}
	}()

	c, err := client.New(clientConn)
	if err != nil {
		t.Fatal(err)
	}
	c.ErrorLog = log.New(io.Discard, "", 0)
	defer c.Terminate()
	c.SetState(imap.SelectedState, &imap.MailboxStatus{Name: "INBOX", Messages: 4, UidNext: 5, UidValidity: 1})

	var fetched []uint32
	fetcher := &Fetcher{BatchSize: 10, BatchBytes: 8 << 20}
	err = fetcher.fetchUIDs(context.Background(), c, "INBOX", []uint32{1, 2, 3, 4}, func(message syncer.FetchedMessage) error {
		fetched = append(fetched, message.UID)
		return nil
	})
	if err != nil {
		t.Fatalf("fetchUIDs() error = %v", err)
	}
	if want := []uint32{1, 2, 3, 4}; !reflect.DeepEqual(fetched, want) {
		t.Fatalf("fetched UIDs = %v, want %v", fetched, want)
	}
	close(commands)
	issued := make([]string, 0, 4)
	for command := range commands {
		issued = append(issued, command)
	}
	want := []string{
		"(UID RFC822.SIZE) 1:4",
		"(UID INTERNALDATE RFC822.SIZE FLAGS BODY.PEEK[]) 1:2",
		"(UID INTERNALDATE RFC822.SIZE FLAGS BODY.PEEK[]) 3:4",
	}
	if !reflect.DeepEqual(issued, want) {
		t.Fatalf("issued commands = %#v, want %#v", issued, want)
	}
	// The scripted server reads until the connection closes, so close it before
	// collecting its result.
	c.Terminate()
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func parseTestUIDSet(value string) ([]uint32, error) {
	uids := make([]uint32, 0, 4)
	for _, part := range strings.Split(value, ",") {
		bounds := strings.SplitN(part, ":", 2)
		first, err := strconv.ParseUint(bounds[0], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("parse UID set %q: %w", value, err)
		}
		last := first
		if len(bounds) == 2 {
			if last, err = strconv.ParseUint(bounds[1], 10, 32); err != nil {
				return nil, fmt.Errorf("parse UID set %q: %w", value, err)
			}
		}
		for uid := first; uid <= last; uid++ {
			uids = append(uids, uint32(uid))
		}
	}
	return uids, nil
}

// RFC 2683 asks for short command lines, and a sparse repair set of high UIDs
// is what makes one long. Ranges are what the server receives, so the window is
// measured as encoded rather than counted in UIDs.
func TestUIDProbeWindowsBoundTheEncodedCommand(t *testing.T) {
	contiguous := make([]uint32, uidSizeProbeWindow)
	for i := range contiguous {
		contiguous[i] = uint32(i + 1)
	}
	if windows := uidProbeWindows(contiguous, 10); len(windows) != 1 || len(windows[0]) != uidSizeProbeWindow {
		t.Fatalf("contiguous windows = %d, want one full window", len(windows))
	}

	sparse := make([]uint32, uidSizeProbeWindow)
	for i := range sparse {
		sparse[i] = uint32(1_000_001 + i*3)
	}
	windows := uidProbeWindows(sparse, 10)
	if len(windows) < 2 {
		t.Fatalf("sparse windows = %d, want the encoded command to split it", len(windows))
	}
	covered := 0
	for _, window := range windows {
		if got := encodedUIDSetBytes(window); got > maxUIDProbeCommandBytes {
			t.Fatalf("window encodes to %d bytes, want at most %d", got, maxUIDProbeCommandBytes)
		}
		for i, uid := range window {
			if uid != sparse[covered+i] {
				t.Fatalf("window UID %d = %d, want %d", covered+i, uid, sparse[covered+i])
			}
		}
		covered += len(window)
	}
	if covered != len(sparse) {
		t.Fatalf("windows covered %d of %d UIDs", covered, len(sparse))
	}
}

// The probe only exists to size batches. A server that refuses it must still
// deliver the folder, with the message-count batching used before probing.
func TestFetchUIDsFallsBackToCountBatchingWhenTheProbeFails(t *testing.T) {
	logs := testlog.Capture(t)
	clientConn, serverConn := net.Pipe()
	serverDone := make(chan error, 1)
	commands := make(chan string, 8)
	go func() {
		defer serverConn.Close()
		if _, err := io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev1] test server ready\r\n"); err != nil {
			serverDone <- err
			return
		}
		reader := bufio.NewReader(serverConn)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				serverDone <- nil
				return
			}
			fields := strings.Fields(line)
			if len(fields) < 4 {
				serverDone <- fmt.Errorf("unexpected command %q", strings.TrimSpace(line))
				return
			}
			commands <- fmt.Sprintf("%s %s", strings.ToUpper(strings.Join(fields[4:], " ")), fields[3])
			if strings.Contains(strings.ToUpper(line), "RFC822.SIZE)") {
				if _, err := fmt.Fprintf(serverConn, "%s NO command not supported\r\n", fields[0]); err != nil {
					serverDone <- err
					return
				}
				continue
			}
			requested, err := parseTestUIDSet(fields[3])
			if err != nil {
				serverDone <- err
				return
			}
			for i, uid := range requested {
				raw := []byte(fmt.Sprintf("Subject: UID %d\r\n\r\nbody", uid))
				if _, err := fmt.Fprintf(serverConn,
					"* %d FETCH (UID %d INTERNALDATE \"16-Jul-2026 00:00:00 +0000\" RFC822.SIZE %d FLAGS () BODY[] {%d}\r\n",
					i+1, uid, len(raw), len(raw)); err != nil {
					serverDone <- err
					return
				}
				if _, err := serverConn.Write(raw); err != nil {
					serverDone <- err
					return
				}
				if _, err := io.WriteString(serverConn, ")\r\n"); err != nil {
					serverDone <- err
					return
				}
			}
			if _, err := fmt.Fprintf(serverConn, "%s OK UID FETCH complete\r\n", fields[0]); err != nil {
				serverDone <- err
				return
			}
		}
	}()

	c, err := client.New(clientConn)
	if err != nil {
		t.Fatal(err)
	}
	c.ErrorLog = log.New(io.Discard, "", 0)
	defer c.Terminate()
	c.SetState(imap.SelectedState, &imap.MailboxStatus{Name: "INBOX", Messages: 3, UidNext: 4, UidValidity: 1})

	var fetched []uint32
	err = (&Fetcher{BatchSize: 2}).fetchUIDs(context.Background(), c, "INBOX", []uint32{1, 2, 3},
		func(message syncer.FetchedMessage) error {
			fetched = append(fetched, message.UID)
			return nil
		})
	if err != nil {
		t.Fatalf("fetchUIDs() error = %v", err)
	}
	if want := []uint32{1, 2, 3}; !reflect.DeepEqual(fetched, want) {
		t.Fatalf("fetched UIDs = %v, want %v", fetched, want)
	}
	close(commands)
	issued := make([]string, 0, 4)
	for command := range commands {
		issued = append(issued, command)
	}
	want := []string{
		"(UID RFC822.SIZE) 1:3",
		"(UID INTERNALDATE RFC822.SIZE FLAGS BODY.PEEK[]) 1:2",
		"(UID INTERNALDATE RFC822.SIZE FLAGS BODY.PEEK[]) 3",
	}
	if !reflect.DeepEqual(issued, want) {
		t.Fatalf("issued commands = %#v, want %#v", issued, want)
	}
	if !strings.Contains(logs.String(), "plans batches by message count") {
		t.Fatalf("the fallback was not reported: %s", logs.String())
	}
	c.Terminate()
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

// File overview: Tests that a failing search index cannot stop mail from arriving.

package syncer

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"rolltop/backend/blob"
	"rolltop/backend/search"
)

func TestStoreFetchedMessageSurvivesFailingSearchIndex(t *testing.T) {
	fixture := newMoveTestFixture(t)
	ctx := context.Background()
	fixture.service.Blobs = blob.New(t.TempDir())

	// A tenant directory that is a regular file: every attempt to create the
	// index below it fails with the same error, and unlike a permission bit that
	// failure does not depend on which user the test runs as.
	root := t.TempDir()
	blocked := filepath.Join(root, strconv.FormatInt(fixture.userID, 10))
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	searchSvc, err := search.OpenPerUser(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = searchSvc.Close() })
	fixture.service.Search = searchSvc

	raw := []byte("From: sender@example.test\r\n" +
		"To: receiver@example.test\r\n" +
		"Subject: arrives without search\r\n" +
		"Date: Tue, 14 Jul 2026 12:00:00 +0000\r\n" +
		"Message-ID: <no-search@example.test>\r\n\r\nbody\r\n")
	item := FetchedMessage{
		Mailbox:      fixture.source.Name,
		UID:          4711,
		UIDValidity:  uint32(fixture.source.UIDValidity),
		InternalDate: time.Date(2026, 7, 14, 12, 1, 0, 0, time.UTC),
		Raw:          raw,
	}

	msg, _, pending, err := fixture.service.storeFetchedMessage(ctx, fixture.userID,
		fixture.account, fixture.source, item, true)
	if err != nil {
		t.Fatalf("store message while the search index is unusable: %v", err)
	}
	batch := newFetchedSearchIndexBatch(fixture.service)
	if err := batch.Add(ctx, pending); err != nil {
		t.Fatalf("queue search document: %v", err)
	}
	if err := batch.Flush(ctx); err != nil {
		t.Fatalf("flush search batch while the index is unusable: %v", err)
	}

	stored, err := fixture.store.GetMessageForUser(ctx, fixture.userID, msg.ID)
	if err != nil {
		t.Fatalf("stored message is missing: %v", err)
	}
	if stored.Subject != "arrives without search" {
		t.Fatalf("stored subject = %q", stored.Subject)
	}
	// The message is stored but not indexed, and nothing retries that on its
	// own: the folder must therefore be left reporting coverage nothing has
	// verified, which is the state an explicit rebuild acts on. Without the
	// mark the folder claims to be searchable and the message is lost to
	// search for good.
	summaries, err := fixture.store.ListMailboxesForUser(ctx, fixture.userID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, summary := range summaries {
		if summary.ID != fixture.source.ID {
			continue
		}
		found = true
		if summary.SearchIndexKnown {
			t.Fatal("dropped search batch left the folder claiming verified coverage")
		}
	}
	if !found {
		t.Fatalf("mailbox %d is missing from the summaries", fixture.source.ID)
	}
}

func TestSearchIndexFailureIsNotSurvivableWhenStopping(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if searchIndexFailureIsSurvivable(cancelled, errors.New("any failure")) {
		t.Fatal("a cancelled sync turn kept going")
	}
	if searchIndexFailureIsSurvivable(context.Background(), context.Canceled) {
		t.Fatal("a cancellation was treated as an index problem")
	}
	if searchIndexFailureIsSurvivable(context.Background(), context.DeadlineExceeded) {
		t.Fatal("a deadline was treated as an index problem")
	}
	if !searchIndexFailureIsSurvivable(context.Background(), errors.New("invalid database")) {
		t.Fatal("an index failure aborted the sync")
	}
}

// Both halves of this failure are rate limited together. A database that is
// down fails the index write and the repair mark, once per folder per batch,
// and an unthrottled second line buries the first at the moment it matters.
func TestDroppedSearchIndexReportingIsRateLimitedPerTenant(t *testing.T) {
	service := &Service{}
	var lines int
	previous := log.Writer()
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		lines++
		return len(p), nil
	}))
	t.Cleanup(func() { log.SetOutput(previous) })

	marks := droppedSearchIndexMarks{failed: 3, cause: errors.New("database is down")}
	for range 50 {
		service.logDroppedSearchIndexBatch(1, 25, 3, marks, errors.New("invalid database"))
	}
	// The first report carries both lines; everything after it is inside the
	// interval and must be silent.
	if lines != 2 {
		t.Fatalf("log lines = %d, want 2 (one drop report and one mark report)", lines)
	}

	// A second tenant reports independently: the throttle is per tenant, so one
	// noisy tenant cannot silence another.
	service.logDroppedSearchIndexBatch(2, 25, 1, droppedSearchIndexMarks{}, errors.New("invalid database"))
	if lines != 3 {
		t.Fatalf("log lines = %d after a second tenant, want 3", lines)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

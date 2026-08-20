package store

import (
	"fmt"
	"testing"
)

// The figure the settings page shows next to two measured directories has to be
// this tenant's own rows and nobody else's, or the page reports one person's
// mailbox as another's storage.
func TestUserMailRowBytesCountsOnlyTheTenantsOwnRows(t *testing.T) {
	f := newDuplicateFixture(t)
	empty, err := f.db.CreateUser(f.ctx, "second@example.test", "Second", "hash", false)
	if err != nil {
		t.Fatal(err)
	}

	before, err := f.db.UserMailRowBytes(f.ctx, f.userID)
	if err != nil {
		t.Fatal(err)
	}
	f.storeMessage(t, f.original, f.originalInbox, 21, "<sized@partner.test>", "info@firma.test")
	after, err := f.db.UserMailRowBytes(f.ctx, f.userID)
	if err != nil {
		t.Fatal(err)
	}
	if after <= before {
		t.Fatalf("mail row bytes after=%d, want more than before=%d", after, before)
	}

	other, err := f.db.UserMailRowBytes(f.ctx, empty.ID)
	if err != nil {
		t.Fatal(err)
	}
	if other != 0 {
		t.Fatalf("a tenant with no mail measured %d bytes, want 0", other)
	}
}

// The per-column sum exists to avoid flattening rows, which reads every TOAST
// chunk of the tenant's message previews. It is only worth doing if it measures
// the same thing, so this compares it against the expensive figure: the two may
// differ by row overhead and alignment, not by a column nobody summed.
func TestUserMailRowBytesMatchesTheWholeRowFigureItAvoids(t *testing.T) {
	f := newDuplicateFixture(t)
	for uid := uint32(41); uid < 51; uid++ {
		f.storeMessage(t, f.original, f.originalInbox, uid, fmt.Sprintf("<toasted-%d@partner.test>", uid), "info@firma.test")
	}

	measured, err := f.db.UserMailRowBytes(f.ctx, f.userID)
	if err != nil {
		t.Fatal(err)
	}
	db, err := f.db.dataDB(f.ctx, f.userID)
	if err != nil {
		t.Fatal(err)
	}
	var wholeRow int64
	if err := db.QueryRowContext(f.ctx, `SELECT
			(SELECT coalesce(sum(pg_column_size(m.*)), 0) FROM messages m WHERE m.user_id = ?)
			+ (SELECT coalesce(sum(pg_column_size(a.*)), 0) FROM attachments a WHERE a.user_id = ?)
			+ (SELECT coalesce(sum(pg_column_size(l.*)), 0) FROM locations l WHERE l.user_id = ?)
			+ (SELECT coalesce(sum(pg_column_size(b.*)), 0) FROM blobs b WHERE b.user_id = ?)`,
		f.userID, f.userID, f.userID, f.userID).Scan(&wholeRow); err != nil {
		t.Fatal(err)
	}
	if wholeRow == 0 {
		t.Fatal("whole-row figure is zero, so this comparison proves nothing")
	}
	difference := measured - wholeRow
	if difference < 0 {
		difference = -difference
	}
	// Generous on purpose: what this rejects is a missing column, which on rows
	// carrying subjects and addresses is a large fraction, not a few bytes of
	// per-row padding.
	if difference*10 > wholeRow {
		t.Fatalf("per-column bytes=%d, whole-row bytes=%d: too far apart to be the same measurement", measured, wholeRow)
	}
}

// The sweep that finds mail missing from the index walks search-visible rows and
// puts them back in the indexing queue. Both halves are tenant-scoped, and the
// walk skips folders the reader took out of search.
func TestSearchVisibleIDWalkAndRequeueStayInsideOneTenantsSearchScope(t *testing.T) {
	f := newDuplicateFixture(t)
	visible := f.storeMessage(t, f.original, f.originalInbox, 31, "<walked@partner.test>", "info@firma.test")
	excluded, err := f.db.GetOrCreateMailbox(f.ctx, f.userID, f.original, "Archive")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateMailboxSettings(f.ctx, f.userID, excluded.ID, MailboxSettings{
		SyncMode: "auto", ShowInSidebar: true, ShowInAllMail: true, IncludeInSearch: false,
	}); err != nil {
		t.Fatal(err)
	}
	hidden := f.storeMessage(t, f.original, excluded.ID, 32, "<not-walked@partner.test>", "info@firma.test")

	ids, err := f.db.ListSearchVisibleMessageIDsAfter(f.ctx, f.userID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := map[int64]bool{}
	for _, id := range ids {
		found[id] = true
	}
	if !found[visible.ID] {
		t.Fatalf("search-visible message %d missing from ids=%v", visible.ID, ids)
	}
	if found[hidden.ID] {
		t.Fatalf("message %d in a folder excluded from search was walked", hidden.ID)
	}

	if err := f.db.MarkMessageAttachmentIndexed(f.ctx, f.userID, visible.ID, false); err != nil {
		t.Fatal(err)
	}
	queued, err := f.db.MarkMessagesAttachmentIndexPending(f.ctx, f.userID, []int64{visible.ID})
	if err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Fatalf("requeued=%d, want 1", queued)
	}
	pending, err := f.db.ListMessagesNeedingAttachmentIndex(f.ctx, f.userID, 10)
	if err != nil {
		t.Fatal(err)
	}
	queuedIDs := map[int64]bool{}
	for _, msg := range pending {
		queuedIDs[msg.ID] = true
	}
	if !queuedIDs[visible.ID] {
		t.Fatalf("message %d is not back in the indexing queue", visible.ID)
	}
}

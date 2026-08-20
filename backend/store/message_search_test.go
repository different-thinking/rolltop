package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// seedSearchMessage creates one stored message the search rows can reference.
func seedSearchMessage(t *testing.T, ctx context.Context, db *Store, user User, account MailAccount, mailbox Mailbox, uid uint32, subject string) MessageRecord {
	t.Helper()
	path := fmt.Sprintf("users/%d/search-tests/uid-%d.eml", user.ID, uid)
	blob, err := db.CreateBlob(ctx, BlobRecord{UserID: user.ID, Kind: "message", Path: path, SHA256: fmt.Sprintf("%064d", uid), Size: 1})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := db.CreateMessage(ctx, CreateMessage{
		UserID: user.ID, AccountID: account.ID, MailboxID: mailbox.ID, BlobID: blob.ID,
		MessageIDHeader: fmt.Sprintf("<search-%d@example.test>", uid),
		CanonicalSHA256: fmt.Sprintf("%064d", uid), MessageIDHash: fmt.Sprintf("hash-%d", uid),
		ThreadKey: fmt.Sprintf("thread-%d", uid), Subject: subject,
		FromAddr: "sender@example.test", Date: time.Now(), InternalDate: time.Now(),
		UID: uid, UIDValidity: mailbox.UIDValidity, Size: 1, BlobPath: path,
	})
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func searchTestFixtures(t *testing.T, ctx context.Context, db *Store) (User, MailAccount, Mailbox) {
	t.Helper()
	user, err := db.CreateUser(ctx, "search-rows@example.test", "Search", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.CreateMailAccount(ctx, MailAccount{UserID: user.ID, Label: "Test", Email: "search-rows@example.test", Host: "imap.example.test", Port: 993, Username: "u", EncryptedPassword: "x"})
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := db.GetOrCreateMailbox(ctx, user.ID, account.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	return user, account, mailbox
}

func TestMessageSearchRowsRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := mustOpenTestStore(t)
	user, account, mailbox := searchTestFixtures(t, ctx, db)
	first := seedSearchMessage(t, ctx, db, user, account, mailbox, 1, "Quartalsbericht Rechnung")
	second := seedSearchMessage(t, ctx, db, user, account, mailbox, 2, "Sitzungsprotokoll")

	docs := []MessageSearchDoc{
		{MessageID: first.ID, UserID: user.ID, TextA: "Quartalsbericht Rechnung", TextB: "sender@example.test", TextC: "die rechnung liegt bei", TextD: "rechnung.pdf"},
		{MessageID: second.ID, UserID: user.ID, TextA: "Sitzungsprotokoll", TextB: "sender@example.test", TextC: "protokoll der sitzung", TextD: ""},
	}
	if err := db.UpsertMessageSearch(ctx, user.ID, docs); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	count, err := db.CountMessageSearchForUser(ctx, user.ID)
	if err != nil || count != 2 {
		t.Fatalf("count = %d, err = %v, want 2", count, err)
	}
	perMailbox, err := db.CountMessageSearchForMailbox(ctx, user.ID, mailbox.ID)
	if err != nil || perMailbox != 2 {
		t.Fatalf("mailbox count = %d, err = %v, want 2", perMailbox, err)
	}

	// The vector is weighted and searchable: subject terms carry weight A.
	var hits int
	if err := db.db.QueryRowContext(ctx, `SELECT count(*) FROM message_search
		WHERE user_id = ? AND tsv @@ to_tsquery('simple', 'rechnung')`, user.ID).Scan(&hits); err != nil {
		t.Fatalf("query tsv: %v", err)
	}
	if hits != 1 {
		t.Fatalf("tsquery hits = %d, want 1", hits)
	}
	var weightedHits int
	if err := db.db.QueryRowContext(ctx, `SELECT count(*) FROM message_search
		WHERE user_id = ? AND tsv @@ to_tsquery('simple', 'quartalsbericht:A')`, user.ID).Scan(&weightedHits); err != nil {
		t.Fatalf("query weighted tsv: %v", err)
	}
	if weightedHits != 1 {
		t.Fatalf("weighted tsquery hits = %d, want 1", weightedHits)
	}

	// Re-upserting replaces the vector rather than duplicating the row.
	docs[0].TextA = "Ersetzter Betreff"
	if err := db.UpsertMessageSearch(ctx, user.ID, docs[:1]); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if err := db.db.QueryRowContext(ctx, `SELECT count(*) FROM message_search
		WHERE user_id = ? AND tsv @@ to_tsquery('simple', 'quartalsbericht')`, user.ID).Scan(&hits); err != nil {
		t.Fatalf("query replaced tsv: %v", err)
	}
	if hits != 0 {
		t.Fatalf("replaced subject still matches, hits = %d", hits)
	}

	present, err := db.MessageSearchPresence(ctx, user.ID, []int64{first.ID, second.ID, 999999})
	if err != nil {
		t.Fatalf("presence: %v", err)
	}
	if !present[first.ID] || !present[second.ID] || present[999999] {
		t.Fatalf("presence = %v", present)
	}
	ids, err := db.MessageSearchMailboxIDs(ctx, user.ID, mailbox.ID)
	if err != nil || len(ids) != 2 {
		t.Fatalf("mailbox ids = %v, err = %v", ids, err)
	}
	bytes, err := db.MessageSearchBytes(ctx, user.ID)
	if err != nil || bytes <= 0 {
		t.Fatalf("bytes = %d, err = %v, want > 0", bytes, err)
	}

	deleted, err := db.DeleteMessageSearch(ctx, user.ID, []int64{first.ID})
	if err != nil || deleted != 1 {
		t.Fatalf("delete = %d, err = %v, want 1", deleted, err)
	}
	purged, err := db.PurgeMessageSearchForMailboxBatch(ctx, user.ID, mailbox.ID, 100)
	if err != nil || purged != 1 {
		t.Fatalf("purge = %d, err = %v, want 1", purged, err)
	}
	// A second call on a cleared mailbox reports nothing left, which is what
	// ends the caller's purge loop.
	if again, err := db.PurgeMessageSearchForMailboxBatch(ctx, user.ID, mailbox.ID, 100); err != nil || again != 0 {
		t.Fatalf("second purge = %d, err = %v, want 0", again, err)
	}
	count, err = db.CountMessageSearchForUser(ctx, user.ID)
	if err != nil || count != 0 {
		t.Fatalf("count after purge = %d, err = %v, want 0", count, err)
	}
}

func TestMessageSearchRowsFollowMessageDeletion(t *testing.T) {
	ctx := context.Background()
	db := mustOpenTestStore(t)
	user, account, mailbox := searchTestFixtures(t, ctx, db)
	msg := seedSearchMessage(t, ctx, db, user, account, mailbox, 7, "Kaskadentest")
	if err := db.UpsertMessageSearch(ctx, user.ID, []MessageSearchDoc{
		{MessageID: msg.ID, UserID: user.ID, TextA: "Kaskadentest"},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := db.DeleteMessageForUser(ctx, user.ID, msg.ID); err != nil {
		t.Fatalf("delete message: %v", err)
	}
	count, err := db.CountMessageSearchForUser(ctx, user.ID)
	if err != nil || count != 0 {
		t.Fatalf("count after message delete = %d, err = %v, want 0 via cascade", count, err)
	}
}

func TestMessageSearchScopesByUser(t *testing.T) {
	ctx := context.Background()
	db := mustOpenTestStore(t)
	user, account, mailbox := searchTestFixtures(t, ctx, db)
	other, err := db.CreateUser(ctx, "search-other@example.test", "Other", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	msg := seedSearchMessage(t, ctx, db, user, account, mailbox, 3, "Mandantengrenze")
	if err := db.UpsertMessageSearch(ctx, user.ID, []MessageSearchDoc{
		{MessageID: msg.ID, UserID: user.ID, TextA: "Mandantengrenze"},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if count, err := db.CountMessageSearchForUser(ctx, other.ID); err != nil || count != 0 {
		t.Fatalf("other tenant count = %d, err = %v, want 0", count, err)
	}
	if deleted, err := db.DeleteMessageSearch(ctx, other.ID, []int64{msg.ID}); err != nil || deleted != 0 {
		t.Fatalf("cross-tenant delete = %d, err = %v, want 0", deleted, err)
	}
	if err := db.DropMessageSearchForUser(ctx, other.ID); err != nil {
		t.Fatalf("drop other: %v", err)
	}
	if count, err := db.CountMessageSearchForUser(ctx, user.ID); err != nil || count != 1 {
		t.Fatalf("count after cross-tenant ops = %d, err = %v, want 1", count, err)
	}
	if mismatch := db.UpsertMessageSearch(ctx, user.ID, []MessageSearchDoc{
		{MessageID: msg.ID, UserID: other.ID, TextA: "falscher Mandant"},
	}); mismatch == nil {
		t.Fatal("cross-tenant doc in a batch was accepted")
	}
}

// TestCountMessageSearchEnabledCountsOnlySearchableFolders pins what the
// backfill comparison needs: rows for messages whose folder has left search
// must not count as coverage, or a real shortfall hides behind them.
func TestCountMessageSearchEnabledCountsOnlySearchableFolders(t *testing.T) {
	ctx := context.Background()
	db := mustOpenTestStore(t)
	user, account, mailbox := searchTestFixtures(t, ctx, db)
	hidden, err := db.GetOrCreateMailbox(ctx, user.ID, account.ID, "Archiv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE mailboxes SET include_in_search = 0 WHERE id = $1`, hidden.ID); err != nil {
		t.Fatal(err)
	}

	// Two searchable messages, one of them indexed, plus a row left behind by
	// the folder that has since left search. That stale row is exactly what
	// made the unfiltered count look like full coverage.
	indexed := seedSearchMessage(t, ctx, db, user, account, mailbox, 40, "Sichtbar")
	seedSearchMessage(t, ctx, db, user, account, mailbox, 42, "Noch nicht indiziert")
	stale := seedSearchMessage(t, ctx, db, user, account, hidden, 41, "Ausgeblendet")
	if err := db.UpsertMessageSearch(ctx, user.ID, []MessageSearchDoc{
		{MessageID: indexed.ID, UserID: user.ID, TextA: "Sichtbar"},
		{MessageID: stale.ID, UserID: user.ID, TextA: "Ausgeblendet"},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	searchable, err := db.CountSearchEnabledMessagesForUser(ctx, user.ID)
	if err != nil || searchable != 2 {
		t.Fatalf("searchable = %d, err = %v, want 2", searchable, err)
	}
	all, err := db.CountMessageSearchForUser(ctx, user.ID)
	if err != nil || all != 2 {
		t.Fatalf("unfiltered count = %d, err = %v, want 2", all, err)
	}
	if all < searchable {
		t.Fatal("the unfiltered count no longer hides the shortfall; this test has lost its point")
	}
	covered, err := db.CountMessageSearchEnabledForUser(ctx, user.ID)
	if err != nil || covered != 1 {
		t.Fatalf("search-enabled count = %d, err = %v, want 1", covered, err)
	}
	if covered >= searchable {
		t.Fatalf("covered %d vs searchable %d: the shortfall must stay visible", covered, searchable)
	}
}

// TestUpsertMessageSearchChunksLargeBatches covers the parameter ceiling: seven
// bind parameters per row put PostgreSQL's 65,535 limit at 9,363 documents, and
// this method builds the statement, so it carries the bound itself.
func TestUpsertMessageSearchChunksLargeBatches(t *testing.T) {
	ctx := context.Background()
	db := mustOpenTestStore(t)
	user, account, mailbox := searchTestFixtures(t, ctx, db)

	const count = messageSearchUpsertChunk*2 + 7
	docs := make([]MessageSearchDoc, 0, count)
	for i := 0; i < count; i++ {
		msg := seedSearchMessage(t, ctx, db, user, account, mailbox, uint32(500+i), "Stapel")
		docs = append(docs, MessageSearchDoc{MessageID: msg.ID, UserID: user.ID, TextA: "Stapel", Words: "stapel"})
	}
	if err := db.UpsertMessageSearch(ctx, user.ID, docs); err != nil {
		t.Fatalf("upsert of %d documents: %v", count, err)
	}
	got, err := db.CountMessageSearchForUser(ctx, user.ID)
	if err != nil || got != count {
		t.Fatalf("count = %d, err = %v, want %d", got, err, count)
	}
}

func TestSearchMessageIDsRefusesUnboundedTermLists(t *testing.T) {
	ctx := context.Background()
	db := mustOpenTestStore(t)
	user, _, _ := searchTestFixtures(t, ctx, db)

	terms := make([]MessageSearchTextTerm, maxMessageSearchTextTerms+1)
	for i := range terms {
		terms[i] = MessageSearchTextTerm{TSQuery: "'wort'"}
	}
	if _, err := db.SearchMessageIDs(ctx, MessageSearchQuery{UserID: user.ID, TextTerms: terms}); err == nil {
		t.Fatal("an unbounded term list was accepted")
	}
	negations := make([]string, maxMessageSearchTextTerms+1)
	for i := range negations {
		negations[i] = "'wort'"
	}
	if _, err := db.SearchMessageIDs(ctx, MessageSearchQuery{UserID: user.ID, TSQuery: "'x'", NotTSQueries: negations}); err == nil {
		t.Fatal("an unbounded negation list was accepted")
	}
}

// The gate in front of the expensive query has to answer the same population
// question the query would, stop at its ceiling, and respect the filters - a
// count that ignored them would wave through a query that finds nothing.
func TestCountMessageSearchMatchesRespectsItsCeilingAndFilters(t *testing.T) {
	ctx := context.Background()
	db := mustOpenTestStore(t)
	user, account, mailbox := searchTestFixtures(t, ctx, db)
	var docs []MessageSearchDoc
	for uid := uint32(1); uid <= 6; uid++ {
		msg := seedSearchMessage(t, ctx, db, user, account, mailbox, uid, "Rechnung")
		docs = append(docs, MessageSearchDoc{
			MessageID: msg.ID, UserID: user.ID, TextA: "Rechnung",
			TextB: "sender@example.test", TextC: "anbei die rechnung",
		})
	}
	if err := db.UpsertMessageSearch(ctx, user.ID, docs); err != nil {
		t.Fatal(err)
	}
	query := MessageSearchQuery{UserID: user.ID, TSQuery: "'rechnung'"}

	full, err := db.CountMessageSearchMatches(ctx, query, 100)
	if err != nil || full != 6 {
		t.Fatalf("count = %d, err = %v, want 6", full, err)
	}
	capped, err := db.CountMessageSearchMatches(ctx, query, 4)
	if err != nil || capped != 4 {
		t.Fatalf("capped count = %d, err = %v, want the ceiling 4", capped, err)
	}

	filtered := query
	filtered.FromPattern = "%nobody%"
	none, err := db.CountMessageSearchMatches(ctx, filtered, 100)
	if err != nil || none != 0 {
		t.Fatalf("filtered count = %d, err = %v, want 0", none, err)
	}
	if _, err := db.CountMessageSearchMatches(ctx, query, 0); err == nil {
		t.Fatal("a ceiling of zero was accepted, so an unbounded count can be asked for by accident")
	}

	// The count decides whether a reader's search runs with typo tolerance, so
	// a neighbour's mail landing in it would let one tenant's mailbox change
	// what another tenant's search finds.
	other, err := db.CreateUser(ctx, "search-rows-neighbour@example.test", "Neighbour", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	otherAccount, err := db.CreateMailAccount(ctx, MailAccount{UserID: other.ID, Label: "Test", Email: "search-rows-neighbour@example.test", Host: "imap.example.test", Port: 993, Username: "u", EncryptedPassword: "x"})
	if err != nil {
		t.Fatal(err)
	}
	otherInbox, err := db.GetOrCreateMailbox(ctx, other.ID, otherAccount.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	var otherDocs []MessageSearchDoc
	for uid := uint32(100); uid <= 109; uid++ {
		msg := seedSearchMessage(t, ctx, db, other, otherAccount, otherInbox, uid, "Rechnung")
		otherDocs = append(otherDocs, MessageSearchDoc{
			MessageID: msg.ID, UserID: other.ID, TextA: "Rechnung",
			TextB: "sender@example.test", TextC: "anbei die rechnung",
		})
	}
	if err := db.UpsertMessageSearch(ctx, other.ID, otherDocs); err != nil {
		t.Fatal(err)
	}
	stillSix, err := db.CountMessageSearchMatches(ctx, query, 100)
	if err != nil || stillSix != 6 {
		t.Fatalf("count = %d, err = %v, want the tenant's own 6 with a neighbour holding 10 more", stillSix, err)
	}
	theirs, err := db.CountMessageSearchMatches(ctx, MessageSearchQuery{UserID: other.ID, TSQuery: "'rechnung'"}, 100)
	if err != nil || theirs != 10 {
		t.Fatalf("neighbour count = %d, err = %v, want 10", theirs, err)
	}
}

package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"rolltop/backend/mailparse"
)

// categoryFixture is one tenant with an Inbox, an Archive folder wired up the
// way the Archive action resolves it, and a helper that files messages.
type categoryFixture struct {
	db      *Store
	user    User
	account MailAccount
	inbox   Mailbox
	archive Mailbox
	blob    BlobRecord
	base    time.Time
}

func newCategoryFixture(t *testing.T) categoryFixture {
	t.Helper()
	ctx := context.Background()
	db, err := openTestStore(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	user, account, inbox, blob := testMailbox(t, ctx, db)
	archive, err := db.GetOrCreateMailbox(ctx, user.ID, account.ID, "Archive")
	if err != nil {
		t.Fatal(err)
	}
	prefs := DefaultSwipePreferences(user.ID)
	prefs.ArchiveMailboxes = []SwipeArchiveMailbox{{AccountID: account.ID, MailboxID: archive.ID}}
	if _, err := db.SaveSwipePreferences(ctx, prefs); err != nil {
		t.Fatal(err)
	}
	return categoryFixture{db: db, user: user, account: account, inbox: inbox, archive: archive, blob: blob,
		base: time.Unix(1700000000, 0)}
}

func (f categoryFixture) create(t *testing.T, mailbox Mailbox, uid uint32, from, category string) MessageRecord {
	t.Helper()
	message, err := f.db.CreateMessage(context.Background(), CreateMessage{
		UserID: f.user.ID, AccountID: f.account.ID, MailboxID: mailbox.ID, BlobID: f.blob.ID,
		MessageIDHeader: fmt.Sprintf("<category-%d@example.test>", uid),
		Subject:         fmt.Sprintf("Message %d", uid),
		FromAddr:        from, Category: category,
		Date: f.base.Add(time.Duration(uid) * time.Minute), UID: uid, BlobPath: f.blob.Path,
	})
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func TestCategoryListsShowOneCategoryAndSkipArchivedMail(t *testing.T) {
	ctx := context.Background()
	f := newCategoryFixture(t)
	relevant := f.create(t, f.inbox, 1, "Ada <ada@example.test>", mailparse.CategoryRelevant)
	newsletter := f.create(t, f.inbox, 2, "news@example.test", mailparse.CategoryNewsletters)
	// An archived newsletter is deliberately out of the Newsletters list: a
	// category answers what kind of mail this is, over the mail still in play.
	archivedNewsletter := f.create(t, f.archive, 3, "news@example.test", mailparse.CategoryNewsletters)

	messages, err := f.db.ListCategoryLatestThreadMessagesForUser(ctx, f.user.ID, mailparse.CategoryNewsletters, 10, 0, ThreadListNewestFirst)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].ID != newsletter.ID {
		t.Fatalf("newsletters = %v, want only %d (archived %d and relevant %d must stay out)",
			messageIDsOf(messages), newsletter.ID, archivedNewsletter.ID, relevant.ID)
	}
	scope, err := f.db.ListCategoryMailScopeMessagesForUser(ctx, f.user.ID, mailparse.CategoryNewsletters, ScopeFilter{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(scope) != 1 || scope[0].ID != newsletter.ID {
		t.Fatalf("newsletters scope = %+v, want only %d", scope, newsletter.ID)
	}
	if _, err := f.db.ListCategoryLatestThreadMessagesForUser(ctx, f.user.ID, "everything", 10, 0, ThreadListNewestFirst); err == nil {
		t.Fatal("an unknown category name must be rejected rather than listing nothing")
	}
}

func TestSenderCorrectionMovesStoredMailAndHoldsForNewMail(t *testing.T) {
	ctx := context.Background()
	f := newCategoryFixture(t)
	first := f.create(t, f.inbox, 1, `"Shop" <offers@example.test>`, mailparse.CategoryNewsletters)
	other := f.create(t, f.inbox, 2, "ada@example.test", mailparse.CategoryRelevant)

	// The display name differs between messages from the same sender, so the
	// correction has to key on the address alone.
	moved, err := f.db.SetSenderCategoryOverride(ctx, f.user.ID, "Shop Deals <OFFERS@Example.test>", mailparse.CategoryNotifications)
	if err != nil {
		t.Fatal(err)
	}
	if moved != 1 {
		t.Fatalf("moved = %d, want 1", moved)
	}
	notifications, err := f.db.ListCategoryLatestThreadMessagesForUser(ctx, f.user.ID, mailparse.CategoryNotifications, 10, 0, ThreadListNewestFirst)
	if err != nil {
		t.Fatal(err)
	}
	if len(notifications) != 1 || notifications[0].ID != first.ID {
		t.Fatalf("notifications = %v, want only %d", messageIDsOf(notifications), first.ID)
	}

	// New mail from a corrected sender must not be re-decided by its headers,
	// or the correction would be undone by the next delivery.
	second := f.create(t, f.inbox, 3, "offers@example.test", mailparse.CategoryNewsletters)
	stored, err := f.db.GetMessageForUser(ctx, f.user.ID, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Category != mailparse.CategoryNotifications {
		t.Fatalf("newly stored message category = %q, want %q", stored.Category, mailparse.CategoryNotifications)
	}
	if pinned, err := f.db.SenderCategoryOverride(ctx, f.user.ID, "offers@example.test"); err != nil || pinned != mailparse.CategoryNotifications {
		t.Fatalf("stored override = %q err=%v", pinned, err)
	}
	if pinned, err := f.db.SenderCategoryOverride(ctx, f.user.ID, "ada@example.test"); err != nil || pinned != "" {
		t.Fatalf("uncorrected sender override = %q err=%v, want empty", pinned, err)
	}

	// Clearing hands the sender back to the classifier without rewriting the
	// mail that is already filed.
	if err := f.db.ClearSenderCategoryOverride(ctx, f.user.ID, "offers@example.test"); err != nil {
		t.Fatal(err)
	}
	third := f.create(t, f.inbox, 4, "offers@example.test", mailparse.CategoryNewsletters)
	stored, err = f.db.GetMessageForUser(ctx, f.user.ID, third.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Category != mailparse.CategoryNewsletters {
		t.Fatalf("category after clearing the override = %q, want %q", stored.Category, mailparse.CategoryNewsletters)
	}
	if _, err := f.db.SetSenderCategoryOverride(ctx, f.user.ID, "offers@example.test", "everything"); err == nil {
		t.Fatalf("an unknown category must be rejected; message %d proves the fixture works", other.ID)
	}
}

func TestClassificationQueueDrainsAndHonoursCorrections(t *testing.T) {
	ctx := context.Background()
	f := newCategoryFixture(t)
	pending := f.create(t, f.inbox, 1, "news@example.test", "")
	classified := f.create(t, f.inbox, 2, "ada@example.test", mailparse.CategoryRelevant)

	candidates, err := f.db.ListMessagesNeedingCategory(ctx, f.user.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].ID != pending.ID {
		t.Fatalf("candidates = %+v, want only %d (%d is already classified)", candidates, pending.ID, classified.ID)
	}
	if count, err := f.db.CountMessagesNeedingCategory(ctx, f.user.ID); err != nil || count != 1 {
		t.Fatalf("pending count = %d err=%v, want 1", count, err)
	}

	// A correction made while the message was still queued must win over what
	// the classifier decides when it finally reads the message.
	if _, err := f.db.SetSenderCategoryOverride(ctx, f.user.ID, "news@example.test", mailparse.CategoryForums); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetMessageCategories(ctx, f.user.ID, []MessageCategoryUpdate{
		{MessageID: pending.ID, FromAddr: "News <news@example.test>", Category: mailparse.CategoryNewsletters},
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := f.db.GetMessageForUser(ctx, f.user.ID, pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Category != mailparse.CategoryForums {
		t.Fatalf("classified category = %q, want the corrected %q", stored.Category, mailparse.CategoryForums)
	}
	if count, err := f.db.CountMessagesNeedingCategory(ctx, f.user.ID); err != nil || count != 0 {
		t.Fatalf("pending count after classification = %d err=%v, want 0", count, err)
	}
}

func TestCategoryDataStaysInsideOneTenant(t *testing.T) {
	ctx := context.Background()
	f := newCategoryFixture(t)
	f.create(t, f.inbox, 1, "news@example.test", mailparse.CategoryNewsletters)
	other, err := f.db.CreateUser(ctx, "other@example.test", "Other", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SetSenderCategoryOverride(ctx, f.user.ID, "news@example.test", mailparse.CategoryForums); err != nil {
		t.Fatal(err)
	}
	pinned, err := f.db.SenderCategoryOverride(ctx, other.ID, "news@example.test")
	if err != nil || pinned != "" {
		t.Fatalf("other tenant override = %q err=%v, want empty", pinned, err)
	}
	messages, err := f.db.ListCategoryLatestThreadMessagesForUser(ctx, other.ID, mailparse.CategoryForums, 10, 0, ThreadListNewestFirst)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 0 {
		t.Fatalf("other tenant sees %v", messageIDsOf(messages))
	}
}

func TestNormalizeCategorySenderMatchesTheClassifiersOwnReading(t *testing.T) {
	// The correction key and the classifier's address fallback must agree, or a
	// sender the classifier files is a sender the user cannot correct. Sharing
	// one reader is how that is guaranteed; this checks the two have not drifted
	// apart again behind separate implementations.
	for _, from := range []string{
		`"Shop" <Offers@Example.test>`,
		"ada@example.test",
		"  ADA@example.test  ",
		"Two <a@x.test>, <b@x.test>",
		"Jane Doe <jane@x.test",
		"",
		"not-an-address",
	} {
		if got, want := NormalizeCategorySender(from), mailparse.BareAddress(from); got != want {
			t.Fatalf("NormalizeCategorySender(%q) = %q, want the classifier's %q", from, got, want)
		}
	}
}

func TestPendingCountDescribesOnlyMailTheCategoryListsCanShow(t *testing.T) {
	ctx := context.Background()
	f := newCategoryFixture(t)
	trash, err := f.db.GetOrCreateMailboxWithRole(ctx, f.user.ID, f.account.ID, "Trash", "trash")
	if err != nil {
		t.Fatal(err)
	}
	shown := f.create(t, f.inbox, 1, "news@example.test", "")
	f.create(t, f.archive, 2, "news@example.test", "")
	f.create(t, trash, 3, "news@example.test", "")

	// The worker still classifies every message it finds, but a count that
	// included folders no category list draws from would leave the browser
	// reporting work the user can never see finish.
	count, err := f.db.CountMessagesNeedingCategory(ctx, f.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("pending count = %d, want only the inbox message %d", count, shown.ID)
	}
	candidates, err := f.db.ListMessagesNeedingCategory(ctx, f.user.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 3 {
		t.Fatalf("backfill queue = %d rows, want all three regardless of folder", len(candidates))
	}
}

func TestReportedSpamLeavesEveryWholeAccountList(t *testing.T) {
	ctx := context.Background()
	f := newCategoryFixture(t)
	junk, err := f.db.GetOrCreateMailboxWithRole(ctx, f.user.ID, f.account.ID, "Junk", "junk")
	if err != nil {
		t.Fatal(err)
	}
	// A Junk folder that opts into All Mail is the case this guards: the folder
	// setting must not be able to put reported spam back in front of the user,
	// because Report spam promises the message is gone from these lists.
	if err := f.db.UpdateMailboxSettings(ctx, f.user.ID, junk.ID, MailboxSettings{
		SyncMode: "full", Role: "junk", ShowInSidebar: true, ShowInAllMail: true, IncludeInSearch: true,
	}); err != nil {
		t.Fatal(err)
	}
	kept := f.create(t, f.inbox, 1, "ada@example.test", mailparse.CategoryRelevant)
	spam := f.create(t, junk, 2, "spammer@example.test", mailparse.CategoryNewsletters)

	inbox, err := f.db.ListUnarchivedLatestThreadMessagesForUser(ctx, f.user.ID, 10, 0, ThreadListNewestFirst)
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 1 || inbox[0].ID != kept.ID {
		t.Fatalf("inbox = %v, want only %d (spam %d must stay out)", messageIDsOf(inbox), kept.ID, spam.ID)
	}
	all, err := f.db.ListLatestThreadMessagesForUser(ctx, f.user.ID, 10, 0, ThreadListNewestFirst)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].ID != kept.ID {
		t.Fatalf("all mail = %v, want only %d", messageIDsOf(all), kept.ID)
	}
	newsletters, err := f.db.ListCategoryLatestThreadMessagesForUser(ctx, f.user.ID, mailparse.CategoryNewsletters, 10, 0, ThreadListNewestFirst)
	if err != nil {
		t.Fatal(err)
	}
	if len(newsletters) != 0 {
		t.Fatalf("newsletters = %v, want nothing: %d is spam now", messageIDsOf(newsletters), spam.ID)
	}
	counts, err := f.db.CountMessagesByCategoryForUser(ctx, f.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if counts[mailparse.CategoryNewsletters].Total != 0 {
		t.Fatalf("newsletters badge = %+v, want nothing counted", counts[mailparse.CategoryNewsletters])
	}
	// A whole-view selection has to cover exactly what the list showed, or
	// acting on "everything here" would reach the spam the list hid.
	scope, err := f.db.ListUnarchivedMailScopeMessagesForUser(ctx, f.user.ID, ScopeFilter{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(scope) != 1 || scope[0].ID != kept.ID {
		t.Fatalf("inbox scope = %+v, want only %d", scope, kept.ID)
	}
	// The folder itself still lists its own mail: hiding spam from the combined
	// views is not the same as making it unreachable.
	folder, err := f.db.ListLatestThreadMessagesForMailbox(ctx, f.user.ID, junk.ID, 10, 0, ThreadListNewestFirst)
	if err != nil {
		t.Fatal(err)
	}
	if len(folder) != 1 || folder[0].ID != spam.ID {
		t.Fatalf("junk folder = %v, want %d", messageIDsOf(folder), spam.ID)
	}
}

func TestFilingSeveralSendersLandsWholeOrNotAtAll(t *testing.T) {
	ctx := context.Background()
	f := newCategoryFixture(t)
	f.create(t, f.inbox, 1, `"Shop" <offers@example.test>`, mailparse.CategoryNewsletters)
	f.create(t, f.inbox, 2, "offers@example.test", mailparse.CategoryNewsletters)
	f.create(t, f.inbox, 3, "list@example.test", mailparse.CategoryNewsletters)

	// A sender with no address to key on, or a category that does not exist,
	// fails the call before anything is written. Filing the senders that did
	// parse would leave the user with a correction that covered part of what
	// they dropped and no way to tell which part.
	for _, broken := range []struct {
		senders  []string
		category string
	}{
		{senders: []string{"offers@example.test", "   "}, category: mailparse.CategoryForums},
		{senders: []string{"offers@example.test", "list@example.test"}, category: "everything"},
	} {
		if _, err := f.db.SetSenderCategoryOverrides(ctx, f.user.ID, broken.senders, broken.category); err == nil {
			t.Fatalf("SetSenderCategoryOverrides(%v, %q) succeeded, want a rejection", broken.senders, broken.category)
		}
		for _, sender := range []string{"offers@example.test", "list@example.test"} {
			if pinned, err := f.db.SenderCategoryOverride(ctx, f.user.ID, sender); err != nil || pinned != "" {
				t.Fatalf("override for %q after a rejected call = %q err=%v, want none", sender, pinned, err)
			}
		}
	}

	// The same address named twice is one sender, and the count reports the
	// messages that actually changed category rather than the rows examined.
	moved, err := f.db.SetSenderCategoryOverrides(ctx, f.user.ID,
		[]string{"Shop <offers@example.test>", "offers@example.test", "list@example.test"}, mailparse.CategoryForums)
	if err != nil {
		t.Fatal(err)
	}
	if moved != 3 {
		t.Fatalf("moved = %d, want all 3 messages", moved)
	}
	for _, sender := range []string{"offers@example.test", "list@example.test"} {
		if pinned, err := f.db.SenderCategoryOverride(ctx, f.user.ID, sender); err != nil || pinned != mailparse.CategoryForums {
			t.Fatalf("override for %q = %q err=%v", sender, pinned, err)
		}
	}
	forums, err := f.db.ListCategoryLatestThreadMessagesForUser(ctx, f.user.ID, mailparse.CategoryForums, 10, 0, ThreadListNewestFirst)
	if err != nil {
		t.Fatal(err)
	}
	if len(forums) != 3 {
		t.Fatalf("forums = %v, want all three messages", messageIDsOf(forums))
	}
}

// The pending counter beside the category lists counts only mail those lists
// can show, so a queue ordered purely by id lets the counter sit still while
// the worker spends its batches on a Trash folder nobody is looking at. What
// the user is waiting for goes first; the rest is queued behind it, not
// dropped.
func TestBackfillQueueFilesVisibleMailBeforeTheBacklog(t *testing.T) {
	ctx := context.Background()
	f := newCategoryFixture(t)
	trash, err := f.db.GetOrCreateMailboxWithRole(ctx, f.user.ID, f.account.ID, "Trash", "trash")
	if err != nil {
		t.Fatal(err)
	}
	// Every backlog row is older than the visible one, so id order alone would
	// put the message the counter describes last.
	for uid := uint32(1); uid <= 5; uid++ {
		f.create(t, trash, uid, "news@example.test", "")
	}
	f.create(t, f.archive, 6, "news@example.test", "")
	shown := f.create(t, f.inbox, 7, "news@example.test", "")

	candidates, err := f.db.ListMessagesNeedingCategory(ctx, f.user.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 7 {
		t.Fatalf("backfill queue = %d rows, want all seven regardless of folder", len(candidates))
	}
	if candidates[0].ID != shown.ID {
		t.Fatalf("first candidate = %d, want the inbox message %d that the pending count describes",
			candidates[0].ID, shown.ID)
	}

	// A batch smaller than the queue must still reach it, which is the case
	// that leaves the counter stuck when the order is wrong.
	batch, err := f.db.ListMessagesNeedingCategory(ctx, f.user.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 1 || batch[0].ID != shown.ID {
		t.Fatalf("first batch = %v, want only the inbox message %d", candidateIDsOf(batch), shown.ID)
	}
}

func candidateIDsOf(candidates []CategoryCandidate) []int64 {
	out := make([]int64, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, candidate.ID)
	}
	return out
}

// ageCategoryGeneration puts a message back into the state a build shipped
// before the current classifier rules left it in: filed, and stamped by an
// older generation.
func (f categoryFixture) ageCategoryGeneration(t *testing.T, messageID int64) {
	t.Helper()
	ctx := context.Background()
	db, err := f.db.dataDB(ctx, f.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE messages SET category_version = ? WHERE user_id = ? AND id = ?`,
		CategoryVersion-1, f.user.ID, messageID); err != nil {
		t.Fatal(err)
	}
}

// pruneBlobPath is what blob retention leaves behind: the row, without the raw
// message it was classified from.
func (f categoryFixture) pruneBlobPath(t *testing.T, messageID int64) {
	t.Helper()
	ctx := context.Background()
	db, err := f.db.dataDB(ctx, f.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE messages SET blob_path = '' WHERE user_id = ? AND id = ?`,
		f.user.ID, messageID); err != nil {
		t.Fatal(err)
	}
}

func TestARuleChangeReachesMailThatIsAlreadyFiled(t *testing.T) {
	ctx := context.Background()
	f := newCategoryFixture(t)
	current := f.create(t, f.inbox, 1, "news@example.test", mailparse.CategoryNewsletters)
	stale := f.create(t, f.inbox, 2, "billing@shop.test", mailparse.CategoryNotifications)
	pending := f.create(t, f.inbox, 3, "later@example.test", "")
	// Blob retention has taken this one's raw message, so there is nothing left
	// to re-read: it must not be selected and guessed at from its address.
	pruned := f.create(t, f.inbox, 4, "news@example.test", mailparse.CategoryNewsletters)
	f.ageCategoryGeneration(t, stale.ID)
	f.ageCategoryGeneration(t, pruned.ID)
	f.pruneBlobPath(t, pruned.ID)

	// Unclassified mail is taken first: it is the only kind that is missing
	// from every list while it waits.
	first, err := f.db.ListMessagesNeedingCategory(ctx, f.user.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].ID != pending.ID {
		t.Fatalf("first batch = %+v, want only %d", first, pending.ID)
	}
	candidates, err := f.db.ListMessagesNeedingCategory(ctx, f.user.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 || candidates[0].ID != pending.ID || candidates[1].ID != stale.ID {
		t.Fatalf("candidates = %+v, want %d then %d (%d was filed by this generation, %d has no stored message left)",
			candidates, pending.ID, stale.ID, current.ID, pruned.ID)
	}
	// The candidate carries what it already holds, which is what lets the
	// worker tell "decide this" from "improve this if you can".
	if candidates[0].Category != "" || candidates[1].Category != mailparse.CategoryNotifications {
		t.Fatalf("candidate categories = %q and %q, want empty then %q",
			candidates[0].Category, candidates[1].Category, mailparse.CategoryNotifications)
	}
	// A re-pass is not a mailbox that came undone: the rows it will re-read are
	// in a list and readable, so they are not reported as waiting.
	if count, err := f.db.CountMessagesNeedingCategory(ctx, f.user.ID); err != nil || count != 1 {
		t.Fatalf("pending count = %d err=%v, want 1", count, err)
	}
	// The message keeps the category it has until the new answer replaces it,
	// so no list is empty while the pass runs.
	filed, err := f.db.GetMessageForUser(ctx, f.user.ID, stale.ID)
	if err != nil {
		t.Fatal(err)
	}
	if filed.Category != mailparse.CategoryNotifications {
		t.Fatalf("category while the re-pass is queued = %q, want %q", filed.Category, mailparse.CategoryNotifications)
	}

	if err := f.db.SetMessageCategories(ctx, f.user.ID, []MessageCategoryUpdate{
		{MessageID: pending.ID, FromAddr: "later@example.test", Category: mailparse.CategoryRelevant},
		{MessageID: stale.ID, FromAddr: "billing@shop.test", Category: mailparse.CategoryInvoices},
	}); err != nil {
		t.Fatal(err)
	}
	refiled, err := f.db.GetMessageForUser(ctx, f.user.ID, stale.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refiled.Category != mailparse.CategoryInvoices {
		t.Fatalf("re-classified category = %q, want %q", refiled.Category, mailparse.CategoryInvoices)
	}
	// And the row is stamped, so the same pass cannot pick it up forever.
	remaining, err := f.db.ListMessagesNeedingCategory(ctx, f.user.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("candidates after the pass = %+v, want none", remaining)
	}
	// The row whose raw message is gone kept the category its headers earned
	// it, rather than being re-decided from its address.
	kept, err := f.db.GetMessageForUser(ctx, f.user.ID, pruned.ID)
	if err != nil {
		t.Fatal(err)
	}
	if kept.Category != mailparse.CategoryNewsletters {
		t.Fatalf("category of mail with no stored message = %q, want %q", kept.Category, mailparse.CategoryNewsletters)
	}
}

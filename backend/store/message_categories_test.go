package store

import (
	"context"
	"fmt"
	"path/filepath"
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
	db, err := Open(filepath.Join(t.TempDir(), "rolltop.db"))
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

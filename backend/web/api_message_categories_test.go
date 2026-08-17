package web

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"rolltop/backend/mailparse"
	"rolltop/backend/store"
)

func createCategoryTestMessage(t *testing.T, ctx context.Context, db *store.Store, tenant scopeTestTenant,
	mailbox store.Mailbox, uid uint32, from, category string,
) store.MessageRecord {
	t.Helper()
	blob, err := db.CreateBlob(ctx, store.BlobRecord{
		UserID: tenant.user.ID, Kind: "message",
		Path:   fmt.Sprintf("users/%d/category-%d.eml", tenant.user.ID, uid),
		SHA256: fmt.Sprintf("category-%d-%d", tenant.user.ID, uid), Size: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	message, err := db.CreateMessage(ctx, store.CreateMessage{
		UserID: tenant.user.ID, AccountID: tenant.accountID, MailboxID: mailbox.ID, BlobID: blob.ID,
		FromAddr: from, Subject: fmt.Sprintf("Category %d", uid), Category: category,
		Date: time.Now().UTC(), InternalDate: time.Now().UTC(), UID: uid, BlobPath: blob.Path,
	})
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func TestMailViewNamesResolveOnlyToListsThisServerRenders(t *testing.T) {
	known := []string{"", "unarchived", "sent", "drafts", "relevant", "newsletters", "forums", "notifications"}
	for _, name := range known {
		view, ok := parseMailView(name)
		if !ok || string(view) != name {
			t.Fatalf("parseMailView(%q) = %q ok=%t, want the same name", name, view, ok)
		}
	}
	for _, name := range []string{"everything", "category:newsletters", "junk", "Relevant "} {
		if view, ok := parseMailView(name); ok && string(view) != "relevant" {
			t.Fatalf("parseMailView(%q) = %q ok=%t, want a rejection", name, view, ok)
		}
	}
	// The trimmed, lowercased form of a real name is still that name, which is
	// what lets a URL carry it without the handler doing its own normalizing.
	if view, ok := parseMailView(" Newsletters "); !ok || view != "newsletters" {
		t.Fatalf("parseMailView(\" Newsletters \") = %q ok=%t", view, ok)
	}
}

func TestCategoryScopeCoversOnlyItsOwnUnarchivedMail(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tenant := newScopeTestTenant(t, ctx, db, "scope-categories@example.test")
	archive, err := db.GetOrCreateMailbox(ctx, tenant.user.ID, tenant.accountID, "Archive")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveSwipePreferences(ctx, store.SwipePreferences{
		UserID:     tenant.user.ID,
		LeftAction: store.SwipeActionSnooze, LeftSnoozePreset: store.SwipeSnoozeTomorrow,
		RightAction: store.SwipeActionMarkRead, RightSnoozePreset: store.SwipeSnoozeTomorrow,
		ArchiveMailboxes: []store.SwipeArchiveMailbox{{AccountID: tenant.accountID, MailboxID: archive.ID}},
	}); err != nil {
		t.Fatal(err)
	}
	newsletter := createCategoryTestMessage(t, ctx, db, tenant, tenant.inbox, 701, "news@example.test", mailparse.CategoryNewsletters)
	personal := createCategoryTestMessage(t, ctx, db, tenant, tenant.inbox, 702, "ada@example.test", mailparse.CategoryRelevant)
	archived := createCategoryTestMessage(t, ctx, db, tenant, archive, 703, "news@example.test", mailparse.CategoryNewsletters)
	server := &Server{store: db, masterKey: []byte("12345678901234567890123456789012")}

	plan, err := server.scopeTrashPlan(ctx, tenant.user, scopeSelection{View: mailView(mailparse.CategoryNewsletters)})
	if err != nil {
		t.Fatal(err)
	}
	ids := planMessageIDs(plan)
	if plan.Matched != 1 || len(ids) != 1 || ids[0] != newsletter.ID {
		t.Fatalf("newsletters plan = matched %d ids %v, want only %d (not %d or %d)",
			plan.Matched, ids, newsletter.ID, personal.ID, archived.ID)
	}
}

func TestCategoryChromeCountsWhatTheListShows(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tenant := newScopeTestTenant(t, ctx, db, "chrome-categories@example.test")
	read := createCategoryTestMessage(t, ctx, db, tenant, tenant.inbox, 801, "news@example.test", mailparse.CategoryNewsletters)
	createCategoryTestMessage(t, ctx, db, tenant, tenant.inbox, 802, "news@example.test", mailparse.CategoryNewsletters)
	// A message still waiting for classification counts as pending, not as a
	// member of any category.
	createCategoryTestMessage(t, ctx, db, tenant, tenant.inbox, 803, "later@example.test", "")
	if err := db.MarkMessageReadForUser(ctx, tenant.user.ID, read.ID, true, false); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: db, masterKey: []byte("12345678901234567890123456789012")}

	categories, pending, err := server.mailCategoryChrome(ctx, tenant.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("pending = %d, want 1", pending)
	}
	names := make([]string, 0, len(categories))
	for _, category := range categories {
		names = append(names, category.Name)
		if category.Label == "" || category.Icon == "" {
			t.Fatalf("category %q has no display text: %+v", category.Name, category)
		}
		if category.Name != mailparse.CategoryNewsletters {
			continue
		}
		if category.Total != 2 || category.Unread != 1 {
			t.Fatalf("newsletters counts = total %d unread %d, want 2/1", category.Total, category.Unread)
		}
	}
	// Every category is listed even when empty, so the sidebar does not grow a
	// new entry the first time a message happens to land in one.
	if !slices.Equal(names, mailparse.Categories()) {
		t.Fatalf("category names = %v, want %v", names, mailparse.Categories())
	}
}

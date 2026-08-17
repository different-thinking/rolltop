package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestUser030IsLatestRegisteredUserMigration(t *testing.T) {
	sets := currentUserMigrationSetsForUpgradeTest()
	if len(sets) < 2 {
		t.Fatalf("registered user migrations=%d, want at least 2", len(sets))
	}
	latest := sets[len(sets)-1]
	predecessor := sets[len(sets)-2]
	if latest.Version != UserSchemaVersion030 {
		t.Fatalf("latest user migration=%q, want %q", latest.Version, UserSchemaVersion030)
	}
	if predecessor.Version != UserSchemaVersion029 {
		t.Fatalf("user-030 predecessor=%q, want %q", predecessor.Version, UserSchemaVersion029)
	}
}

// A Sent choice is a pointer to a folder, so removing that folder must remove
// the choice rather than leave the view reading a mailbox that no longer exists.
func TestUser030SentMailboxChoiceFollowsItsFolder(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, account, _, _ := testMailbox(t, ctx, db)
	sent, err := db.GetOrCreateMailbox(ctx, user.ID, account.ID, "Sent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveSwipePreferences(ctx, SwipePreferences{
		UserID:     user.ID,
		LeftAction: SwipeActionSnooze, LeftSnoozePreset: SwipeSnoozeTomorrow,
		RightAction: SwipeActionMarkRead, RightSnoozePreset: SwipeSnoozeTomorrow,
		SentMailboxes: []SentMailbox{{AccountID: account.ID, MailboxID: sent.ID}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.mustDataDB(ctx, user.ID).ExecContext(ctx,
		`DELETE FROM mailboxes WHERE user_id = ? AND id = ?`, user.ID, sent.ID); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := db.mustDataDB(ctx, user.ID).QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sent_mailboxes WHERE user_id = ?`, user.ID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("sent mailbox choices left after deleting the folder = %d, want 0", remaining)
	}
}

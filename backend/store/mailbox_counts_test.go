package store

import (
	"context"
	"testing"
	"time"
)

// The folder counters read every way a row can be hidden from a list - a
// duplicate pointer, a snoozed thread, a snoozed row whose thread key is empty
// - and count a row that is hidden twice over once. They are aggregated per
// counter rather than per joined row, so this pins each case down along with
// the tenant boundary.
func TestListMailboxesForUserCountsHiddenRowsOnceAndPerTenant(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owner, err := db.CreateUser(ctx, "counts-owner@example.test", "Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateUser(ctx, "counts-other@example.test", "Other", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	plainUnread := createNewMailEventMessage(t, ctx, db, owner, 1, "A", "Plain unread")
	plainRead := createNewMailEventMessage(t, ctx, db, owner, 2, "B", "Plain read")
	duplicate := createNewMailEventMessage(t, ctx, db, owner, 3, "C", "Duplicate")
	snoozed := createNewMailEventMessage(t, ctx, db, owner, 4, "D", "Snoozed")
	snoozedNoKey := createNewMailEventMessage(t, ctx, db, owner, 5, "E", "Snoozed without key")
	snoozedDuplicate := createNewMailEventMessage(t, ctx, db, owner, 6, "F", "Snoozed duplicate")
	expired := createNewMailEventMessage(t, ctx, db, owner, 7, "G", "Snooze expired")
	otherSnoozed := createNewMailEventMessage(t, ctx, db, other, 1, "H", "Other tenant")

	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.DB().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE messages SET is_read = 0 WHERE user_id = ?`, owner.ID)
	exec(`UPDATE messages SET is_read = 1 WHERE user_id = ? AND id IN (?, ?)`, owner.ID, plainRead.ID, snoozedNoKey.ID)
	exec(`UPDATE messages SET blob_path = '' WHERE user_id = ? AND id = ?`, owner.ID, plainRead.ID)
	exec(`UPDATE messages SET duplicate_of_message_id = ? WHERE user_id = ? AND id IN (?, ?)`, plainUnread.ID, owner.ID, duplicate.ID, snoozedDuplicate.ID)
	exec(`UPDATE messages SET thread_key = '' WHERE user_id = ? AND id = ?`, owner.ID, snoozedNoKey.ID)
	// Thread keys are derived from headers and subject, so two tenants share one
	// whenever they receive the same thread. The other tenant's snooze names the
	// owner's plain thread, which stays visible only while the snooze match keeps
	// its user_id predicate.
	exec(`UPDATE messages SET thread_key = 'shared-thread' WHERE user_id = ? AND id = ?`, owner.ID, plainUnread.ID)
	exec(`UPDATE messages SET thread_key = 'shared-thread' WHERE user_id = ? AND id = ?`, other.ID, otherSnoozed.ID)
	for _, message := range []MessageRecord{snoozed, snoozedNoKey, snoozedDuplicate, expired} {
		if _, err := db.SnoozeMessage(ctx, owner.ID, message.ID, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE message_snoozes SET snoozed_until = ? WHERE user_id = ? AND message_id = ?`, time.Now().Add(-time.Minute).Unix(), owner.ID, expired.ID)
	if _, err := db.SnoozeMessage(ctx, other.ID, otherSnoozed.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	// The helper files each message under an account and folder of its own, so
	// every case below is also a check that it lands on its own folder.
	type counts struct{ local, messages, unread, cached int }
	want := map[int64]counts{
		plainUnread.MailboxID:      {1, 1, 1, 1},
		plainRead.MailboxID:        {1, 1, 0, 0},
		duplicate.MailboxID:        {1, 0, 0, 1},
		snoozed.MailboxID:          {1, 0, 0, 1},
		snoozedNoKey.MailboxID:     {1, 0, 0, 1},
		snoozedDuplicate.MailboxID: {1, 0, 0, 1},
		expired.MailboxID:          {1, 1, 1, 1},
	}
	mailboxes, err := db.ListMailboxesForUser(ctx, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(mailboxes) != len(want) {
		t.Fatalf("owner has %d mailboxes, want %d", len(mailboxes), len(want))
	}
	for _, box := range mailboxes {
		got := counts{box.LocalMessageCount, box.MessageCount, box.UnreadCount, box.CachedMessageCount}
		if expected, ok := want[box.ID]; !ok || got != expected {
			t.Fatalf("mailbox %d counts = %+v, want %+v (known: %v)", box.ID, got, expected, ok)
		}
	}

	otherBoxes, err := db.ListMailboxesForUser(ctx, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(otherBoxes) != 1 || otherBoxes[0].ID != otherSnoozed.MailboxID {
		t.Fatalf("other mailboxes = %+v", otherBoxes)
	}
	if otherBoxes[0].LocalMessageCount != 1 || otherBoxes[0].MessageCount != 0 {
		t.Fatalf("other counts = local:%d messages:%d", otherBoxes[0].LocalMessageCount, otherBoxes[0].MessageCount)
	}
}

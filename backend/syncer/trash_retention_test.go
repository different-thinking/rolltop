// File overview: Coverage for the scheduled half of emptying a Trash folder: what a
// retention purge selects, and what it deliberately leaves behind.

package syncer

import (
	"context"
	"slices"
	"testing"
	"time"

	"rolltop/backend/store"
)

// backdateTrashArrival makes one mirrored row look like a message that has been
// sitting in the Trash since before the cutoff. Arrival is the row's own
// created_at, which is when this mirror first stored the message in that folder.
func backdateTrashArrival(t *testing.T, f emptyTrashFixture, uid uint32, arrived time.Time) {
	t.Helper()
	ctx := context.Background()
	db, err := f.store.UserDB(ctx, f.userID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE messages SET created_at = ? WHERE user_id = ? AND mailbox_id = ? AND uid = ?`,
		arrived.UTC().Unix(), f.userID, f.trash.ID, uid); err != nil {
		t.Fatal(err)
	}
}

func (f emptyTrashFixture) runRetention(t *testing.T, before time.Time) store.SyncRun {
	t.Helper()
	ctx := context.Background()
	run, err := f.store.CreateSyncRun(ctx, f.userID, f.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.service.runEmptyTrash(ctx, f.userID, f.account, f.trash, run.ID, before, store.SyncProgress{}, nil, nil)
	finished, err := f.store.GetSyncRunForUser(ctx, f.userID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return finished
}

// A retention purge deletes the mail that has been in the folder long enough and
// nothing else. Everything the folder still holds is mail the reader threw away
// more recently than the policy deletes, which is the whole point of the Trash
// being a waiting room rather than a deletion.
func TestTrashRetentionPurgeDeletesOnlyMailThatHasWaitedLongEnough(t *testing.T) {
	fixture := newEmptyTrashFixture(t, []uint32{21, 22, 23})
	ctx := context.Background()
	cutoff := time.Now().UTC().AddDate(0, 0, -30)
	backdateTrashArrival(t, fixture, 22, cutoff.AddDate(0, 0, -10))

	run := fixture.runRetention(t, cutoff)

	if run.Status != "ok" {
		t.Fatalf("run status = %q (%s), want ok", run.Status, run.Error)
	}
	if len(fixture.fetcher.expungeCalls) != 1 {
		t.Fatalf("expunge calls = %d, want one batch", len(fixture.fetcher.expungeCalls))
	}
	if !slices.Equal(fixture.fetcher.expungeCalls[0].uids, []uint32{22}) {
		t.Fatalf("expunged uids = %v, want only the message that had waited out the policy",
			fixture.fetcher.expungeCalls[0].uids)
	}
	if fixture.fetcher.expungeCalls[0].uidValidity != emptyTrashUIDValidity {
		t.Fatalf("expunge generation = %d, want the folder's own %d",
			fixture.fetcher.expungeCalls[0].uidValidity, emptyTrashUIDValidity)
	}
	if !slices.Equal(fixture.fetcher.uids, []uint32{21, 23}) {
		t.Fatalf("server holds %v, want the mail thrown away since the cutoff left alone", fixture.fetcher.uids)
	}
	if _, err := fixture.store.GetMessageForUser(ctx, fixture.userID, fixture.messages[22].ID); !store.IsNotFound(err) {
		t.Fatalf("local row for the purged message = %v, want it removed with the remote one", err)
	}
	for _, uid := range []uint32{21, 23} {
		if _, err := fixture.store.GetMessageForUser(ctx, fixture.userID, fixture.messages[uid].ID); err != nil {
			t.Fatalf("local row for uid %d = %v, want it kept", uid, err)
		}
	}
}

// Nothing due is a finished pass, not a failure, and it must not reach for the
// server's delete command at all.
func TestTrashRetentionPurgeDeletesNothingWhenNothingHasWaitedLongEnough(t *testing.T) {
	fixture := newEmptyTrashFixture(t, []uint32{31, 32})

	run := fixture.runRetention(t, time.Now().UTC().AddDate(0, 0, -30))

	if run.Status != "ok" {
		t.Fatalf("run status = %q (%s), want ok", run.Status, run.Error)
	}
	if len(fixture.fetcher.expungeCalls) != 0 {
		t.Fatalf("expunge calls = %+v, want none", fixture.fetcher.expungeCalls)
	}
	if !slices.Equal(fixture.fetcher.uids, []uint32{31, 32}) {
		t.Fatalf("server holds %v, want everything still there", fixture.fetcher.uids)
	}
}

// Mail the mirror has never seen has no measurable stay, so a scheduled purge
// leaves it alone however long it has really been there. Emptying the folder by
// hand still takes all of it, which is what the same run without a cutoff does.
func TestTrashRetentionPurgeLeavesUnmirroredMailAloneAndEmptyingDoesNot(t *testing.T) {
	fixture := newEmptyTrashFixture(t, []uint32{41})
	// A UID the server holds and the mirror has no row for.
	fixture.fetcher.uids = append(fixture.fetcher.uids, 42)
	backdateTrashArrival(t, fixture, 41, time.Now().UTC().AddDate(0, 0, -90))

	fixture.runRetention(t, time.Now().UTC().AddDate(0, 0, -30))
	if !slices.Equal(fixture.fetcher.uids, []uint32{42}) {
		t.Fatalf("server holds %v, want the mail this mirror never saw left alone", fixture.fetcher.uids)
	}

	fixture.fetcher.expungeCalls = nil
	fixture.runEmpty(t)
	if len(fixture.fetcher.uids) != 0 {
		t.Fatalf("server holds %v after emptying by hand, want nothing: emptying is still emptying", fixture.fetcher.uids)
	}
}

// The public entry point refuses a purge with no cutoff rather than treating it
// as an instruction to empty the folder.
func TestStartTrashRetentionPurgeRefusesAMissingCutoff(t *testing.T) {
	fixture := newEmptyTrashFixture(t, []uint32{51})
	if _, err := fixture.service.StartTrashRetentionPurge(context.Background(), fixture.userID, fixture.trash.ID,
		time.Time{}, nil, nil); err == nil {
		t.Fatal("StartTrashRetentionPurge with no cutoff = nil error, want a refusal: an empty cutoff must never read as everything")
	}
}

// A retention purge names the UIDs it means, so the expunge must be the one
// that removes exactly those. Emptying the folder by hand is the only caller
// allowed the fallback that takes everything flagged \Deleted with it.
func TestTrashRetentionPurgeAsksForASelectiveExpungeAndEmptyingDoesNot(t *testing.T) {
	fixture := newEmptyTrashFixture(t, []uint32{61, 62})
	backdateTrashArrival(t, fixture, 61, time.Now().UTC().AddDate(0, 0, -90))

	fixture.runRetention(t, time.Now().UTC().AddDate(0, 0, -30))
	if len(fixture.fetcher.expungeCalls) != 1 {
		t.Fatalf("expunge calls = %d, want one", len(fixture.fetcher.expungeCalls))
	}
	if fixture.fetcher.expungeCalls[0].scope != ExpungeNamedUIDsOnly {
		t.Fatalf("retention purge scope = %q, want %q", fixture.fetcher.expungeCalls[0].scope, ExpungeNamedUIDsOnly)
	}

	fixture.fetcher.expungeCalls = nil
	fixture.runEmpty(t)
	if len(fixture.fetcher.expungeCalls) != 1 {
		t.Fatalf("expunge calls = %d, want one", len(fixture.fetcher.expungeCalls))
	}
	if fixture.fetcher.expungeCalls[0].scope != ExpungeWholeFolder {
		t.Fatalf("empty-trash scope = %q, want %q", fixture.fetcher.expungeCalls[0].scope, ExpungeWholeFolder)
	}
}

// A server whose only expunge takes the whole folder cannot serve a retention
// purge at all. The run says so and leaves every message where it is, rather
// than deleting mail that has not waited long enough or that another client
// flagged and has not expunged yet.
func TestTrashRetentionPurgeStopsRatherThanWidenOnAServerWithoutUIDPlus(t *testing.T) {
	fixture := newEmptyTrashFixture(t, []uint32{71, 72, 73})
	fixture.fetcher.noSelectiveExpunge = true
	backdateTrashArrival(t, fixture, 71, time.Now().UTC().AddDate(0, 0, -90))

	run := fixture.runRetention(t, time.Now().UTC().AddDate(0, 0, -30))

	if run.Status != "failed" {
		t.Fatalf("run status = %q, want failed: a purge that cannot be served must say so", run.Status)
	}
	if !slices.Equal(fixture.fetcher.uids, []uint32{71, 72, 73}) {
		t.Fatalf("server holds %v, want the folder untouched", fixture.fetcher.uids)
	}
	// One answer, not one per batch and not one per retry: a missing capability
	// reads the same way on a fresh login.
	if len(fixture.fetcher.expungeCalls) != 1 {
		t.Fatalf("expunge calls = %d, want exactly one: a missing capability is not worth retrying",
			len(fixture.fetcher.expungeCalls))
	}
	for uid, message := range fixture.messages {
		if _, err := fixture.store.GetMessageForUser(context.Background(), fixture.userID, message.ID); err != nil {
			t.Fatalf("local row for uid %d = %v, want it kept alongside the remote message", uid, err)
		}
	}

	// Emptying the folder by hand is still served: there, taking everything
	// flagged is what was asked for.
	fixture.fetcher.expungeCalls = nil
	fixture.runEmpty(t)
	if len(fixture.fetcher.uids) != 0 {
		t.Fatalf("server holds %v after emptying by hand, want nothing", fixture.fetcher.uids)
	}
}

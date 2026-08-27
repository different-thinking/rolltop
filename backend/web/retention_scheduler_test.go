package web

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	"rolltop/backend/store"
	"rolltop/backend/store/storetest"
	"rolltop/backend/syncer"
)

// A category rule resolves to the mail that category holds across every folder
// it is filed in, older than the cutoff, on its way to that account's Trash.
func TestCategoryRetentionPlanReachesEveryFolderTheCategoryIsFiledIn(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tenant := newScopeTestTenant(t, ctx, db, "retention-scope@example.test")
	archive, err := db.GetOrCreateMailbox(ctx, tenant.user.ID, tenant.accountID, "Archive")
	if err != nil {
		t.Fatal(err)
	}
	filed, err := db.GetOrCreateMailbox(ctx, tenant.user.ID, tenant.accountID, "Reading")
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
	now := time.Now().UTC()
	old := now.AddDate(0, 0, -90)
	oldInbox := createScopeTestMessageFrom(t, ctx, db, tenant, tenant.inbox, 701, "", "list@example.test", "newsletters", old)
	oldArchived := createScopeTestMessageFrom(t, ctx, db, tenant, archive, 702, "", "list@example.test", "newsletters", old)
	oldFiled := createScopeTestMessageFrom(t, ctx, db, tenant, filed, 703, "", "list@example.test", "newsletters", old)
	recentInbox := createScopeTestMessageFrom(t, ctx, db, tenant, tenant.inbox, 704, "", "list@example.test", "newsletters", old.AddDate(0, 0, 89))
	otherCategory := createScopeTestMessageFrom(t, ctx, db, tenant, tenant.inbox, 705, "", "friend@example.test", "relevant", old)
	alreadyTrashed := createScopeTestMessageFrom(t, ctx, db, tenant, tenant.trash, 706, "", "list@example.test", "newsletters", old)
	server := &Server{store: db, masterKey: bytes.Repeat([]byte{9}, 32)}

	messages, complete, err := server.categoryRetentionMessages(ctx, tenant.user,
		[]store.CategoryRetention{{Category: "newsletters", Mode: store.RetentionModeRelative, Count: 30, Unit: store.RetentionUnitDays}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !complete {
		t.Fatal("pass reported itself cut short for three messages")
	}
	plan, err := server.trashPlanForMessages(ctx, tenant.user, messages)
	if err != nil {
		t.Fatal(err)
	}
	ids := planMessageIDs(plan)
	slices.Sort(ids)
	want := []int64{oldInbox.ID, oldArchived.ID, oldFiled.ID}
	slices.Sort(want)
	if !slices.Equal(ids, want) {
		t.Fatalf("planned ids = %v, want the old newsletters in every folder %v", ids, want)
	}
	for _, unwanted := range []int64{recentInbox.ID, otherCategory.ID, alreadyTrashed.ID} {
		if slices.Contains(ids, unwanted) {
			t.Fatalf("planned ids = %v, must not include %d", ids, unwanted)
		}
	}
	if len(plan.Groups) != 1 || plan.Groups[0].Target.ID != tenant.trash.ID {
		t.Fatalf("plan groups = %+v, want one move into this account's Trash %d", plan.Groups, tenant.trash.ID)
	}
}

// A rule that resolves to no cutoff selects nothing. It must never read as "no
// filter", which would take the whole category.
func TestCategoryRetentionPlanSelectsNothingWithoutACutoff(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tenant := newScopeTestTenant(t, ctx, db, "retention-nocutoff@example.test")
	createScopeTestMessageFrom(t, ctx, db, tenant, tenant.inbox, 711, "", "list@example.test", "newsletters",
		time.Now().UTC().AddDate(-1, 0, 0))
	server := &Server{store: db, masterKey: bytes.Repeat([]byte{9}, 32)}

	for _, rule := range []store.CategoryRetention{
		{Category: "newsletters", Mode: store.RetentionModeOff},
		{Category: "newsletters", Mode: store.RetentionModeRelative, Count: 0, Unit: store.RetentionUnitDays},
		{Category: "newsletters", Mode: store.RetentionModeRelative, Count: 30, Unit: "fortnights"},
		{Category: "newsletters", Mode: store.RetentionModeFixed},
	} {
		messages, _, err := server.categoryRetentionMessages(ctx, tenant.user, []store.CategoryRetention{rule}, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) != 0 {
			t.Fatalf("selection for %+v = %d messages, want nothing selected", rule, len(messages))
		}
	}
}

// A pass runs when it is due and stays quiet when it is not, and it records that
// it ran either way so a reader whose sweep fails is retried on the interval
// rather than in a loop.
func TestRetentionSweepHonoursTheInterval(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tenant := newScopeTestTenant(t, ctx, db, "retention-interval@example.test")
	server := &Server{
		store: db, masterKey: bytes.Repeat([]byte{9}, 32), events: newEventHub(), mailListCache: newMailListCache(),
		syncer: &syncer.Service{Store: db, Fetcher: &acceptingMoveFetcher{}},
	}
	if _, err := db.SaveRetentionSettings(ctx, store.RetentionSettings{
		UserID: tenant.user.ID, TrashEnabled: false,
		Categories: []store.CategoryRetention{{Category: "newsletters", Mode: store.RetentionModeRelative, Count: 30, Unit: store.RetentionUnitDays}},
	}); err != nil {
		t.Fatal(err)
	}

	first := time.Now().UTC().Truncate(time.Second)
	if err := server.sweepUserRetention(ctx, tenant.user, first); err != nil {
		t.Fatal(err)
	}
	swept, err := db.GetRetentionSettings(ctx, tenant.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !swept.CategoriesSweptAt.Equal(first) {
		t.Fatalf("category mark after a pass = %v, want %v", swept.CategoriesSweptAt, first)
	}
	if !swept.TrashSweptAt.IsZero() {
		t.Fatalf("Trash mark = %v, want it untouched while the Trash rule is off", swept.TrashSweptAt)
	}

	// Too soon: the mark must not move.
	if err := server.sweepUserRetention(ctx, tenant.user, first.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	again, err := db.GetRetentionSettings(ctx, tenant.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !again.CategoriesSweptAt.Equal(first) {
		t.Fatalf("category mark an hour later = %v, want the earlier %v: the pass is not due yet", again.CategoriesSweptAt, first)
	}

	// Past the interval: it runs again.
	due := first.Add(retentionSweepInterval + time.Minute).Truncate(time.Second)
	if err := server.sweepUserRetention(ctx, tenant.user, due); err != nil {
		t.Fatal(err)
	}
	third, err := db.GetRetentionSettings(ctx, tenant.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !third.CategoriesSweptAt.Equal(due) {
		t.Fatalf("category mark past the interval = %v, want %v", third.CategoriesSweptAt, due)
	}
}

// A reader with no rules at all is not swept, and nothing about the pass writes
// a policy they never chose.
func TestRetentionSweepLeavesAReaderWithNoRulesAlone(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tenant := newScopeTestTenant(t, ctx, db, "retention-idle@example.test")
	server := &Server{
		store: db, masterKey: bytes.Repeat([]byte{9}, 32), events: newEventHub(), mailListCache: newMailListCache(),
		syncer: &syncer.Service{Store: db, Fetcher: &acceptingMoveFetcher{}},
	}
	if _, err := db.SaveRetentionSettings(ctx, store.RetentionSettings{UserID: tenant.user.ID, TrashEnabled: false}); err != nil {
		t.Fatal(err)
	}

	if err := server.sweepUserRetention(ctx, tenant.user, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	settings, err := db.GetRetentionSettings(ctx, tenant.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !settings.CategoriesSweptAt.IsZero() || !settings.TrashSweptAt.IsZero() {
		t.Fatalf("marks = %v/%v, want both untouched: there was nothing to sweep",
			settings.CategoriesSweptAt, settings.TrashSweptAt)
	}
}

func TestRetentionCatchUpMarkBringsTheNextPassForward(t *testing.T) {
	now := time.Now().UTC()
	mark := retentionCatchUpMark(now)
	if retentionDue(mark, now) {
		t.Fatal("a cut-short pass is due immediately, which would spin rather than let the moves it queued land")
	}
	if !retentionDue(mark, now.Add(retentionCatchUpInterval+time.Second)) {
		t.Fatalf("a cut-short pass is not due again after %v, want it to catch up sooner than a full interval", retentionCatchUpInterval)
	}
	if !retentionDue(time.Time{}, now) {
		t.Fatal("a cleared mark is not due, which is what saving a policy leaves behind")
	}
}

// Every category's mail goes into one move plan. A plan per rule took the
// tenant's exclusive foreground reservation and held it until its runs landed,
// so the second rule waited out the whole reservation timeout and then failed:
// one rule applied per pass and the sweep stalled on each of the others.
func TestCategoryRetentionCollectsEveryRuleIntoOneMovePlan(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tenant := newScopeTestTenant(t, ctx, db, "retention-one-plan@example.test")
	now := time.Now().UTC()
	old := now.AddDate(0, 0, -90)
	newsletter := createScopeTestMessageFrom(t, ctx, db, tenant, tenant.inbox, 721, "", "list@example.test", "newsletters", old)
	forum := createScopeTestMessageFrom(t, ctx, db, tenant, tenant.inbox, 722, "", "forum@example.test", "forums", old)
	relevant := createScopeTestMessageFrom(t, ctx, db, tenant, tenant.inbox, 723, "", "friend@example.test", "relevant", old)
	server := &Server{store: db, masterKey: bytes.Repeat([]byte{9}, 32)}

	messages, complete, err := server.categoryRetentionMessages(ctx, tenant.user, []store.CategoryRetention{
		{Category: "newsletters", Mode: store.RetentionModeRelative, Count: 30, Unit: store.RetentionUnitDays},
		{Category: "forums", Mode: store.RetentionModeRelative, Count: 30, Unit: store.RetentionUnitDays},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !complete {
		t.Fatal("pass reported itself cut short for two messages")
	}
	plan, err := server.trashPlanForMessages(ctx, tenant.user, messages)
	if err != nil {
		t.Fatal(err)
	}
	ids := planMessageIDs(plan)
	slices.Sort(ids)
	want := []int64{newsletter.ID, forum.ID}
	slices.Sort(want)
	if !slices.Equal(ids, want) {
		t.Fatalf("planned ids = %v, want both rules' mail in one plan %v", ids, want)
	}
	if slices.Contains(ids, relevant.ID) {
		t.Fatalf("planned ids = %v, must not include the category with no rule", ids)
	}
	if len(plan.Groups) != 1 {
		t.Fatalf("plan groups = %d, want one move for the account rather than one per rule", len(plan.Groups))
	}
}

// A folder the pass could not finish clearing brings the next pass forward
// rather than waiting out the full interval. That covers both the bounded
// selection one purge takes and the mail a server refused.
func TestTrashRetentionSweepAsksWhetherTheFolderIsActuallyClear(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tenant := newScopeTestTenant(t, ctx, db, "retention-still-due@example.test")
	cutoff := time.Now().UTC().AddDate(0, 0, -30)

	due, err := db.TrashRetentionStillDue(ctx, tenant.user.ID, tenant.trash.ID, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if due {
		t.Fatal("an empty Trash folder reported mail still due")
	}

	message := createScopeTestMessageDated(t, ctx, db, tenant, tenant.trash, 731, "Old", time.Now().UTC())
	userDB, err := db.UserDB(ctx, tenant.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := userDB.ExecContext(ctx, `UPDATE messages SET created_at = ? WHERE user_id = ? AND id = ?`,
		time.Now().UTC().AddDate(0, 0, -90).Unix(), tenant.user.ID, message.ID); err != nil {
		t.Fatal(err)
	}
	due, err = db.TrashRetentionStillDue(ctx, tenant.user.ID, tenant.trash.ID, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if !due {
		t.Fatal("a folder still holding mail that arrived before the cutoff reported itself clear")
	}
}

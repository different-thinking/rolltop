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

	plan, err := server.categoryRetentionPlan(ctx, tenant.user,
		store.CategoryRetention{Category: "newsletters", Mode: store.RetentionModeRelative, Count: 30, Unit: store.RetentionUnitDays}, now)
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
		{Category: "newsletters", Mode: store.RetentionModeFixed},
	} {
		plan, err := server.categoryRetentionPlan(ctx, tenant.user, rule, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if plan.Matched != 0 || len(plan.Groups) != 0 {
			t.Fatalf("plan for %+v = matched %d groups %d, want nothing selected", rule, plan.Matched, len(plan.Groups))
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

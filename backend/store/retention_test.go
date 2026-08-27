// File overview: Retention defaults, persistence, validation, and what the Trash
// half selects — which is the mail the mirror saw arrive, not the mail that is old.

package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"rolltop/backend/store/pgtestdb"
)

// retentionBackfillStatement is the shipped statement that switches the
// automatic Trash purge off for accounts that predate retention. The test runs
// the real SQL rather than a copy of it: what is being checked is that the
// statement in the migration list does what the migration says it does.
func retentionBackfillStatement(t *testing.T) string {
	t.Helper()
	for _, migration := range postgresMigrations {
		if migration.Version != "0010-retention" {
			continue
		}
		for _, statement := range migration.Statements {
			if strings.Contains(statement, "INSERT INTO retention_settings") {
				return statement
			}
		}
	}
	t.Fatal("the 0010-retention migration no longer backfills retention_settings, so upgrading an existing install now switches an irreversible purge on for everybody")
	return ""
}

// Upgrading must not start deleting anybody's mail off their mail server. The
// migration writes every account that already exists an explicit "off" row; an
// account created afterwards has no row, and the absence of one carries the
// shipped default, which is that the Trash empties itself after 30 days.
func TestUpgradingLeavesExistingReadersOutOfTheAutomaticTrashPurge(t *testing.T) {
	dsn := pgtestdb.New(t)
	conn, ctx := openMigrationTestConn(t, dsn)

	// A user the migration has not seen: the schema was applied to an empty
	// database above, so this row stands in for one that predates it.
	var userID int64
	if err := conn.QueryRowContext(ctx, `INSERT INTO users (email, name, password_hash, is_admin, created_at, updated_at)
		VALUES ('grandfathered@example.test', 'Existing', 'hash', 0, 0, 0) RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	if _, err := conn.ExecContext(ctx, retentionBackfillStatement(t)); err != nil {
		t.Fatalf("run the shipped backfill: %v", err)
	}

	var enabled, days int64
	if err := conn.QueryRowContext(ctx,
		`SELECT trash_enabled, trash_days FROM retention_settings WHERE user_id = $1`, userID).
		Scan(&enabled, &days); err != nil {
		t.Fatalf("read the backfilled row: %v", err)
	}
	if enabled != 0 {
		t.Fatal("an account that predates retention came out of the upgrade with the automatic purge on, which deletes their mail from their mail server without anybody asking")
	}
	if days != DefaultTrashRetentionDays {
		t.Fatalf("Trash days = %d, want the default %d waiting to be switched on", days, DefaultTrashRetentionDays)
	}

	// It is a backfill, not an overwrite: a reader who has already chosen keeps
	// their choice if the statement ever runs again.
	if _, err := conn.ExecContext(ctx,
		`UPDATE retention_settings SET trash_enabled = 1, trash_days = 7 WHERE user_id = $1`, userID); err != nil {
		t.Fatalf("record a choice: %v", err)
	}
	if _, err := conn.ExecContext(ctx, retentionBackfillStatement(t)); err != nil {
		t.Fatalf("re-run the shipped backfill: %v", err)
	}
	if err := conn.QueryRowContext(ctx,
		`SELECT trash_enabled, trash_days FROM retention_settings WHERE user_id = $1`, userID).
		Scan(&enabled, &days); err != nil {
		t.Fatalf("re-read the row: %v", err)
	}
	if enabled != 1 || days != 7 {
		t.Fatalf("row after a second backfill = enabled %d days %d, want the reader's own choice kept", enabled, days)
	}
}

// A sweep mark must not switch the purge on behind a reader who has it off.
func TestMarkRetentionSweptDoesNotSwitchTheTrashPurgeOn(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "retention-off@example.test", "Off", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveRetentionSettings(ctx, RetentionSettings{UserID: user.ID, TrashEnabled: false}); err != nil {
		t.Fatal(err)
	}

	if err := db.MarkRetentionSwept(ctx, user.ID, time.Now().UTC(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	settings, err := db.GetRetentionSettings(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if settings.TrashEnabled {
		t.Fatal("recording a sweep switched the automatic purge on")
	}
}

func TestRetentionSettingsDefaultToAnEmptyingTrashAndNoCategoryRules(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "retention@example.test", "Retention", "hash", false)
	if err != nil {
		t.Fatal(err)
	}

	settings, err := db.GetRetentionSettings(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(settings, DefaultRetentionSettings(user.ID)) {
		t.Fatalf("default retention = %+v, want %+v", settings, DefaultRetentionSettings(user.ID))
	}
	if settings.TrashDays != DefaultTrashRetentionDays || !settings.TrashEnabled {
		t.Fatalf("default Trash rule = %d days enabled=%v, want %d days enabled",
			settings.TrashDays, settings.TrashEnabled, DefaultTrashRetentionDays)
	}
	if len(settings.Categories) != 0 {
		t.Fatalf("default category rules = %+v, want none: nothing may be deleted from a category nobody has spoken about", settings.Categories)
	}
}

func TestRetentionSettingsRoundTripBothCutoffSpellings(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "retention@example.test", "Retention", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)

	saved, err := db.SaveRetentionSettings(ctx, RetentionSettings{
		UserID:       user.ID,
		TrashEnabled: true,
		TrashDays:    14,
		Categories: []CategoryRetention{
			{Category: "newsletters", Mode: RetentionModeRelative, Count: 30, Unit: RetentionUnitDays},
			{Category: "forums", Mode: RetentionModeFixed, Before: fixed},
			// An "off" rule is dropped rather than stored: the absence of a row
			// is what says a category deletes nothing.
			{Category: "relevant", Mode: RetentionModeOff},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if saved.TrashDays != 14 || !saved.TrashEnabled {
		t.Fatalf("saved Trash rule = %d days enabled=%v, want 14 days enabled", saved.TrashDays, saved.TrashEnabled)
	}
	// Registry order, and without the rule that was off.
	want := []CategoryRetention{
		{Category: "newsletters", Mode: RetentionModeRelative, Count: 30, Unit: RetentionUnitDays},
		{Category: "forums", Mode: RetentionModeFixed, Before: fixed},
	}
	if !reflect.DeepEqual(saved.Categories, want) {
		t.Fatalf("saved category rules = %+v, want %+v", saved.Categories, want)
	}

	reread, err := db.GetRetentionSettings(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reread.Categories, want) {
		t.Fatalf("reread category rules = %+v, want %+v", reread.Categories, want)
	}
	if reread.TrashDays != 14 {
		t.Fatalf("reread Trash rule = %d days, want 14", reread.TrashDays)
	}
}

func TestSaveRetentionSettingsRefusesRulesThatSelectNothingOrEverything(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "retention@example.test", "Retention", "hash", false)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		settings RetentionSettings
	}{
		{"unknown category", RetentionSettings{UserID: user.ID, TrashEnabled: true, TrashDays: 30,
			Categories: []CategoryRetention{{Category: "postcards", Mode: RetentionModeRelative, Count: 5, Unit: RetentionUnitDays}}}},
		{"relative rule with no count", RetentionSettings{UserID: user.ID, TrashEnabled: true, TrashDays: 30,
			Categories: []CategoryRetention{{Category: "newsletters", Mode: RetentionModeRelative, Unit: RetentionUnitDays}}}},
		{"relative rule counted in nothing this build knows", RetentionSettings{UserID: user.ID, TrashEnabled: true, TrashDays: 30,
			Categories: []CategoryRetention{{Category: "newsletters", Mode: RetentionModeRelative, Count: 2, Unit: "fortnights"}}}},
		{"fixed rule with no date", RetentionSettings{UserID: user.ID, TrashEnabled: true, TrashDays: 30,
			Categories: []CategoryRetention{{Category: "newsletters", Mode: RetentionModeFixed}}}},
		{"unknown mode", RetentionSettings{UserID: user.ID, TrashEnabled: true, TrashDays: 30,
			Categories: []CategoryRetention{{Category: "newsletters", Mode: "someday"}}}},
		{"Trash rule with no days", RetentionSettings{UserID: user.ID, TrashEnabled: true, TrashDays: 0}},
	}
	for _, tc := range cases {
		if _, err := db.SaveRetentionSettings(ctx, tc.settings); !errors.Is(err, ErrInvalidRetentionSettings) {
			t.Fatalf("%s: err = %v, want ErrInvalidRetentionSettings", tc.name, err)
		}
	}

	// Switching the Trash rule off does not need a day count to switch off with.
	if _, err := db.SaveRetentionSettings(ctx, RetentionSettings{UserID: user.ID, TrashEnabled: false}); err != nil {
		t.Fatalf("switching the Trash rule off: %v", err)
	}
}

// Saving clears the sweep marks so a policy takes effect on the next pass
// rather than waiting out the interval of the pass that ran before it existed.
func TestSaveRetentionSettingsMakesThePolicyDueAgain(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "retention@example.test", "Retention", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	swept := time.Now().UTC().Truncate(time.Second)
	if err := db.MarkRetentionSwept(ctx, user.ID, swept, swept); err != nil {
		t.Fatal(err)
	}
	marked, err := db.GetRetentionSettings(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !marked.CategoriesSweptAt.Equal(swept) || !marked.TrashSweptAt.Equal(swept) {
		t.Fatalf("sweep marks = %v/%v, want both %v", marked.CategoriesSweptAt, marked.TrashSweptAt, swept)
	}

	saved, err := db.SaveRetentionSettings(ctx, RetentionSettings{UserID: user.ID, TrashEnabled: true, TrashDays: 7})
	if err != nil {
		t.Fatal(err)
	}
	if !saved.CategoriesSweptAt.IsZero() || !saved.TrashSweptAt.IsZero() {
		t.Fatalf("sweep marks after saving = %v/%v, want both cleared so the new policy runs on the next pass",
			saved.CategoriesSweptAt, saved.TrashSweptAt)
	}
}

// A half that did not run keeps the mark it had. Writing the row whole would
// otherwise reset the other half's clock every time one of them ran.
func TestMarkRetentionSweptLeavesTheOtherHalfAlone(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "retention@example.test", "Retention", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	categories := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	trash := time.Now().UTC().Truncate(time.Second)
	if err := db.MarkRetentionSwept(ctx, user.ID, categories, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkRetentionSwept(ctx, user.ID, time.Time{}, trash); err != nil {
		t.Fatal(err)
	}
	settings, err := db.GetRetentionSettings(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !settings.CategoriesSweptAt.Equal(categories) {
		t.Fatalf("category mark = %v, want the earlier %v kept", settings.CategoriesSweptAt, categories)
	}
	if !settings.TrashSweptAt.Equal(trash) {
		t.Fatalf("Trash mark = %v, want %v", settings.TrashSweptAt, trash)
	}
}

func TestCategoryRetentionCutoffResolvesBothSpellings(t *testing.T) {
	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	relative, ok := CategoryRetention{Mode: RetentionModeRelative, Count: 30, Unit: RetentionUnitDays}.Cutoff(now)
	if !ok || !relative.Equal(time.Date(2025, 5, 16, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("relative cutoff = %v (ok=%v), want 30 days before %v", relative, ok, now)
	}
	// A month is a calendar month, which is the whole reason the unit is stored
	// rather than reduced to a number of days.
	months, ok := CategoryRetention{Mode: RetentionModeRelative, Count: 6, Unit: RetentionUnitMonths}.Cutoff(now)
	if !ok || !months.Equal(time.Date(2024, 12, 15, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("six-month cutoff = %v (ok=%v), want six calendar months before %v", months, ok, now)
	}
	years, ok := CategoryRetention{Mode: RetentionModeRelative, Count: 2, Unit: RetentionUnitYears}.Cutoff(now)
	if !ok || !years.Equal(time.Date(2023, 6, 15, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("two-year cutoff = %v (ok=%v), want two calendar years before %v", years, ok, now)
	}
	fixed := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	got, ok := CategoryRetention{Mode: RetentionModeFixed, Before: fixed}.Cutoff(now)
	if !ok || !got.Equal(fixed) {
		t.Fatalf("fixed cutoff = %v (ok=%v), want %v", got, ok, fixed)
	}
	// A rule that resolves to nothing must never read as "everything": an action
	// with no cutoff would take the whole category rather than its backlog.
	for _, rule := range []CategoryRetention{
		{Mode: RetentionModeOff},
		{Mode: RetentionModeRelative, Count: 0, Unit: RetentionUnitDays},
		{Mode: RetentionModeRelative, Count: 30, Unit: "fortnights"},
		{Mode: RetentionModeFixed},
	} {
		if cutoff, ok := rule.Cutoff(now); ok || !cutoff.IsZero() {
			t.Fatalf("cutoff of %+v = %v (ok=%v), want no cutoff at all", rule, cutoff, ok)
		}
	}
}

// The Trash half counts from when a message arrived in the Trash, not from when
// it was sent: a year-old newsletter thrown away this morning has a full stay
// ahead of it, and a message the mirror has never seen has no stay to measure.
func TestListTrashRetentionUIDsCountsFromArrivalInTheFolder(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, account, _, blob := testMailbox(t, ctx, db)
	trash, err := db.GetOrCreateMailboxWithRole(ctx, user.ID, account.ID, "Trash", "trash")
	if err != nil {
		t.Fatal(err)
	}
	const uidValidity = uint32(4242)
	if err := db.UpdateMailboxRemoteStatus(ctx, user.ID, trash.ID, 0, 0, 9, uidValidity); err != nil {
		t.Fatal(err)
	}
	sent := time.Now().UTC().AddDate(-2, 0, 0)
	for _, uid := range []uint32{1, 2, 3} {
		if _, err := db.CreateMessage(ctx, CreateMessage{
			UserID: user.ID, AccountID: account.ID, MailboxID: trash.ID, BlobID: blob.ID,
			MessageIDHeader: "<trash-" + string(rune('a'+uid)) + "@example.test>",
			Subject:         "Thrown away", Date: sent, InternalDate: sent,
			UID: uid, UIDValidity: int64(uidValidity), BlobPath: blob.Path,
		}); err != nil {
			t.Fatal(err)
		}
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -30)

	// Every message is two years old, and every one of them arrived in the
	// Trash a moment ago: nothing is due.
	due, err := db.ListTrashRetentionUIDs(ctx, user.ID, trash.ID, cutoff, uidValidity)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("due = %+v, want nothing: mail thrown away today keeps its full stay however old the mail is", due)
	}

	// Backdate one row's arrival, which is what a message that really has been
	// sitting in the Trash for months looks like.
	userDB, err := db.UserDB(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := userDB.ExecContext(ctx, `UPDATE messages SET created_at = ? WHERE user_id = ? AND mailbox_id = ? AND uid = ?`,
		time.Now().UTC().AddDate(0, 0, -90).Unix(), user.ID, trash.ID, 2); err != nil {
		t.Fatal(err)
	}
	due, err = db.ListTrashRetentionUIDs(ctx, user.ID, trash.ID, cutoff, uidValidity)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].UID != 2 {
		t.Fatalf("due = %+v, want only the message that arrived before the cutoff", due)
	}

	// A folder whose generation has moved on says nothing about those UIDs.
	due, err = db.ListTrashRetentionUIDs(ctx, user.ID, trash.ID, cutoff, uidValidity+1)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("due under another generation = %+v, want nothing", due)
	}

	// No cutoff selects nothing rather than everything.
	due, err = db.ListTrashRetentionUIDs(ctx, user.ID, trash.ID, time.Time{}, uidValidity)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("due with no cutoff = %+v, want nothing", due)
	}
}

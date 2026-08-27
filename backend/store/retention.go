// File overview: How long mail is kept. Two rules live here and they hand over to
// one another: a category rule decides when mail is thrown away (moved to Trash),
// and the Trash rule decides how long the Trash keeps what was thrown away before
// the server is told to delete it for good.

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"rolltop/backend/mailparse"
)

const (
	// RetentionModeOff is a category nothing is deleted from.
	RetentionModeOff = "off"
	// RetentionModeRelative keeps mail for a length of time, so the cutoff moves
	// with the calendar and the rule keeps meaning the same thing next month.
	RetentionModeRelative = "relative"
	// RetentionModeFixed keeps mail dated on or after one named day, which is
	// how "clear out everything from before we moved house" is expressed.
	RetentionModeFixed = "fixed"
)

const (
	// RetentionUnitDays, RetentionUnitMonths and RetentionUnitYears are the
	// steps a relative cutoff counts in. The unit is stored rather than reduced
	// to days, because the calendar is what the reader meant: six months is six
	// months whatever those months are worth in days.
	RetentionUnitDays   = "days"
	RetentionUnitMonths = "months"
	RetentionUnitYears  = "years"
)

// validRetentionUnit reports whether a stored or submitted unit is one of the
// three, which is also what stops an unknown one from being read as days.
func validRetentionUnit(unit string) bool {
	switch unit {
	case RetentionUnitDays, RetentionUnitMonths, RetentionUnitYears:
		return true
	default:
		return false
	}
}

// DefaultTrashRetentionDays is how long the Trash keeps a message before the
// server is told to delete it. It is a default rather than a fixed rule: the
// reader can change the number, and switch the whole thing off.
const DefaultTrashRetentionDays = 30

// maxRetentionDays bounds a stored day count. It is far past any useful
// retention policy and exists so a request cannot store a number whose
// arithmetic overflows the cutoff it is turned into.
const maxRetentionDays = 36500

// maxRetentionCount bounds a relative cutoff's own number, whatever unit it
// counts in, for the same reason.
const maxRetentionCount = 36500

// ErrInvalidRetentionSettings reports a policy that was refused rather than a
// failure to store one.
var ErrInvalidRetentionSettings = errors.New("invalid retention settings")

// CategoryRetention is one category's answer to "how long is this kept". A
// category with no rule is absent from the settings rather than present and
// off, so nothing is ever deleted from a category the reader has not spoken
// about.
type CategoryRetention struct {
	Category string
	Mode     string
	// Count and Unit are the retention length under RetentionModeRelative, kept
	// on the calendar: "6 months" steps six months back rather than a hundred
	// and eighty days.
	Count int
	Unit  string
	// Before is the cutoff under RetentionModeFixed: mail dated before this
	// instant is thrown away, and mail dated at or after it is kept.
	Before time.Time
}

// Cutoff resolves the rule against a moment, and reports whether it selects
// anything at all. A rule that resolves to the zero time selects nothing: an
// action with no cutoff would take the whole category rather than its backlog,
// so "no cutoff" must never read as "everything".
func (r CategoryRetention) Cutoff(now time.Time) (time.Time, bool) {
	switch r.Mode {
	case RetentionModeRelative:
		if r.Count <= 0 {
			return time.Time{}, false
		}
		switch r.Unit {
		case RetentionUnitMonths:
			return now.UTC().AddDate(0, -r.Count, 0), true
		case RetentionUnitYears:
			return now.UTC().AddDate(-r.Count, 0, 0), true
		case RetentionUnitDays:
			return now.UTC().AddDate(0, 0, -r.Count), true
		default:
			// An unrecognised unit is not silently read as days: a rule nothing
			// can resolve must select nothing rather than the wrong backlog.
			return time.Time{}, false
		}
	case RetentionModeFixed:
		if r.Before.IsZero() {
			return time.Time{}, false
		}
		return r.Before.UTC(), true
	default:
		return time.Time{}, false
	}
}

// RetentionSettings is one reader's whole policy: the Trash rule, and whatever
// the categories say.
type RetentionSettings struct {
	UserID int64
	// TrashEnabled reports whether the Trash is emptied on a schedule at all.
	TrashEnabled bool
	// TrashDays is how long a message stays in the Trash after it arrives there.
	TrashDays int
	// Categories holds only the categories with a rule, in registry order.
	Categories []CategoryRetention
	// CategoriesSweptAt and TrashSweptAt are when each half last ran. They are
	// stored rather than kept in memory so a restart does not re-run a purge
	// that has just happened.
	CategoriesSweptAt time.Time
	TrashSweptAt      time.Time
}

// DefaultRetentionSettings is what a reader who has never saved a policy has.
// The Trash empties itself; no category deletes anything.
func DefaultRetentionSettings(userID int64) RetentionSettings {
	return RetentionSettings{
		UserID:       userID,
		TrashEnabled: true,
		TrashDays:    DefaultTrashRetentionDays,
		Categories:   []CategoryRetention{},
	}
}

// TrashCutoff resolves the Trash rule against a moment: mail that reached the
// Trash before it has been there long enough to be deleted for good.
func (s RetentionSettings) TrashCutoff(now time.Time) (time.Time, bool) {
	if !s.TrashEnabled || s.TrashDays <= 0 {
		return time.Time{}, false
	}
	return now.UTC().AddDate(0, 0, -s.TrashDays), true
}

// CategoryRule returns the rule for one category, if it has one.
func (s RetentionSettings) CategoryRule(category string) (CategoryRetention, bool) {
	for _, rule := range s.Categories {
		if rule.Category == category {
			return rule, true
		}
	}
	return CategoryRetention{}, false
}

// GetRetentionSettings loads one reader's policy, answering with the defaults
// before they have saved one.
func (s *Store) GetRetentionSettings(ctx context.Context, userID int64) (RetentionSettings, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return RetentionSettings{}, err
	}
	settings := DefaultRetentionSettings(userID)
	var enabled, days, categoriesSweptAt, trashSweptAt int64
	err = db.QueryRowContext(ctx, `SELECT trash_enabled, trash_days, categories_swept_at, trash_swept_at
		FROM retention_settings WHERE user_id = ?`, userID).
		Scan(&enabled, &days, &categoriesSweptAt, &trashSweptAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return RetentionSettings{}, err
	}
	if err == nil {
		settings.TrashEnabled = enabled != 0
		settings.TrashDays = int(days)
		settings.CategoriesSweptAt = unixTime(categoriesSweptAt)
		settings.TrashSweptAt = unixTime(trashSweptAt)
	}
	rules, err := s.categoryRetentionRules(ctx, db, userID)
	if err != nil {
		return RetentionSettings{}, err
	}
	settings.Categories = rules
	return settings, nil
}

// categoryRetentionRules reads the stored rules in registry order, dropping any
// row naming a category this build no longer has. A stored name that is not in
// the registry is not an error: the registry is the one place a category is
// defined, and a row it does not recognise simply does not apply.
func (s *Store) categoryRetentionRules(ctx context.Context, db *sql.DB, userID int64) ([]CategoryRetention, error) {
	rows, err := db.QueryContext(ctx, `SELECT category, mode, cutoff_count, cutoff_unit, before_unix
		FROM category_retention_rules WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byCategory := map[string]CategoryRetention{}
	for rows.Next() {
		var rule CategoryRetention
		var count, beforeUnix int64
		if err := rows.Scan(&rule.Category, &rule.Mode, &count, &rule.Unit, &beforeUnix); err != nil {
			return nil, err
		}
		if !mailparse.ValidCategory(rule.Category) || rule.Mode == RetentionModeOff {
			continue
		}
		rule.Count = int(count)
		if rule.Mode != RetentionModeRelative {
			// The column carries a default for every row; only a relative rule
			// counts in anything, so the others read back with no unit at all
			// rather than with one that means nothing.
			rule.Count, rule.Unit = 0, ""
		}
		if beforeUnix > 0 {
			rule.Before = unixTime(beforeUnix)
		}
		if _, ok := rule.Cutoff(time.Now().UTC()); !ok {
			continue
		}
		byCategory[rule.Category] = rule
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]CategoryRetention, 0, len(byCategory))
	for _, category := range mailparse.CategoryRegistry() {
		if rule, ok := byCategory[category.Name]; ok {
			out = append(out, rule)
		}
	}
	return out, nil
}

// SaveRetentionSettings validates and atomically replaces one reader's policy.
// Saving clears the sweep marks, so a rule takes effect on the next pass rather
// than waiting out the interval of the pass that ran before it existed.
func (s *Store) SaveRetentionSettings(ctx context.Context, settings RetentionSettings) (RetentionSettings, error) {
	if settings.UserID <= 0 {
		return RetentionSettings{}, fmt.Errorf("%w: user is required", ErrInvalidRetentionSettings)
	}
	if settings.TrashEnabled && (settings.TrashDays <= 0 || settings.TrashDays > maxRetentionDays) {
		return RetentionSettings{}, fmt.Errorf("%w: the Trash retention must be between 1 and %d days",
			ErrInvalidRetentionSettings, maxRetentionDays)
	}
	rules, err := validateCategoryRetention(settings.Categories)
	if err != nil {
		return RetentionSettings{}, err
	}
	db := s.mustDataDB(ctx, settings.UserID)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return RetentionSettings{}, err
	}
	defer tx.Rollback()
	ts := nowUnix()
	trashEnabled := int64(0)
	if settings.TrashEnabled {
		trashEnabled = 1
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO retention_settings
			(user_id, trash_enabled, trash_days, categories_swept_at, trash_swept_at, created_at, updated_at)
		VALUES (?, ?, ?, 0, 0, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			trash_enabled = excluded.trash_enabled,
			trash_days = excluded.trash_days,
			categories_swept_at = 0,
			trash_swept_at = 0,
			updated_at = excluded.updated_at
		WHERE retention_settings.user_id = ?`,
		settings.UserID, trashEnabled, settings.TrashDays, ts, ts, settings.UserID); err != nil {
		return RetentionSettings{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM category_retention_rules WHERE user_id = ?`, settings.UserID); err != nil {
		return RetentionSettings{}, err
	}
	for _, rule := range rules {
		beforeUnix := int64(0)
		if !rule.Before.IsZero() {
			beforeUnix = rule.Before.UTC().Unix()
		}
		unit := rule.Unit
		if unit == "" {
			unit = RetentionUnitDays
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO category_retention_rules
				(user_id, category, mode, cutoff_count, cutoff_unit, before_unix, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			settings.UserID, rule.Category, rule.Mode, rule.Count, unit, beforeUnix, ts, ts); err != nil {
			return RetentionSettings{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return RetentionSettings{}, err
	}
	return s.GetRetentionSettings(ctx, settings.UserID)
}

// validateCategoryRetention normalizes the submitted rules, refusing a rule
// that names no category this build has or that resolves to no cutoff at all.
// Rules that are simply off are dropped rather than stored: the absence of a
// row is what says a category deletes nothing.
func validateCategoryRetention(rules []CategoryRetention) ([]CategoryRetention, error) {
	byCategory := map[string]CategoryRetention{}
	for _, rule := range rules {
		category := strings.ToLower(strings.TrimSpace(rule.Category))
		if !mailparse.ValidCategory(category) {
			return nil, fmt.Errorf("%w: unknown category %q", ErrInvalidRetentionSettings, rule.Category)
		}
		mode := strings.ToLower(strings.TrimSpace(rule.Mode))
		if _, taken := byCategory[category]; taken {
			return nil, fmt.Errorf("%w: %s was given two rules", ErrInvalidRetentionSettings, category)
		}
		switch mode {
		case "", RetentionModeOff:
			continue
		case RetentionModeRelative:
			unit := strings.ToLower(strings.TrimSpace(rule.Unit))
			if unit == "" {
				unit = RetentionUnitDays
			}
			if !validRetentionUnit(unit) {
				return nil, fmt.Errorf("%w: %s is not counted in %q", ErrInvalidRetentionSettings, category, rule.Unit)
			}
			if rule.Count <= 0 || rule.Count > maxRetentionCount {
				return nil, fmt.Errorf("%w: %s must keep mail for between 1 and %d %s",
					ErrInvalidRetentionSettings, category, maxRetentionCount, unit)
			}
			byCategory[category] = CategoryRetention{Category: category, Mode: RetentionModeRelative, Count: rule.Count, Unit: unit}
		case RetentionModeFixed:
			if rule.Before.IsZero() {
				return nil, fmt.Errorf("%w: %s needs the date to delete before", ErrInvalidRetentionSettings, category)
			}
			byCategory[category] = CategoryRetention{Category: category, Mode: RetentionModeFixed, Before: rule.Before.UTC()}
		default:
			return nil, fmt.Errorf("%w: unsupported retention mode %q", ErrInvalidRetentionSettings, rule.Mode)
		}
	}
	out := make([]CategoryRetention, 0, len(byCategory))
	for _, category := range mailparse.CategoryRegistry() {
		if rule, ok := byCategory[category.Name]; ok {
			out = append(out, rule)
		}
	}
	return out, nil
}

// MarkRetentionSwept records when each half of a pass is treated as having last
// run. A zero time leaves that half's mark alone; a time in the past is how a
// pass that was cut short asks to be resumed sooner than the full interval.
//
// The mark is written whether the pass did anything or not, and whether it
// failed or not: a reader whose account errors on every sweep must be retried
// on the interval rather than in a loop.
func (s *Store) MarkRetentionSwept(ctx context.Context, userID int64, categoriesAt, trashAt time.Time) error {
	if userID <= 0 || (categoriesAt.IsZero() && trashAt.IsZero()) {
		return nil
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return err
	}
	categoriesUnix, trashUnix := int64(0), int64(0)
	if !categoriesAt.IsZero() {
		categoriesUnix = categoriesAt.UTC().Unix()
	}
	if !trashAt.IsZero() {
		trashUnix = trashAt.UTC().Unix()
	}
	// The row is written whole either way, so the half that is being left alone
	// has to re-state what it already holds rather than be left out.
	_, err = db.ExecContext(ctx, `INSERT INTO retention_settings
			(user_id, trash_enabled, trash_days, categories_swept_at, trash_swept_at, created_at, updated_at)
		VALUES (?, 1, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			categories_swept_at = CASE WHEN ? <> 0 THEN ? ELSE retention_settings.categories_swept_at END,
			trash_swept_at = CASE WHEN ? <> 0 THEN ? ELSE retention_settings.trash_swept_at END,
			updated_at = ?
		WHERE retention_settings.user_id = ?`,
		userID, DefaultTrashRetentionDays, categoriesUnix, trashUnix, nowUnix(), nowUnix(),
		categoriesUnix, categoriesUnix, trashUnix, trashUnix, nowUnix(), userID)
	return err
}

// TrashRetentionUID is one message the Trash has held long enough to delete for
// good, named the way the server names it.
type TrashRetentionUID struct {
	MessageID int64
	UID       uint32
}

// maxTrashRetentionUIDs bounds one pass over a Trash folder, so a folder with
// years of mail in it cannot pin an unbounded UID list in memory. What a pass
// leaves behind is taken by the next one.
const maxTrashRetentionUIDs = 20000

// ListTrashRetentionUIDs names the messages one Trash folder has held since
// before the cutoff.
//
// The clock is `created_at`, which for a row in a Trash folder is when this
// mirror first stored the message *there*: a move is an IMAP MOVE followed by
// the source row being reconciled away and a new row being created in the
// destination, so the row in Trash is as old as the message's stay in Trash and
// not as old as the message. That is the clock the reader means -- mail sits in
// the Trash for so many days and is then gone -- and it is the only one either
// side has: the message's own date is the date it was *sent*, so counting from
// it would delete a year-old newsletter the moment a retention rule threw it
// away, and IMAP's INTERNALDATE is preserved across a MOVE by every server that
// implements it, so it says the same thing.
//
// Only mirrored mail is named. A Trash folder holding messages this install has
// never seen -- an account with a sync start date, a folder still syncing -- is
// holding mail whose stay nothing here can measure, and mail whose age is
// unknown is not mail to delete on a schedule. Emptying the Trash by hand still
// empties all of it.
func (s *Store) ListTrashRetentionUIDs(ctx context.Context, userID, mailboxID int64, before time.Time, uidValidity uint32) ([]TrashRetentionUID, error) {
	if before.IsZero() {
		return nil, nil
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT m.id, m.uid
		FROM messages m
		WHERE m.user_id = ? AND m.mailbox_id = ? AND m.created_at < ? AND m.uid > 0 AND m.uid_validity = ?
		ORDER BY m.uid
		LIMIT ?`, userID, mailboxID, before.UTC().Unix(), int64(uidValidity), maxTrashRetentionUIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TrashRetentionUID, 0, 256)
	for rows.Next() {
		var item TrashRetentionUID
		var uid int64
		if err := rows.Scan(&item.MessageID, &uid); err != nil {
			return nil, err
		}
		if uid <= 0 {
			continue
		}
		item.UID = uint32(uid)
		out = append(out, item)
	}
	return out, rows.Err()
}

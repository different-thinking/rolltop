// File overview: Tenant-scoped persistence and validation for Android message swipe behavior.

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	SwipeActionTrash      = "trash"
	SwipeActionArchive    = "archive"
	SwipeActionSnooze     = "snooze"
	SwipeActionMarkRead   = "mark_read"
	SwipeActionMarkUnread = "mark_unread"

	SwipeSnoozeLaterToday = "later_today"
	SwipeSnoozeTomorrow   = "tomorrow"
	SwipeSnoozeNextWeek   = "next_week"
)

// SwipeArchiveMailbox selects one archive destination for an IMAP account.
// Move operations cannot cross accounts, so archive targets are account-specific.
type SwipeArchiveMailbox struct {
	AccountID int64
	MailboxID int64
}

// SentMailbox names the folder one account's sent mail lands in, backing the
// global Sent view. It is an override rather than the only source: an account
// without a row falls back to the folder sync gave the sent role.
type SentMailbox struct {
	AccountID int64
	MailboxID int64
}

// accountMailboxChoice is the shared shape behind both per-account folder
// choices, so ownership and uniqueness are validated in one place.
type accountMailboxChoice struct {
	AccountID int64
	MailboxID int64
}

// SwipePreferences carries the per-account folder choices the message actions
// depend on alongside the swipe directions themselves. Archive destinations
// already back the pointer row action as well as the swipe, and the Sent
// choices back the global Sent view, so this is the shared settings payload
// rather than strictly swipe-only state.
type SwipePreferences struct {
	UserID            int64
	LeftAction        string
	LeftSnoozePreset  string
	RightAction       string
	RightSnoozePreset string
	ArchiveMailboxes  []SwipeArchiveMailbox
	SentMailboxes     []SentMailbox
}

// DefaultSwipePreferences preserves the original Android swipe behavior where
// possible while making read-state actions explicit rather than toggles.
func DefaultSwipePreferences(userID int64) SwipePreferences {
	return SwipePreferences{
		UserID:            userID,
		LeftAction:        SwipeActionSnooze,
		LeftSnoozePreset:  SwipeSnoozeTomorrow,
		RightAction:       SwipeActionMarkRead,
		RightSnoozePreset: SwipeSnoozeTomorrow,
		ArchiveMailboxes:  []SwipeArchiveMailbox{},
		SentMailboxes:     []SentMailbox{},
	}
}

// archiveRoleAllowed keeps archive destinations on ordinary folders: Inbox,
// Sent, Drafts, Trash, and Junk all have their own meaning already.
func archiveRoleAllowed(role string) bool { return role == "" }

// sentRoleAllowed accepts the folder sync already recognized as Sent plus any
// ordinary folder, which is what an account with a differently named or
// non-standard sent folder needs.
func sentRoleAllowed(role string) bool { return role == "sent" || role == "" }

// GetSwipePreferences loads one user's swipe settings, returning stable defaults
// before that user has saved an explicit preference row.
func (s *Store) GetSwipePreferences(ctx context.Context, userID int64) (SwipePreferences, error) {
	prefs := DefaultSwipePreferences(userID)
	db := s.mustDataDB(ctx, userID)
	err := db.QueryRowContext(ctx, `SELECT left_action, left_snooze_preset, right_action, right_snooze_preset
		FROM swipe_preferences WHERE user_id = ?`, userID).
		Scan(&prefs.LeftAction, &prefs.LeftSnoozePreset, &prefs.RightAction, &prefs.RightSnoozePreset)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return SwipePreferences{}, err
	}
	archives, err := loadAccountMailboxChoices(ctx, db, userID, "swipe_archive_mailboxes", "mailboxes.role = ''")
	if err != nil {
		return SwipePreferences{}, err
	}
	prefs.ArchiveMailboxes = []SwipeArchiveMailbox{}
	for _, choice := range archives {
		prefs.ArchiveMailboxes = append(prefs.ArchiveMailboxes, SwipeArchiveMailbox(choice))
	}
	sent, err := loadAccountMailboxChoices(ctx, db, userID, "sent_mailboxes", "mailboxes.role IN ('sent', '')")
	if err != nil {
		return SwipePreferences{}, err
	}
	prefs.SentMailboxes = []SentMailbox{}
	for _, choice := range sent {
		prefs.SentMailboxes = append(prefs.SentMailboxes, SentMailbox(choice))
	}
	return prefs, nil
}

// loadAccountMailboxChoices reads one mapping table, dropping rows whose folder
// no longer qualifies. A folder that changed role since it was chosen stops
// being a valid destination rather than silently keeping its old meaning.
func loadAccountMailboxChoices(ctx context.Context, db *sql.DB, userID int64, table, roleFilter string) ([]accountMailboxChoice, error) {
	rows, err := db.QueryContext(ctx, `SELECT mappings.account_id, mappings.mailbox_id
		FROM `+table+` AS mappings
		JOIN mailboxes ON mailboxes.user_id = mappings.user_id
			AND mailboxes.account_id = mappings.account_id AND mailboxes.id = mappings.mailbox_id
		WHERE mappings.user_id = ? AND `+roleFilter+` ORDER BY mappings.account_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]accountMailboxChoice, 0, 4)
	for rows.Next() {
		var choice accountMailboxChoice
		if err := rows.Scan(&choice.AccountID, &choice.MailboxID); err != nil {
			return nil, err
		}
		out = append(out, choice)
	}
	return out, rows.Err()
}

// SaveSwipePreferences validates and atomically replaces one user's swipe
// settings and per-account archive destinations.
func (s *Store) SaveSwipePreferences(ctx context.Context, prefs SwipePreferences) (SwipePreferences, error) {
	prefs.LeftAction = strings.ToLower(strings.TrimSpace(prefs.LeftAction))
	prefs.LeftSnoozePreset = strings.ToLower(strings.TrimSpace(prefs.LeftSnoozePreset))
	prefs.RightAction = strings.ToLower(strings.TrimSpace(prefs.RightAction))
	prefs.RightSnoozePreset = strings.ToLower(strings.TrimSpace(prefs.RightSnoozePreset))
	if prefs.UserID <= 0 {
		return SwipePreferences{}, fmt.Errorf("%w: user is required", ErrInvalidSwipePreferences)
	}
	if !validSwipeAction(prefs.LeftAction) || !validSwipeAction(prefs.RightAction) {
		return SwipePreferences{}, fmt.Errorf("%w: unsupported swipe action", ErrInvalidSwipePreferences)
	}
	if !validSwipeSnoozePreset(prefs.LeftSnoozePreset) || !validSwipeSnoozePreset(prefs.RightSnoozePreset) {
		return SwipePreferences{}, fmt.Errorf("%w: unsupported snooze preset", ErrInvalidSwipePreferences)
	}

	archiveChoices := make([]accountMailboxChoice, 0, len(prefs.ArchiveMailboxes))
	for _, target := range prefs.ArchiveMailboxes {
		archiveChoices = append(archiveChoices, accountMailboxChoice(target))
	}
	targets, err := s.validateAccountMailboxChoices(ctx, prefs.UserID, archiveChoices, "archive", archiveRoleAllowed)
	if err != nil {
		return SwipePreferences{}, err
	}
	prefs.ArchiveMailboxes = make([]SwipeArchiveMailbox, 0, len(targets))
	for _, choice := range targets {
		prefs.ArchiveMailboxes = append(prefs.ArchiveMailboxes, SwipeArchiveMailbox(choice))
	}
	sentChoices := make([]accountMailboxChoice, 0, len(prefs.SentMailboxes))
	for _, target := range prefs.SentMailboxes {
		sentChoices = append(sentChoices, accountMailboxChoice(target))
	}
	sentTargets, err := s.validateAccountMailboxChoices(ctx, prefs.UserID, sentChoices, "sent", sentRoleAllowed)
	if err != nil {
		return SwipePreferences{}, err
	}
	prefs.SentMailboxes = make([]SentMailbox, 0, len(sentTargets))
	for _, choice := range sentTargets {
		prefs.SentMailboxes = append(prefs.SentMailboxes, SentMailbox(choice))
	}
	if prefs.LeftAction == SwipeActionArchive || prefs.RightAction == SwipeActionArchive {
		if err := s.validateSwipeArchiveCoverage(ctx, prefs.UserID, prefs.ArchiveMailboxes); err != nil {
			return SwipePreferences{}, err
		}
	}
	if prefs.LeftAction == SwipeActionTrash || prefs.RightAction == SwipeActionTrash {
		if err := s.validateSwipeTrashCoverage(ctx, prefs.UserID); err != nil {
			return SwipePreferences{}, err
		}
	}

	db := s.mustDataDB(ctx, prefs.UserID)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return SwipePreferences{}, err
	}
	ts := nowUnix()
	if _, err := tx.ExecContext(ctx, `INSERT INTO swipe_preferences
			(user_id, left_action, left_snooze_preset, right_action, right_snooze_preset, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			left_action = excluded.left_action,
			left_snooze_preset = excluded.left_snooze_preset,
			right_action = excluded.right_action,
			right_snooze_preset = excluded.right_snooze_preset,
			updated_at = excluded.updated_at
		WHERE swipe_preferences.user_id = ?`,
		prefs.UserID, prefs.LeftAction, prefs.LeftSnoozePreset, prefs.RightAction, prefs.RightSnoozePreset, ts, ts, prefs.UserID); err != nil {
		_ = tx.Rollback()
		return SwipePreferences{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM swipe_archive_mailboxes WHERE user_id = ?`, prefs.UserID); err != nil {
		_ = tx.Rollback()
		return SwipePreferences{}, err
	}
	for _, target := range targets {
		if _, err := tx.ExecContext(ctx, `INSERT INTO swipe_archive_mailboxes
				(user_id, account_id, mailbox_id, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)`, prefs.UserID, target.AccountID, target.MailboxID, ts, ts); err != nil {
			_ = tx.Rollback()
			return SwipePreferences{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sent_mailboxes WHERE user_id = ?`, prefs.UserID); err != nil {
		_ = tx.Rollback()
		return SwipePreferences{}, err
	}
	for _, target := range sentTargets {
		if _, err := tx.ExecContext(ctx, `INSERT INTO sent_mailboxes
				(user_id, account_id, mailbox_id, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)`, prefs.UserID, target.AccountID, target.MailboxID, ts, ts); err != nil {
			_ = tx.Rollback()
			return SwipePreferences{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return SwipePreferences{}, err
	}
	return s.GetSwipePreferences(ctx, prefs.UserID)
}

func validSwipeAction(action string) bool {
	switch action {
	case SwipeActionTrash, SwipeActionArchive, SwipeActionSnooze, SwipeActionMarkRead, SwipeActionMarkUnread:
		return true
	default:
		return false
	}
}

func validSwipeSnoozePreset(preset string) bool {
	switch preset {
	case SwipeSnoozeLaterToday, SwipeSnoozeTomorrow, SwipeSnoozeNextWeek:
		return true
	default:
		return false
	}
}

// validateAccountMailboxChoices ownership-checks one set of per-account folder
// choices and returns them deduplicated in account order. noun names the choice
// in validation errors and roleAllowed decides which folders may serve it.
func (s *Store) validateAccountMailboxChoices(ctx context.Context, userID int64, targets []accountMailboxChoice, noun string, roleAllowed func(string) bool) ([]accountMailboxChoice, error) {
	byAccount := make(map[int64]int64, len(targets))
	mailboxes := make(map[int64]int64, len(targets))
	for _, target := range targets {
		if target.AccountID <= 0 || target.MailboxID <= 0 {
			return nil, fmt.Errorf("%w: %s account and mailbox are required", ErrInvalidSwipePreferences, noun)
		}
		if existing, ok := byAccount[target.AccountID]; ok && existing != target.MailboxID {
			return nil, fmt.Errorf("%w: multiple %s folders selected for one account", ErrInvalidSwipePreferences, noun)
		}
		if existingAccount, ok := mailboxes[target.MailboxID]; ok && existingAccount != target.AccountID {
			return nil, fmt.Errorf("%w: %s folder cannot belong to multiple accounts", ErrInvalidSwipePreferences, noun)
		}
		account, err := s.GetMailAccountForUser(ctx, userID, target.AccountID)
		if err != nil {
			if IsNotFound(err) {
				return nil, fmt.Errorf("%w: %s account is not owned by user", ErrInvalidSwipePreferences, noun)
			}
			return nil, err
		}
		mailbox, err := s.GetMailboxForUser(ctx, userID, target.MailboxID)
		if err != nil {
			if IsNotFound(err) {
				return nil, fmt.Errorf("%w: %s mailbox is not owned by user", ErrInvalidSwipePreferences, noun)
			}
			return nil, err
		}
		if mailbox.AccountID != account.ID {
			return nil, fmt.Errorf("%w: %s mailbox belongs to another account", ErrInvalidSwipePreferences, noun)
		}
		if !roleAllowed(mailbox.Role) {
			return nil, fmt.Errorf("%w: %s destination cannot be this folder", ErrInvalidSwipePreferences, noun)
		}
		byAccount[target.AccountID] = target.MailboxID
		mailboxes[target.MailboxID] = target.AccountID
	}
	accountIDs := make([]int64, 0, len(byAccount))
	for accountID := range byAccount {
		accountIDs = append(accountIDs, accountID)
	}
	sort.Slice(accountIDs, func(i, j int) bool { return accountIDs[i] < accountIDs[j] })
	out := make([]accountMailboxChoice, 0, len(accountIDs))
	for _, accountID := range accountIDs {
		out = append(out, accountMailboxChoice{AccountID: accountID, MailboxID: byAccount[accountID]})
	}
	return out, nil
}

func (s *Store) validateSwipeArchiveCoverage(ctx context.Context, userID int64, targets []SwipeArchiveMailbox) error {
	accounts, err := s.ListMailAccountsForUser(ctx, userID)
	if err != nil {
		return err
	}
	if len(accounts) == 0 || len(targets) != len(accounts) {
		return fmt.Errorf("%w: archive action requires a destination folder for every account", ErrInvalidSwipePreferences)
	}
	byAccount := make(map[int64]bool, len(targets))
	for _, target := range targets {
		byAccount[target.AccountID] = true
	}
	for _, account := range accounts {
		if !byAccount[account.ID] {
			return fmt.Errorf("%w: archive action requires a destination folder for every account", ErrInvalidSwipePreferences)
		}
	}
	return nil
}

func (s *Store) validateSwipeTrashCoverage(ctx context.Context, userID int64) error {
	accounts, err := s.ListMailAccountsForUser(ctx, userID)
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		return fmt.Errorf("%w: trash action requires a Trash folder for every account", ErrInvalidSwipePreferences)
	}
	for _, account := range accounts {
		if _, err := s.GetMailboxByRoleForAccount(ctx, userID, account.ID, "trash"); err != nil {
			if IsNotFound(err) {
				return fmt.Errorf("%w: trash action requires a Trash folder for every account", ErrInvalidSwipePreferences)
			}
			return err
		}
	}
	return nil
}

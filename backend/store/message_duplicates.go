// File overview: Cross-account duplicate copy detection. An account that
// aggregates other mailboxes - Gmail fetching POP3 mail, or a provider-side
// forward - delivers the same message a second time, so the mirror holds one
// row per account for a single delivery. Detection decides which row is the
// original and points the others at it; every read path then filters on that
// pointer instead of guessing at display time.

package store

import (
	"context"
	"database/sql"
	"errors"
	"net/mail"
	"strings"
)

const (
	// duplicateScanGroupPage is how many Message-IDs one query pass resolves.
	// Paging by group rather than by row keeps a group's copies together, so a
	// page boundary can never split the evidence a decision is made from.
	duplicateScanGroupPage = 500
	// maxDuplicateScanGroups bounds one call. A tenant with more duplicate
	// groups than this resumes from the returned cursor instead of restarting,
	// so repeated passes actually finish the mailbox.
	maxDuplicateScanGroups = 50000
)

// duplicateHideableRole names the folders a copy may be hidden in. Sent and
// Drafts copies are the user's own writing rather than a second delivery, and a
// Trash copy is already on its way out; hiding either would be a surprise the
// user cannot undo from the list.
func duplicateHideableRole(role string) bool {
	switch normalizeMailboxRole(role) {
	case "sent", "drafts", "trash":
		return false
	}
	return true
}

// duplicateOriginalEligible reports whether a row can be the copy everything
// else hides behind. Being hideable is not enough: the row also has to sit in a
// folder the reader actually reads. Junk, Drafts, and Trash are forced out of
// All Mail, and a user can take any other folder out of it too, so a Spam-filed
// row standing in as the original would leave the message reachable only from
// that account's Spam list - which is exactly the "hid the only copy" failure
// this rule exists to prevent.
func duplicateOriginalEligible(item DuplicateCopy) bool {
	return item.ShowInAllMail && duplicateHideableRole(item.MailboxRole)
}

// DuplicateCopy is one message row participating in duplicate detection.
type DuplicateCopy struct {
	ID            int64
	AccountID     int64
	MailboxID     int64
	MailboxRole   string
	ShowInAllMail bool
	MessageID     string
	ToAddr        string
	CCAddr        string
	DuplicateOf   int64
}

// DuplicateScanStats reports what one detection pass changed.
type DuplicateScanStats struct {
	// Groups counts Message-IDs held by more than one account.
	Groups int
	// Hidden counts rows now pointing at an original.
	Hidden int
	// Revealed counts rows that stopped pointing at one.
	Revealed int
	// NextHeader is the cursor a follow-up call passes as after. It is empty
	// once the scan has seen every group.
	NextHeader string
	// Truncated reports that this call stopped early and NextHeader has more.
	Truncated bool
}

// RefreshDuplicateCopiesForUser rescans the Message-IDs held by more than one
// account and rewrites the duplicate pointers. It is the repair path for mail
// that was mirrored before detection existed, and for accounts that gained an
// alias after their mail arrived.
//
// after is the cursor from a previous call's NextHeader, or empty to start from
// the beginning. A call that stops on its group budget reports where to resume,
// so a mailbox larger than one pass is finished by repeating the call rather
// than by re-reading the same groups forever.
func (s *Store) RefreshDuplicateCopiesForUser(ctx context.Context, userID int64, after string) (DuplicateScanStats, error) {
	var stats DuplicateScanStats
	if userID <= 0 {
		return stats, nil
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return stats, err
	}
	addresses, err := s.accountAddressesForUser(ctx, userID)
	if err != nil {
		return stats, err
	}
	cursor := strings.TrimSpace(after)
	for stats.Groups < maxDuplicateScanGroups {
		headers, err := s.duplicateGroupHeaders(ctx, db, userID, cursor, duplicateScanGroupPage)
		if err != nil {
			return stats, err
		}
		if len(headers) == 0 {
			cursor = ""
			break
		}
		copies, err := s.duplicateCopiesForHeaders(ctx, db, userID, headers)
		if err != nil {
			return stats, err
		}
		updates := map[int64]int64{}
		forEachDuplicateGroup(copies, func(group []DuplicateCopy) {
			stats.Groups++
			for id, original := range resolveDuplicateGroup(group, addresses) {
				updates[id] = original
			}
		})
		for _, item := range copies {
			if _, planned := updates[item.ID]; planned {
				continue
			}
			// A row the current rule no longer hides has to be released
			// explicitly: leaving the old pointer in place would keep mail
			// invisible after an alias, a folder role, or the winning copy's
			// account changed.
			if item.DuplicateOf != 0 {
				updates[item.ID] = 0
			}
		}
		hidden, revealed, err := s.applyDuplicatePointers(ctx, db, userID, copies, updates)
		if err != nil {
			return stats, err
		}
		stats.Hidden += hidden
		stats.Revealed += revealed
		cursor = headers[len(headers)-1]
		if len(headers) < duplicateScanGroupPage {
			cursor = ""
			break
		}
	}
	stats.NextHeader = cursor
	stats.Truncated = cursor != ""
	return stats, nil
}

// duplicateGroupHeaders pages the Message-IDs that more than one account holds.
// Ordering by header makes the last one a usable resume cursor.
func (s *Store) duplicateGroupHeaders(ctx context.Context, db *sql.DB, userID int64, after string, limit int) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT message_id_header FROM messages
		WHERE user_id = ? AND message_id_header <> '' AND message_id_header > ?
		GROUP BY message_id_header
		HAVING COUNT(DISTINCT account_id) > 1
		ORDER BY message_id_header
		LIMIT ?`, userID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0, limit)
	for rows.Next() {
		var header string
		if err := rows.Scan(&header); err != nil {
			return nil, err
		}
		out = append(out, header)
	}
	return out, rows.Err()
}

// duplicateCopiesForHeaders loads every copy of the given Message-IDs, ordered
// so a caller can walk one group at a time.
func (s *Store) duplicateCopiesForHeaders(ctx context.Context, db *sql.DB, userID int64, headers []string) ([]DuplicateCopy, error) {
	if len(headers) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(headers)+1)
	args = append(args, userID)
	for _, header := range headers {
		args = append(args, header)
	}
	rows, err := db.QueryContext(ctx, duplicateCopyColumns+`
		WHERE m.user_id = ? AND m.message_id_header IN (`+sqlPlaceholders(len(headers))+`)
		ORDER BY m.message_id_header, m.id`, args...)
	if err != nil {
		return nil, err
	}
	copies, err := scanDuplicateCopies(rows)
	closeErr := rows.Close()
	if err != nil {
		return nil, err
	}
	return copies, closeErr
}

// duplicateCopyColumns is the envelope projection every detection query reads.
// Detection never loads bodies: it decides from recipients and folder placement.
const duplicateCopyColumns = `SELECT m.id, m.account_id, m.mailbox_id, mb.role, mb.show_in_all_mail,
		m.message_id_header, m.to_addr, m.cc_addr, m.duplicate_of_message_id
	FROM messages m
	JOIN mailboxes mb ON mb.id = m.mailbox_id AND mb.user_id = m.user_id`

// RefreshDuplicateCopiesForMessageID rescans the copies of one Message-ID. Sync
// calls it for each stored message, so a copy that arrives after its original is
// hidden on arrival rather than at the next full scan.
func (s *Store) RefreshDuplicateCopiesForMessageID(ctx context.Context, userID int64, messageIDHeader string) error {
	header := strings.TrimSpace(messageIDHeader)
	if userID <= 0 || header == "" {
		return nil
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return err
	}
	copies, err := s.duplicateCopiesForHeaders(ctx, db, userID, []string{header})
	if err != nil {
		return err
	}
	if len(copies) < 2 {
		return nil
	}
	addresses, err := s.accountAddressesForUser(ctx, userID)
	if err != nil {
		return err
	}
	// resolveDuplicateGroup returns nil for every group it declines to judge, so
	// the release pass below needs a map it can always write to.
	updates := map[int64]int64{}
	for id, original := range resolveDuplicateGroup(copies, addresses) {
		updates[id] = original
	}
	for _, item := range copies {
		if _, planned := updates[item.ID]; !planned && item.DuplicateOf != 0 {
			updates[item.ID] = 0
		}
	}
	_, _, err = s.applyDuplicatePointers(ctx, db, userID, copies, updates)
	return err
}

// applyDuplicatePointers writes only the rows whose pointer actually changes, so
// a repeated scan over settled data does no writes at all.
func (s *Store) applyDuplicatePointers(ctx context.Context, db *sql.DB, userID int64, copies []DuplicateCopy, updates map[int64]int64) (int, int, error) {
	current := make(map[int64]int64, len(copies))
	for _, item := range copies {
		current[item.ID] = item.DuplicateOf
	}
	pending := make([][2]int64, 0, len(updates))
	hidden, revealed := 0, 0
	for id, original := range updates {
		if current[id] == original {
			continue
		}
		pending = append(pending, [2]int64{id, original})
		if original == 0 {
			revealed++
			continue
		}
		hidden++
	}
	if len(pending) == 0 {
		return 0, 0, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `UPDATE messages SET duplicate_of_message_id = ?, updated_at = ?
		WHERE user_id = ? AND id = ?`)
	if err != nil {
		return 0, 0, err
	}
	defer stmt.Close()
	now := nowUnix()
	for _, update := range pending {
		if _, err := stmt.ExecContext(ctx, update[1], now, userID, update[0]); err != nil {
			return 0, 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return hidden, revealed, nil
}

// resolveDuplicateGroup decides which copies of one Message-ID stay visible. It
// returns the pointer each hidden copy should carry; copies missing from the
// result stay visible.
//
// The rule is deliberately narrow. A copy loses only when exactly one account in
// the group was actually addressed - its own address appears in To or Cc - which
// is what separates the original delivery from an aggregating account that
// merely fetched a copy of it. A message addressed to several of the user's
// accounts, or to none of them because it arrived via Bcc or a mailing list,
// keeps every copy visible: the mirror cannot tell which delivery is the real
// one, and showing mail twice is a smaller failure than hiding the only copy the
// user has.
func resolveDuplicateGroup(copies []DuplicateCopy, addresses map[int64]map[string]bool) map[int64]int64 {
	if len(copies) < 2 {
		return nil
	}
	accounts := map[int64]bool{}
	addressed := map[int64]bool{}
	for _, item := range copies {
		accounts[item.AccountID] = true
		if item.DuplicateOf == item.ID {
			// A row pointing at itself would hide itself forever. Treat the group
			// as unresolvable rather than trusting the stored pointer.
			return nil
		}
		// Only a row that could stand in as the original counts its account as
		// addressed. A message the addressed account holds solely in Spam or
		// Trash cannot cover for the copies, so its account does not get to
		// decide the group.
		if duplicateOriginalEligible(item) && messageAddressesAccount(item, addresses[item.AccountID]) {
			addressed[item.AccountID] = true
		}
	}
	if len(accounts) < 2 || len(addressed) != 1 {
		return nil
	}
	originalAccount := int64(0)
	for accountID := range addressed {
		originalAccount = accountID
	}
	original := DuplicateCopy{}
	for _, item := range copies {
		if item.AccountID != originalAccount || !duplicateOriginalEligible(item) {
			continue
		}
		if original.ID == 0 || preferredDuplicateOriginal(item, original) {
			original = item
		}
	}
	if original.ID == 0 {
		// The addressed account holds the message only where the reader would not
		// find it. Whatever the other accounts hold, hiding it behind that row
		// would take the message out of view entirely.
		return nil
	}
	out := map[int64]int64{}
	for _, item := range copies {
		if item.AccountID == originalAccount || !duplicateHideableRole(item.MailboxRole) {
			continue
		}
		out[item.ID] = original.ID
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// preferredDuplicateOriginal picks the copy that represents the delivery best.
// An Inbox copy outranks any other folder, and the lowest id breaks the tie so
// repeated scans settle on the same row.
func preferredDuplicateOriginal(candidate, current DuplicateCopy) bool {
	candidateInbox := normalizeMailboxRole(candidate.MailboxRole) == "inbox"
	currentInbox := normalizeMailboxRole(current.MailboxRole) == "inbox"
	if candidateInbox != currentInbox {
		return candidateInbox
	}
	return candidate.ID < current.ID
}

// messageAddressesAccount reports whether the copy was addressed to the account
// holding it. Bcc is invisible here by design: the header the sender wrote is
// the only recipient evidence a mirrored message carries.
func messageAddressesAccount(item DuplicateCopy, accountAddresses map[string]bool) bool {
	if len(accountAddresses) == 0 {
		return false
	}
	for _, field := range []string{item.ToAddr, item.CCAddr} {
		for _, identity := range recipientIdentities(field) {
			if accountAddresses[identity] {
				return true
			}
		}
	}
	return false
}

// recipientIdentities lowercases every address in one header field. A field that
// does not parse still yields its bare address form, which is what senders with
// unquoted display names produce.
func recipientIdentities(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if addrs, err := mail.ParseAddressList(value); err == nil {
		out := make([]string, 0, len(addrs))
		for _, addr := range addrs {
			if identity := SenderIdentity(addr.Address); identity != "" {
				out = append(out, identity)
			}
		}
		return out
	}
	out := make([]string, 0, 4)
	for _, part := range strings.Split(value, ",") {
		if identity := SenderIdentity(part); identity != "" {
			out = append(out, identity)
		}
	}
	return out
}

// accountAddressesForUser maps each mail account to the addresses that count as
// its own: the account login address plus every identity bound to it.
func (s *Store) accountAddressesForUser(ctx context.Context, userID int64) (map[int64]map[string]bool, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := map[int64]map[string]bool{}
	add := func(accountID int64, email string) {
		identity := SenderIdentity(email)
		if accountID <= 0 || identity == "" {
			return
		}
		if out[accountID] == nil {
			out[accountID] = map[string]bool{}
		}
		out[accountID][identity] = true
	}
	rows, err := db.QueryContext(ctx, `SELECT id, email FROM mail_accounts WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var accountID int64
		var email string
		if err := rows.Scan(&accountID, &email); err != nil {
			rows.Close()
			return nil, err
		}
		add(accountID, email)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	identityRows, err := db.QueryContext(ctx, `SELECT imap_account_id, email FROM mail_identities
		WHERE user_id = ? AND imap_account_id <> 0`, userID)
	if err != nil {
		return nil, err
	}
	defer identityRows.Close()
	for identityRows.Next() {
		var accountID int64
		var email string
		if err := identityRows.Scan(&accountID, &email); err != nil {
			return nil, err
		}
		add(accountID, email)
	}
	return out, identityRows.Err()
}

// CountHiddenDuplicateCopiesForUser reports how many rows are currently hidden
// behind an original, grouped by the account holding the hidden copy.
func (s *Store) CountHiddenDuplicateCopiesForUser(ctx context.Context, userID int64) (map[int64]int, error) {
	if userID <= 0 {
		return nil, nil
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT account_id, COUNT(*) FROM messages
		WHERE user_id = ? AND duplicate_of_message_id <> 0
		GROUP BY account_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var accountID int64
		var count int
		if err := rows.Scan(&accountID, &count); err != nil {
			return nil, err
		}
		out[accountID] = count
	}
	return out, rows.Err()
}

// DuplicateCopyIDsForUser reports which of the given rows are currently hidden
// behind an original. Callers that hydrate messages by id use it to apply the
// same exclusion the list queries apply in SQL.
func (s *Store) DuplicateCopyIDsForUser(ctx context.Context, userID int64, ids []int64) (map[int64]bool, error) {
	unique := uniquePositiveIDs(ids, maxMessageSimilarityCandidates)
	if userID <= 0 || len(unique) == 0 {
		return nil, nil
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := map[int64]bool{}
	for start := 0; start < len(unique); start += 500 {
		end := start + 500
		if end > len(unique) {
			end = len(unique)
		}
		chunk := unique[start:end]
		args := make([]any, 0, len(chunk)+1)
		args = append(args, userID)
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := db.QueryContext(ctx, `SELECT id FROM messages
			WHERE user_id = ? AND duplicate_of_message_id <> 0
				AND id IN (`+sqlPlaceholders(len(chunk))+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ListHiddenDuplicateCopiesForUser resolves the hidden copies into the selection
// shape the move machinery consumes.
func (s *Store) ListHiddenDuplicateCopiesForUser(ctx context.Context, userID int64, limit int) ([]ScopeMessage, error) {
	if userID <= 0 {
		return nil, nil
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT id, account_id, mailbox_id FROM messages
		WHERE user_id = ? AND duplicate_of_message_id <> 0
		ORDER BY date_unix DESC, id DESC
		LIMIT ?`, userID, scopeMessageLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanScopeMessages(rows)
}

// messageIsDuplicateCopyTx reports whether one row is currently hidden behind an
// original. A row that has since been deleted answers false, which keeps the
// caller on its normal path instead of failing an arrival over a missing row.
func messageIsDuplicateCopyTx(ctx context.Context, tx *sql.Tx, userID, messageID int64) (bool, error) {
	var duplicateOf int64
	err := tx.QueryRowContext(ctx, `SELECT duplicate_of_message_id FROM messages
		WHERE user_id = ? AND id = ?`, userID, messageID).Scan(&duplicateOf)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return duplicateOf != 0, nil
}

func scanDuplicateCopies(rows *sql.Rows) ([]DuplicateCopy, error) {
	out := make([]DuplicateCopy, 0, 16)
	for rows.Next() {
		var item DuplicateCopy
		if err := rows.Scan(&item.ID, &item.AccountID, &item.MailboxID, &item.MailboxRole,
			&item.ShowInAllMail, &item.MessageID, &item.ToAddr, &item.CCAddr, &item.DuplicateOf); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// forEachDuplicateGroup walks Message-ID runs in a scan ordered by header.
func forEachDuplicateGroup(copies []DuplicateCopy, fn func([]DuplicateCopy)) {
	start := 0
	for i := 1; i <= len(copies); i++ {
		if i < len(copies) && copies[i].MessageID == copies[start].MessageID {
			continue
		}
		fn(copies[start:i])
		start = i
	}
}

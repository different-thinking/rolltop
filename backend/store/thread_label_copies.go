// File overview: Collapsing the second copy a mirrored label view puts in a
// thread. Gmail's All Mail is a view over messages that already live in a real
// folder, so an account mirroring it holds two rows per delivery and the thread
// rendered both - the same mail, twice, one above the other.
//
// Cross-account detection in message_duplicates.go deliberately never touches
// two rows of one account, and that stays true: nothing here writes a duplicate
// pointer, hides a row from a folder list, or changes what the account is
// reported to hold. This is a display decision made while a thread is read, and
// it is made only where one of the copies is a view - two real folders of one
// account are two places the user filed the message, and both keep their row.

package store

import (
	"context"
	"database/sql"
	"strings"
)

// threadCopyLookupBatch caps one IN list of mailbox ids, keeping the statement
// inside the driver's parameter limit for a thread that spans many folders.
const threadCopyLookupBatch = 500

// threadCopyKey groups the rows that are one delivery to one account. The
// account is part of the key because a message two accounts received is two
// deliveries, which is the question message_duplicates.go answers and this must
// not pre-empt.
type threadCopyKey struct {
	account int64
	header  string
}

// threadCopyGroups indexes the rows sharing an account and a Message-ID, and
// keeps only the groups holding more than one. A row without a Message-ID is
// left out entirely: there is no evidence it is a second copy of anything, and
// grouping those together would merge unrelated mail.
func threadCopyGroups(messages []MessageRecord) map[threadCopyKey][]int {
	groups := map[threadCopyKey][]int{}
	for i, msg := range messages {
		header := strings.TrimSpace(msg.MessageIDHeader)
		if header == "" {
			continue
		}
		key := threadCopyKey{account: msg.AccountID, header: header}
		groups[key] = append(groups[key], i)
	}
	for key, group := range groups {
		if len(group) < 2 {
			delete(groups, key)
		}
	}
	return groups
}

// labelViewMailboxIDs reports which of the given folders are views over mail
// stored elsewhere. It reads name and role because that is what
// isLabelViewMailbox decides from once the SPECIAL-USE attributes the folder
// was discovered with are no longer at hand.
func (s *Store) labelViewMailboxIDs(ctx context.Context, db *sql.DB, userID int64, mailboxIDs []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	for start := 0; start < len(mailboxIDs); start += threadCopyLookupBatch {
		end := min(start+threadCopyLookupBatch, len(mailboxIDs))
		chunk := mailboxIDs[start:end]
		args := make([]any, 0, len(chunk)+1)
		args = append(args, userID)
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := db.QueryContext(ctx, `SELECT id, name, role FROM mailboxes
			WHERE user_id = ? AND id IN (`+sqlPlaceholders(len(chunk))+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var name, role string
			if err := rows.Scan(&id, &name, &role); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if isLabelViewMailbox(name, role, nil) {
				out[id] = true
			}
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	return out, nil
}

// threadCopyMailboxIDs lists the distinct folders the grouped rows sit in, so
// the folder lookup asks only about rows a decision is actually made from.
func threadCopyMailboxIDs(messages []MessageRecord, groups map[threadCopyKey][]int) []int64 {
	seen := map[int64]bool{}
	out := make([]int64, 0, len(groups))
	for _, group := range groups {
		for _, i := range group {
			id := messages[i].MailboxID
			if id == 0 || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// labelViewCopiesInThread loads the label-view folders a thread's repeated
// Message-IDs sit in. It returns nil without querying when the thread holds no
// Message-ID twice, so the ordinary thread - which is every thread on an account
// that mirrors no label view - costs nothing at all.
func (s *Store) labelViewCopiesInThread(ctx context.Context, userID int64, messages []MessageRecord) (map[int64]bool, error) {
	mailboxIDs := threadCopyMailboxIDs(messages, threadCopyGroups(messages))
	if len(mailboxIDs) == 0 {
		return nil, nil
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	return s.labelViewMailboxIDs(ctx, db, userID, mailboxIDs)
}

// collapsedThreadCopyWinner picks the row of one group the thread keeps, or -1
// when the group is left alone.
//
// selected is the row the reader opened. It wins whatever folder it sits in,
// because the thread it opens has to contain it: the message view reads its own
// row back out of the rendered thread, and a thread that dropped it would answer
// with a message nothing in the thread matches. The lists reach a label-view
// copy often enough for this to be the common case rather than a corner - the
// row a list selects for a conversation is its newest by date and id, and a
// label view mirrored after the folder it duplicates holds the higher id.
func collapsedThreadCopyWinner(messages []MessageRecord, group []int, labelView map[int64]bool, selected int64) int {
	views := 0
	for _, i := range group {
		if labelView[messages[i].MailboxID] {
			views++
		}
	}
	// No view in the group means the copies are folders the user filed the
	// message in, which are theirs to see. Only views means the message is
	// nowhere else, and hiding the last copy of a message is never right.
	if views == 0 || views == len(group) {
		return -1
	}
	for _, i := range group {
		if messages[i].ID == selected {
			return i
		}
	}
	// The real folders are already in the order the thread query returned -
	// date, then id - so the first of them is the copy that was mirrored first,
	// which is the folder the message was delivered to rather than the view
	// that picked it up afterwards.
	for _, i := range group {
		if !labelView[messages[i].MailboxID] {
			return i
		}
	}
	return -1
}

// collapseLabelViewCopies removes the copies of a message the thread also holds
// outside a label view, keeping the rest of the thread in its original order.
func collapseLabelViewCopies(messages []MessageRecord, labelView map[int64]bool, selected int64) []MessageRecord {
	if len(labelView) == 0 || len(messages) < 2 {
		return messages
	}
	drop := map[int]bool{}
	for _, group := range threadCopyGroups(messages) {
		winner := collapsedThreadCopyWinner(messages, group, labelView, selected)
		if winner < 0 {
			continue
		}
		for _, i := range group {
			if i != winner {
				drop[i] = true
			}
		}
	}
	if len(drop) == 0 {
		return messages
	}
	out := make([]MessageRecord, 0, len(messages)-len(drop))
	for i, msg := range messages {
		if !drop[i] {
			out = append(out, msg)
		}
	}
	return out
}

// File overview: Collapsing the second copy a mirrored label view puts in a
// thread. Gmail's All Mail is a view over messages that already live in a real
// folder, so an account mirroring it holds two rows per delivery and a rendered
// thread showed both - the same mail, twice, one above the other.
//
// This is a rendering decision and nothing else. The thread queries keep
// returning every row, because read state, star state, mailbox filters and
// conversation-level moves all have to reach every physical copy; only the view
// that draws the thread collapses, and it is handed the ids it hid so the
// actions taken on a drawn message still reach the rows behind it.
//
// Cross-account detection in message_duplicates.go is not pre-empted either:
// copies two accounts hold are two deliveries and are left to it, and two real
// folders of one account are two places the reader filed the message and keep
// both rows.

package store

import (
	"context"
	"database/sql"
	"strings"
)

// threadCopyLookupBatch caps one IN list of mailbox ids, keeping the statement
// inside the driver's parameter limit for a thread that spans many folders.
const threadCopyLookupBatch = 500

// ThreadCopyCollapse is what a thread renders and what it hid to get there.
type ThreadCopyCollapse struct {
	// Messages is the thread to draw. A row that stood in for others carries
	// their read and star state merged in, so the drawn thread agrees with the
	// conversation row the list printed from the same copies.
	Messages []MessageRecord
	// CopyIDs maps a drawn row to every physical row it stands for, itself
	// included, and holds an entry only where that is more than one. Marking a
	// drawn message read or unread has to reach all of them: the rows are one
	// Gmail message, and leaving one behind leaves the conversation unread in
	// the list after the reader has read it.
	CopyIDs map[int64][]int64
	// StandIn maps a hidden row to the row drawn in its place. The message view
	// opens a row by id and reads it back out of the thread it rendered, so a
	// caller whose row was hidden follows this to the one that replaced it.
	StandIn map[int64]int64
}

// threadCopyKey groups the rows that are one delivery to one account. The
// account is part of the key because a message two accounts received is two
// deliveries, which is the question message_duplicates.go answers and this must
// not answer first.
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

// labelViewMailboxIDs reports which of the given folders are views over mail
// stored elsewhere. It reads name and role because that is what
// isLabelViewMailbox decides from once the SPECIAL-USE attributes the folder was
// discovered with are no longer at hand.
//
// That covers the folder this exists for: Gmail advertises \All on All Mail in
// every language and the syncer stores it as a role. \Flagged and \Important
// have no role to be stored as, so Starred and Important are recognized only
// under their English names - a localized account mirroring one of those still
// sees it repeated. Storing the attributes would close that, and is a schema
// change rather than a read-time one.
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

// CollapseLabelViewCopies decides what a thread draws for the rows it was given.
// A thread holding no Message-ID twice - which is every thread on an account
// that mirrors no label view - is returned as it came, without a query.
func (s *Store) CollapseLabelViewCopies(ctx context.Context, userID int64, messages []MessageRecord) (ThreadCopyCollapse, error) {
	collapse := ThreadCopyCollapse{Messages: messages}
	groups := threadCopyGroups(messages)
	mailboxIDs := threadCopyMailboxIDs(messages, groups)
	if len(mailboxIDs) == 0 {
		return collapse, nil
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return collapse, err
	}
	labelView, err := s.labelViewMailboxIDs(ctx, db, userID, mailboxIDs)
	if err != nil {
		return collapse, err
	}
	return collapseLabelViewCopies(messages, groups, labelView), nil
}

// hiddenThreadCopies names, for one group, the row each hidden row is drawn as.
// The rule is the narrow one: a copy is hidden only because it sits in a view
// over a copy the thread already draws.
//
// Real folders always survive - two of them are two places the reader filed the
// message, and the thread has no business deciding one of them away. A group
// that is nothing but views still draws one of them, because such a message
// lives nowhere else: archived Gmail mail carries no label at all, and hiding
// the last copy of a message is never right. Which one it draws is the first,
// and the rows arrive ordered by date and id, so the choice does not move
// between two renders of the same thread.
func hiddenThreadCopies(messages []MessageRecord, group []int, labelView map[int64]bool) (drawn int, hidden []int) {
	drawn = -1
	for _, i := range group {
		if !labelView[messages[i].MailboxID] {
			if drawn < 0 {
				drawn = i
			}
			continue
		}
		hidden = append(hidden, i)
	}
	if len(hidden) == 0 {
		return -1, nil
	}
	if drawn < 0 {
		// Only views. The first of them is drawn and stops being hidden.
		drawn = hidden[0]
		hidden = hidden[1:]
		if len(hidden) == 0 {
			return -1, nil
		}
	}
	return drawn, hidden
}

// collapseLabelViewCopies applies hiddenThreadCopies to every group and rebuilds
// the thread around what is left.
func collapseLabelViewCopies(messages []MessageRecord, groups map[threadCopyKey][]int, labelView map[int64]bool) ThreadCopyCollapse {
	collapse := ThreadCopyCollapse{Messages: messages}
	if len(labelView) == 0 || len(groups) == 0 {
		return collapse
	}
	drop := map[int]bool{}
	copies := map[int64][]int64{}
	standIn := map[int64]int64{}
	merged := map[int]MessageRecord{}
	for _, group := range groups {
		drawn, hidden := hiddenThreadCopies(messages, group, labelView)
		if drawn < 0 {
			continue
		}
		keep := messages[drawn]
		ids := []int64{keep.ID}
		for _, i := range hidden {
			drop[i] = true
			standIn[messages[i].ID] = keep.ID
			ids = append(ids, messages[i].ID)
			// The list ANDs read state and ORs stars across the same copies
			// (dedupeConversationMessages), so a thread that merged them any
			// other way would disagree with the row the reader clicked.
			keep.IsRead = keep.IsRead && messages[i].IsRead
			keep.IsStarred = keep.IsStarred || messages[i].IsStarred
			keep.HasAttachments = keep.HasAttachments || messages[i].HasAttachments
		}
		merged[drawn] = keep
		copies[keep.ID] = ids
	}
	if len(drop) == 0 {
		return collapse
	}
	out := make([]MessageRecord, 0, len(messages)-len(drop))
	for i, msg := range messages {
		if drop[i] {
			continue
		}
		if replacement, ok := merged[i]; ok {
			msg = replacement
		}
		out = append(out, msg)
	}
	collapse.Messages = out
	collapse.CopyIDs = copies
	collapse.StandIn = standIn
	return collapse
}

// LabelViewCopyIDsForMessage lists the rows a label view holds of the same
// message, for an account that mirrors one. A flag the reader sets on a drawn
// message has to reach them: \Seen and \Flagged belong to the Gmail message
// rather than to one of its labels, so a star cleared on one row and left on
// another is a star the list keeps showing and the reader cannot remove.
func (s *Store) LabelViewCopyIDsForMessage(ctx context.Context, userID, messageID int64) ([]int64, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT other.id, other.mailbox_id
		FROM messages self
		JOIN messages other ON other.user_id = self.user_id
			AND other.account_id = self.account_id
			AND other.message_id_header = self.message_id_header
			AND other.id <> self.id
		WHERE self.user_id = ? AND self.id = ? AND self.message_id_header <> ''`, userID, messageID)
	if err != nil {
		return nil, err
	}
	type candidate struct{ id, mailbox int64 }
	candidates := make([]candidate, 0, 2)
	mailboxIDs := make([]int64, 0, 2)
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.id, &item.mailbox); err != nil {
			_ = rows.Close()
			return nil, err
		}
		candidates = append(candidates, item)
		mailboxIDs = append(mailboxIDs, item.mailbox)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	labelView, err := s.labelViewMailboxIDs(ctx, db, userID, mailboxIDs)
	if err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(candidates))
	for _, item := range candidates {
		if labelView[item.mailbox] {
			out = append(out, item.id)
		}
	}
	return out, nil
}

// File overview: Search query filter parsing for dates, mailboxes, language, and flags.

package web

import (
	"context"
	"regexp"
	"strings"

	"rolltop/backend/store"
)

var inSearchOperatorRE = regexp.MustCompile(`(?i)(^|\s)in:("[^"]+"|\S+)`)

// searchMailboxFilter is either an allow list built from in: operators or, when
// the query names no folder, the default view: everything except deleted mail.
type searchMailboxFilter struct {
	enabled  bool
	ids      map[int64]bool
	excluded map[int64]bool
}

func (f searchMailboxFilter) matches(msg store.MessageRecord) bool {
	if f.enabled {
		return f.ids[msg.MailboxID]
	}
	return !f.excluded[msg.MailboxID]
}

// searchMailboxFilter resolves in: operators into an allow list. Without one,
// Trash is left out the same way All Mail leaves it out: mail the user deleted
// should not come back as a result, and `in:trash` is how they ask for it.
func (s *Server) searchMailboxFilter(ctx context.Context, userID int64, query string) (string, searchMailboxFilter, error) {
	cleaned, names := stripInSearchOperators(query)
	if len(names) == 0 {
		// Only the role lookup, not the full folder summary: this runs on every
		// search, while the allow-list path below needs names and stays rarer.
		trashIDs, err := s.store.ListMailboxIDsWithRoleForUser(ctx, userID, "trash")
		if err != nil {
			return "", searchMailboxFilter{}, err
		}
		excluded := make(map[int64]bool, len(trashIDs))
		for _, id := range trashIDs {
			excluded[id] = true
		}
		return query, searchMailboxFilter{excluded: excluded}, nil
	}
	mailboxes, err := s.store.ListMailboxesForUser(ctx, userID)
	if err != nil {
		return "", searchMailboxFilter{}, err
	}
	ids := map[int64]bool{}
	for _, name := range names {
		for _, id := range matchingSearchMailboxIDs(mailboxes, name) {
			ids[id] = true
		}
	}
	return cleaned, searchMailboxFilter{enabled: true, ids: ids}, nil
}

func stripInSearchOperators(query string) (string, []string) {
	matches := inSearchOperatorRE.FindAllStringSubmatchIndex(query, -1)
	if len(matches) == 0 {
		return query, nil
	}
	var cleaned strings.Builder
	names := make([]string, 0, len(matches))
	last := 0
	for _, m := range matches {
		start, end := m[0], m[1]
		if start > last {
			cleaned.WriteString(query[last:start])
		}
		if len(m) >= 6 && m[4] >= 0 {
			names = append(names, strings.Trim(strings.TrimSpace(query[m[4]:m[5]]), `"`))
		}
		last = end
	}
	if last < len(query) {
		cleaned.WriteString(query[last:])
	}
	return strings.Join(strings.Fields(cleaned.String()), " "), names
}

func matchingSearchMailboxIDs(mailboxes []store.MailboxSummary, raw string) []int64 {
	needle := normalizeSearchMailboxName(raw)
	if needle == "" {
		return nil
	}
	var exact []int64
	var partial []int64
	for _, mailbox := range mailboxes {
		name := normalizeSearchMailboxName(mailbox.Name)
		leaf := normalizeSearchMailboxName(searchMailboxLeaf(mailbox.Name))
		role := normalizeSearchMailboxName(mailbox.Role)
		switch needle {
		case name, leaf, role:
			exact = append(exact, mailbox.ID)
			continue
		}
		if strings.Contains(name, needle) || strings.Contains(leaf, needle) {
			partial = append(partial, mailbox.ID)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return partial
}

func normalizeSearchMailboxName(value string) string {
	return strings.ToLower(strings.TrimSpace(strings.Trim(value, `"`)))
}

func searchMailboxLeaf(value string) string {
	value = strings.TrimSpace(value)
	if i := strings.LastIndexAny(value, `/\.`); i >= 0 && i+1 < len(value) {
		return value[i+1:]
	}
	return value
}

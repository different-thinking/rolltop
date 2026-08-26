// File overview: Conversation grouping, summary construction, search seed handling, and sender identity helpers.

package web

import (
	"context"
	"fmt"
	"net/mail"
	"sort"
	"strings"
	"time"

	"rolltop/backend/search"
	"rolltop/backend/store"
)

type conversationSeed struct {
	Message         store.MessageRecord
	MatchTerms      []string
	MatchFields     []string
	MatchQueryTerms []string
	// ListDate is the instant the list actually ordered this row by, and only a
	// list that ordered by something other than the row's own reckoning sets it.
	// The row then prints it verbatim, which is what keeps the date it shows and
	// the date it sorted by the same one - a row cannot be placed by its message
	// date and headed by a snooze return.
	ListDate time.Time
}

func (s *Server) conversationViews(ctx context.Context, userID int64, seeds []store.MessageRecord, own map[string]bool) ([]conversationView, error) {
	return s.conversationViewsFromSeeds(ctx, userID, conversationSeedsFromMessages(seeds), own, "")
}

func (s *Server) conversationViewsWithSearchSnippet(ctx context.Context, userID int64, seeds []store.MessageRecord, own map[string]bool, query string) ([]conversationView, error) {
	return s.conversationViewsFromSeeds(ctx, userID, conversationSeedsFromMessages(seeds), own, query)
}

func (s *Server) conversationViewsWithSearchDetails(ctx context.Context, userID int64, seeds []conversationSeed, own map[string]bool, query string) ([]conversationView, error) {
	return s.conversationViewsFromSeeds(ctx, userID, seeds, own, query)
}

func conversationSeedsFromMessages(messages []store.MessageRecord) []conversationSeed {
	seeds := make([]conversationSeed, 0, len(messages))
	for _, msg := range messages {
		seeds = append(seeds, conversationSeed{Message: msg})
	}
	return seeds
}

func stripStarSearchOperators(query string) (string, *bool) {
	fields := strings.Fields(query)
	if len(fields) == 0 {
		return query, nil
	}
	out := make([]string, 0, len(fields))
	var filter *bool
	changed := false
	for _, token := range fields {
		negated := strings.HasPrefix(token, "-")
		operator := strings.TrimPrefix(token, "-")
		var value bool
		switch strings.ToLower(operator) {
		case "is:starred":
			value = !negated
		case "is:notstarred":
			value = negated
		default:
			out = append(out, token)
			continue
		}
		filter = &value
		changed = true
	}
	if !changed {
		return query, nil
	}
	return strings.Join(out, " "), filter
}

func (s *Server) conversationViewsFromSeeds(ctx context.Context, userID int64, seeds []conversationSeed, own map[string]bool, query string) ([]conversationView, error) {
	type group struct {
		messages     []store.MessageRecord
		seen         map[int64]bool
		seed         store.MessageRecord
		hasSeed      bool
		terms        []string
		fields       []string
		queryTerms   []string
		snoozedUntil time.Time
		snoozeReturn time.Time
		// listDate is the date the list placed the row by, when the list decided
		// it rather than the row. Zero leaves the row to work it out itself.
		listDate time.Time
	}
	threadKeys := make([]string, 0, len(seeds))
	snoozeKeys := make([]string, 0, len(seeds))
	for _, seed := range seeds {
		snoozeKeys = append(snoozeKeys, store.SnoozeThreadKey(seed.Message))
		if key := strings.TrimSpace(seed.Message.ThreadKey); key != "" {
			threadKeys = append(threadKeys, key)
		}
	}
	threadsByKey, err := s.store.ListThreadMessagesByKeysForUser(ctx, userID, threadKeys)
	if err != nil {
		return nil, err
	}
	activeSnoozes, err := s.store.ActiveMessageSnoozesForThreads(ctx, userID, snoozeKeys, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	// A snooze that has come due keeps deciding where its message sorts, so the
	// due ones are read as well: they are what ListDate is built from.
	dueSnoozes, err := s.store.DueMessageSnoozesForThreads(ctx, userID, snoozeKeys, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	groups := map[string]*group{}
	order := make([]string, 0, len(seeds))
	for _, seed := range seeds {
		thread := threadsByKey[strings.TrimSpace(seed.Message.ThreadKey)]
		if len(thread) == 0 {
			thread = []store.MessageRecord{seed.Message}
		}
		key := conversationListKey(thread, own)
		g := groups[key]
		if g == nil {
			g = &group{seen: map[int64]bool{}}
			groups[key] = g
			order = append(order, key)
		}
		if !g.hasSeed {
			g.seed = seed.Message
			g.hasSeed = true
			g.listDate = seed.ListDate
		}
		if snooze, ok := activeSnoozes[store.SnoozeThreadKey(seed.Message)]; ok {
			g.snoozedUntil = snooze.SnoozedUntil
		}
		// Seeds sharing a conversation can carry different reminders, and the
		// row sorts by the latest of them. Taking the last one iterated would
		// let an earlier seed's later return time decide nothing.
		if snooze, ok := dueSnoozes[store.SnoozeThreadKey(seed.Message)]; ok && snooze.SnoozedUntil.After(g.snoozeReturn) {
			g.snoozeReturn = snooze.SnoozedUntil
		}
		g.terms = mergeTerms(g.terms, seed.MatchTerms)
		g.fields = mergeFields(g.fields, seed.MatchFields)
		g.queryTerms = mergeFields(g.queryTerms, seed.MatchQueryTerms)
		for _, msg := range thread {
			if g.seen[msg.ID] {
				continue
			}
			g.seen[msg.ID] = true
			g.messages = append(g.messages, msg)
		}
	}
	out := make([]conversationView, 0, len(order))
	for _, key := range order {
		group := groups[key]
		view := summarizeConversation(group.messages, own)
		// The row stands for the message this list selected, not for the newest
		// message the thread holds anywhere. A thread is hydrated in full, so it
		// carries rows the list itself excludes - a Sent reply, an archived or
		// trashed copy, a draft - and letting one of those supply the row would
		// print a date the row is not sorted by. ListDate is what the date
		// section headings group on, so the two have to be the same message.
		if group.hasSeed {
			view.Message = conversationRowMessage(view, group.seed)
			view.Snippet = messageSnippet(group.seed.BodyText, group.seed.BodyHTML)
			if !view.Message.IsStarred {
				view.StarredMessageID = group.seed.ID
			}
		}
		view.SnoozedUntil = group.snoozedUntil
		// A list that ordered by something of its own says so, and then the row
		// prints that. Only a row left to its own reckoning is headed by a
		// snooze return, which is what puts mail coming back from a reminder at
		// the top of a list ordered by when it last became current.
		if !group.listDate.IsZero() {
			view.ListDate = group.listDate
		} else {
			view.ListDate = view.Message.Date
			if group.snoozeReturn.After(view.ListDate) {
				view.ListDate = group.snoozeReturn
			}
		}
		if view.HasAttachments {
			view.AttachmentNames = s.conversationAttachmentNames(ctx, userID, group.messages, 3)
			if len(view.AttachmentNames) == 0 {
				view.HasAttachments = false
			}
		}
		if strings.TrimSpace(query) != "" && group.hasSeed {
			// The row already carries the seed, which for a search is the message
			// that matched; only the snippet has to be rebuilt around the terms.
			view.Snippet = searchResultSnippet(query, group.terms, group.seed, view.Snippet)
			view.MatchTerms = group.terms
			view.MatchFields = group.fields
			view.MatchQueryTerms = group.queryTerms
			view.AttachmentMatches, view.AttachmentContentMatched = s.conversationAttachmentMatches(ctx, userID, group.messages, group.terms, group.fields, query)
		}
		out = append(out, view)
	}
	return out, nil
}

func (s *Server) searchConversationSeeds(ctx context.Context, userID int64, q string, page, pageSize int, opts search.SearchOptions, own map[string]bool) ([]store.MessageRecord, error) {
	seeds, err := s.searchConversationSeedHits(ctx, userID, q, page, pageSize, opts, own, searchMailboxFilter{}, nil)
	if err != nil {
		return nil, err
	}
	messages := make([]store.MessageRecord, 0, len(seeds))
	for _, seed := range seeds {
		messages = append(messages, seed.Message)
	}
	return messages, nil
}

// Search hits are messages and the list shows conversations, so one page of
// conversations is collected by paging hits until enough distinct threads have
// been seen. How many that takes is a property of the mail, not of the request:
// a term every message of one long thread carries yields one conversation per
// hundred hits.
//
// Each of those pages costs the same as the first - a ranked search reads every
// matching message whatever slice of the ranking it returns - so a thread-heavy
// result set is where this loop turns one search into dozens. It starts at a
// page that can answer the request in one round and doubles when that was not
// enough, which leaves the common case as cheap as it was and bounds the
// pathological one to a handful of rounds instead of dozens.
const (
	searchSeedBatchStart = 100
	searchSeedBatchMax   = 500
)

func (s *Server) searchConversationSeedHits(ctx context.Context, userID int64, q string, page, pageSize int, opts search.SearchOptions, own map[string]bool, mailboxFilter searchMailboxFilter, timing *searchTiming) ([]conversationSeed, error) {
	searchQuery, starFilter := stripStarSearchOperators(q)
	targetStart := (page - 1) * pageSize
	targetEnd := targetStart + pageSize + 1
	seen := map[string]bool{}
	unique := make([]conversationSeed, 0, targetEnd)
	dueSeeds, err := s.dueSearchConversationSeeds(ctx, userID, searchQuery, opts, own, mailboxFilter, starFilter)
	if err != nil {
		return nil, err
	}
	for _, seed := range dueSeeds {
		key := conversationListKey([]store.MessageRecord{seed.Message}, own)
		if seen[key] {
			continue
		}
		seen[key] = true
		unique = append(unique, seed)
	}
	rawOffset := 0
	fromHits := 0
	// What counts towards the page being assembled. Under best match that is
	// every seed collected, due snoozes included, which is what puts them at the
	// front of the results. A date order sorts them into place instead, and then
	// only the hits may count: the window has to hold the whole top of the date
	// order to slice a page out of it, and a due snooze counted here would cut
	// the hits short by one - crowding a row off this page that the next page,
	// collecting the same way, would show again.
	collected := func() int {
		if opts.Order == search.SearchOrderBest {
			return len(unique)
		}
		return fromHits
	}
	// One hit can only ever become one conversation, so a batch smaller than the
	// page being assembled cannot fill it - asking for a hundred hits to collect
	// the hundred-and-first conversation is a round that is wasted before it is
	// made.
	batchSize := min(max(searchSeedBatchStart, targetEnd), searchSeedBatchMax)
	for collected() < targetEnd {
		bleveStart := time.Now()
		hits, err := s.search.SearchHitsWithOptions(ctx, userID, searchQuery, batchSize, rawOffset, opts)
		if timing != nil {
			timing.bleve += time.Since(bleveStart)
			timing.batches++
			timing.rawHits += len(hits)
		}
		if err != nil {
			return nil, err
		}
		if len(hits) == 0 {
			break
		}
		ids := make([]int64, 0, len(hits))
		termsByID := map[int64][]string{}
		fieldsByID := map[int64][]string{}
		queryTermsByID := map[int64][]string{}
		for _, hit := range hits {
			ids = append(ids, hit.ID)
			termsByID[hit.ID] = hit.Terms
			fieldsByID[hit.ID] = hit.Fields
			queryTermsByID[hit.ID] = hit.QueryTerms
		}
		hydrateStart := time.Now()
		messages, err := s.store.ListUnsnoozedMessagesByIDsForUser(ctx, userID, ids, time.Now().UTC())
		if err != nil {
			if timing != nil {
				timing.hydrate += time.Since(hydrateStart)
			}
			return nil, err
		}
		for _, msg := range messages {
			if !mailboxFilter.matches(msg) {
				continue
			}
			if starFilter != nil && msg.IsStarred != *starFilter {
				continue
			}
			key := conversationListKey([]store.MessageRecord{msg}, own)
			if seen[key] {
				continue
			}
			seen[key] = true
			unique = append(unique, conversationSeed{Message: msg, MatchTerms: termsByID[msg.ID], MatchFields: fieldsByID[msg.ID], MatchQueryTerms: queryTermsByID[msg.ID]})
			fromHits++
			if collected() >= targetEnd {
				break
			}
		}
		if timing != nil {
			timing.hydrate += time.Since(hydrateStart)
		}
		rawOffset += len(hits)
		if len(hits) < batchSize {
			break
		}
		batchSize = min(batchSize*2, searchSeedBatchMax)
	}
	orderSearchSeedsByDate(unique, opts.Order)
	if targetStart >= len(unique) {
		return nil, nil
	}
	if targetEnd > len(unique) {
		targetEnd = len(unique)
	}
	return unique[targetStart:targetEnd], nil
}

// orderSearchSeedsByDate merges the collected window into date order, and it is
// a merge rather than a sort on purpose. The hits already arrive in the order
// the reader asked for; what does not is the mail returning from a snooze, which
// is collected before the first hit is read. So this compares message dates and
// nothing else, and leans on the sort being stable: seeds that share a date keep
// the order they were collected in, which for the hits is the order the search
// backend put them in.
//
// That is what keeps the pages a partition of the matches. Two rows written in
// the same second are ordinary, and the backends break that tie their own way -
// Bleve by the decimal spelling of the document id, PostgreSQL by its value, so
// the two disagree as soon as a reader holds more than nine matching messages.
// Either is a total order, and pages carved out of one fit together; a second
// tie-break here, disagreeing with whichever of them selected the window, is
// what would make a row show up on two pages while another was never reached.
//
// Sorting the window at all is sound because the window is every page up to the
// one asked for: the loop rebuilds it from hit zero on every request, so page
// four merges the same rows the same way page one did.
//
// Every seed is then stamped with the date it was placed by, so the row prints
// that instead of promoting a snooze return the list did not order by.
func orderSearchSeedsByDate(seeds []conversationSeed, order search.SearchOrder) {
	newestFirst := order == search.SearchOrderNewest
	if !newestFirst && order != search.SearchOrderOldest {
		return
	}
	sort.SliceStable(seeds, func(i, j int) bool {
		if newestFirst {
			return seeds[i].Message.Date.After(seeds[j].Message.Date)
		}
		return seeds[i].Message.Date.Before(seeds[j].Message.Date)
	})
	for i := range seeds {
		seeds[i].ListDate = seeds[i].Message.Date
	}
}

func (s *Server) dueSearchConversationSeeds(ctx context.Context, userID int64, query string, opts search.SearchOptions, own map[string]bool, mailboxFilter searchMailboxFilter, starFilter *bool) ([]conversationSeed, error) {
	items, err := s.store.ListDueSnoozedMessagesForUser(ctx, userID, 200, 0, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	out := make([]conversationSeed, 0, len(items))
	for _, item := range items {
		thread, err := s.store.ListThreadMessagesForUser(ctx, userID, item.Message)
		if err != nil {
			return nil, err
		}
		candidates := make([]store.MessageRecord, 0, len(thread))
		ids := make([]int64, 0, len(thread))
		byID := make(map[int64]store.MessageRecord, len(thread))
		for _, msg := range thread {
			if !mailboxFilter.matches(msg) || (starFilter != nil && msg.IsStarred != *starFilter) {
				continue
			}
			candidates = append(candidates, msg)
			ids = append(ids, msg.ID)
			byID[msg.ID] = msg
		}
		if len(candidates) == 0 {
			continue
		}
		if strings.TrimSpace(query) == "" {
			out = append(out, conversationSeed{Message: candidates[len(candidates)-1]})
			continue
		}
		result, matched, err := s.search.ExplainMessagesWithOptions(ctx, userID, ids, query, opts)
		if err != nil {
			return nil, err
		}
		if !matched {
			continue
		}
		msg, ok := byID[result.ID]
		if !ok {
			continue
		}
		out = append(out, conversationSeed{
			Message: msg, MatchTerms: result.Terms, MatchFields: result.Fields, MatchQueryTerms: result.QueryTerms,
		})
	}
	return out, nil
}

// conversationRowMessage puts the list's own selection in front of a row.
// Thread-wide state that the row renders stays as summarizeConversation read it
// from the whole thread: a star anywhere in a conversation stars its row, and
// the seed carries only its own flag.
func conversationRowMessage(summary conversationView, seed store.MessageRecord) store.MessageRecord {
	display := seed
	display.IsStarred = summary.Message.IsStarred
	return display
}

func mergeTerms(existing []string, next []string) []string {
	if len(next) == 0 {
		return existing
	}
	seen := map[string]bool{}
	for _, term := range existing {
		seen[strings.ToLower(term)] = true
	}
	out := append([]string{}, existing...)
	for _, term := range next {
		term = strings.TrimSpace(strings.ToLower(term))
		if term == "" || seen[term] {
			continue
		}
		seen[term] = true
		out = append(out, term)
		if len(out) >= 10 {
			break
		}
	}
	return out
}

func mergeFields(existing []string, next []string) []string {
	if len(next) == 0 {
		return existing
	}
	seen := map[string]bool{}
	for _, field := range existing {
		seen[field] = true
	}
	out := append([]string{}, existing...)
	for _, field := range next {
		field = strings.TrimSpace(field)
		if field == "" || seen[field] {
			continue
		}
		seen[field] = true
		out = append(out, field)
	}
	return out
}

func summarizeConversation(thread []store.MessageRecord, own map[string]bool) conversationView {
	messageIDs, accountIDs := conversationTransferIDs(thread)
	thread = dedupeConversationMessages(thread)
	latest := thread[0]
	starred := false
	starredMessage := store.MessageRecord{}
	allRead := true
	hasAttachments := false
	for _, msg := range thread {
		if msg.Date.After(latest.Date) || (msg.Date.Equal(latest.Date) && msg.ID > latest.ID) {
			latest = msg
		}
		if msg.IsStarred && (!starred || msg.Date.After(starredMessage.Date) || (msg.Date.Equal(starredMessage.Date) && msg.ID > starredMessage.ID)) {
			starred = true
			starredMessage = msg
		}
		if !msg.IsRead {
			allRead = false
		}
		if msg.HasAttachments {
			hasAttachments = true
		}
	}
	displayMessage := latest
	displayMessage.IsStarred = starred
	starredMessageID := latest.ID
	if starred {
		starredMessageID = starredMessage.ID
	}
	return conversationView{
		Message:               displayMessage,
		MessageIDs:            messageIDs,
		MessageAccountIDs:     accountIDs,
		StarredMessageID:      starredMessageID,
		Participants:          participantSummary(thread, own),
		RecipientParticipants: recipientParticipantSummary(thread, own),
		Count:                 len(thread),
		IsRead:                allRead,
		HasAttachments:        hasAttachments,
		Snippet:               messageSnippet(latest.BodyText, latest.BodyHTML),
	}
}

// conversationTransferIDs lists the thread's message IDs and, at the same
// index, the account each of those messages belongs to. The two slices are
// parallel rather than the distinct account set they used to be, because a
// thread can hold copies of the same mail in several accounts and a folder
// belongs to exactly one of them: filing such a row means splitting it, and a
// distinct set says how many accounts are involved without saying which
// message sits in which. Every message row carries a non-null account, so the
// account slice never has a hole a caller would have to guess at.
func conversationTransferIDs(messages []store.MessageRecord) ([]int64, []int64) {
	seen := map[int64]bool{}
	ids := make([]int64, 0, len(messages))
	accountIDs := make([]int64, 0, len(messages))
	for _, msg := range messages {
		if msg.ID <= 0 || seen[msg.ID] {
			continue
		}
		seen[msg.ID] = true
		ids = append(ids, msg.ID)
		accountIDs = append(accountIDs, msg.AccountID)
	}
	return ids, accountIDs
}

func (s *Server) conversationAttachmentNames(ctx context.Context, userID int64, messages []store.MessageRecord, limit int) []string {
	attachments, err := s.conversationAttachments(ctx, userID, messages, limit)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(attachments))
	for _, att := range attachments {
		name := strings.TrimSpace(att.Filename)
		if name == "" {
			name = strings.TrimSpace(att.ContentType)
		}
		if name == "" {
			name = "Attachment"
		}
		names = append(names, name)
	}
	return names
}

func (s *Server) conversationAttachmentMatches(ctx context.Context, userID int64, messages []store.MessageRecord, terms, fields []string, query string) ([]string, bool) {
	if !searchFieldsInclude(fields, "attachment_names", "attachments", "attachment_types") {
		return nil, false
	}
	attachments, err := s.conversationAttachments(ctx, userID, messages, 12)
	if err != nil {
		return nil, searchFieldsInclude(fields, "attachments")
	}
	needles := mergeSnippetTerms(terms, searchSnippetTerms(query))
	var matches []string
	if searchFieldsInclude(fields, "attachment_names") {
		for _, att := range attachments {
			name := strings.TrimSpace(att.Filename)
			if name == "" {
				continue
			}
			if attachmentNameMatches(name, needles) {
				matches = append(matches, name)
			}
		}
	}
	return uniqueStrings(matches, 4), searchFieldsInclude(fields, "attachments") && len(matches) == 0
}

func (s *Server) conversationAttachments(ctx context.Context, userID int64, messages []store.MessageRecord, limit int) ([]store.Attachment, error) {
	if limit <= 0 {
		limit = 3
	}
	var out []store.Attachment
	seen := map[string]bool{}
	for _, msg := range messages {
		if !msg.HasAttachments {
			continue
		}
		attachments, err := s.store.ListAttachmentsForMessage(ctx, userID, msg.ID)
		if err != nil {
			return nil, err
		}
		for _, att := range attachments {
			if !isDisplayAttachment(att) {
				continue
			}
			key := strings.ToLower(strings.TrimSpace(att.Filename))
			if key == "" {
				key = fmt.Sprintf("%d", att.ID)
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, att)
			if len(out) >= limit {
				return out, nil
			}
		}
	}
	return out, nil
}

func searchFieldsInclude(fields []string, candidates ...string) bool {
	for _, field := range fields {
		for _, candidate := range candidates {
			if field == candidate {
				return true
			}
		}
	}
	return false
}

func attachmentNameMatches(name string, terms []string) bool {
	normalized := strings.ToLower(name)
	for _, term := range terms {
		term = strings.TrimSpace(strings.ToLower(term))
		if term == "" {
			continue
		}
		if strings.Contains(normalized, term) {
			return true
		}
		for _, word := range strings.FieldsFunc(normalized, func(r rune) bool {
			return !isSearchWordRune(r)
		}) {
			if word == term {
				return true
			}
		}
	}
	return false
}

func isSearchWordRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r > 127
}

func uniqueStrings(values []string, limit int) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		key := strings.ToLower(value)
		if value == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func dedupeConversationMessages(thread []store.MessageRecord) []store.MessageRecord {
	if len(thread) < 2 {
		return thread
	}
	out := make([]store.MessageRecord, 0, len(thread))
	seen := map[string]int{}
	for _, msg := range thread {
		key := conversationMessageIdentity(msg)
		if idx, ok := seen[key]; ok {
			existing := out[idx]
			previous := existing
			read := existing.IsRead && msg.IsRead
			starred := existing.IsStarred || msg.IsStarred
			hasAttachments := existing.HasAttachments || msg.HasAttachments
			if msg.Date.After(existing.Date) || (msg.Date.Equal(existing.Date) && msg.ID > existing.ID) {
				existing = msg
			}
			if starred && !existing.IsStarred {
				if previous.IsStarred {
					existing = previous
				} else if msg.IsStarred {
					existing = msg
				}
			}
			existing.IsRead = read
			existing.IsStarred = starred
			existing.HasAttachments = hasAttachments
			out[idx] = existing
			continue
		}
		seen[key] = len(out)
		out = append(out, msg)
	}
	return out
}

func conversationMessageIdentity(msg store.MessageRecord) string {
	if id := strings.ToLower(strings.TrimSpace(msg.MessageIDHeader)); id != "" {
		return "message-id:" + id
	}
	return fmt.Sprintf("local:%d", msg.ID)
}

func participantSummary(thread []store.MessageRecord, own map[string]bool) string {
	var labels []string
	seen := map[string]bool{}
	hasMe := false
	for _, msg := range thread {
		identity := store.SenderIdentity(msg.FromAddr)
		label := senderDisplayName(msg.FromAddr)
		if own[identity] {
			label = "me"
			hasMe = true
		}
		if label == "" {
			label = "Unknown sender"
		}
		key := strings.ToLower(label)
		if seen[key] {
			continue
		}
		seen[key] = true
		labels = append(labels, label)
	}
	if hasMe && len(labels) > 1 {
		for i, label := range labels {
			if label == "me" {
				labels = append(labels[:i], labels[i+1:]...)
				labels = append(labels, "me")
				break
			}
		}
	}
	if len(labels) == 0 {
		return "Unknown sender"
	}
	if len(labels) > 3 {
		return fmt.Sprintf("%s, %s, %s +%d", labels[0], labels[1], labels[2], len(labels)-3)
	}
	return strings.Join(labels, ", ")
}

func recipientParticipantSummary(thread []store.MessageRecord, own map[string]bool) string {
	var labels []string
	seen := map[string]bool{}
	hasMe := false
	for _, msg := range thread {
		for _, value := range []string{msg.ToAddr, msg.CCAddr} {
			for _, label := range recipientAddressLabels(value, own) {
				if label == "me" {
					hasMe = true
				}
				key := strings.ToLower(label)
				if key == "" || seen[key] {
					continue
				}
				seen[key] = true
				labels = append(labels, label)
			}
		}
	}
	if hasMe && len(labels) > 1 {
		for i, label := range labels {
			if label == "me" {
				labels = append(labels[:i], labels[i+1:]...)
				labels = append(labels, "me")
				break
			}
		}
	}
	if len(labels) == 0 {
		return "undisclosed recipients"
	}
	if len(labels) > 3 {
		return fmt.Sprintf("%s, %s, %s +%d", labels[0], labels[1], labels[2], len(labels)-3)
	}
	return strings.Join(labels, ", ")
}

func recipientAddressLabels(value string, own map[string]bool) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if addrs, err := mail.ParseAddressList(value); err == nil {
		out := make([]string, 0, len(addrs))
		for _, addr := range addrs {
			identity := store.SenderIdentity(addr.Address)
			if identity == "" {
				continue
			}
			if own[identity] {
				out = append(out, "me")
			} else {
				out = append(out, identity)
			}
		}
		return out
	}
	if identity := store.SenderIdentity(value); identity != "" {
		if own[identity] {
			return []string{"me"}
		}
		return []string{identity}
	}
	return nil
}

func senderDisplayName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if addrs, err := mail.ParseAddressList(value); err == nil && len(addrs) > 0 {
		if strings.TrimSpace(addrs[0].Name) != "" {
			return strings.TrimSpace(addrs[0].Name)
		}
		return strings.TrimSpace(addrs[0].Address)
	}
	return strings.Trim(value, "<> \t\r\n")
}

func senderEmail(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if addrs, err := mail.ParseAddressList(value); err == nil && len(addrs) > 0 {
		return strings.TrimSpace(addrs[0].Address)
	}
	return strings.Trim(value, "<> \t\r\n")
}

func senderInitial(value string) string {
	label := senderDisplayName(value)
	if label == "" {
		label = senderEmail(value)
	}
	for _, r := range strings.TrimSpace(label) {
		return strings.ToUpper(string(r))
	}
	return "?"
}

func recipientLine(msg store.MessageRecord) string {
	to := strings.TrimSpace(msg.ToAddr)
	cc := strings.TrimSpace(msg.CCAddr)
	switch {
	case to != "" && cc != "":
		return "to " + to + ", cc " + cc
	case to != "":
		return "to " + to
	case cc != "":
		return "cc " + cc
	default:
		return "to undisclosed recipients"
	}
}

func conversationKey(msg store.MessageRecord) string {
	if strings.TrimSpace(msg.ThreadKey) != "" {
		return "thread:" + msg.ThreadKey
	}
	return fmt.Sprintf("message:%d", msg.ID)
}

func conversationListKey(messages []store.MessageRecord, own map[string]bool) string {
	if len(messages) == 0 {
		return ""
	}
	if key := reliableThreadBoundaryKey(messages); key != "" {
		return key
	}
	subject := ""
	ids := map[string]bool{}
	for _, msg := range messages {
		if subject == "" {
			subject = store.NormalizedThreadSubject(msg.Subject)
		}
		for _, value := range []string{msg.FromAddr, msg.ToAddr, msg.CCAddr} {
			for _, identity := range addressIdentities(value) {
				if own[identity] {
					identity = "me"
				}
				if identity != "" {
					ids[identity] = true
				}
			}
		}
	}
	if subject == "" {
		return conversationKey(messages[0])
	}
	parts := make([]string, 0, len(ids))
	for identity := range ids {
		parts = append(parts, identity)
	}
	sortStrings(parts)
	if len(parts) == 0 {
		return "subject:" + subject
	}
	return "subject:" + subject + "|people:" + strings.Join(parts, ",")
}

func reliableThreadBoundaryKey(messages []store.MessageRecord) string {
	key := reliableMessageIDThreadKey(messages[0])
	if key == "" {
		return ""
	}
	for _, msg := range messages[1:] {
		if reliableMessageIDThreadKey(msg) != key {
			return ""
		}
	}
	return "thread:" + key
}

func reliableMessageIDThreadKey(msg store.MessageRecord) string {
	key := strings.TrimSpace(msg.ThreadKey)
	if key == "" {
		key = store.ThreadKey(msg.MessageIDHeader, msg.InReplyTo, msg.ReferencesHeader, msg.Subject)
	}
	if strings.HasPrefix(key, "msgid:") {
		return key
	}
	return ""
}

func addressIdentities(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if addrs, err := mail.ParseAddressList(value); err == nil {
		out := make([]string, 0, len(addrs))
		for _, addr := range addrs {
			identity := store.SenderIdentity(addr.Address)
			if identity != "" {
				out = append(out, identity)
			}
		}
		return out
	}
	if identity := store.SenderIdentity(value); identity != "" {
		return []string{identity}
	}
	return nil
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func (s *Server) ownAddresses(ctx context.Context, user store.User) map[string]bool {
	own := map[string]bool{}
	for _, value := range []string{user.Email} {
		if identity := store.SenderIdentity(value); identity != "" {
			own[identity] = true
		}
	}
	if account, err := s.store.GetMailAccount(ctx, user.ID); err == nil {
		for _, value := range []string{account.Email, account.Username, account.SMTPUsername} {
			if identity := store.SenderIdentity(value); identity != "" {
				own[identity] = true
			}
		}
	}
	if emails, err := s.store.ListMeContactEmailsForUser(ctx, user.ID); err == nil {
		for _, value := range emails {
			if identity := store.SenderIdentity(value); identity != "" {
				own[identity] = true
			}
		}
	}
	return own
}

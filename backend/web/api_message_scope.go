// File overview: Whole-filter message actions. The browser can only name a page
// of message IDs, so "delete everything this filter matches" is resolved here
// from the same scope description the list view was rendered from.

package web

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"rolltop/backend/store"
)

const (
	// scopeTrashMessageLimit bounds the IDs one request resolves. A filter with
	// more matches is trashed in repeated passes: the response reports that it
	// was cut short so the browser can say so and offer another pass.
	scopeTrashMessageLimit = 20000
	// scopeSearchMessageLimit is lower because a search scope is paged out of
	// Bleve rather than read in one query, and deep offsets get progressively
	// more expensive. Moved messages leave the index, so the next pass starts
	// from the front again.
	scopeSearchMessageLimit = 5000
	// scopeSearchHitBatch pages Bleve while a search scope is being resolved. It
	// must stay within the search service's own per-request ceiling: a larger
	// value is silently clamped, and a short batch is what ends the paging loop.
	scopeSearchHitBatch = 100
)

// scopeSelection mirrors the list view the user is looking at. A query wins over
// a mailbox because search results are their own list; without either, View
// decides which whole-account list is meant, so a delete from a named view
// reaches exactly the rows that view shows and no others. Filter narrows that
// list further, which is how "everything older than a date" is expressed.
type scopeSelection struct {
	MailboxID int64
	Query     string
	View      mailView
	Filter    store.ScopeFilter
}

// scopeMoveGroup is one account's share of a whole-filter move: both Trash and
// Archive belong to an account, so a filter spanning accounts becomes one move
// per account.
type scopeMoveGroup struct {
	AccountID        int64
	Target           store.MailboxSummary
	MessageIDs       []int64
	RefreshMailboxes []string
}

type scopeMovePlan struct {
	Groups []scopeMoveGroup
	// Matched counts resolved messages, Skipped those already in the folder they
	// would be moved to.
	Matched   int
	Skipped   int
	Truncated bool
}

// scopeMoveDestination resolves the per-account folder one whole-filter move
// sends mail to, and names the action for the messages it cannot route.
type scopeMoveDestination struct {
	folder string
	verb   string
	// targets maps account ID to its destination folder. An account missing from
	// the map has nothing configured for this action.
	targets map[int64]store.MailboxSummary
}

// missingScopeTargetError is returned when a resolved account has no folder
// assigned for the action, which is a setup question rather than a server
// failure.
type missingScopeTargetError struct {
	folder  string
	verb    string
	account string
}

func (e missingScopeTargetError) Error() string {
	who := "this account"
	if strings.TrimSpace(e.account) != "" {
		who = e.account
	}
	return "choose " + article(e.folder) + " " + e.folder + " folder for " + who + " before " + e.verb + " messages"
}

// article picks the indefinite article for a folder name, so one error string
// serves both "a Trash folder" and "an Archive folder".
func article(folder string) string {
	if strings.HasPrefix(folder, "A") || strings.HasPrefix(folder, "E") ||
		strings.HasPrefix(folder, "I") || strings.HasPrefix(folder, "O") || strings.HasPrefix(folder, "U") {
		return "an"
	}
	return "a"
}

// scopeTrashDestination resolves each account's Trash folder, which is a
// mailbox role rather than a saved preference.
func (s *Server) scopeTrashDestination(mailboxes []store.MailboxSummary) scopeMoveDestination {
	targets := map[int64]store.MailboxSummary{}
	for _, mailbox := range mailboxes {
		if mailbox.Role == "trash" {
			targets[mailbox.AccountID] = mailbox
		}
	}
	return scopeMoveDestination{folder: "Trash", verb: "deleting", targets: targets}
}

// scopeArchiveDestination resolves each account's Archive folder from the same
// saved mapping the swipe and row Archive actions use, so a whole-filter
// archive lands wherever single-message archiving already lands.
func (s *Server) scopeArchiveDestination(ctx context.Context, userID int64, mailboxes []store.MailboxSummary) (scopeMoveDestination, error) {
	destination := scopeMoveDestination{folder: "Archive", verb: "archiving", targets: map[int64]store.MailboxSummary{}}
	choices, err := s.store.ArchiveMailboxesForUser(ctx, userID)
	if err != nil {
		return scopeMoveDestination{}, err
	}
	byID := map[int64]store.MailboxSummary{}
	for _, mailbox := range mailboxes {
		byID[mailbox.ID] = mailbox
	}
	for _, choice := range choices {
		mailbox, ok := byID[choice.MailboxID]
		if !ok || mailbox.AccountID != choice.AccountID {
			continue
		}
		destination.targets[choice.AccountID] = mailbox
	}
	return destination, nil
}

// apiScopeTrashMessages moves every message the given filter matches into each
// account's Trash folder as background sync runs. Unlike bulk-move it never
// receives message IDs: only the browser's current scope, so the selection is
// resolved server-side and is not capped by a request body.
func (s *Server) apiScopeTrashMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	if !s.verifyCSRF(w, r) {
		return
	}
	if s.syncer == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "IMAP sync is not configured")
		return
	}
	scope, ok := s.scopeSelectionFromRequest(w, r)
	if !ok {
		return
	}
	plan, err := s.scopeTrashPlan(r.Context(), cu.User, scope)
	if err != nil {
		s.writeScopePlanError(w, r, err)
		return
	}
	s.startScopeMove(w, r, cu, plan, "delete")
}

// apiScopeArchiveMessages moves every message the given filter matches into each
// account's Archive folder. It exists for "archive everything older than this
// date": the cutoff is required, because a scope archive without one would move
// the list the user is looking at, not the backlog behind it.
func (s *Server) apiScopeArchiveMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	if !s.verifyCSRF(w, r) {
		return
	}
	if s.syncer == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "IMAP sync is not configured")
		return
	}
	scope, ok := s.scopeSelectionFromRequest(w, r)
	if !ok {
		return
	}
	if scope.Filter.Before.IsZero() {
		writeAPIError(w, http.StatusBadRequest, "choose the date to archive before")
		return
	}
	plan, err := s.scopeArchivePlan(r.Context(), cu.User, scope)
	if err != nil {
		s.writeScopePlanError(w, r, err)
		return
	}
	s.startScopeMove(w, r, cu, plan, "archive")
}

// scopeSelectionFromRequest reads the browser's current filter, plus the
// optional cutoff that narrows it to mail older than a date.
func (s *Server) scopeSelectionFromRequest(w http.ResponseWriter, r *http.Request) (scopeSelection, bool) {
	var in struct {
		ScopeMailboxID int64  `json:"scope_mailbox_id"`
		ScopeQuery     string `json:"scope_query"`
		ScopeView      string `json:"scope_view"`
		Before         string `json:"before"`
	}
	if !decodeJSON(w, r, &in) {
		return scopeSelection{}, false
	}
	view, viewKnown := parseMailView(in.ScopeView)
	if !viewKnown {
		writeAPIError(w, http.StatusBadRequest, "unknown list view")
		return scopeSelection{}, false
	}
	before, err := parseScopeCutoff(in.Before)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return scopeSelection{}, false
	}
	return scopeSelection{
		MailboxID: in.ScopeMailboxID,
		Query:     strings.TrimSpace(in.ScopeQuery),
		View:      view,
		Filter:    store.ScopeFilter{Before: before},
	}, true
}

// parseScopeCutoff reads the "older than" date. A bare calendar date is taken as
// the start of that day in UTC, so the day the user names is itself kept: mail
// stamped on it is not older than it.
func parseScopeCutoff(raw string) (time.Time, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return time.Time{}, nil
	}
	if day, err := time.ParseInLocation("2006-01-02", value, time.UTC); err == nil {
		return day, nil
	}
	moment, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, errors.New("could not read the date to archive before")
	}
	return moment.UTC(), nil
}

// writeScopePlanError answers a failed plan. A missing destination folder is a
// setup question the user can act on, not a server failure.
func (s *Server) writeScopePlanError(w http.ResponseWriter, r *http.Request, err error) {
	var missingTarget missingScopeTargetError
	switch {
	case errors.As(err, &missingTarget):
		writeAPIError(w, http.StatusBadRequest, missingTarget.Error())
	case store.IsNotFound(err):
		http.NotFound(w, r)
	default:
		s.serverError(w, r, err)
	}
}

// startScopeMove queues one background move per account and reports what the
// pass covered. action names the operation in the errors it may have to write.
func (s *Server) startScopeMove(w http.ResponseWriter, r *http.Request, cu currentUser, plan scopeMovePlan, action string) {
	if len(plan.Groups) == 0 {
		writeJSON(w, map[string]any{
			"ok": true, "queued": false, "matched": plan.Matched, "skipped": plan.Skipped,
			"truncated": plan.Truncated, "runs": []any{},
		})
		return
	}
	finishForeground := func() {}
	if s.syncRunner != nil {
		finish, err := s.syncRunner.BeginForegroundOperation(r.Context(), cu.User.ID)
		if err != nil {
			s.apiError(w, r, http.StatusServiceUnavailable, "could not schedule the "+action, err)
			return
		}
		finishForeground = finish
	}
	// One reservation covers every account's run: release it when the last one
	// reports back, so a second foreground writer cannot interleave with a
	// half-finished multi-account move.
	pending := int64(len(plan.Groups))
	release := func() {
		if atomic.AddInt64(&pending, -1) <= 0 {
			finishForeground()
		}
	}
	runs := make([]map[string]any, 0, len(plan.Groups))
	queued := 0
	var startErr error
	for _, group := range plan.Groups {
		group := group
		run, err := s.syncer.StartMoveMessages(r.Context(), cu.User.ID, group.MessageIDs, group.Target.ID, func() {
			s.startMoveRefresh(cu.User.ID, group.Target.AccountID, group.RefreshMailboxes)
			release()
		})
		if err != nil {
			if startErr == nil {
				startErr = err
			}
			release()
			continue
		}
		queued += len(group.MessageIDs)
		runs = append(runs, map[string]any{
			"run_id": run.ID, "account_id": group.AccountID,
			"mailbox": group.Target.Name, "messages": len(group.MessageIDs),
		})
	}
	if len(runs) == 0 {
		if store.IsNotFound(startErr) {
			http.NotFound(w, r)
			return
		}
		s.apiError(w, r, http.StatusBadGateway, "could not start the "+action, startErr)
		return
	}
	response := map[string]any{
		"ok": true, "queued": true, "matched": plan.Matched, "skipped": plan.Skipped,
		"queued_messages": queued, "truncated": plan.Truncated, "runs": runs,
	}
	if startErr != nil {
		response["partial_error"] = "Some accounts could not be started."
	}
	writeJSON(w, response)
}

// scopeTrashPlan resolves a scope into one move per account, skipping messages
// that already sit in the Trash folder they would be moved to.
func (s *Server) scopeTrashPlan(ctx context.Context, user store.User, scope scopeSelection) (scopeMovePlan, error) {
	return s.scopeMovePlan(ctx, user, scope, func(_ context.Context, mailboxes []store.MailboxSummary) (scopeMoveDestination, error) {
		return s.scopeTrashDestination(mailboxes), nil
	})
}

// scopeArchivePlan resolves a scope into one move per account towards each
// account's Archive folder, skipping messages already archived there.
func (s *Server) scopeArchivePlan(ctx context.Context, user store.User, scope scopeSelection) (scopeMovePlan, error) {
	return s.scopeMovePlan(ctx, user, scope, func(ctx context.Context, mailboxes []store.MailboxSummary) (scopeMoveDestination, error) {
		return s.scopeArchiveDestination(ctx, user.ID, mailboxes)
	})
}

// scopeMovePlan resolves a scope into one move per account, skipping messages
// that already sit in the folder they would be moved to.
func (s *Server) scopeMovePlan(ctx context.Context, user store.User, scope scopeSelection,
	resolveDestination func(context.Context, []store.MailboxSummary) (scopeMoveDestination, error)) (scopeMovePlan, error) {
	var plan scopeMovePlan
	messages, truncated, err := s.resolveScopeMessages(ctx, user, scope)
	if err != nil {
		return plan, err
	}
	plan.Truncated = truncated
	plan.Matched = len(messages)
	if len(messages) == 0 {
		return plan, nil
	}
	mailboxes, err := s.store.ListMailboxesForUser(ctx, user.ID)
	if err != nil {
		return plan, err
	}
	destination, err := resolveDestination(ctx, mailboxes)
	if err != nil {
		return plan, err
	}
	nameByID := map[int64]string{}
	emailByAccount := map[int64]string{}
	for _, mailbox := range mailboxes {
		nameByID[mailbox.ID] = mailbox.Name
		emailByAccount[mailbox.AccountID] = mailbox.AccountEmail
	}
	groups := map[int64]*scopeMoveGroup{}
	refreshSeen := map[int64]map[string]bool{}
	for _, message := range messages {
		target, ok := destination.targets[message.AccountID]
		if !ok {
			return scopeMovePlan{}, missingScopeTargetError{
				folder: destination.folder, verb: destination.verb, account: emailByAccount[message.AccountID],
			}
		}
		if message.MailboxID == target.ID {
			plan.Skipped++
			continue
		}
		group, ok := groups[message.AccountID]
		if !ok {
			group = &scopeMoveGroup{AccountID: message.AccountID, Target: target}
			groups[message.AccountID] = group
			refreshSeen[message.AccountID] = map[string]bool{}
			group.RefreshMailboxes = append(group.RefreshMailboxes, target.Name)
			refreshSeen[message.AccountID][strings.ToLower(target.Name)] = true
		}
		group.MessageIDs = append(group.MessageIDs, message.ID)
		source := nameByID[message.MailboxID]
		key := strings.ToLower(source)
		if source != "" && !refreshSeen[message.AccountID][key] {
			refreshSeen[message.AccountID][key] = true
			group.RefreshMailboxes = append(group.RefreshMailboxes, source)
		}
	}
	accountIDs := make([]int64, 0, len(groups))
	for accountID := range groups {
		accountIDs = append(accountIDs, accountID)
	}
	sort.Slice(accountIDs, func(a, b int) bool { return accountIDs[a] < accountIDs[b] })
	for _, accountID := range accountIDs {
		plan.Groups = append(plan.Groups, *groups[accountID])
	}
	return plan, nil
}

// resolveScopeMessages turns a list scope into the messages it currently shows.
// The second result reports that the scope has more matches than one pass moves.
func (s *Server) resolveScopeMessages(ctx context.Context, user store.User, scope scopeSelection) ([]store.ScopeMessage, bool, error) {
	if strings.TrimSpace(scope.Query) != "" {
		return s.resolveSearchScopeMessages(ctx, user, scope.Query, scope.Filter)
	}
	if scope.MailboxID > 0 {
		if _, err := s.store.GetMailboxForUser(ctx, user.ID, scope.MailboxID); err != nil {
			return nil, false, err
		}
		messages, err := s.store.ListMailboxScopeMessagesForUser(ctx, user.ID, scope.MailboxID, scope.Filter, scopeTrashMessageLimit+1)
		return trimScopeMessages(messages, scopeTrashMessageLimit, err)
	}
	if role := scope.View.role(); role != "" {
		messages, err := s.store.ListRoleMailScopeMessagesForUser(ctx, user.ID, role, scope.Filter, scopeTrashMessageLimit+1)
		return trimScopeMessages(messages, scopeTrashMessageLimit, err)
	}
	if scope.View == mailViewInbox {
		messages, err := s.store.ListUnarchivedMailScopeMessagesForUser(ctx, user.ID, scope.Filter, scopeTrashMessageLimit+1)
		return trimScopeMessages(messages, scopeTrashMessageLimit, err)
	}
	messages, err := s.store.ListAllMailScopeMessagesForUser(ctx, user.ID, scope.Filter, scopeTrashMessageLimit+1)
	return trimScopeMessages(messages, scopeTrashMessageLimit, err)
}

// resolveSearchScopeMessages walks the whole hit list for a search scope. Only
// messages the search itself matches are collected, so a thread is never
// trashed wholesale because one of its messages matched.
func (s *Server) resolveSearchScopeMessages(ctx context.Context, user store.User, query string, filter store.ScopeFilter) ([]store.ScopeMessage, bool, error) {
	searchQuery, mailboxFilter, err := s.searchMailboxFilter(ctx, user.ID, query)
	if err != nil {
		return nil, false, err
	}
	searchQuery, starFilter := stripStarSearchOperators(searchQuery)
	if strings.TrimSpace(searchQuery) == "" && !mailboxFilter.enabled && starFilter == nil {
		// A search box holding only whitespace lists All Mail, so it selects it too.
		messages, err := s.store.ListAllMailScopeMessagesForUser(ctx, user.ID, filter, scopeTrashMessageLimit+1)
		return trimScopeMessages(messages, scopeTrashMessageLimit, err)
	}
	if s.search == nil {
		return nil, false, errors.New("full-text search is not configured")
	}
	if strings.TrimSpace(searchQuery) != "" {
		if _, err := s.ensureRecentSearchDocuments(ctx, user.ID); err != nil {
			return nil, false, err
		}
	}
	opts := s.searchOptionsWithRankingBoosts(ctx, user)
	out := make([]store.ScopeMessage, 0, 256)
	offset := 0
	for len(out) <= scopeSearchMessageLimit {
		hits, err := s.search.SearchHitsWithOptions(ctx, user.ID, searchQuery, scopeSearchHitBatch, offset, opts)
		if err != nil {
			return nil, false, err
		}
		if len(hits) == 0 {
			break
		}
		ids := make([]int64, 0, len(hits))
		for _, hit := range hits {
			ids = append(ids, hit.ID)
		}
		messages, err := s.store.ListUnsnoozedMessagesByIDsForUser(ctx, user.ID, ids, time.Now().UTC())
		if err != nil {
			return nil, false, err
		}
		for _, message := range messages {
			if !mailboxFilter.matches(message) {
				continue
			}
			if starFilter != nil && message.IsStarred != *starFilter {
				continue
			}
			if !filter.Matches(message) {
				continue
			}
			out = append(out, store.ScopeMessage{ID: message.ID, AccountID: message.AccountID, MailboxID: message.MailboxID})
			if len(out) > scopeSearchMessageLimit {
				break
			}
		}
		offset += len(hits)
		if len(hits) < scopeSearchHitBatch {
			break
		}
	}
	return trimScopeMessages(out, scopeSearchMessageLimit, nil)
}

// trimScopeMessages cuts an over-fetched selection back to its limit. Callers
// fetch one more than they can use, so a full slice is how truncation is known.
func trimScopeMessages(messages []store.ScopeMessage, limit int, err error) ([]store.ScopeMessage, bool, error) {
	if err != nil {
		return nil, false, err
	}
	if len(messages) > limit {
		return messages[:limit], true, nil
	}
	return messages, false, nil
}

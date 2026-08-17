// File overview: Whole-filter message actions. The browser can only name a page
// of message IDs, so "delete everything this filter matches" is resolved here
// from the same scope description the list view was rendered from.

package web

import (
	"context"
	"errors"
	"fmt"
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
// reaches exactly the rows that view shows and no others.
type scopeSelection struct {
	MailboxID int64
	Query     string
	View      mailView
}

// scopeTrashGroup is one account's share of a scope trash: Trash belongs to an
// account, so a filter spanning accounts becomes one move per account.
type scopeTrashGroup struct {
	AccountID        int64
	Target           store.MailboxSummary
	MessageIDs       []int64
	RefreshMailboxes []string
}

type scopeTrashPlan struct {
	Groups []scopeTrashGroup
	// Matched counts resolved messages, Skipped those already in their Trash.
	Matched   int
	Skipped   int
	Truncated bool
}

// missingTrashMailboxError is returned when a resolved account has no Trash
// folder assigned, which is a setup question rather than a server failure.
type missingTrashMailboxError struct {
	account string
}

func (e missingTrashMailboxError) Error() string {
	if strings.TrimSpace(e.account) == "" {
		return "choose a Trash folder for this account before deleting messages"
	}
	return "choose a Trash folder for " + e.account + " before deleting messages"
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
	var in struct {
		ScopeMailboxID int64  `json:"scope_mailbox_id"`
		ScopeQuery     string `json:"scope_query"`
		ScopeView      string `json:"scope_view"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	view, viewKnown := parseMailView(in.ScopeView)
	if !viewKnown {
		writeAPIError(w, http.StatusBadRequest, "unknown list view")
		return
	}
	scope := scopeSelection{MailboxID: in.ScopeMailboxID, Query: strings.TrimSpace(in.ScopeQuery), View: view}
	plan, err := s.scopeTrashPlan(r.Context(), cu.User, scope)
	if err != nil {
		var missingTrash missingTrashMailboxError
		switch {
		case errors.As(err, &missingTrash):
			writeAPIError(w, http.StatusBadRequest, missingTrash.Error())
		case store.IsNotFound(err):
			http.NotFound(w, r)
		default:
			s.serverError(w, r, err)
		}
		return
	}
	if len(plan.Groups) == 0 {
		writeJSON(w, map[string]any{
			"ok": true, "queued": false, "matched": plan.Matched, "skipped": plan.Skipped,
			"truncated": plan.Truncated, "runs": []any{},
		})
		return
	}
	s.respondTrashPlan(w, r, cu.User.ID, plan, "delete")
}

// respondTrashPlan starts a plan and writes the shared Trash response, so every
// caller reports queued runs, partial starts, and truncation the same way and a
// change to that contract cannot land in one handler and miss the other. action
// names the operation in the two error messages.
func (s *Server) respondTrashPlan(w http.ResponseWriter, r *http.Request, userID int64, plan scopeTrashPlan, action string) {
	runs, queued, startErr := s.startTrashPlan(r.Context(), userID, plan)
	if len(runs) == 0 {
		switch {
		case errors.Is(startErr, errForegroundBusy):
			s.apiError(w, r, http.StatusServiceUnavailable, "could not schedule the "+action, startErr)
		case store.IsNotFound(startErr):
			http.NotFound(w, r)
		default:
			s.apiError(w, r, http.StatusBadGateway, "could not start the "+action, startErr)
		}
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

// errForegroundBusy separates "another writer holds the tenant" from a failure
// inside a move, because only the former is worth retrying unchanged.
var errForegroundBusy = errors.New("foreground operation unavailable")

// startTrashPlan runs one move per account under a single foreground
// reservation. It reports the runs it managed to start plus the first failure,
// so a partly started multi-account delete is still visible to the caller.
func (s *Server) startTrashPlan(ctx context.Context, userID int64, plan scopeTrashPlan) ([]map[string]any, int, error) {
	finishForeground := func() {}
	if s.syncRunner != nil {
		reserved, err := s.syncRunner.BeginForegroundOperation(ctx, userID)
		if err != nil {
			return nil, 0, fmt.Errorf("%w: %w", errForegroundBusy, err)
		}
		finishForeground = reserved
	}
	// One reservation covers every account's run: release it when the last one
	// reports back, so a second foreground writer cannot interleave with a
	// half-finished multi-account delete.
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
		run, err := s.syncer.StartMoveMessages(ctx, userID, group.MessageIDs, group.Target.ID, func() {
			s.startMoveRefresh(userID, group.Target.AccountID, group.RefreshMailboxes)
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
	return runs, queued, startErr
}

// scopeTrashPlan resolves a scope into one move per account, skipping messages
// that already sit in the Trash folder they would be moved to.
func (s *Server) scopeTrashPlan(ctx context.Context, user store.User, scope scopeSelection) (scopeTrashPlan, error) {
	messages, truncated, err := s.resolveScopeMessages(ctx, user, scope)
	if err != nil {
		return scopeTrashPlan{}, err
	}
	plan, err := s.trashPlanForMessages(ctx, user, messages)
	if err != nil {
		return scopeTrashPlan{}, err
	}
	plan.Truncated = truncated
	return plan, nil
}

// trashPlanForMessages groups an already resolved selection into one move per
// account. Every caller that deletes more than a page of mail shares it, because
// Trash belongs to an account and a selection rarely does.
func (s *Server) trashPlanForMessages(ctx context.Context, user store.User, messages []store.ScopeMessage) (scopeTrashPlan, error) {
	var plan scopeTrashPlan
	plan.Matched = len(messages)
	if len(messages) == 0 {
		return plan, nil
	}
	mailboxes, err := s.store.ListMailboxesForUser(ctx, user.ID)
	if err != nil {
		return plan, err
	}
	trashByAccount := map[int64]store.MailboxSummary{}
	nameByID := map[int64]string{}
	emailByAccount := map[int64]string{}
	for _, mailbox := range mailboxes {
		nameByID[mailbox.ID] = mailbox.Name
		emailByAccount[mailbox.AccountID] = mailbox.AccountEmail
		if mailbox.Role == "trash" {
			trashByAccount[mailbox.AccountID] = mailbox
		}
	}
	groups := map[int64]*scopeTrashGroup{}
	refreshSeen := map[int64]map[string]bool{}
	for _, message := range messages {
		target, ok := trashByAccount[message.AccountID]
		if !ok {
			return scopeTrashPlan{}, missingTrashMailboxError{account: emailByAccount[message.AccountID]}
		}
		if message.MailboxID == target.ID {
			plan.Skipped++
			continue
		}
		group, ok := groups[message.AccountID]
		if !ok {
			group = &scopeTrashGroup{AccountID: message.AccountID, Target: target}
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
		return s.resolveSearchScopeMessages(ctx, user, scope.Query)
	}
	if scope.MailboxID > 0 {
		if _, err := s.store.GetMailboxForUser(ctx, user.ID, scope.MailboxID); err != nil {
			return nil, false, err
		}
		messages, err := s.store.ListMailboxScopeMessagesForUser(ctx, user.ID, scope.MailboxID, scopeTrashMessageLimit+1)
		return trimScopeMessages(messages, scopeTrashMessageLimit, err)
	}
	if role := scope.View.role(); role != "" {
		messages, err := s.store.ListRoleMailScopeMessagesForUser(ctx, user.ID, role, scopeTrashMessageLimit+1)
		return trimScopeMessages(messages, scopeTrashMessageLimit, err)
	}
	if category := scope.View.category(); category != "" {
		messages, err := s.store.ListCategoryMailScopeMessagesForUser(ctx, user.ID, category, scopeTrashMessageLimit+1)
		return trimScopeMessages(messages, scopeTrashMessageLimit, err)
	}
	if scope.View == mailViewUnarchived {
		messages, err := s.store.ListUnarchivedMailScopeMessagesForUser(ctx, user.ID, scopeTrashMessageLimit+1)
		return trimScopeMessages(messages, scopeTrashMessageLimit, err)
	}
	messages, err := s.store.ListAllMailScopeMessagesForUser(ctx, user.ID, scopeTrashMessageLimit+1)
	return trimScopeMessages(messages, scopeTrashMessageLimit, err)
}

// resolveSearchScopeMessages walks the whole hit list for a search scope. Only
// messages the search itself matches are collected, so a thread is never
// trashed wholesale because one of its messages matched.
func (s *Server) resolveSearchScopeMessages(ctx context.Context, user store.User, query string) ([]store.ScopeMessage, bool, error) {
	searchQuery, mailboxFilter, err := s.searchMailboxFilter(ctx, user.ID, query)
	if err != nil {
		return nil, false, err
	}
	searchQuery, starFilter := stripStarSearchOperators(searchQuery)
	if strings.TrimSpace(searchQuery) == "" && !mailboxFilter.enabled && starFilter == nil {
		// A search box holding only whitespace lists All Mail, so it selects it too.
		messages, err := s.store.ListAllMailScopeMessagesForUser(ctx, user.ID, scopeTrashMessageLimit+1)
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

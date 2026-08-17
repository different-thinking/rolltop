// File overview: Mailbox listing, search, pagination, and message flag API handlers.

package web

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"rolltop/backend/plugins"
	"rolltop/backend/store"
)

// mailView names a whole-account list that is not a single folder. All Mail is
// the default and the others narrow or replace it, so a named view is only
// meaningful when no mailbox is selected.
type mailView string

const (
	mailViewAll        mailView = ""
	mailViewUnarchived mailView = "unarchived"
	mailViewSent       mailView = "sent"
	mailViewDrafts     mailView = "drafts"
)

// role maps the views that are simply "every folder carrying this role" onto
// that role. An empty result means the view is built some other way.
func (v mailView) role() string {
	switch v {
	case mailViewSent:
		return "sent"
	case mailViewDrafts:
		return "drafts"
	default:
		return ""
	}
}

// parseMailView resolves a view name. The bool reports whether the value names
// a list this server can render: an unknown name is a request for a view that
// does not exist rather than a silent fall back to All Mail.
func parseMailView(raw string) (mailView, bool) {
	switch view := mailView(strings.ToLower(strings.TrimSpace(raw))); view {
	case mailViewAll, mailViewUnarchived, mailViewSent, mailViewDrafts:
		return view, true
	default:
		return mailViewAll, false
	}
}

// mailViewFromRequest reads the named view from the URL.
func mailViewFromRequest(r *http.Request) (mailView, bool) {
	return parseMailView(r.URL.Query().Get("view"))
}

// apiMail returns a paged conversation list for All Mail, a named view, or one
// mailbox. It asks SQLite for extra rows because conversation grouping can
// collapse several message rows into one visible thread.
func (s *Server) apiMail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	timing := newSearchTiming()
	page := pageFromRequest(r)
	var mailboxID int64
	if raw := strings.TrimSpace(r.URL.Query().Get("mailbox")); raw != "" {
		id, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil || id <= 0 {
			http.NotFound(w, r)
			return
		}
		mailboxID = id
	}
	// A named view describes the whole-account list, so pairing it with a single
	// mailbox is a contradiction rather than a narrowing.
	view, viewKnown := mailViewFromRequest(r)
	if !viewKnown || (view != mailViewAll && mailboxID != 0) {
		http.NotFound(w, r)
		return
	}
	order := mailSortFromRequest(r)
	cacheKey := mailListCacheKey{UserID: cu.User.ID, MailboxID: mailboxID, Page: page, Sort: mailSortCacheKey(order), View: string(view)}
	if mailboxID == 0 && page == 1 && view == mailViewAll && s.writeMailListPageIfFresh(w, r, cacheKey) {
		return
	}
	if s.writeMailListNotModifiedIfFresh(w, r, cacheKey) {
		return
	}
	generation := s.mailListGeneration(cu.User.ID)
	response, err := s.mailPageResponse(r.Context(), cu.User, mailboxID, view, page, order, timing)
	if err != nil {
		if store.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		s.serverError(w, r, err)
		return
	}
	writeMailTimingHeaders(w, timing, page)
	etag, ok := writeJSONCachedWithETag(w, r, response)
	if ok {
		s.rememberMailListETag(cacheKey, etag, generation)
	}
}

// mailSortFromRequest reads the list's date direction from the URL. Only the
// reversed order has to be spelled out; every other value keeps the newest-first
// default that the warmed first page and stale clients rely on.
func mailSortFromRequest(r *http.Request) store.ThreadListOrder {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sort"))) {
	case "oldest", "asc", "date_asc":
		return store.ThreadListOldestFirst
	default:
		return store.ThreadListNewestFirst
	}
}

// mailSortCacheKey keeps the default order on the empty cache key so warmed
// first pages stay reachable, and only the reversed order gets its own entry.
func mailSortCacheKey(order store.ThreadListOrder) string {
	if order == store.ThreadListOldestFirst {
		return string(store.ThreadListOldestFirst)
	}
	return ""
}

func (s *Server) mailPageResponse(ctx context.Context, user store.User, mailboxID int64, view mailView, page int, order store.ThreadListOrder, timing *searchTiming) (map[string]any, error) {
	const pageSize = 50
	offset := (page - 1) * pageSize
	fetchLimit := pageSize*3 + 1
	var activeMailbox *apiMailbox
	var messages []store.MessageRecord
	var err error
	if mailboxID != 0 {
		mb, mbErr := s.store.GetMailboxForUser(ctx, user.ID, mailboxID)
		if mbErr != nil {
			return nil, mbErr
		}
		active := apiMailboxFromStore(mb)
		activeMailbox = &active
		hydrateDone := timing.measure(&timing.hydrate)
		messages, err = s.store.ListLatestThreadMessagesForMailbox(ctx, user.ID, mb.ID, fetchLimit, offset, order)
		hydrateDone()
	} else {
		hydrateDone := timing.measure(&timing.hydrate)
		switch role := view.role(); {
		case role != "":
			messages, err = s.store.ListRoleLatestThreadMessagesForUser(ctx, user.ID, role, fetchLimit, offset, order)
		case view == mailViewUnarchived:
			messages, err = s.store.ListUnarchivedLatestThreadMessagesForUser(ctx, user.ID, fetchLimit, offset, order)
		default:
			messages, err = s.store.ListLatestThreadMessagesForUser(ctx, user.ID, fetchLimit, offset, order)
		}
		hydrateDone()
	}
	if err != nil {
		return nil, err
	}
	timing.seeds = len(messages)
	own := s.ownAddresses(ctx, user)
	renderDone := timing.measure(&timing.render)
	conversations, err := s.conversationViews(ctx, user.ID, messages, own)
	renderDone()
	if err != nil {
		return nil, err
	}
	hasNext := len(conversations) > pageSize
	if hasNext {
		conversations = conversations[:pageSize]
	}
	return map[string]any{
		"conversations":  s.apiConversationsWithAnnotations(ctx, user.ID, conversations),
		"page":           page,
		"has_prev":       page > 1,
		"has_next":       hasNext,
		"sort":           string(order),
		"active_mailbox": activeMailbox,
	}, nil
}

// apiSearch combines URL query parsing, optional mailbox filtering, sender-history
// boosts, Bleve search hits, and SQLite conversation hydration into the search
// result payload consumed by SearchView.
func (s *Server) apiSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	const pageSize = 50
	timing := newSearchTiming()
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	filterDone := timing.measure(&timing.filter)
	searchQuery, mailboxFilter, err := s.searchMailboxFilter(r.Context(), cu.User.ID, q)
	filterDone()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if searchQuery != "" && strings.Contains(strings.ToLower(searchQuery), "lang:") && !s.pluginEnabled(r.Context(), plugins.LanguageSearch) {
		writeAPIError(w, http.StatusBadRequest, "Language search is disabled.")
		return
	}
	// The index repair has to run before the ETag check, not after it: repairing a
	// missing document is what invalidates the cached result set, so a revalidated
	// request would otherwise keep serving the incomplete answer forever.
	if strings.TrimSpace(searchQuery) != "" {
		if _, err := s.ensureRecentSearchDocuments(r.Context(), cu.User.ID); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	page := pageFromRequest(r)
	cacheKey := mailListCacheKey{UserID: cu.User.ID, Page: page, Search: true, Query: q}
	if s.writeSearchNotModifiedIfFresh(w, r, cacheKey) {
		return
	}
	generation := s.mailListGeneration(cu.User.ID)
	offset := (page - 1) * pageSize
	own := s.ownAddresses(r.Context(), cu.User)
	var seeds []conversationSeed
	if searchQuery == "" && !mailboxFilter.enabled {
		var messages []store.MessageRecord
		hydrateDone := timing.measure(&timing.hydrate)
		messages, err = s.store.ListLatestThreadMessagesForUser(r.Context(), cu.User.ID, pageSize*3+1, offset, store.ThreadListNewestFirst)
		hydrateDone()
		seeds = conversationSeedsFromMessages(messages)
	} else {
		boostDone := timing.measure(&timing.sender)
		opts := s.searchOptionsWithRankingBoosts(r.Context(), cu.User)
		boostDone()
		seeds, err = s.searchConversationSeedHits(r.Context(), cu.User.ID, searchQuery, page, pageSize, opts, own, mailboxFilter, timing)
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	timing.seeds = len(seeds)
	renderDone := timing.measure(&timing.render)
	conversations, err := s.conversationViewsWithSearchDetails(r.Context(), cu.User.ID, seeds, own, searchQuery)
	renderDone()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	hasNext := len(conversations) > pageSize
	if hasNext {
		conversations = conversations[:pageSize]
	}
	writeSearchTimingHeaders(w, timing, page)
	etag, ok := writeJSONCachedWithETag(w, r, map[string]any{
		"conversations": s.apiConversationsWithAnnotations(r.Context(), cu.User.ID, conversations),
		"page":          page,
		"has_prev":      page > 1,
		"has_next":      hasNext,
		"query":         q,
	})
	if ok {
		s.rememberMailListETag(cacheKey, etag, generation)
	}
}

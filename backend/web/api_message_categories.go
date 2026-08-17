// File overview: The category sidebar payload and the correction endpoint
// behind "this sender belongs somewhere else".

package web

import (
	"context"
	"net/http"
	"strings"
	"sync"

	"rolltop/backend/mailparse"
	"rolltop/backend/store"
)

// apiMailCategory is one entry in the categories section of the sidebar. The
// name is both the stored category and the view name, so the browser never has
// to translate between the two.
type apiMailCategory struct {
	Name   string `json:"name"`
	Label  string `json:"label"`
	Icon   string `json:"icon"`
	Total  int    `json:"total"`
	Unread int    `json:"unread"`
}

// mailCategoriesFromCounts renders the sidebar entries. Every category is
// listed whether or not it holds anything, so the section does not grow a new
// entry the first time a message happens to land in one. The names and display
// text come from the classifier's own registry: a category cannot exist here
// without existing there.
func mailCategoriesFromCounts(counts map[string]store.CategoryCounts) []apiMailCategory {
	registry := mailparse.CategoryRegistry()
	out := make([]apiMailCategory, 0, len(registry))
	for _, category := range registry {
		count := counts[category.Name]
		out = append(out, apiMailCategory{
			Name:   category.Name,
			Label:  category.Label,
			Icon:   category.Icon,
			Total:  count.Total,
			Unread: count.Unread,
		})
	}
	return out
}

// emptyMailCategories is what a tenant whose database cannot be read gets: the
// section still lists what exists, with nothing counted in it.
func emptyMailCategories() []apiMailCategory {
	return mailCategoriesFromCounts(nil)
}

// mailCategoryChromeEntry is one tenant's cached sidebar numbers.
type mailCategoryChromeEntry struct {
	Generation uint64
	Categories []apiMailCategory
	Pending    int
}

// mailCategoryChromeCache holds the counts between changes to a tenant's mail.
// The numbers come from aggregates over every message the tenant owns, and they
// are rebuilt for the bootstrap payload and again for every live chrome event —
// which arrive per connected tab, several times a minute during a sync. Reusing
// them until the mail-list generation moves turns that back into one aggregate
// per actual change.
type mailCategoryChromeCache struct {
	mu      sync.Mutex
	entries map[int64]mailCategoryChromeEntry
}

func newMailCategoryChromeCache() *mailCategoryChromeCache {
	return &mailCategoryChromeCache{entries: map[int64]mailCategoryChromeEntry{}}
}

func (c *mailCategoryChromeCache) lookup(userID int64, generation uint64) (mailCategoryChromeEntry, bool) {
	if c == nil {
		return mailCategoryChromeEntry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[userID]
	return entry, ok && entry.Generation == generation
}

func (c *mailCategoryChromeCache) remember(userID int64, entry mailCategoryChromeEntry) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// One entry per signed-in tenant is a bounded set in this app, but a server
	// that has served many users over a long uptime should not keep every one
	// of them: the cheapest correct answer is to start the map over.
	if len(c.entries) > 512 {
		c.entries = map[int64]mailCategoryChromeEntry{}
	}
	c.entries[userID] = entry
}

// mailCategoryChrome builds the sidebar's category entries with their counts,
// reusing the last answer while the tenant's mail has not changed.
func (s *Server) mailCategoryChrome(ctx context.Context, userID int64) ([]apiMailCategory, int, error) {
	generation := s.mailListGeneration(userID)
	if entry, ok := s.mailCategoryCache.lookup(userID, generation); ok {
		return entry.Categories, entry.Pending, nil
	}
	counts, err := s.store.CountMessagesByCategoryForUser(ctx, userID)
	if err != nil {
		return nil, 0, err
	}
	pending, err := s.store.CountMessagesNeedingCategory(ctx, userID)
	if err != nil {
		return nil, 0, err
	}
	categories := mailCategoriesFromCounts(counts)
	s.mailCategoryCache.remember(userID, mailCategoryChromeEntry{
		Generation: generation,
		Categories: categories,
		Pending:    pending,
	})
	return categories, pending, nil
}

// apiMessageCategory moves every message from one sender into a category and
// remembers the choice. Corrections are per sender rather than per message
// because the misclassification is a property of the sender: filing one message
// by hand and watching the next twenty arrive in the same wrong list is not a
// correction the user would call finished.
func (s *Server) apiMessageCategory(w http.ResponseWriter, r *http.Request) {
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
	var in struct {
		MessageID int64  `json:"message_id"`
		Category  string `json:"category"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	category := strings.ToLower(strings.TrimSpace(in.Category))
	if !mailparse.ValidCategory(category) {
		writeAPIError(w, http.StatusBadRequest, "unknown category")
		return
	}
	// The request names the row the user acted on and the server reads its
	// sender, so a correction can only ever be aimed at an address that appears
	// in the caller's own mail. Taking an address from the request body instead
	// would let overrides be created for senders the tenant never heard from.
	if in.MessageID <= 0 {
		writeAPIError(w, http.StatusBadRequest, "message_id is required")
		return
	}
	msg, err := s.store.GetMessageEnvelopeForUser(r.Context(), cu.User.ID, in.MessageID)
	if err != nil {
		if store.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		s.serverError(w, r, err)
		return
	}
	sender := store.NormalizeCategorySender(msg.FromAddr)
	if sender == "" {
		writeAPIError(w, http.StatusBadRequest, "no sender address to file under")
		return
	}
	moved, err := s.store.SetSenderCategoryOverride(r.Context(), cu.User.ID, sender, category)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.noteMailListChanged(cu.User.ID)
	if s.events != nil {
		s.events.Notify(cu.User.ID)
	}
	writeJSON(w, map[string]any{
		"category": category,
		"sender":   sender,
		"moved":    moved,
	})
}

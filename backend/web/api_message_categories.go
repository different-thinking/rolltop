// File overview: The category sidebar payload and the correction endpoint
// behind "this sender belongs somewhere else".

package web

import (
	"context"
	"net/http"
	"strings"

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

// mailCategoryLabels names each category for the sidebar. Labels are display
// text and may be reworded freely; the names beside them are stored data.
var mailCategoryLabels = map[string]struct {
	Label string
	Icon  string
}{
	mailparse.CategoryRelevant:      {Label: "Relevant", Icon: "person"},
	mailparse.CategoryNewsletters:   {Label: "Newsletters", Icon: "newspaper"},
	mailparse.CategoryForums:        {Label: "Forums", Icon: "forum"},
	mailparse.CategoryNotifications: {Label: "Notifications", Icon: "notifications"},
}

// mailCategoryChrome builds the sidebar's category entries with their unread
// counts. A tenant whose classification has not run yet still gets the full set
// of entries: the lists are simply empty, which is the honest state, rather than
// the section appearing later without explanation.
func (s *Server) mailCategoryChrome(ctx context.Context, userID int64) ([]apiMailCategory, int, error) {
	counts, err := s.store.CountMessagesByCategoryForUser(ctx, userID)
	if err != nil {
		return nil, 0, err
	}
	pending, err := s.store.CountMessagesNeedingCategory(ctx, userID)
	if err != nil {
		return nil, 0, err
	}
	names := mailparse.Categories()
	out := make([]apiMailCategory, 0, len(names))
	for _, name := range names {
		display := mailCategoryLabels[name]
		count := counts[name]
		out = append(out, apiMailCategory{Name: name, Label: display.Label, Icon: display.Icon, Total: count.Total, Unread: count.Unread})
	}
	return out, pending, nil
}

// emptyMailCategories is what a tenant whose database cannot be read gets: the
// section still lists what exists, with nothing counted in it.
func emptyMailCategories() []apiMailCategory {
	names := mailparse.Categories()
	out := make([]apiMailCategory, 0, len(names))
	for _, name := range names {
		display := mailCategoryLabels[name]
		out = append(out, apiMailCategory{Name: name, Label: display.Label, Icon: display.Icon})
	}
	return out
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
		Sender    string `json:"sender"`
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
	// A message id is the ordinary path: the browser names the row the user
	// acted on and the server reads its sender, so a correction can never be
	// aimed at an address the user cannot see in their own mail.
	sender := strings.TrimSpace(in.Sender)
	if in.MessageID > 0 {
		msg, err := s.store.GetMessageForUser(r.Context(), cu.User.ID, in.MessageID)
		if err != nil {
			if store.IsNotFound(err) {
				http.NotFound(w, r)
				return
			}
			s.serverError(w, r, err)
			return
		}
		sender = msg.FromAddr
	}
	if store.NormalizeCategorySender(sender) == "" {
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
		"sender":   store.NormalizeCategorySender(sender),
		"moved":    moved,
	})
}

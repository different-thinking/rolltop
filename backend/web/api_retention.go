// File overview: The retention policy route. It reads and writes how long mail is
// kept in each category before it is thrown away, and how long the Trash keeps
// what was thrown away before the server is told to delete it.

package web

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"rolltop/backend/mailparse"
	"rolltop/backend/store"
)

// apiCategoryRetention is one category's rule as the browser states it. The
// cutoff travels in both spellings because both are stored: a relative rule
// resolved to a date on save would stop being a retention policy the next day.
type apiCategoryRetention struct {
	Category string `json:"category"`
	Mode     string `json:"mode"`
	// Count and Unit are the relative cutoff, on the calendar: 6 months is six
	// months rather than a rounded number of days.
	Count int    `json:"count"`
	Unit  string `json:"unit"`
	// Before is the fixed cutoff, sent the way the archive and delete dialogs
	// send theirs: a timestamp naming the instant the reader's chosen day begins
	// at, or a bare calendar date read as the start of that day in UTC.
	Before string `json:"before"`
}

// apiRetentionSettings is the whole policy plus the labels the settings page
// needs, so the page does not carry a second list of what the categories are.
type apiRetentionSettings struct {
	TrashEnabled bool                   `json:"trash_enabled"`
	TrashDays    int                    `json:"trash_days"`
	Categories   []apiCategoryRetention `json:"categories"`
}

func apiRetentionFromStore(settings store.RetentionSettings) apiRetentionSettings {
	out := apiRetentionSettings{
		TrashEnabled: settings.TrashEnabled,
		TrashDays:    settings.TrashDays,
		Categories:   make([]apiCategoryRetention, 0, len(settings.Categories)),
	}
	for _, rule := range settings.Categories {
		entry := apiCategoryRetention{Category: rule.Category, Mode: rule.Mode, Count: rule.Count, Unit: rule.Unit}
		if !rule.Before.IsZero() {
			entry.Before = rule.Before.UTC().Format(time.RFC3339)
		}
		out.Categories = append(out.Categories, entry)
	}
	return out
}

// apiRetention reads and writes one reader's retention policy.
func (s *Server) apiRetention(w http.ResponseWriter, r *http.Request) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		settings, err := s.store.GetRetentionSettings(r.Context(), cu.User.ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		writeJSON(w, map[string]any{"retention": apiRetentionFromStore(settings)})
	case http.MethodPost:
		if !s.verifyCSRF(w, r) {
			return
		}
		var in apiRetentionSettings
		if !decodeJSON(w, r, &in) {
			return
		}
		settings := store.RetentionSettings{
			UserID:       cu.User.ID,
			TrashEnabled: in.TrashEnabled,
			TrashDays:    in.TrashDays,
			Categories:   make([]store.CategoryRetention, 0, len(in.Categories)),
		}
		for _, rule := range in.Categories {
			mode := strings.ToLower(strings.TrimSpace(rule.Mode))
			parsed := store.CategoryRetention{Category: rule.Category, Mode: mode, Count: rule.Count, Unit: rule.Unit}
			if mode == store.RetentionModeFixed {
				before, err := parseScopeCutoff(rule.Before)
				if err != nil || before.IsZero() {
					writeAPIError(w, http.StatusBadRequest,
						"choose the date to delete "+retentionCategoryLabel(rule.Category)+" mail before")
					return
				}
				parsed.Before = before
			}
			settings.Categories = append(settings.Categories, parsed)
		}
		saved, err := s.store.SaveRetentionSettings(r.Context(), settings)
		if errors.Is(err, store.ErrInvalidRetentionSettings) {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		// Saving clears the sweep marks, so the policy is due now rather than at
		// the end of an interval that started before it existed.
		s.wakeRetentionScheduler()
		writeJSON(w, map[string]any{"retention": apiRetentionFromStore(saved)})
	default:
		methodNotAllowed(w)
	}
}

// retentionCategoryLabel renders a category the way the sidebar names it, so an
// error message about Newsletters does not say "newsletters". The registry is
// the one place the set is defined, so an unknown name is echoed as it came.
func retentionCategoryLabel(name string) string {
	normalized := strings.ToLower(strings.TrimSpace(name))
	for _, category := range mailparse.CategoryRegistry() {
		if category.Name == normalized {
			return category.Label
		}
	}
	return name
}

// File overview: Cross-account duplicate copy reporting and cleanup. An account
// that aggregates other mailboxes hands the mirror a second row for a delivery
// the original account already holds. The store hides those rows; these routes
// let the user see how many there are and move them to the aggregating
// account's Trash so the server stops sending them.

package web

import (
	"errors"
	"net/http"
	"sort"

	"rolltop/backend/store"
)

// duplicateAccountSummary reports one account's share of the hidden copies.
type duplicateAccountSummary struct {
	AccountID int64  `json:"account_id"`
	Email     string `json:"email"`
	Label     string `json:"label"`
	Hidden    int    `json:"hidden"`
}

// apiAccountDuplicates reports the copies currently hidden behind an original,
// grouped by the account holding them.
func (s *Server) apiAccountDuplicates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	summaries, total, err := s.duplicateAccountSummaries(r, cu.User.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "hidden": total, "accounts": summaries})
}

// apiAccountDuplicatesRescan re-runs detection over the whole tenant. Sync
// classifies each message as it arrives, so this is the repair path: mail that
// was mirrored before detection existed, and accounts that gained an alias after
// their mail arrived, are only reconsidered here.
func (s *Server) apiAccountDuplicatesRescan(w http.ResponseWriter, r *http.Request) {
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
	stats, err := s.store.RefreshDuplicateCopiesForUser(r.Context(), cu.User.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.notifyUserChanged(cu.User.ID)
	summaries, total, err := s.duplicateAccountSummaries(r, cu.User.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, map[string]any{
		"ok": true, "hidden": total, "accounts": summaries,
		"groups": stats.Groups, "newly_hidden": stats.Hidden,
		"revealed": stats.Revealed, "truncated": stats.Truncated,
	})
}

// apiAccountDuplicatesTrash moves every hidden copy into the Trash folder of the
// account holding it. Nothing is deleted remotely: the copies land in the
// aggregating account's Trash, reconciliation drops the local rows once the
// server stops listing them in their old folder, and the original account's mail
// is never touched.
func (s *Server) apiAccountDuplicatesTrash(w http.ResponseWriter, r *http.Request) {
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
	messages, err := s.store.ListHiddenDuplicateCopiesForUser(r.Context(), cu.User.ID, scopeTrashMessageLimit+1)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	truncated := len(messages) > scopeTrashMessageLimit
	if truncated {
		messages = messages[:scopeTrashMessageLimit]
	}
	plan, err := s.trashPlanForMessages(r.Context(), cu.User, messages)
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
	plan.Truncated = truncated
	if len(plan.Groups) == 0 {
		writeJSON(w, map[string]any{
			"ok": true, "queued": false, "matched": plan.Matched, "skipped": plan.Skipped,
			"truncated": plan.Truncated, "runs": []any{},
		})
		return
	}
	runs, queued, startErr := s.startTrashPlan(r.Context(), cu.User.ID, plan)
	if len(runs) == 0 {
		if errors.Is(startErr, errForegroundBusy) {
			s.apiError(w, r, http.StatusServiceUnavailable, "could not schedule the cleanup", startErr)
			return
		}
		if store.IsNotFound(startErr) {
			http.NotFound(w, r)
			return
		}
		s.apiError(w, r, http.StatusBadGateway, "could not start the cleanup", startErr)
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

// duplicateAccountSummaries joins the per-account counts with the account names
// the settings view shows. Accounts holding no hidden copy are left out.
func (s *Server) duplicateAccountSummaries(r *http.Request, userID int64) ([]duplicateAccountSummary, int, error) {
	counts, err := s.store.CountHiddenDuplicateCopiesForUser(r.Context(), userID)
	if err != nil {
		return nil, 0, err
	}
	if len(counts) == 0 {
		return []duplicateAccountSummary{}, 0, nil
	}
	accounts, err := s.store.ListMailAccountsForUser(r.Context(), userID)
	if err != nil {
		return nil, 0, err
	}
	out := make([]duplicateAccountSummary, 0, len(counts))
	total := 0
	for _, account := range accounts {
		hidden := counts[account.ID]
		if hidden == 0 {
			continue
		}
		total += hidden
		out = append(out, duplicateAccountSummary{
			AccountID: account.ID,
			Email:     account.Email,
			Label:     account.Label,
			Hidden:    hidden,
		})
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Hidden == out[b].Hidden {
			return out[a].AccountID < out[b].AccountID
		}
		return out[a].Hidden > out[b].Hidden
	})
	return out, total, nil
}

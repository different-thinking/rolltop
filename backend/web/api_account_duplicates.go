// File overview: Cross-account duplicate copy reporting and cleanup. An account
// that aggregates other mailboxes hands the mirror a second row for a delivery
// the original account already holds. The store hides those rows; these routes
// let the user see how many there are and move them to the aggregating
// account's Trash so the server stops sending them.

package web

import (
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
	// A tenant with more duplicate groups than one pass covers resumes from the
	// cursor the previous pass returned, so repeating the request finishes the
	// mailbox instead of re-reading its first groups.
	var in struct {
		After string `json:"after"`
	}
	if r.ContentLength > 0 && !decodeJSON(w, r, &in) {
		return
	}
	stats, err := s.store.RefreshDuplicateCopiesForUser(r.Context(), cu.User.ID, in.After)
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
	payload := map[string]any{
		"ok": true, "hidden": total, "accounts": summaries,
		"groups": stats.Groups, "newly_hidden": stats.Hidden,
		"revealed": stats.Revealed, "truncated": stats.Truncated,
		"next": stats.NextHeader, "outcomes": duplicateScanOutcomes(stats),
	}
	// Copies one account holds of its own mail are outside detection entirely.
	// Counting them is what separates "Rolltop looked and left these visible on
	// purpose" from "Rolltop never saw them", which is the whole question a scan
	// that hides nothing raises.
	//
	// It is a grouping query over the tenant's messages and it answers for the
	// whole mailbox, so it runs on the pass that finishes the scan rather than on
	// each of the up-to-two-hundred passes a large mailbox takes - which would
	// repeat one whole-mailbox scan per page for a figure only the last page's
	// answer is ever read from.
	if !stats.Truncated {
		withinAccount, err := s.store.CountWithinAccountDuplicatedMessagesForUser(r.Context(), cu.User.ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		payload["within_account_messages"] = withinAccount
	}
	writeJSON(w, payload)
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
		s.writeScopePlanError(w, r, err)
		return
	}
	plan.Truncated = truncated
	if s.writeEmptyScopePlan(w, plan) {
		return
	}
	s.respondMovePlan(w, r, cu.User.ID, plan, "cleanup")
}

// duplicateScanOutcomes renders the store's per-group decisions as plain JSON
// keys. The counts are of groups, not of messages: one group is one Message-ID
// several accounts hold, and what the view reports is how many of those
// detection declined to act on and why.
func duplicateScanOutcomes(stats store.DuplicateScanStats) map[string]int {
	out := map[string]int{}
	for outcome, count := range stats.Outcomes {
		if count > 0 {
			out[string(outcome)] = count
		}
	}
	return out
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

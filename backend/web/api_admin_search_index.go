// File overview: Admin API for the per-user search index card.
//
// The search index is the one piece of derived state an operator still has to
// be able to act on. It lives on the data volume rather than in PostgreSQL, it
// can be damaged independently of the mail it describes, and rebuilding it
// costs nothing but time — the messages are all still there. Until now the only
// way to do that was the offline `rolltop reset-search` command, which is no
// help to anyone running the server as a container without a shell.
//
// Reading is deliberately side-effect free: the status walks the directory
// instead of opening the index, so viewing this page can never quarantine
// anything. Only the POST acts, and it is admin-only and CSRF-protected because
// it re-reads a tenant's whole mailbox.
//
// The rebuild is the same one the account settings already offer per account —
// purge each search-visible folder's documents, then index what the folder has
// that the index does not — run here for every account a tenant owns. It is
// deliberately not a second implementation: marking rows and hoping a worker
// picks them up does not refill an index, because the flag that looks like a
// reindex queue (`attachment_indexed_at`) is an attachment-enrichment flag that
// the maintenance worker clears without indexing anything.

package web

import (
	"context"
	"log"
	"net/http"
	"time"

	"rolltop/backend/store"
)

// searchIndexTimeout bounds one card render or rebuild. A rebuild is a directory
// rename plus one UPDATE; the walk behind the sizes is the slower half.
const searchIndexTimeout = 2 * time.Minute

// searchIndexMaxBody caps the request body, matching the other admin routes.
const searchIndexMaxBody = 64 << 10

// searchIndexTenant is one row of the card.
type searchIndexTenant struct {
	UserID int64  `json:"user_id"`
	Email  string `json:"email"`
	Name   string `json:"name,omitempty"`
	// Present is false when the tenant has no live index yet, which is the
	// normal state for a new user and the state right after a rebuild.
	Present bool  `json:"present"`
	Bytes   int64 `json:"bytes"`
	// FoldersNeedingRebuild is how many of the tenant's search-visible folders
	// have coverage nothing has verified — a folder whose index write was
	// dropped, or every folder after an index was quarantined. It is the number
	// the rebuild acts on, so it is the number worth showing.
	FoldersNeedingRebuild int64 `json:"folders_needing_rebuild"`
	// Error is why this tenant's numbers could not be read, if they could not.
	// One unreadable tenant must not blank the card for the others.
	Error string `json:"error,omitempty"`
}

type searchIndexReport struct {
	Tenants []searchIndexTenant `json:"tenants"`
	// Rebuilt names the tenant a POST acted on, so the response can be rendered
	// as a confirmation without the client having to correlate it.
	Rebuilt int64 `json:"rebuilt,omitempty"`
	// StartedRuns is how many per-account rebuild runs the request queued, and
	// BusyAccounts how many could not start because that account was already
	// syncing or reindexing.
	StartedRuns  int `json:"started_runs,omitempty"`
	BusyAccounts int `json:"busy_accounts,omitempty"`
}

func (s *Server) apiAdminSearchIndex(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAPIAdmin(w, r); !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		ctx, cancel := context.WithTimeout(r.Context(), searchIndexTimeout)
		defer cancel()
		report, err := s.searchIndexReport(ctx)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		writeJSON(w, report)
	case http.MethodPost:
		s.apiAdminSearchIndexRebuild(w, r)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) apiAdminSearchIndexRebuild(w http.ResponseWriter, r *http.Request) {
	if !s.verifyCSRF(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, searchIndexMaxBody)
	var in struct {
		UserID int64 `json:"user_id"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.UserID <= 0 {
		writeAPIError(w, http.StatusBadRequest, "Choose which user's search index to rebuild.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), searchIndexTimeout)
	defer cancel()
	if _, err := s.store.GetUserByID(ctx, in.UserID); err != nil {
		if store.IsNotFound(err) {
			writeAPIError(w, http.StatusNotFound, "That user does not exist.")
			return
		}
		s.serverError(w, r, err)
		return
	}
	// Checked after the request has been validated, so a malformed one is
	// answered by what is wrong with it rather than by what this server cannot
	// do about it.
	if s.syncer == nil || s.syncer.Search == nil || s.syncRunner == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "Search indexing is not configured on this server.")
		return
	}
	started, busy, err := s.startSearchRebuildForUser(ctx, in.UserID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if started == 0 {
		if busy > 0 {
			writeAPIError(w, http.StatusConflict,
				"Sync or full-text reindexing is already running for this user's mail servers.")
			return
		}
		writeAPIError(w, http.StatusBadRequest, "This user has no search-visible folders to rebuild.")
		return
	}
	s.notifyUserChanged(in.UserID)
	report, err := s.searchIndexReport(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	report.Rebuilt = in.UserID
	report.StartedRuns = started
	report.BusyAccounts = busy
	writeJSON(w, report)
}

// startSearchRebuildForUser queues one rebuild run per mail account. Accounts
// whose runner is busy are reported rather than waited on: the caller is an
// HTTP request, and a sync that has just started can take hours.
func (s *Server) startSearchRebuildForUser(ctx context.Context, userID int64) (started, busy int, err error) {
	accounts, err := s.store.ListMailAccountsForUser(ctx, userID)
	if err != nil {
		return 0, 0, err
	}
	summaries, err := s.store.ListMailboxesForUser(ctx, userID)
	if err != nil {
		return 0, 0, err
	}
	for _, account := range accounts {
		mailboxes := make([]store.Mailbox, 0)
		for _, summary := range summaries {
			if summary.AccountID == account.ID && summary.IncludeInSearch {
				mailboxes = append(mailboxes, summary.Mailbox)
			}
		}
		if len(mailboxes) == 0 {
			continue
		}
		_, ok, startErr := s.syncRunner.StartAccountSearchRebuildToCompletion(userID, account, mailboxes,
			"Rebuilding full-text indexes", func(runCtx context.Context, runID int64, progress *store.SyncProgress) error {
				for i, mailbox := range mailboxes {
					if err := s.rebuildMailboxSearchIndex(runCtx, userID, mailbox, runID, progress); err != nil {
						return err
					}
					progress.MailboxesDone = i + 1
					if err := s.store.UpdateSyncRunProgress(runCtx, userID, runID, *progress); err != nil {
						return err
					}
				}
				return nil
			})
		if startErr != nil {
			return started, busy, startErr
		}
		if !ok {
			busy++
			continue
		}
		started++
	}
	log.Printf("search index rebuild requested user_id=%d runs_started=%d accounts_busy=%d", userID, started, busy)
	return started, busy, nil
}

func (s *Server) searchIndexReport(ctx context.Context) (searchIndexReport, error) {
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return searchIndexReport{}, err
	}
	report := searchIndexReport{Tenants: make([]searchIndexTenant, 0, len(users))}
	for _, user := range users {
		tenant := searchIndexTenant{UserID: user.ID, Email: user.Email, Name: user.Name}
		if s.search != nil {
			tenant.Bytes, tenant.Present = s.search.PerUserIndexBytes(user.ID)
		}
		pending, err := s.store.CountMailboxesNeedingSearchIndexRepair(ctx, user.ID)
		if err != nil {
			// The row says only that the number is missing. Without this line
			// the reason is nowhere - not even in the log tail further down
			// the same page, which is where an operator looks next.
			log.Printf("read search index repair state user_id=%d error_type=%T error=%v", user.ID, err, err)
			tenant.Error = "Could not read this user's indexing state."
		} else {
			tenant.FoldersNeedingRebuild = pending
		}
		report.Tenants = append(report.Tenants, tenant)
	}
	return report, nil
}

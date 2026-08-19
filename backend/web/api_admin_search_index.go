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
// it throws away an index and schedules a full reindex of a tenant's mail.

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
	// PendingMessages is how many messages are still waiting to be indexed.
	// After a rebuild this is the tenant's whole searchable mailbox, and it
	// falls as the indexing worker gets through it.
	PendingMessages int64 `json:"pending_messages"`
	// Error is why this tenant's numbers could not be read, if they could not.
	// One unreadable tenant must not blank the card for the others.
	Error string `json:"error,omitempty"`
}

type searchIndexReport struct {
	Tenants []searchIndexTenant `json:"tenants"`
	// Rebuilt names the tenant a POST acted on, so the response can be rendered
	// as a confirmation without the client having to correlate it.
	Rebuilt int64 `json:"rebuilt,omitempty"`
	// QueuedMessages is how many messages the rebuild marked for reindexing.
	QueuedMessages int64 `json:"queued_messages,omitempty"`
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
	if s.search == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "Search is not configured on this server.")
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
	// Queued before the index moves, for the reason search.CorruptIndexHandler
	// describes: the reverse order can leave an empty index with every row
	// still flagged as indexed, and a search that answers nothing forever.
	queued, err := s.store.MarkSearchVisibleMessagesPendingIndex(ctx, in.UserID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	quarantine, err := s.search.RebuildPerUserIndex(ctx, in.UserID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	log.Printf("search index rebuild requested user_id=%d queued_messages=%d quarantine=%q",
		in.UserID, queued, quarantine.QuarantinePath)
	report, err := s.searchIndexReport(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	report.Rebuilt = in.UserID
	report.QueuedMessages = queued
	writeJSON(w, report)
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
		pending, err := s.store.CountMessagesNeedingSearchIndex(ctx, user.ID)
		if err != nil {
			tenant.Error = "Could not read this user's indexing state."
		} else {
			tenant.PendingMessages = pending
		}
		report.Tenants = append(report.Tenants, tenant)
	}
	return report, nil
}

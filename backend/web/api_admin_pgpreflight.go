// File overview: Admin API for the PostgreSQL migration preflight. The admin
// pastes a candidate DSN into the Database page and this route runs
// backend/pgpreflight against it from inside the app container — the same
// network path the migrated store would use. The DSN is held for the one
// request only: it is never persisted, never logged, and never echoed back;
// the response carries only check results. Admin-only and CSRF-protected
// because it makes the server open an outbound connection to an
// admin-chosen host.

package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"rolltop/backend/pgpreflight"
)

// pgPreflightTimeout bounds the whole run. Every individual check is a few
// round trips; a minute only runs out when the target is unreachable slowly.
const pgPreflightTimeout = time.Minute

func (s *Server) apiAdminPostgresPreflight(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAPIAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !s.verifyCSRF(w, r) {
		return
	}
	var in struct {
		DSN string `json:"dsn"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	dsn := strings.TrimSpace(in.DSN)
	if dsn == "" {
		writeAPIError(w, http.StatusBadRequest, "Enter the PostgreSQL connection string to test.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), pgPreflightTimeout)
	defer cancel()
	writeJSON(w, pgpreflight.Run(ctx, dsn))
}

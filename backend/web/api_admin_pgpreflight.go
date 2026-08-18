// File overview: Admin API for the PostgreSQL migration preflight. The admin
// pastes a candidate DSN into the Database page and this route runs
// backend/pgpreflight against it from inside the app container — the same
// network path the migrated store would use. The DSN is held for the one
// request only: it is never persisted, never logged, and never echoed back;
// the package redacts credential material out of driver errors before they
// reach the response. Admin-only and CSRF-protected because it makes the
// server open an outbound connection to an admin-chosen host.

package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"rolltop/backend/pgpreflight"
)

// pgPreflightTimeout bounds the whole run. Every individual check is a few
// round trips; a minute only runs out when the target is unreachable slowly.
const pgPreflightTimeout = time.Minute

// pgPreflightMaxBody caps the request body. A DSN is a short string, and the
// body-carrying handlers in this package bound their input the same way.
const pgPreflightMaxBody = 64 << 10

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
	r.Body = http.MaxBytesReader(w, r.Body, pgPreflightMaxBody)
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
	report, err := pgpreflight.Run(ctx, dsn)
	if err != nil {
		// Runs share one scratch schema and each starts by dropping it, so a
		// second concurrent run would delete the first one's tables.
		if errors.Is(err, pgpreflight.ErrBusy) {
			writeAPIError(w, http.StatusConflict, "A preflight check is already running. Wait for it to finish.")
			return
		}
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, report)
}

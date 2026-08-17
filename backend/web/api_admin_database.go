// File overview: Admin API for database maintenance. These routes expose file
// paths, sizes, and integrity state — never message content — and every
// mutation is admin-only and CSRF-protected. Scheduling a repair is the only
// destructive action here, and it never touches the database directly: it
// records the request and restarts the process so the repair runs at startup.

package web

import (
	"net/http"
	"strconv"
	"strings"

	"rolltop/backend/store"
)

func (s *Server) apiAdminDatabase(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAPIAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	overview, err := s.databaseOverview(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, overview)
}

// apiAdminDatabaseAction handles the POST endpoints under admin/database/.
func (s *Server) apiAdminDatabaseAction(w http.ResponseWriter, r *http.Request, action string) {
	cu, ok := s.requireAPIAdmin(w, r)
	if !ok {
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
		UserID  int64 `json:"user_id"`
		Confirm bool  `json:"confirm"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}

	switch strings.Trim(action, "/") {
	case "check":
		job, err := s.startIntegrityCheck(in.UserID)
		if err != nil {
			writeAPIError(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, map[string]any{"job": job})
	case "backup":
		job, err := s.startBackup(in.UserID)
		if err != nil {
			writeAPIError(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, map[string]any{"job": job})
	case "repair":
		if in.UserID <= 0 {
			writeAPIError(w, http.StatusBadRequest, "Select the user database to repair.")
			return
		}
		// A repair can lose rows on damaged pages, so the client has to say so
		// explicitly rather than the server inferring intent from a click.
		if !in.Confirm {
			writeAPIError(w, http.StatusBadRequest, "Confirm the repair before scheduling it.")
			return
		}
		if err := s.scheduleDatabaseRepair(r.Context(), in.UserID, cu.User.Email); err != nil {
			if store.IsNotFound(err) {
				writeAPIError(w, http.StatusNotFound, "That user does not exist.")
				return
			}
			s.serverError(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "restarting": s.requestRestart != nil})
	case "repair/cancel":
		if in.UserID <= 0 {
			writeAPIError(w, http.StatusBadRequest, "Select the scheduled repair to cancel.")
			return
		}
		if err := store.ClearUserDatabaseRepair(s.dataDir, in.UserID); err != nil {
			s.serverError(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	default:
		http.NotFound(w, r)
	}
}

// parseUserIDParam reads an optional numeric user id from a query string.
func parseUserIDParam(raw string) int64 {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id < 0 {
		return 0
	}
	return id
}

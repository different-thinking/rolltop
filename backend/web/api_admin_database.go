// File overview: Admin API for database maintenance. These routes expose file
// paths, sizes, and integrity state — never message content — and every
// mutation is admin-only and CSRF-protected. Scheduling a repair is the only
// destructive action here, and it never touches the database directly: it
// records the request and restarts the process so the repair runs at startup.

package web

import (
	"errors"
	"net/http"
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
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, overview)
}

// apiAdminDatabaseJob serves only the running job. The panel polls this every
// couple of seconds while a job runs, so it must not carry the full overview's
// per-tenant file reads and backup-directory sizing along with it.
func (s *Server) apiAdminDatabaseJob(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAPIAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	writeJSON(w, map[string]any{"job": s.maintenance.snapshot()})
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
		// Scope selects "system", "user", or every database when empty. It is
		// what lets the installation database be checked on its own instead of
		// dragging every tenant scan along with it.
		Scope   string `json:"scope"`
		UserID  int64  `json:"user_id"`
		Confirm bool   `json:"confirm"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	scope, ok := maintenanceScopeFromRequest(in.Scope, in.UserID)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "Select a valid database scope.")
		return
	}

	switch strings.Trim(action, "/") {
	case "job":
		http.NotFound(w, r)
	case "check":
		job, err := s.startIntegrityCheck(scope, in.UserID)
		if err != nil {
			s.writeMaintenanceStartError(w, r, err)
			return
		}
		writeJSON(w, map[string]any{"job": job})
	case "backup":
		job, err := s.startBackup(scope, in.UserID)
		if err != nil {
			s.writeMaintenanceStartError(w, r, err)
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
			if errors.Is(err, errMaintenanceJobRunning) {
				writeAPIError(w, http.StatusConflict, err.Error())
				return
			}
			s.serverError(w, r, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "restarting": s.requestRestart != nil})
	case "repair/cancel":
		if in.UserID <= 0 {
			writeAPIError(w, http.StatusBadRequest, "Select the scheduled repair to cancel.")
			return
		}
		if err := store.ClearUserDatabaseRepair(s.dataDir, in.UserID); err != nil {
			s.serverError(w, r, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	default:
		http.NotFound(w, r)
	}
}

// maintenanceScopeFromRequest validates the requested scope. An empty scope
// keeps the old whole-installation behavior; "user" requires a tenant.
func maintenanceScopeFromRequest(raw string, userID int64) (maintenanceScope, bool) {
	switch maintenanceScope(strings.ToLower(strings.TrimSpace(raw))) {
	case maintenanceScopeAll:
		return maintenanceScopeAll, true
	case maintenanceScopeSystem:
		return maintenanceScopeSystem, true
	case maintenanceScopeUser:
		return maintenanceScopeUser, userID > 0
	default:
		return maintenanceScopeAll, false
	}
}

// writeMaintenanceStartError separates "the slot is taken" from a target that
// cannot be read at all, so the UI can say which happened.
func (s *Server) writeMaintenanceStartError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrDatabaseFileMissing) {
		writeAPIError(w, http.StatusNotFound, err.Error())
		return
	}
	if store.IsNotFound(err) {
		writeAPIError(w, http.StatusNotFound, "That user does not exist.")
		return
	}
	if errors.Is(err, errMaintenanceJobRunning) {
		writeAPIError(w, http.StatusConflict, err.Error())
		return
	}
	// An I/O failure or an unreadable installation database must not be
	// reported to the admin as "another job is already running".
	s.serverError(w, r, err)
}

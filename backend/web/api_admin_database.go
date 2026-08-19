// File overview: Admin API for the database page. It reports storage state and
// nothing else — no file paths, no message content, and no mutations.
//
// Every action this file used to offer (integrity check, backup, scheduled
// repair) was SQLite maintenance and is gone with it. The page's remaining
// mutating surface lives in the PostgreSQL migration console
// (api_admin_postgres.go), which is deliberately separate: those actions create
// and drop a schema, and mixing them into the routes an operator visits to read
// a size would be how a mislabelled button becomes a dropped database.

package web

import (
	"context"
	"net/http"
)

func (s *Server) apiAdminDatabase(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAPIAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), maintenanceStatusTimeout)
	defer cancel()
	overview, err := s.databaseOverview(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, overview)
}

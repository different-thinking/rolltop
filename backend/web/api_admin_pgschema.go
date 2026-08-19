// File overview: Admin API for the PostgreSQL migration console. The preflight
// route next door answers "could this database work"; this one answers "what is
// actually in it", and lets an admin create and drop the generated schema there
// so the migration can be rehearsed against the real target long before any data
// moves.
//
// The DSN is handled exactly as the preflight handles it: held for the one
// request, never persisted, never logged, and never echoed back — the store
// redacts credential material out of driver errors before they reach the
// response. Admin-only and CSRF-protected, because it makes the server open an
// outbound connection to an admin-chosen host and, for two of the three actions,
// change what is in it.

package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"rolltop/backend/store"
)

// pgSchemaTimeout bounds one console action. Creating the schema is the slowest
// of them at a few hundred milliseconds locally, more across a network.
const pgSchemaTimeout = 2 * time.Minute

// pgSchemaMaxBody caps the request body, matching the preflight route.
const pgSchemaMaxBody = 64 << 10

// Actions the console offers. Only inspect is read-only.
const (
	pgSchemaActionInspect = "inspect"
	pgSchemaActionCreate  = "create"
	pgSchemaActionDrop    = "drop"
)

// pgSchemaLock serializes console actions in this process. The store also takes
// a PostgreSQL advisory lock, which coordinates with a starting server and with
// another Rolltop process; this one keeps a double-clicked button from queueing
// a second create behind the first.
var pgSchemaLock = make(chan struct{}, 1)

func (s *Server) apiAdminPostgresSchema(w http.ResponseWriter, r *http.Request) {
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
	r.Body = http.MaxBytesReader(w, r.Body, pgSchemaMaxBody)
	var in struct {
		DSN    string `json:"dsn"`
		Action string `json:"action"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	dsn := strings.TrimSpace(in.DSN)
	if dsn == "" {
		writeAPIError(w, http.StatusBadRequest, "Enter the PostgreSQL connection string.")
		return
	}
	action := strings.TrimSpace(in.Action)
	switch action {
	case pgSchemaActionInspect, pgSchemaActionCreate, pgSchemaActionDrop:
	default:
		writeAPIError(w, http.StatusBadRequest, "Unknown action.")
		return
	}

	select {
	case pgSchemaLock <- struct{}{}:
		defer func() { <-pgSchemaLock }()
	default:
		writeAPIError(w, http.StatusConflict, "Another schema action is already running. Wait for it to finish.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), pgSchemaTimeout)
	defer cancel()

	var state store.PostgresState
	var err error
	switch action {
	case pgSchemaActionInspect:
		state, err = store.InspectPostgres(ctx, dsn)
	case pgSchemaActionCreate:
		state, err = store.CreatePostgresSchema(ctx, dsn)
	case pgSchemaActionDrop:
		state, err = store.DropPostgresSchema(ctx, dsn)
	}
	if err != nil {
		// The store's errors are written for the operator — they name the stage
		// that blocked the action and what to do about it — and they are already
		// redacted, so they are shown rather than swallowed into a 500.
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, state)
}

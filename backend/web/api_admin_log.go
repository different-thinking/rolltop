// File overview: Admin API for the in-memory tail of the process log. The
// browser only ever sees "internal server error"; the line naming the actual
// failure goes to the container log, which a hosted operator cannot read. This
// route hands that line to the admin who can act on it, and to nobody else:
// log records carry request paths and backend error text.

package web

import (
	"net/http"
	"strconv"
	"strings"

	"rolltop/backend/logging"
)

// defaultLogTailLines is what the admin page asks for when it does not say.
const defaultLogTailLines = 200

type apiLogLine struct {
	Time    string `json:"time"`
	Message string `json:"message"`
	Error   bool   `json:"error"`
}

func (s *Server) apiAdminLog(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAPIAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	records := logging.Recent(logTailLimitFromRequest(r))
	lines := make([]apiLogLine, 0, len(records))
	for _, record := range records {
		lines = append(lines, apiLogLine{
			Time:    timeString(record.Time),
			Message: record.Message,
			// The level prefix is what logging.Errorf writes, so the page can
			// pick the failures out of a tail that is mostly routine progress.
			Error: strings.HasPrefix(record.Message, "error "),
		})
	}
	writeJSON(w, map[string]any{"lines": lines})
}

// logTailLimitFromRequest reads how many lines to return. An unparseable or
// out-of-range value falls back to the default rather than failing the request:
// this page exists to be reachable when something else is already broken.
func logTailLimitFromRequest(r *http.Request) int {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return defaultLogTailLines
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit <= 0 {
		return defaultLogTailLines
	}
	if limit > 500 {
		return 500
	}
	return limit
}

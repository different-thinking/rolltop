// File overview: The handle returned when a tenant database cannot be opened.
// mustDataDB exists because several helpers must satisfy database/sql callback
// shapes and cannot return an error where they resolve the tenant handle. It
// used to panic there, which turned one damaged tenant into a process crash as
// soon as any background goroutine touched it. Handing back a handle whose
// every statement fails keeps that failure inside the caller's normal error
// path instead, so the rest of the installation keeps serving.

package store

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"
)

const unavailableDriverName = "rolltop-unavailable"

var errDatabaseUnavailable = errors.New("tenant database is unavailable; see the log for the corruption report and the repair command")

func init() {
	sql.Register(unavailableDriverName, unavailableDriver{})
}

type unavailableDriver struct{}

func (unavailableDriver) Open(string) (driver.Conn, error) {
	return nil, errDatabaseUnavailable
}

// unavailableDB is process-wide: it owns one idle database/sql pool that never
// opens a connection, so sharing it costs one goroutine rather than one per
// failed lookup.
var unavailableDB = sync.OnceValue(func() *sql.DB {
	db, err := sql.Open(unavailableDriverName, "")
	if err != nil {
		// Registration above cannot fail, so this is unreachable; returning nil
		// would only move the panic to the caller.
		panic(err)
	}
	db.SetMaxOpenConns(1)
	return db
})

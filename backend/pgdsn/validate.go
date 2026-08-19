// File overview: Checking a PostgreSQL connection string before the driver
// sees it, so a typo is a startup failure naming the environment variable
// rather than a driver error quoting the password back.

package pgdsn

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// Validate reports whether the string parses as a connection string pgx can
// use, and whether it names a database to connect to.
//
// The parse is delegated to pgconn, so this accepts exactly what the driver
// accepts — both the URL form and the `host=… dbname=…` keyword form — instead
// of a second, subtly different grammar that would reject working DSNs.
//
// Two things are checked beyond parsing, because pgconn accepts both and they
// are ways a DSN silently connects somewhere else:
//
//   - An empty database name. libpq then falls back to the operating-system
//     user name, so `postgres://user:pw@host:5432` connects to a database
//     called after the role, which on a managed server usually does not exist
//     and on a local one is somebody's scratch database.
//   - No host at all. This is a backstop rather than a live check: pgconn
//     resolves a missing host to the local unix socket directory, so a DSN
//     whose host was lost to a formatting mistake parses with a host of
//     `/var/run/postgresql` and reaches here looking valid. What catches that
//     one is the `role@host/database` line Describe writes at startup, which
//     shows the socket path instead of the server that was meant. The check
//     stays because an empty host is not something to pass to the driver if a
//     future pgconn ever produces one.
//
// The error never contains the DSN. pgconn's parse errors quote it in full,
// which is the whole reason Redact exists, so the driver's message is redacted
// before it is wrapped.
func Validate(dsn string) error {
	if dsn == "" {
		return errors.New("is empty")
	}
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return errors.New(Redact(err.Error()))
	}
	if cfg.Database == "" {
		return errors.New("names no database; add the database name (…/rolltop) to the connection string")
	}
	if cfg.Host == "" {
		return errors.New("names no host")
	}
	return nil
}

// Describe renders a connection string as a short, credential-free summary for
// a log line: which role reaches which database on which server. Startup prints
// it so a deployment pointed at the wrong database is visible in the first
// lines of the container log rather than in the data an hour later.
func Describe(dsn string) string {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return "unparseable connection string"
	}
	host := cfg.Host
	if host == "" {
		host = "local socket"
	}
	if cfg.Port != 0 {
		host = fmt.Sprintf("%s:%d", host, cfg.Port)
	}
	return fmt.Sprintf("%s@%s/%s", cfg.User, host, cfg.Database)
}

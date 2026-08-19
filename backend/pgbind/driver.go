// File overview: A database/sql driver that wraps pgx's and rebinds every
// statement on its way through.
//
// Putting the translation in the driver rather than at the call sites is what
// makes it apply to the finished statement — the one the server will actually
// parse — including the ones this codebase assembles from fragments at run
// time. It is also the only place that sees every route into the database:
// direct Exec, Query, prepared statements, and statements run inside a
// transaction all pass through a Conn.
//
// The wrapper forwards, rather than reimplements, every optional interface pgx
// provides. Two of them are load-bearing and would fail quietly if dropped:
// CheckNamedValue is what lets a Go slice reach the server as an array for
// `= ANY($1)`, and without it database/sql rejects the argument before pgx ever
// sees it; SessionResetter is what returns a pooled connection to a clean
// state, and without it a connection that ended a query mid-transaction is
// handed to the next caller as-is.

package pgbind

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"

	"github.com/jackc/pgx/v5/stdlib"
)

// DriverName is the driver to open the store's connections with.
const DriverName = "pgx-rebind"

var registerOnce sync.Once

// Register makes DriverName available to sql.Open. It is safe to call more than
// once; database/sql panics on a duplicate registration, which a test binary
// opening several stores would otherwise trigger.
func Register() {
	registerOnce.Do(func() {
		sql.Register(DriverName, wrappedDriver{base: stdlib.GetDefaultDriver()})
	})
}

type wrappedDriver struct{ base driver.Driver }

func (d wrappedDriver) Open(name string) (driver.Conn, error) {
	c, err := d.base.Open(name)
	if err != nil {
		return nil, err
	}
	return wrappedConn{base: c}, nil
}

// OpenConnector is what carries a context into connection establishment, so a
// cancelled startup stops waiting on a dial instead of finishing it.
func (d wrappedDriver) OpenConnector(name string) (driver.Connector, error) {
	dc, ok := d.base.(driver.DriverContext)
	if !ok {
		return nil, errors.New("pgbind: the pgx driver no longer implements driver.DriverContext")
	}
	c, err := dc.OpenConnector(name)
	if err != nil {
		return nil, err
	}
	return wrappedConnector{base: c, driver: d}, nil
}

type wrappedConnector struct {
	base   driver.Connector
	driver driver.Driver
}

func (c wrappedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return wrappedConn{base: conn}, nil
}

func (c wrappedConnector) Driver() driver.Driver { return c.driver }

// wrappedConn rebinds the statement text and delegates everything else.
type wrappedConn struct{ base driver.Conn }

func (c wrappedConn) Prepare(query string) (driver.Stmt, error) {
	bound, err := Rebind(query)
	if err != nil {
		return nil, err
	}
	return c.base.Prepare(bound)
}

func (c wrappedConn) Close() error { return c.base.Close() }

func (c wrappedConn) Begin() (driver.Tx, error) { return c.base.Begin() }

func (c wrappedConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	bound, err := Rebind(query)
	if err != nil {
		return nil, err
	}
	p, ok := c.base.(driver.ConnPrepareContext)
	if !ok {
		return c.base.Prepare(bound)
	}
	return p.PrepareContext(ctx, bound)
}

func (c wrappedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	b, ok := c.base.(driver.ConnBeginTx)
	if !ok {
		return c.base.Begin()
	}
	return b.BeginTx(ctx, opts)
}

func (c wrappedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	e, ok := c.base.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	bound, err := Rebind(query)
	if err != nil {
		return nil, err
	}
	return e.ExecContext(ctx, bound, args)
}

func (c wrappedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	q, ok := c.base.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	bound, err := Rebind(query)
	if err != nil {
		return nil, err
	}
	return q.QueryContext(ctx, bound, args)
}

func (c wrappedConn) Ping(ctx context.Context) error {
	p, ok := c.base.(driver.Pinger)
	if !ok {
		return nil
	}
	return p.Ping(ctx)
}

// CheckNamedValue forwards pgx's own argument check. Dropping it would make
// database/sql apply its default conversion, which accepts only a handful of
// primitive types — and would therefore reject the slices `= ANY($1)` is passed.
func (c wrappedConn) CheckNamedValue(v *driver.NamedValue) error {
	n, ok := c.base.(driver.NamedValueChecker)
	if !ok {
		return driver.ErrSkip
	}
	return n.CheckNamedValue(v)
}

func (c wrappedConn) ResetSession(ctx context.Context) error {
	r, ok := c.base.(driver.SessionResetter)
	if !ok {
		return nil
	}
	return r.ResetSession(ctx)
}

func (c wrappedConn) IsValid() bool {
	v, ok := c.base.(driver.Validator)
	if !ok {
		return true
	}
	return v.IsValid()
}

// Unwrap returns the pgx connection behind a driver connection handed out by
// sql.Conn.Raw.
//
// Callers that reach for the raw connection want pgx itself: the schema
// baseline is applied through pgx's simple protocol, because the extended
// protocol takes one statement per call and the baseline is a script whose
// dollar-quoted trigger body cannot be split on semicolons. Without this they
// would find the wrapper and fail a type assertion that reads as "pgx changed",
// which is a long way from "there is a driver in between".
func Unwrap(driverConn any) any {
	if w, ok := driverConn.(wrappedConn); ok {
		return w.base
	}
	return driverConn
}

var (
	_ driver.Driver             = wrappedDriver{}
	_ driver.DriverContext      = wrappedDriver{}
	_ driver.Connector          = wrappedConnector{}
	_ driver.Conn               = wrappedConn{}
	_ driver.ConnPrepareContext = wrappedConn{}
	_ driver.ConnBeginTx        = wrappedConn{}
	_ driver.ExecerContext      = wrappedConn{}
	_ driver.QueryerContext     = wrappedConn{}
	_ driver.Pinger             = wrappedConn{}
	_ driver.NamedValueChecker  = wrappedConn{}
	_ driver.SessionResetter    = wrappedConn{}
	_ driver.Validator          = wrappedConn{}
)

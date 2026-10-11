package state

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
)

// noInterruptDriver wraps the modernc SQLite driver so a cancelled context
// never reaches it. In modernc.org/sqlite v1.29.x a context cancelled while a
// statement runs calls sqlite3_interrupt on the connection, and the interrupt
// can leak into the next statement on the same pooled connection: an
// unrelated read with context.Background() then fails with SQLITE_INTERRUPT
// (issue #5). Store statements are short and local (busy_timeout bounds lock
// waits), so cancellation is still honoured by database/sql before a
// statement starts and between rows, but is never turned into an interrupt.
type noInterruptDriver struct{ d driver.Driver }

const noInterruptDriverName = "sqlite-nointerrupt"

// The wrapped driver is the registered "sqlite" instance, so the connection
// hook (foreign_keys, busy_timeout) still runs on every connection.
func init() {
	base, err := sql.Open("sqlite", "")
	if err != nil {
		panic(err)
	}
	sql.Register(noInterruptDriverName, noInterruptDriver{base.Driver()})
	_ = base.Close()
}

func (n noInterruptDriver) Open(name string) (driver.Conn, error) {
	c, err := n.d.Open(name)
	if err != nil {
		return nil, err
	}
	return niConn{c}, nil
}

type niConn struct{ driver.Conn }

var errNoCtxSupport = errors.New("state: sqlite driver lacks context support")

func detach(ctx context.Context) context.Context { return context.WithoutCancel(ctx) }

func (c niConn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	b, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		return nil, errNoCtxSupport
	}
	return b.BeginTx(detach(ctx), o)
}

func (c niConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	p, ok := c.Conn.(driver.ConnPrepareContext)
	if !ok {
		return nil, errNoCtxSupport
	}
	s, err := p.PrepareContext(detach(ctx), q)
	if err != nil {
		return nil, err
	}
	return niStmt{s}, nil
}

func (c niConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	e, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return e.ExecContext(detach(ctx), q, args)
}

func (c niConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	e, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return e.QueryContext(detach(ctx), q, args)
}

func (c niConn) Ping(ctx context.Context) error {
	if p, ok := c.Conn.(driver.Pinger); ok {
		return p.Ping(detach(ctx))
	}
	return nil
}

type niStmt struct{ driver.Stmt }

func (s niStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	e, ok := s.Stmt.(driver.StmtExecContext)
	if !ok {
		return nil, errNoCtxSupport
	}
	return e.ExecContext(detach(ctx), args)
}

func (s niStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	q, ok := s.Stmt.(driver.StmtQueryContext)
	if !ok {
		return nil, errNoCtxSupport
	}
	return q.QueryContext(detach(ctx), args)
}

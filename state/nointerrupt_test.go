package state

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"
	"time"
)

// Issue #5: a statement whose context is cancelled must not poison the next
// statement on the same pooled connection. Fails on modernc.org/sqlite
// v1.29.8 without the wrapping driver (thousands of SQLITE_INTERRUPT).
func TestCancelledQueryDoesNotInterruptNextStatement(t *testing.T) {
	s, err := OpenWithOptions(t.TempDir()+"/a.db", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedScope(t, s)
	ctx := context.Background()
	for i := 0; i < 2000; i++ {
		c, cancel := context.WithTimeout(ctx, time.Duration(i%50)*time.Microsecond)
		_, _ = s.Scope(c, "s1")
		_ = s.MarkSyncPending(c, "s1", i%2 == 0)
		cancel()
		if _, err := s.Scope(ctx, "s1"); err != nil {
			t.Fatalf("iteration %d: read after cancelled statement: %v", i, err)
		}
		if err := s.MarkSyncPending(ctx, "s1", true); err != nil {
			t.Fatalf("iteration %d: write after cancelled statement: %v", i, err)
		}
	}
	// cancellation is still honoured before a statement starts
	c, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Scope(c, "s1"); err == nil {
		t.Fatal("cancelled context ran a statement")
	}
	// transactions and prepared statements go through the wrapper too
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := tx.PrepareContext(ctx, `SELECT cursor FROM sync_scopes WHERE id=?`)
	if err != nil {
		t.Fatal(err)
	}
	var cur string
	if err := st.QueryRowContext(ctx, "s1").Scan(&cur); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ExecContext(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback()
	if err := s.DB().PingContext(ctx); err != nil {
		t.Fatal(err)
	}
}

type bareConn struct{}

func (bareConn) Prepare(string) (driver.Stmt, error) { return bareStmt{}, nil }
func (bareConn) Close() error                        { return nil }
func (bareConn) Begin() (driver.Tx, error)           { return nil, errors.New("x") }

type bareStmt struct{}

func (bareStmt) Close() error                               { return nil }
func (bareStmt) NumInput() int                              { return 0 }
func (bareStmt) Exec([]driver.Value) (driver.Result, error) { return nil, nil }
func (bareStmt) Query([]driver.Value) (driver.Rows, error)  { return nil, nil }

type bareDriver struct{ err error }

func (b bareDriver) Open(string) (driver.Conn, error) { return bareConn{}, b.err }

// Drivers without context support degrade safely (fallback/skip errors).
func TestNoInterruptWrapperFallbacks(t *testing.T) {
	ctx := context.Background()
	if _, err := (noInterruptDriver{bareDriver{errors.New("open")}}).Open(""); err == nil {
		t.Fatal("open error dropped")
	}
	cn, _ := (noInterruptDriver{bareDriver{}}).Open("")
	c := cn.(niConn)
	if _, err := c.BeginTx(ctx, driver.TxOptions{}); !errors.Is(err, errNoCtxSupport) {
		t.Fatal(err)
	}
	if _, err := c.PrepareContext(ctx, "x"); !errors.Is(err, errNoCtxSupport) {
		t.Fatal(err)
	}
	if _, err := c.ExecContext(ctx, "x", nil); !errors.Is(err, driver.ErrSkip) {
		t.Fatal(err)
	}
	if _, err := c.QueryContext(ctx, "x", nil); !errors.Is(err, driver.ErrSkip) {
		t.Fatal(err)
	}
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	s := niStmt{bareStmt{}}
	if _, err := s.ExecContext(ctx, nil); !errors.Is(err, errNoCtxSupport) {
		t.Fatal(err)
	}
	if _, err := s.QueryContext(ctx, nil); !errors.Is(err, errNoCtxSupport) {
		t.Fatal(err)
	}
	// prepare error from the real driver is returned
	if _, err := (niConn{prepErrConn{}}).PrepareContext(ctx, "x"); err == nil {
		t.Fatal("prepare error dropped")
	}
}

type prepErrConn struct{ bareConn }

func (prepErrConn) PrepareContext(context.Context, string) (driver.Stmt, error) {
	return nil, errors.New("prepare")
}

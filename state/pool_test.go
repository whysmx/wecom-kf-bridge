package state

import (
	"context"
	"database/sql"
	"testing"
)

// #38: every pooled connection, not just the first, has foreign keys and
// the busy timeout, for both OpenWithOptions and New(caller *sql.DB).
func TestEveryPooledConnectionIsInitialised(t *testing.T) {
	ctx := context.Background()
	s, err := OpenWithOptions(t.TempDir()+"/p.db", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	raw, _ := sql.Open("sqlite", t.TempDir()+"/n.db")
	raw.SetMaxOpenConns(MaxOpenConns) // New keeps the caller's pool settings
	n, err := New(raw, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	for _, db := range []*sql.DB{s.DB(), n.DB()} {
		if st := db.Stats(); st.MaxOpenConnections != MaxOpenConns {
			t.Fatalf("pool limit %d", st.MaxOpenConnections)
		}
		var conns []*sql.Conn
		for i := 0; i < MaxOpenConns; i++ {
			c, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			conns = append(conns, c)
		}
		for i, c := range conns {
			var fk, bt int
			if err := c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
				t.Fatal(err)
			}
			if err := c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&bt); err != nil {
				t.Fatal(err)
			}
			if fk != 1 || bt != 5000 {
				t.Fatalf("connection %d: foreign_keys=%d busy_timeout=%d", i, fk, bt)
			}
		}
		for _, c := range conns {
			c.Close()
		}
	}
}

package state

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func isBusySnapshot(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "(517)") || strings.Contains(msg, "BUSY_SNAPSHOT")
}

func TestWithImmediateTxLockDSN(t *testing.T) {
	if got := withImmediateTxLock("/tmp/a.db"); got != "/tmp/a.db?_txlock=immediate" {
		t.Fatalf("plain: %s", got)
	}
	if got := withImmediateTxLock("file:/tmp/a.db?mode=ro"); got != "file:/tmp/a.db?mode=ro&_txlock=immediate" {
		t.Fatalf("query: %s", got)
	}
}

// #11: concurrent read-then-write (BeginHandover) racing single-statement
// writers (MarkOutboxSending / SetNickname) must not return SQLITE_BUSY_SNAPSHOT
// (517). OpenWithOptions uses _txlock=immediate so BeginTx takes the write
// lock up front; contention waits via busy_timeout instead.
func TestConcurrentReadModifyWriteNoBusySnapshot(t *testing.T) {
	ctx := context.Background()
	const rounds = 40
	var snaps atomic.Int32
	for i := 0; i < rounds; i++ {
		s, c, o := newSendable(t)
		var wg sync.WaitGroup
		var markErr, takeErr, nickErr error
		wg.Add(3)
		go func() { defer wg.Done(); _, markErr = s.MarkOutboxSending(ctx, o.ID) }()
		go func() { defer wg.Done(); _, takeErr = s.BeginHandover(ctx, c.ID, "agent") }()
		go func() { defer wg.Done(); nickErr = s.SetNickname(ctx, c.ID, "n", s.now()) }()
		wg.Wait()
		for _, err := range []error{markErr, takeErr, nickErr} {
			if isBusySnapshot(err) {
				snaps.Add(1)
			}
		}
		// Ensure takeover can land (product path, not test-only retry storm).
		if takeErr != nil && !isBusySnapshot(takeErr) {
			_, takeErr = s.BeginHandover(ctx, c.ID, "agent")
		}
		if isBusySnapshot(takeErr) {
			snaps.Add(1)
		}
	}
	if n := snaps.Load(); n != 0 {
		t.Fatalf("SQLITE_BUSY_SNAPSHOT (517) seen %d times", n)
	}
}

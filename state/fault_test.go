package state

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Fault injection: a SQLite trigger aborts one statement inside a
// transaction. The operation must fail AND leave no partial state behind.

func faultStore(t *testing.T) (*Store, Customer) {
	t.Helper()
	s, err := OpenWithOptions(t.TempDir()+"/f.db", Options{MasterKey: bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	seedScope(t, s)
	c, err := s.EnsureCustomer(context.Background(), "e1", "b1", "ext-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetCustomerAuthorized(context.Background(), c.ID, true); err != nil {
		t.Fatal(err)
	}
	openWindow(t, s, c.ID)
	return s, c
}

func inject(t *testing.T, s *Store, event, table string) func() {
	t.Helper()
	name := "fault_" + strings.ReplaceAll(event, " ", "_") + "_" + table
	if _, err := s.DB().Exec(`CREATE TRIGGER ` + name + ` BEFORE ` + event + ` ON ` + table + ` BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	return func() { s.DB().Exec(`DROP TRIGGER ` + name) }
}

func count(t *testing.T, s *Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestFaultCustomerCreationAndRotationAreAtomic(t *testing.T) {
	s, c := faultStore(t)
	ctx := context.Background()
	undo := inject(t, s, "INSERT", "customer_uids")
	if _, err := s.EnsureCustomer(ctx, "e1", "b1", "ext-2"); err == nil {
		t.Fatal("ensure succeeded")
	}
	if count(t, s, `SELECT COUNT(1) FROM customers`) != 1 {
		t.Fatal("customer row left without uid history")
	}
	if _, err := s.RotateGeneration(ctx, c.ID, "r"); err == nil {
		t.Fatal("rotate succeeded")
	}
	if _, err := s.RecoverCustomer(ctx, c.ID, "r"); err == nil {
		t.Fatal("recover succeeded")
	}
	undo()
	got, _ := s.Customer(ctx, c.ID)
	if got.Generation != c.Generation || got.UID != c.UID {
		t.Fatalf("generation changed despite failure: %+v", got)
	}
	if _, err := s.RotateGeneration(ctx, "missing", "r"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rotate missing: %v", err)
	}
}

func TestFaultSyncPageIsAtomic(t *testing.T) {
	s, c := faultStore(t)
	ctx := context.Background()
	msgs := []InboxMessage{{BindingID: "b1", ExternalMsgID: "m1", CustomerID: c.ID, Generation: 1, Type: "text", PayloadRef: "a"}, {BindingID: "b1", ExternalMsgID: "m2", CustomerID: c.ID, Generation: 1, Type: "text", PayloadRef: "b", CreateTime: time.Unix(1800000000, 0)}}
	for _, f := range [][2]string{{"INSERT", "inbox"}, {"UPDATE", "compat_seq"}, {"INSERT", "compat_seq"}, {"UPDATE", "sync_scopes"}} {
		undo := inject(t, s, f[0], f[1])
		if _, err := s.CommitSyncPage(ctx, "s1", "next", false, msgs); err == nil && f[1] != "compat_seq" {
			t.Fatalf("%v: commit succeeded", f)
		}
		undo()
		if f[1] != "compat_seq" && count(t, s, `SELECT COUNT(1) FROM inbox`) != 0 {
			t.Fatalf("%v: partial page committed", f)
		}
	}
	// zero CreateTime is stored as unknown, not replaced with now
	s.DB().Exec(`DELETE FROM inbox`)
	if _, err := s.CommitSyncPage(ctx, "s1", "next", false, msgs); err != nil {
		t.Fatal(err)
	}
	m, _ := s.InboxByExternalID(ctx, "s1", "m1")
	if !m.CreateTime.IsZero() {
		t.Fatalf("create time fabricated: %v", m.CreateTime)
	}
}

func TestFaultOutboxLifecycle(t *testing.T) {
	s, c := faultStore(t)
	ctx := context.Background()
	newOut := func() OutboxMessage {
		o, err := s.CreateOutbox(ctx, OutboxMessage{CustomerID: c.ID, Generation: c.Generation, UID: c.UID, Body: "hi", BudgetUnits: 1, ChunksTotal: 1})
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	used := func() int { return count(t, s, `SELECT window_used FROM customers WHERE id=?`, c.ID) }

	undo := inject(t, s, "INSERT", "outbox")
	if _, err := s.CreateOutbox(ctx, OutboxMessage{CustomerID: c.ID, Generation: c.Generation, UID: c.UID, Body: "x", BudgetUnits: 1}); err == nil || used() != 0 {
		t.Fatalf("create: err=%v budget=%d (budget must roll back)", err, used())
	}
	undo()
	undo = inject(t, s, "UPDATE", "customers")
	if _, err := s.CreateOutbox(ctx, OutboxMessage{CustomerID: c.ID, Generation: c.Generation, UID: c.UID, Body: "x", BudgetUnits: 1}); err == nil {
		t.Fatal("budget reservation failure ignored")
	}
	undo()
	if count(t, s, `SELECT COUNT(1) FROM outbox`) != 0 {
		t.Fatal("outbox row without budget")
	}

	o := newOut()
	undo = inject(t, s, "UPDATE", "customers")
	if _, err := s.BlockOutbox(ctx, o.ID, BlockHeld); err == nil {
		t.Fatal("block succeeded without releasing budget")
	}
	undo()
	if got, _ := s.Outbox(ctx, o.ID); got.State == OutboxBlocked {
		t.Fatal("blocked without budget release")
	}
	undo = inject(t, s, "UPDATE", "outbox")
	for name, err := range map[string]error{
		"block":   func() error { _, e := s.BlockOutbox(ctx, o.ID, BlockHeld); return e }(),
		"sending": func() error { _, e := s.MarkOutboxSending(ctx, o.ID); return e }(),
		"trans":   func() error { _, e := s.TransitionOutbox(ctx, o.ID, OutboxRejected, ""); return e }(),
		"unknown": func() error { _, e := s.MarkOutboxUnknown(ctx, o.ID, "x"); return e }(),
	} {
		if err == nil {
			t.Fatalf("%s succeeded", name)
		}
	}
	undo()
	if _, err := s.MarkOutboxSending(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	undo = inject(t, s, "INSERT", "outbox_chunks")
	if err := s.RecordOutboxChunk(ctx, o.ID, 1, "wx1"); err == nil {
		t.Fatal("chunk record failure ignored")
	}
	undo()
	undo = inject(t, s, "UPDATE", "outbox")
	if err := s.RecordOutboxChunk(ctx, o.ID, 1, "wx1"); err == nil {
		t.Fatal("chunk counter failure ignored")
	}
	undo()
	if ids, _ := s.OutboxChunkIDs(ctx, o.ID); len(ids) != 0 {
		t.Fatalf("chunk recorded despite failure: %v", ids)
	}
	if _, err := s.MarkOutboxSending(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("sending missing: %v", err)
	}
	if _, err := s.BlockOutbox(ctx, "missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("block missing: %v", err)
	}
	if _, err := s.CreateOutbox(ctx, OutboxMessage{CustomerID: "missing"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("create missing: %v", err)
	}
}

func TestSealedDataTamperingDetected(t *testing.T) {
	s, c := faultStore(t)
	ctx := context.Background()
	o, err := s.CreateOutbox(ctx, OutboxMessage{CustomerID: c.ID, Generation: c.Generation, UID: c.UID, Body: "secret", BudgetUnits: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"plain", sealPrefix + "!!!", sealPrefix + "AAAA", sealPrefix + strings.Repeat("A", 60)} {
		s.DB().Exec(`UPDATE outbox SET body=? WHERE id=?`, bad, o.ID)
		if _, err := s.Outbox(ctx, o.ID); err == nil {
			t.Fatalf("tampered body %q accepted", bad)
		}
		if _, err := s.PendingOutbox(ctx, "b1"); err == nil {
			t.Fatalf("pending list accepted tampered body %q", bad)
		}
	}
	// a store without a master key cannot read or write sealed fields
	plain, err := OpenWithOptions(t.TempDir()+"/p.db", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if _, err := plain.sealField("x", "a"); !errors.Is(err, ErrNoSealer) {
		t.Fatalf("seal without key: %v", err)
	}
	if _, err := plain.openField(sealPrefix+"x", "a"); !errors.Is(err, ErrNoSealer) {
		t.Fatalf("open without key: %v", err)
	}
	if err := plain.SaveSyncToken(ctx, "s", "t", time.Now()); err == nil {
		t.Fatal("token stored without key")
	}
	if err := s.SaveSyncToken(ctx, "", "t", time.Now()); !errors.Is(err, ErrInvalidID) {
		t.Fatal("empty scope")
	}
	if _, err := NewSealer(make([]byte, 31)); err == nil {
		t.Fatal("short key")
	}
}

func TestWorkerQueriesSkipTamperedRows(t *testing.T) {
	s, c := faultStore(t)
	ctx := context.Background()
	if _, err := s.CommitSyncPage(ctx, "s1", "n", false, []InboxMessage{{BindingID: "b1", ExternalMsgID: "m", CustomerID: c.ID, Generation: 1, Type: "text", PayloadRef: "a"}}); err != nil {
		t.Fatal(err)
	}
	s.DB().Exec(`UPDATE inbox SET payload_ref='bogus'`)
	if _, err := s.InboxForDelivery(ctx, 3, 10); err == nil {
		t.Fatal("tampered inbox payload delivered")
	}
	s.DB().Exec(`UPDATE inbox SET state='POSTING'`)
	undo := inject(t, s, "UPDATE", "inbox")
	if _, _, err := s.RecoverInFlight(ctx); err == nil {
		t.Fatal("recovery failure ignored")
	}
	undo()
	if in, _, err := s.RecoverInFlight(ctx); err != nil || in != 1 {
		t.Fatalf("recovery: %d %v", in, err)
	}
}

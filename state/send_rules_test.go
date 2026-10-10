package state

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestOldUIDIsStaleAndNeverReused(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedScope(t, s)
	c, _ := s.EnsureCustomer(ctx, "e1", "b1", "ext")
	old := c.UID
	if _, err := s.BeginHandover(ctx, c.ID, "human"); err != nil {
		t.Fatal(err)
	}
	n, err := s.RecoverCustomer(ctx, c.ID, "back")
	if err != nil || n.UID == old || n.Generation != 2 {
		t.Fatalf("recover %+v %v", n, err)
	}
	if _, err := s.CustomerByUID(ctx, "b1", old); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("old uid: %v", err)
	}
	if got, err := s.CustomerByUID(ctx, "b1", n.UID); err != nil || got.ID != c.ID {
		t.Fatalf("new uid: %v", err)
	}
	if _, err := s.CustomerByUID(ctx, "b1", "never"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown uid: %v", err)
	}
	if _, err := s.DB().Exec(`INSERT INTO customer_uids(binding_id,uid,customer_id,generation,created_at) VALUES('b1',?,?,3,0)`, old, c.ID); err == nil {
		t.Fatal("old uid could be re-registered")
	}
	openWindow(t, s, c.ID)
	if _, err := s.CreateOutbox(ctx, OutboxMessage{CustomerID: c.ID, Generation: 1, UID: old, Body: "x"}); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("old generation outbox: %v", err)
	}
}

func TestCreateOutboxEnforcesAuthBindingWindowBudget(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedScope(t, s)
	c, _ := s.EnsureCustomer(ctx, "e1", "b1", "ext")
	mk := func(units int, rev int64) error {
		_, err := s.CreateOutbox(ctx, OutboxMessage{CustomerID: c.ID, Generation: c.Generation, UID: c.UID, Body: "x", BudgetUnits: units, BindingRevision: rev})
		return err
	}
	if err := mk(1, 0); !errors.Is(err, ErrWindowClosed) {
		t.Fatalf("no inbound yet: %v", err)
	}
	// customer message opens the window (and a replay does not reset budget)
	page := []InboxMessage{{BindingID: "b1", ExternalMsgID: "m1", CustomerID: c.ID, CustomerInitiated: true, CreateTime: s.now()}}
	if _, err := s.CommitSyncPage(ctx, "s1", "c1", false, page); err != nil {
		t.Fatal(err)
	}
	if err := mk(3, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitSyncPage(ctx, "s1", "c2", false, page); err != nil {
		t.Fatal(err)
	}
	if err := mk(3, 0); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("budget: %v", err)
	}
	if err := s.ReleaseBudget(ctx, c.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err := mk(2, 2); !errors.Is(err, ErrBindingInactive) {
		t.Fatalf("revision: %v", err)
	}
	if err := mk(2, 1); err != nil {
		t.Fatalf("after release: %v", err)
	}
	_ = s.SetCustomerAuthorized(ctx, c.ID, false)
	if err := mk(1, 0); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unauthorized: %v", err)
	}
	_ = s.SetCustomerAuthorized(ctx, c.ID, true)
	_ = s.PutBinding(ctx, Binding{ID: "b1", EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p1", Active: false})
	if err := mk(1, 0); !errors.Is(err, ErrBindingInactive) {
		t.Fatalf("inactive: %v", err)
	}
	if err := s.SetCustomerAuthorized(ctx, "none", true); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	// window expiry
	s2 := testStore(t)
	seedScope(t, s2)
	c2, _ := s2.EnsureCustomer(ctx, "e1", "b1", "x")
	_, _ = s2.DB().Exec(`UPDATE customers SET last_inbound_at=? WHERE id=?`, unix(s2.now().Add(-49*time.Hour)), c2.ID)
	if _, err := s2.CreateOutbox(ctx, OutboxMessage{CustomerID: c2.ID, Generation: 1, UID: c2.UID}); !errors.Is(err, ErrWindowClosed) {
		t.Fatalf("expired window: %v", err)
	}
}

func TestRecordOutboxChunks(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedScope(t, s)
	c, _ := s.EnsureCustomer(ctx, "e1", "b1", "ext")
	openWindow(t, s, c.ID)
	o, err := s.CreateOutbox(ctx, OutboxMessage{CustomerID: c.ID, Generation: 1, UID: c.UID, Body: "x", BudgetUnits: 3, ChunksTotal: 3})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordOutboxChunk(ctx, o.ID, 1, "w1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("chunk before SENDING: %v", err)
	}
	if _, err := s.MarkOutboxSending(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"w1", "w2"} {
		if err := s.RecordOutboxChunk(ctx, o.ID, i+1, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordOutboxChunk(ctx, o.ID, 2, "dup"); err == nil {
		t.Fatal("duplicate chunk accepted")
	}
	if err := s.RecordOutboxChunk(ctx, "", 1, ""); !errors.Is(err, ErrInvalidID) {
		t.Fatal(err)
	}
	got, _ := s.Outbox(ctx, o.ID)
	ids, _ := s.OutboxChunkIDs(ctx, o.ID)
	if got.ChunksTotal != 3 || got.ChunksSent != 2 || got.ExternalMsgID != "w2" || len(ids) != 2 {
		t.Fatalf("%+v %v", got, ids)
	}
	if err := s.ReleaseBudget(ctx, c.ID, 0); err != nil {
		t.Fatal(err)
	}
}

package state

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// #30: a rotation cannot complete while a guarded send is in progress, and
// the send after it is refused as stale.
func TestRotateBindingCustomersSerialisesWithSendGuarded(t *testing.T) {
	s, c := faultStore(t)
	ctx := context.Background()
	o, err := s.CreateOutbox(ctx, OutboxMessage{CustomerID: c.ID, Generation: c.Generation, UID: c.UID, Body: "x", BudgetUnits: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkOutboxSending(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	inSend := make(chan struct{})
	var sendEnd, rotEnd time.Time
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = s.SendGuarded(ctx, o.ID, func() error {
			close(inSend)
			time.Sleep(150 * time.Millisecond)
			sendEnd = time.Now()
			return nil
		})
	}()
	<-inSend
	if _, err := s.RotateBindingCustomers(ctx, "b1"); err != nil {
		t.Fatal(err)
	}
	rotEnd = time.Now()
	wg.Wait()
	if rotEnd.Before(sendEnd) {
		t.Fatal("rotation completed while the old generation was sending")
	}
	err = s.SendGuarded(ctx, o.ID, func() error { t.Fatal("old generation sent after rotation"); return nil })
	if !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("got %v", err)
	}
}

// #31: BlockOutbox never overwrites SENDING/UPSTREAM_ACCEPTED and never
// releases budget it did not take back.
func TestBlockOutboxIsCompareAndSet(t *testing.T) {
	s, c := faultStore(t)
	ctx := context.Background()
	used := func() int { cu, _ := s.Customer(ctx, c.ID); return cu.WindowUsed }
	o, _ := s.CreateOutbox(ctx, OutboxMessage{CustomerID: c.ID, Generation: c.Generation, UID: c.UID, Body: "x", BudgetUnits: 1})
	base := used()
	if base != 1 {
		t.Fatalf("reserved %d", base)
	}
	s.MarkOutboxSending(ctx, o.ID)
	if _, err := s.BlockOutbox(ctx, o.ID, "late"); !errors.Is(err, ErrInvalidState) {
		t.Fatal("SENDING overwritten", err)
	}
	if got, _ := s.Outbox(ctx, o.ID); got.State != OutboxSending || used() != 1 {
		t.Fatal("state/budget changed", got.State, used())
	}
	s.TransitionOutbox(ctx, o.ID, OutboxUpstreamAccepted, "")
	if _, err := s.BlockOutbox(ctx, o.ID, "late"); !errors.Is(err, ErrInvalidState) || used() != 1 {
		t.Fatal("UPSTREAM_ACCEPTED overwritten or budget released")
	}
	// concurrent: block vs mark-sending -> budget released at most once
	o2, _ := s.CreateOutbox(ctx, OutboxMessage{CustomerID: c.ID, Generation: c.Generation, UID: c.UID, Body: "y", BudgetUnits: 1})
	var wg sync.WaitGroup
	var blocked bool
	wg.Add(2)
	go func() { defer wg.Done(); _, err := s.BlockOutbox(ctx, o2.ID, "x"); blocked = err == nil }()
	go func() { defer wg.Done(); s.MarkOutboxSending(ctx, o2.ID) }()
	wg.Wait()
	got, _ := s.Outbox(ctx, o2.ID)
	want := 2
	if blocked {
		want = 1
		if got.State != OutboxBlocked {
			t.Fatal(got.State)
		}
	} else if got.State != OutboxSending {
		t.Fatal(got.State)
	}
	if used() != want {
		t.Fatalf("budget %d want %d (blocked=%v)", used(), want, blocked)
	}
	if _, err := s.BlockOutbox(ctx, o2.ID, "again"); err == nil {
		t.Fatal("double block")
	}
	if used() != want {
		t.Fatal("budget released twice")
	}
}

// #43: an outbox whose binding's kf account is no longer ACTIVE cannot send.
func TestMissingAccountBlocksSends(t *testing.T) {
	s, c := faultStore(t)
	ctx := context.Background()
	b, _ := s.Binding(ctx, "b1")
	if err := s.UpsertAccount(ctx, KFAccount{OpenKfID: b.OpenKfID, Name: "n", Status: AccountActive}); err != nil {
		t.Fatal(err)
	}
	o1, _ := s.CreateOutbox(ctx, OutboxMessage{CustomerID: c.ID, Generation: c.Generation, UID: c.UID, Body: "a", BudgetUnits: 1})
	o2, _ := s.CreateOutbox(ctx, OutboxMessage{CustomerID: c.ID, Generation: c.Generation, UID: c.UID, Body: "b", BudgetUnits: 1})
	if _, err := s.MarkOutboxSending(ctx, o1.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := s.MarkMissingAccounts(ctx, []string{"other"}); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if _, err := s.MarkOutboxSending(ctx, o2.ID); !errors.Is(err, ErrBindingInactive) {
		t.Fatal("send allowed:", err)
	}
	if err := s.SendGuarded(ctx, o1.ID, func() error { t.Fatal("sent"); return nil }); !errors.Is(err, ErrBindingInactive) {
		t.Fatal("in-flight chunk allowed:", err)
	}
	if n, _ := s.MarkMissingAccounts(ctx, nil); n != 0 {
		t.Fatal("already unknown re-marked")
	}
}

func TestRotateRestoreBindingSecretStore(t *testing.T) {
	s, _ := faultStore(t)
	ctx := context.Background()
	if _, err := s.RotateBindingSecret(ctx, "b1", 99, "x"); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	b, err := s.RotateBindingSecret(ctx, "b1", 1, "new")
	if err != nil || b.Revision != 2 {
		t.Fatal(b, err)
	}
	if err := s.RestoreBindingSecret(ctx, "b1", "old", false); err != nil {
		t.Fatal(err)
	}
	if raw, exp, _ := s.BindingSecret(ctx, "b1"); raw != "old" || exp {
		t.Fatal(raw, exp)
	}
	if err := s.RestoreBindingSecret(ctx, "nope", "x", true); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.DiagnosticEnterprise(ctx, "outbox", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	s.Close()
	if _, err := s.RotateBindingSecret(ctx, "b1", 2, "x"); err == nil {
		t.Fatal("closed")
	}
	if err := s.RestoreBindingSecret(ctx, "b1", "x", true); err == nil {
		t.Fatal("closed")
	}
	if _, err := s.MarkMissingAccounts(ctx, nil); err == nil {
		t.Fatal("closed")
	}
	if _, err := s.DiagnosticEnterprise(ctx, "inbox", "1"); err == nil {
		t.Fatal("closed")
	}
}

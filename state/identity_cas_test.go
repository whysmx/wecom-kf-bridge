package state

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestIdentityIsImmutable(t *testing.T) {
	s, c := faultStore(t)
	ctx := context.Background()
	if err := s.PutEnterprise(ctx, Enterprise{ID: "e1", TenantID: "other-tenant", CorpID: "c1", CredentialRef: "env:X"}); !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("tenant change: %v", err)
	}
	if err := s.PutEnterprise(ctx, Enterprise{ID: "e1", TenantID: "t1", CorpID: "c2", CredentialRef: "env:X"}); !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("corp change: %v", err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.PutEnterprise(ctx, Enterprise{ID: "e2", TenantID: "t2", CorpID: "c9", CredentialRef: "env:Y"}))
	if err := s.PutBinding(ctx, Binding{ID: "b1", EnterpriseID: "e2", OpenKfID: "kf1", ProjectID: "p1", Active: true}); !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("enterprise move: %v", err)
	}
	if err := s.PutBinding(ctx, Binding{ID: "b1", EnterpriseID: "e1", OpenKfID: "kf-other", ProjectID: "p1", Active: true}); !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("kf change: %v", err)
	}
	// mutable fields still update
	must(s.PutBinding(ctx, Binding{ID: "b1", EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p2", Active: true, Revision: 2}))
	// the database itself rejects identity updates from any code path
	for _, q := range []string{
		`UPDATE bindings SET open_kfid='x' WHERE id='b1'`,
		`UPDATE bindings SET enterprise_id='e2' WHERE id='b1'`,
		`UPDATE enterprises SET corp_id='x' WHERE id='e1'`,
		`UPDATE enterprises SET tenant_id='x' WHERE id='e1'`,
		`UPDATE customers SET binding_id='b1', external_user_id='someone-else'`,
		`UPDATE customers SET enterprise_id='e2'`,
	} {
		if _, err := s.DB().Exec(q); identityErr(err) != ErrIdentityChanged {
			t.Fatalf("%s: %v", q, err)
		}
	}
	b, _ := s.Binding(ctx, "b1")
	e, _ := s.Enterprise(ctx, "e1")
	if b.EnterpriseID != "e1" || b.OpenKfID != "kf1" || b.ProjectID != "p2" || e.TenantID != "t1" || e.CorpID != "c1" {
		t.Fatalf("identity changed: %+v %+v", b, e)
	}
	// existing customers are still routed to the original enterprise/kf
	got, err := s.EnsureCustomer(ctx, "e1", "b1", "ext-1")
	if err != nil || got.ID != c.ID || got.UID != c.UID {
		t.Fatalf("customer re-routed: %+v %v", got, err)
	}
	if bb, err := s.BindingByOpenKfID(ctx, "e1", "kf1"); err != nil || bb.ID != "b1" {
		t.Fatalf("routing: %+v %v", bb, err)
	}
}

func newSendable(t *testing.T) (*Store, Customer, OutboxMessage) {
	s, c := faultStore(t)
	o, err := s.CreateOutbox(context.Background(), OutboxMessage{CustomerID: c.ID, Generation: c.Generation, UID: c.UID, Body: "x", BudgetUnits: 3, ChunksTotal: 3})
	if err != nil {
		t.Fatal(err)
	}
	if o.BindingRevision != 1 {
		t.Fatalf("binding revision not recorded: %d", o.BindingRevision)
	}
	return s, c, o
}

func TestMarkOutboxSendingIsConditional(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		mut  func(*Store, Customer)
		want error
	}{
		"rotated binding": {func(s *Store, _ Customer) {
			s.PutBinding(ctx, Binding{ID: "b1", EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p1", Active: true, Revision: 2})
		}, ErrBindingInactive},
		"disabled binding": {func(s *Store, _ Customer) {
			s.PutBinding(ctx, Binding{ID: "b1", EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p1", Active: false})
		}, ErrBindingInactive},
		"takeover": {func(s *Store, c Customer) { s.BeginHandover(ctx, c.ID, "agent") }, ErrHeld},
		"new generation": {func(s *Store, c Customer) {
			s.BeginHandover(ctx, c.ID, "agent")
			s.SetHandoverStatus(ctx, c.ID, CustomerHuman, "agent")
			if _, err := s.RecoverCustomer(ctx, c.ID, "back to AI"); err != nil {
				t.Fatal(err)
			}
		}, ErrStaleGeneration},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s, c, o := newSendable(t)
			tc.mut(s, c)
			if _, err := s.MarkOutboxSending(ctx, o.ID); !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
			if got, _ := s.Outbox(ctx, o.ID); got.State == OutboxSending {
				t.Fatal("entered SENDING")
			}
		})
	}
	s, _, o := newSendable(t)
	if _, err := s.MarkOutboxSending(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkOutboxSending(ctx, o.ID); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("second mark: %v", err)
	}
	if _, err := s.MarkOutboxSending(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

// Concurrent takeovers racing MarkOutboxSending: whenever the row reaches
// SENDING, the takeover must not have been committed before it.
func TestMarkSendingRacesTakeover(t *testing.T) {
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		s, c, o := newSendable(t)
		var wg sync.WaitGroup
		var markErr error
		wg.Add(2)
		go func() { defer wg.Done(); _, markErr = s.MarkOutboxSending(ctx, o.ID) }()
		go func() { defer wg.Done(); s.BeginHandover(ctx, c.ID, "agent") }()
		wg.Wait()
		got, _ := s.Outbox(ctx, o.ID)
		if markErr == nil && got.State != OutboxSending || markErr != nil && got.State == OutboxSending {
			t.Fatalf("result %v but state %s", markErr, got.State)
		}
		// a SENDING row is fenced for every further chunk
		if markErr == nil {
			if err := s.SendGuarded(ctx, o.ID, func() error { return nil }); !errors.Is(err, ErrHeld) {
				t.Fatalf("chunk after takeover: %v", err)
			}
		}
	}
}

// A binding rotation that returned can never be followed by an outbound
// call of a request admitted under the old revision.
func TestSendGuardedSerialisesWithBindingRotation(t *testing.T) {
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		s, _, o := newSendable(t)
		if _, err := s.MarkOutboxSending(ctx, o.ID); err != nil {
			t.Fatal(err)
		}
		var rotated atomic.Bool
		var calls, late atomic.Int32
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for k := 0; k < 3; k++ {
				_ = s.SendGuarded(ctx, o.ID, func() error {
					calls.Add(1)
					if rotated.Load() {
						late.Add(1)
					}
					return nil
				})
			}
		}()
		go func() {
			defer wg.Done()
			if err := s.PutBinding(ctx, Binding{ID: "b1", EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p1", Active: true, Revision: 2}); err != nil {
				t.Error(err)
			}
			rotated.Store(true)
		}()
		wg.Wait()
		if late.Load() != 0 {
			t.Fatalf("%d outbound calls after rotation", late.Load())
		}
		if err := s.SendGuarded(ctx, o.ID, func() error { return nil }); !errors.Is(err, ErrBindingInactive) {
			t.Fatalf("after rotation: %v", err)
		}
	}
	s, _, o := newSendable(t)
	if err := s.SendGuarded(ctx, o.ID, func() error { return nil }); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("not sending: %v", err)
	}
	if err := s.SendGuarded(ctx, "missing", nil); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

// Two writers moving the same row out of the same state: exactly one wins.
func TestTransitionsAreCompareAndSet(t *testing.T) {
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		s, _, o := newSendable(t)
		s.MarkOutboxSending(ctx, o.ID)
		var wg sync.WaitGroup
		var ok atomic.Int32
		for _, to := range []string{OutboxUpstreamAccepted, OutboxRejected, OutboxUnknown} {
			wg.Add(1)
			go func(to string) {
				defer wg.Done()
				var err error
				if to == OutboxUnknown {
					_, err = s.MarkOutboxUnknown(ctx, o.ID, "x")
				} else {
					_, err = s.TransitionOutbox(ctx, o.ID, to, "")
				}
				if err == nil {
					ok.Add(1)
				} else if !errors.Is(err, ErrConflict) && !errors.Is(err, ErrInvalidState) && !errors.Is(err, ErrUnknownResult) {
					t.Error(err)
				}
			}(to)
		}
		wg.Wait()
		if ok.Load() != 1 {
			t.Fatalf("%d concurrent outbox transitions succeeded", ok.Load())
		}
	}
	s, c := faultStore(t)
	if _, err := s.CommitSyncPage(ctx, "s1", "n", false, []InboxMessage{{BindingID: "b1", ExternalMsgID: "m", CustomerID: c.ID, Generation: 1, Type: "text", PayloadRef: "a"}}); err != nil {
		t.Fatal(err)
	}
	m, _ := s.InboxByExternalID(ctx, "s1", "m")
	var wg sync.WaitGroup
	var ok atomic.Int32
	// Both targets are terminal, so even a fully serialised schedule lets
	// exactly one writer win (RECEIVED->CLASSIFIED->IGNORED would be legal).
	for _, to := range []string{InboxIgnored, InboxUnsupported} {
		wg.Add(1)
		go func(to string) {
			defer wg.Done()
			if _, err := s.TransitionInbox(ctx, m.ID, to, ""); err == nil {
				ok.Add(1)
			}
		}(to)
	}
	wg.Wait()
	if ok.Load() != 1 {
		t.Fatalf("%d concurrent inbox transitions succeeded", ok.Load())
	}
}

func TestGuardAndTransitionErrorPaths(t *testing.T) {
	ctx := context.Background()
	s, c, o := newSendable(t)
	if _, err := s.MarkOutboxSending(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	undo := inject(t, s, "UPDATE", "outbox")
	called := false
	if err := s.SendGuarded(ctx, o.ID, func() error { called = true; return nil }); err == nil || called {
		t.Fatalf("guard write failure: err=%v called=%v", err, called)
	}
	undo()
	if _, err := s.TransitionOutbox(ctx, o.ID, "BOGUS", ""); !errors.Is(err, ErrInvalidState) {
		t.Fatal(err)
	}
	if _, err := s.TransitionInbox(ctx, 1, "BOGUS", ""); !errors.Is(err, ErrInvalidState) {
		t.Fatal(err)
	}
	ran := false
	if err := s.WithCustomerLock(c.ID, func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatal("customer lock")
	}
	// identity pre-check surfaces read errors instead of overwriting
	s.Close()
	if err := s.PutBinding(ctx, Binding{ID: "b1", EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p"}); err == nil {
		t.Fatal("closed store")
	}
}

type rowsResult int64

func (r rowsResult) LastInsertId() (int64, error) { return 0, nil }
func (r rowsResult) RowsAffected() (int64, error) { return int64(r), nil }

func TestCompareAndSetLostRace(t *testing.T) {
	if casApplied(rowsResult(0)) != ErrConflict || casApplied(rowsResult(1)) != nil {
		t.Fatal("cas")
	}
	// all conditions hold again (the competing change was undone): the
	// failed CAS is reported as a conflict, never as success
	s, _, o := newSendable(t)
	if err := s.unsendableReason(context.Background(), o); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
}

// A database from before #24 gains the identity triggers when opened; a
// read-only one that cannot get them refuses to open.
func TestIdentityTriggersInstalledOnUpgrade(t *testing.T) {
	path := t.TempDir() + "/pre24.db"
	s, err := OpenWithOptions(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tb := range []string{"enterprises", "bindings", "customers"} {
		if _, err := s.DB().Exec(`DROP TRIGGER immutable_` + tb); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	if _, err := OpenWithOptions("file:"+path+"?mode=ro", Options{}); err == nil {
		t.Fatal("opened without identity protection")
	}
	s, err = OpenWithOptions(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	s.DB().QueryRow(`SELECT COUNT(1) FROM sqlite_master WHERE type='trigger' AND name LIKE 'immutable_%'`).Scan(&n)
	if n != 3 {
		t.Fatalf("%d triggers", n)
	}
}

func TestIdentityChecksSurfaceReadErrors(t *testing.T) {
	s, _, o := newSendable(t)
	s.Close()
	ctx := context.Background()
	if err := s.PutEnterprise(ctx, Enterprise{ID: "e1", TenantID: "t1", CorpID: "c1", CredentialRef: "r"}); err == nil || errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("closed: %v", err)
	}
	if err := s.unsendableReason(ctx, o); err == nil || errors.Is(err, ErrConflict) {
		t.Fatalf("closed: %v", err)
	}
}

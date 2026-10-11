package state

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestFenceSourceManualVsAuto(t *testing.T) {
	s, c := faultStore(t)
	ctx := context.Background()
	if err := s.SetHandoverStatus(ctx, c.ID, CustomerHuman, "service_state_precheck"); err != nil {
		t.Fatal(err)
	}
	if cu, _ := s.Customer(ctx, c.ID); cu.FenceSource != FenceAuto {
		t.Fatal(cu.FenceSource)
	}
	// manual takeover is sticky: a later status write keeps it manual
	if _, err := s.BeginHandover(ctx, c.ID, "admin_handover"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetHandoverStatus(ctx, c.ID, CustomerHuman, "admin_handover"); err != nil {
		t.Fatal(err)
	}
	cu, _ := s.Customer(ctx, c.ID)
	if cu.FenceSource != FenceManual {
		t.Fatal(cu.FenceSource)
	}
	// official AI never clears a manual takeover
	if got, rec, err := s.ObserveOfficialAI(ctx, c.ID); err != nil || rec || got.State != CustomerHuman || got.Generation != cu.Generation {
		t.Fatalf("manual cleared: %+v %v %v", got, rec, err)
	}
	// admin recover clears it and the source
	nc, err := s.RecoverCustomer(ctx, c.ID, "admin_recover")
	if err != nil || nc.FenceSource != "" || nc.State != CustomerAIEligible {
		t.Fatal(nc, err)
	}
	if _, rec, _ := s.ObserveOfficialAI(ctx, c.ID); rec {
		t.Fatal("no-op on AI customer recovered")
	}
	if _, _, err := s.ObserveOfficialAI(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

// Auto fence + official AI: new generation, old UID and its outbox can no
// longer send; the pending inbox is rebound with CAS.
func TestObserveOfficialAIRotatesGeneration(t *testing.T) {
	s, c := faultStore(t)
	ctx := context.Background()
	old, _ := s.CreateOutbox(ctx, OutboxMessage{CustomerID: c.ID, Generation: c.Generation, UID: c.UID, Body: "old", BudgetUnits: 1})
	if err := s.SetHandoverStatus(ctx, c.ID, CustomerWaitingHuman, "service_state_precheck"); err != nil {
		t.Fatal(err)
	}
	nc, rec, err := s.ObserveOfficialAI(ctx, c.ID)
	if err != nil || !rec || nc.Generation != c.Generation+1 || nc.UID == c.UID || nc.State != CustomerAIEligible || nc.FenceSource != "" {
		t.Fatalf("%+v %v %v", nc, rec, err)
	}
	if _, err := s.MarkOutboxSending(ctx, old.ID); err == nil {
		t.Fatal("old-generation outbox can send after auto recovery")
	}
	if _, err := s.CustomerByUID(ctx, "b1", c.UID); !errors.Is(err, ErrStaleGeneration) {
		t.Fatal("old UID not stale", err)
	}
	_, _ = s.CommitSyncPage(ctx, "s1", "n", false, []InboxMessage{{BindingID: "b1", ExternalMsgID: "x", CustomerID: c.ID, Generation: c.Generation, Type: "text"}})
	in, _ := s.InboxByExternalID(ctx, "s1", "x")
	if err := s.RebindInboxGeneration(ctx, in.ID, c.Generation, nc.Generation); err != nil {
		t.Fatal(err)
	}
	if err := s.RebindInboxGeneration(ctx, in.ID, c.Generation, nc.Generation); !errors.Is(err, ErrConflict) {
		t.Fatal("rebind not CAS", err)
	}
	s.Close()
	if _, _, err := s.ObserveOfficialAI(ctx, c.ID); err == nil {
		t.Fatal("closed store")
	}
	if err := s.RebindInboxGeneration(ctx, in.ID, 1, 2); err == nil {
		t.Fatal("closed store")
	}
}

func TestObserveOfficialAIConcurrency(t *testing.T) {
	s, c := faultStore(t)
	ctx := context.Background()
	_ = s.SetHandoverStatus(ctx, c.ID, CustomerHuman, "service_state_precheck")
	var wg sync.WaitGroup
	var mu sync.Mutex
	n := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, rec, err := s.ObserveOfficialAI(ctx, c.ID); err == nil && rec {
				mu.Lock()
				n++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	cu, _ := s.Customer(ctx, c.ID)
	if n != 1 || cu.Generation != c.Generation+1 {
		t.Fatalf("recoveries=%d gen=%d", n, cu.Generation)
	}
	// concurrent admin takeover vs auto recovery: the takeover always wins
	// in the end (it is either after the recovery, or makes it a no-op).
	for i := 0; i < 10; i++ {
		_ = s.SetHandoverStatus(ctx, c.ID, CustomerHuman, "service_state_precheck")
		var w sync.WaitGroup
		w.Add(2)
		go func() { defer w.Done(); s.ObserveOfficialAI(ctx, c.ID) }()
		go func() { defer w.Done(); s.BeginHandover(ctx, c.ID, "admin_handover") }()
		w.Wait()
		cu, _ := s.Customer(ctx, c.ID)
		if cu.State == CustomerAIEligible || cu.FenceSource != FenceManual {
			t.Fatalf("manual takeover lost: %s/%s", cu.State, cu.FenceSource)
		}
		if _, err := s.RecoverCustomer(ctx, c.ID, "admin_recover"); err != nil {
			t.Fatal(err)
		}
	}
}

// Legacy fences (before fence_source) are classified on migration.
func TestFenceSourceMigration(t *testing.T) {
	dir := t.TempDir() + "/m.db"
	s, err := OpenWithOptions(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	seedScope(t, s)
	ctx := context.Background()
	mk := func(ext, st, reason string) string {
		c, _ := s.EnsureCustomer(ctx, "e1", "b1", ext)
		s.DB().Exec(`UPDATE customers SET state=?,fence_source='' WHERE id=?`, st, c.ID)
		if reason != "" {
			s.DB().Exec(`INSERT INTO handover_events(customer_id,old_generation,state,reason,created_at) VALUES(?,1,?,?,1)`, c.ID, st, reason)
		}
		return c.ID
	}
	auto := mk("a", CustomerHuman, "service_state_precheck")
	manual := mk("m", CustomerWaitingHuman, "admin_handover")
	closed := mk("c", CustomerClosed, "")
	ai := mk("i", CustomerAIEligible, "")
	s.Close()
	s, err = OpenWithOptions(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for id, want := range map[string]string{auto: FenceAuto, manual: FenceManual, closed: FenceAuto, ai: ""} {
		if c, _ := s.Customer(ctx, id); c.FenceSource != want {
			t.Errorf("%s: %q want %q", id, c.FenceSource, want)
		}
	}
}

func TestFenceSourceFailures(t *testing.T) {
	s, c := faultStore(t)
	ctx := context.Background()
	_ = s.SetHandoverStatus(ctx, c.ID, CustomerHuman, "service_state_precheck")
	s.DB().Exec(`CREATE TRIGGER f1 BEFORE INSERT ON customer_uids BEGIN SELECT RAISE(ABORT,'x'); END`)
	if _, rec, err := s.ObserveOfficialAI(ctx, c.ID); err == nil || rec {
		t.Fatal("uid insert failure not reported")
	}
	s.DB().Exec(`DROP TRIGGER f1`)
	s.DB().Exec(`CREATE TRIGGER f2 BEFORE INSERT ON handover_events BEGIN SELECT RAISE(ABORT,'x'); END`)
	if _, _, err := s.ObserveOfficialAI(ctx, c.ID); err == nil {
		t.Fatal("event insert failure not reported")
	}
	if cu, _ := s.Customer(ctx, c.ID); cu.Generation != c.Generation || cu.State != CustomerHuman {
		t.Fatal("partial recovery committed")
	}
}

func TestFenceSourceMigrationFailure(t *testing.T) {
	dir := t.TempDir() + "/m.db"
	s, err := OpenWithOptions(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	seedScope(t, s)
	cu, _ := s.EnsureCustomer(context.Background(), "e1", "b1", "a")
	s.DB().Exec(`UPDATE customers SET state='HUMAN',fence_source='' WHERE id=?`, cu.ID)
	s.DB().Exec(`CREATE TRIGGER f3 BEFORE UPDATE OF fence_source ON customers BEGIN SELECT RAISE(ABORT,'x'); END`)
	s.Close()
	if s2, err := OpenWithOptions(dir, Options{}); err == nil {
		s2.Close()
		t.Fatal("migration failure not reported")
	}
}

func TestClosedStoreErrors(t *testing.T) {
	s, c := faultStore(t)
	ctx := context.Background()
	o, _ := s.CreateOutbox(ctx, OutboxMessage{CustomerID: c.ID, Generation: c.Generation, UID: c.UID, Body: "x", BudgetUnits: 1})
	s.Close()
	if err := s.SetSyncState(ctx, "s1", "x"); err == nil {
		t.Fatal("SetSyncState")
	}
	if _, err := s.BlockOutbox(ctx, o.ID, "x"); err == nil {
		t.Fatal("BlockOutbox")
	}
	if _, err := s.EnsureCustomer(ctx, "e1", "b1", "zz"); err == nil {
		t.Fatal("EnsureCustomer")
	}
	if _, err := s.CustomerByUID(ctx, "b1", "nope"); err == nil {
		t.Fatal("CustomerByUID")
	}
}

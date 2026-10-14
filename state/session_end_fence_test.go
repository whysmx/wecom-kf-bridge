package state

import (
	"context"
	"testing"
)

func TestClearManualFenceAfterOfficialEnd(t *testing.T) {
	s, c := faultStore(t)
	ctx := context.Background()
	if _, err := s.BeginHandover(ctx, c.ID, "phrase"); err != nil {
		t.Fatal(err)
	}
	_ = s.SetHandoverStatus(ctx, c.ID, CustomerHuman, "phrase")
	nc, err := s.ClearManualFenceAfterOfficialEnd(ctx, c.ID, "session_status_change_end")
	if err != nil || nc.FenceSource != "" || nc.State != CustomerAIEligible || nc.Generation != c.Generation+1 {
		t.Fatalf("%+v %v", nc, err)
	}
	// idempotent
	nc2, err := s.ClearManualFenceAfterOfficialEnd(ctx, c.ID, "again")
	if err != nil || nc2.Generation != nc.Generation || nc2.FenceSource != "" {
		t.Fatalf("idempotent %+v %v", nc2, err)
	}
}

func TestClearManualFenceCompensatedAI(t *testing.T) {
	s, c := faultStore(t)
	ctx := context.Background()
	if _, err := s.MarkManualFence(ctx, c.ID, "compensate"); err != nil {
		t.Fatal(err)
	}
	nc, err := s.ClearManualFenceAfterOfficialEnd(ctx, c.ID, "inbound_probe")
	if err != nil || nc.FenceSource != "" || nc.State != CustomerAIEligible {
		t.Fatalf("%+v %v", nc, err)
	}
}

func TestClearManualFenceAfterOfficialEndErrors(t *testing.T) {
	s, c := faultStore(t)
	ctx := context.Background()
	_, err := s.ClearManualFenceAfterOfficialEnd(ctx, "missing", "x")
	if err == nil {
		t.Fatal("want not found")
	}
	// WAITING_HUMAN → clear to AI
	if _, err := s.BeginHandover(ctx, c.ID, "phrase"); err != nil {
		t.Fatal(err)
	}
	_ = s.SetHandoverStatus(ctx, c.ID, CustomerWaitingHuman, "phrase")
	nc, err := s.ClearManualFenceAfterOfficialEnd(ctx, c.ID, "from_waiting")
	if err != nil || nc.FenceSource != "" || nc.State != CustomerAIEligible {
		t.Fatalf("WAITING clear: %+v %v", nc, err)
	}
	s.Close()
	if _, err := s.ClearManualFenceAfterOfficialEnd(ctx, c.ID, "closed"); err == nil {
		t.Fatal("want closed store error")
	}
}

func TestMarkManualFenceFaultPaths(t *testing.T) {
	s, c := faultStore(t)
	ctx := context.Background()
	if _, err := s.MarkManualFence(ctx, "missing", "x"); err == nil {
		t.Fatal("want not found")
	}
	undo := inject(t, s, "UPDATE", "customers")
	if _, err := s.MarkManualFence(ctx, c.ID, "upd"); err == nil {
		t.Fatal("want update fault")
	}
	undo()
	undo = inject(t, s, "INSERT", "handover_events")
	if _, err := s.MarkManualFence(ctx, c.ID, "ins"); err == nil {
		t.Fatal("want handover insert fault")
	}
	undo()
	if count(t, s, `SELECT COUNT(1) FROM customers WHERE id=? AND fence_source=''`, c.ID) != 1 {
		t.Fatal("partial fence after fault")
	}
	s.Close()
	if _, err := s.MarkManualFence(ctx, c.ID, "closed"); err == nil {
		t.Fatal("want closed store error")
	}
}

func TestClearAutoFenceInPlaceFaultPaths(t *testing.T) {
	s, c := faultStore(t)
	ctx := context.Background()
	if err := s.SetHandoverStatus(ctx, c.ID, CustomerHuman, "service_state_precheck"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Customer(ctx, c.ID)
	if got.FenceSource != FenceAuto {
		t.Fatalf("want auto fence got %q", got.FenceSource)
	}
	undo := inject(t, s, "UPDATE", "customers")
	if _, err := s.ClearAutoFenceInPlace(ctx, c.ID, "upd"); err == nil {
		t.Fatal("want update fault")
	}
	undo()
	undo = inject(t, s, "INSERT", "handover_events")
	if _, err := s.ClearAutoFenceInPlace(ctx, c.ID, "ins"); err == nil {
		t.Fatal("want handover insert fault")
	}
	undo()
	after, err := s.ClearAutoFenceInPlace(ctx, c.ID, "ok")
	if err != nil || after.State != CustomerAIEligible || after.FenceSource != "" {
		t.Fatalf("%+v %v", after, err)
	}
	s.Close()
	if _, err := s.ClearAutoFenceInPlace(ctx, c.ID, "closed"); err == nil {
		t.Fatal("want closed store error")
	}
}

func TestClearManualFenceAfterOfficialEndFaultPaths(t *testing.T) {
	s, c := faultStore(t)
	ctx := context.Background()
	if _, err := s.BeginHandover(ctx, c.ID, "phrase"); err != nil {
		t.Fatal(err)
	}
	_ = s.SetHandoverStatus(ctx, c.ID, CustomerHuman, "phrase")
	undo := inject(t, s, "INSERT", "customer_uids")
	if _, err := s.ClearManualFenceAfterOfficialEnd(ctx, c.ID, "recover_fault"); err == nil {
		t.Fatal("want recoverTx fault")
	}
	undo()
	got, _ := s.Customer(ctx, c.ID)
	if got.State != CustomerHuman || got.FenceSource != FenceManual {
		t.Fatalf("partial clear: %+v", got)
	}
	undo = inject(t, s, "INSERT", "handover_events")
	if _, err := s.ClearManualFenceAfterOfficialEnd(ctx, c.ID, "evt_fault"); err == nil {
		t.Fatal("want handover insert fault")
	}
	undo()
}

func TestRecoverInFlightOutboxFault(t *testing.T) {
	s, c := faultStore(t)
	ctx := context.Background()
	o, err := s.CreateOutbox(ctx, OutboxMessage{CustomerID: c.ID, Generation: c.Generation, UID: c.UID, Body: "hi", BudgetUnits: 1, ChunksTotal: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkOutboxSending(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	undo := inject(t, s, "UPDATE", "outbox")
	if _, _, err := s.RecoverInFlight(ctx); err == nil {
		t.Fatal("want outbox update fault")
	}
	undo()
}

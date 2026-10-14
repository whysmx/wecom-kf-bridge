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

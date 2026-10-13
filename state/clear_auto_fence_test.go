package state

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestClearAutoFenceInPlace(t *testing.T) {
	st, err := OpenWithOptions(t.TempDir()+"/caf.db", Options{MasterKey: bytes.Repeat([]byte{4}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	_ = st.PutEnterprise(ctx, Enterprise{ID: "e1", TenantID: "t", CorpID: "c", CredentialRef: "r"})
	_ = st.PutBinding(ctx, Binding{ID: "b1", EnterpriseID: "e1", OpenKfID: "kf", ProjectID: "p", Active: true})
	c, err := st.EnsureCustomer(ctx, "e1", "b1", "ext-caf")
	if err != nil {
		t.Fatal(err)
	}

	// Already AI → no-op
	nc, err := st.ClearAutoFenceInPlace(ctx, c.ID, "noop")
	if err != nil || nc.State != CustomerAIEligible {
		t.Fatalf("%v %+v", err, nc)
	}

	// Not found
	if _, err := st.ClearAutoFenceInPlace(ctx, "missing", "x"); !errors.Is(err, ErrNotFound) && err == nil {
		// mapNotFound may wrap
		if err == nil {
			t.Fatal("want not found")
		}
	}

	// Manual fence → ErrHeld
	if _, err := st.BeginHandover(ctx, c.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClearAutoFenceInPlace(ctx, c.ID, "x"); !errors.Is(err, ErrHeld) {
		t.Fatalf("manual: %v", err)
	}

	// Auto fence → clear in place
	if _, err := st.RecoverCustomer(ctx, c.ID, "admin_recover"); err != nil {
		t.Fatal(err)
	}
	_ = st.SetHandoverStatus(ctx, c.ID, CustomerHuman, "service_state_precheck")
	before, _ := st.Customer(ctx, c.ID)
	if before.FenceSource != FenceAuto {
		t.Fatal(before.FenceSource)
	}
	after, err := st.ClearAutoFenceInPlace(ctx, c.ID, "force_trans_ai")
	if err != nil {
		t.Fatal(err)
	}
	if after.State != CustomerAIEligible || after.FenceSource != "" || after.Generation != before.Generation {
		t.Fatalf("%+v gen %d→%d", after, before.Generation, after.Generation)
	}
}

func TestMarkManualFence(t *testing.T) {
	st, err := OpenWithOptions(t.TempDir()+"/mmf.db", Options{MasterKey: bytes.Repeat([]byte{5}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	_ = st.PutEnterprise(ctx, Enterprise{ID: "e1", TenantID: "t", CorpID: "c", CredentialRef: "r"})
	_ = st.PutBinding(ctx, Binding{ID: "b1", EnterpriseID: "e1", OpenKfID: "kf", ProjectID: "p", Active: true})
	c, err := st.EnsureCustomer(ctx, "e1", "b1", "ext-mmf")
	if err != nil {
		t.Fatal(err)
	}
	nc, err := st.MarkManualFence(ctx, c.ID, "compensate")
	if err != nil {
		t.Fatal(err)
	}
	if nc.FenceSource != FenceManual || nc.State != CustomerAIEligible {
		t.Fatalf("%+v", nc)
	}
	// outbox must treat as held
	_, err = st.CreateOutbox(ctx, OutboxMessage{CustomerID: c.ID, Generation: nc.Generation, UID: nc.UID, Body: "x", BudgetUnits: 1, ChunksTotal: 1})
	if !errors.Is(err, ErrHeld) {
		t.Fatalf("want ErrHeld got %v", err)
	}
}

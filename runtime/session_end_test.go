package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	bridge "github.com/whysmx/wecom-kf-bridge"
	"github.com/whysmx/wecom-kf-bridge/state"
)

func TestAlignAfterOfficialSessionEnd(t *testing.T) {
	st, c := unitStore(t)
	ctx := context.Background()
	_, _ = st.BeginHandover(ctx, c.ID, "phrase")
	_ = st.SetHandoverStatus(ctx, c.ID, state.CustomerHuman, "phrase")

	nc, cleared, err := AlignAfterOfficialSessionEnd(ctx, st, c.ID, state.CustomerHuman, "t")
	if err != nil || cleared || nc.FenceSource != state.FenceManual {
		t.Fatalf("HUMAN must keep fence: cleared=%v %+v %v", cleared, nc, err)
	}
	nc, cleared, err = AlignAfterOfficialSessionEnd(ctx, st, c.ID, state.CustomerClosed, "session_end")
	if err != nil || !cleared || nc.FenceSource != "" || nc.State != state.CustomerAIEligible {
		t.Fatalf("CLOSED must clear: cleared=%v %+v %v", cleared, nc, err)
	}
	got, _ := st.Customer(ctx, c.ID)
	if got.OfficialStatus != state.CustomerClosed {
		t.Fatal(got.OfficialStatus)
	}
}

func TestInboundManualFenceClearsWhenOfficialClosed(t *testing.T) {
	st, c := unitStore(t)
	ctx := context.Background()
	_ = st.SetCustomerAuthorized(ctx, c.ID, true)
	_, _ = st.BeginHandover(ctx, c.ID, "phrase")
	_ = st.SetHandoverStatus(ctx, c.ID, state.CustomerHuman, "phrase")
	srv, _ := bridge.NewServer(bridge.Config{Store: st, Bindings: []bridge.Binding{{ID: "b1", CorpID: "v", CallbackToken: "t", CallbackAESKey: key43(4), Enabled: true}}})
	var posts, trans int32
	cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&posts, 1) }))
	t.Cleanup(cc.Close)
	d := &DeliveryWorker{
		Store: st, Server: srv, CallbackURL: map[string]string{"b1": cc.URL}, AgentID: map[string]string{},
		HTTP: cc.Client(), MaxAttempts: 3, MaxConc: 1, Logger: nopRuntimeLogger{},
		OfficialState: func(context.Context, string, state.Customer) (string, error) {
			return state.CustomerClosed, nil
		},
		ForceTransToAI: func(context.Context, string, state.Customer, string) error {
			atomic.AddInt32(&trans, 1)
			return nil
		},
	}
	cu, _ := st.Customer(ctx, c.ID)
	if _, err := st.CommitSyncPage(ctx, "sc", "after-end", false, []state.InboxMessage{{
		BindingID: "b1", ExternalMsgID: "after-end", CustomerID: c.ID, Generation: cu.Generation,
		Type: "text", PayloadRef: "next", CreateTime: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	d.DeliverOnce(ctx)
	if atomic.LoadInt32(&trans) != 0 {
		t.Fatal("must not trans 3→4 on seat-end align path")
	}
	after, _ := st.Customer(ctx, c.ID)
	if after.FenceSource != "" || after.State != state.CustomerAIEligible {
		t.Fatalf("fence not cleared: %+v", after)
	}
	m, _ := st.InboxByExternalID(ctx, "sc", "after-end")
	if m.State == state.InboxHeld || atomic.LoadInt32(&posts) != 1 {
		t.Fatalf("want AI deliver: state=%s posts=%d gen=%d/%d", m.State, posts, m.Generation, after.Generation)
	}
}

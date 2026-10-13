package runtime

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	bridge "github.com/whysmx/wecom-kf-bridge"
	"github.com/whysmx/wecom-kf-bridge/state"
)

func TestMatchHandoverPhraseExactTrim(t *testing.T) {
	cfg := HandoverConfig{}
	if !MatchHandoverPhrase(cfg, "转人工") || !MatchHandoverPhrase(cfg, "  转人工  ") {
		t.Fatal("default phrase")
	}
	if MatchHandoverPhrase(cfg, "请转人工谢谢") || MatchHandoverPhrase(cfg, "转人工吧") {
		t.Fatal("must be exact")
	}
	cfg.Phrase = "人工"
	if !MatchHandoverPhrase(cfg, "人工") || MatchHandoverPhrase(cfg, "转人工") {
		t.Fatal("custom phrase")
	}
}

type handoverFakeAdapter struct {
	bridge.Adapter
	trans  []int
	events []string
	svc    string
}

func (a *handoverFakeAdapter) ServiceState(context.Context, bridge.Binding, bridge.Customer) (string, error) {
	if a.svc == "" {
		return state.CustomerAIEligible, nil
	}
	return a.svc, nil
}
func (a *handoverFakeAdapter) TransServiceState(_ context.Context, _ bridge.Binding, _ bridge.Customer, st int) (string, error) {
	a.trans = append(a.trans, st)
	if st == bridge.OfficialStateWaiting {
		a.svc = state.CustomerWaitingHuman
	}
	if st == bridge.OfficialStateAI {
		a.svc = state.CustomerAIEligible
	}
	return "code-1", nil
}
func (a *handoverFakeAdapter) SendMsgOnEvent(_ context.Context, _ bridge.Binding, code, text string) error {
	a.events = append(a.events, code+"|"+text)
	return nil
}
func (a *handoverFakeAdapter) GetUser(_ context.Context, _ bridge.Binding, c bridge.Customer) (bridge.Customer, error) {
	return c, nil
}
func (a *handoverFakeAdapter) SendText(context.Context, bridge.SendRequest) (string, error) {
	return "", nil
}
func (a *handoverFakeAdapter) GetMedia(context.Context, bridge.Binding, string) (bridge.MediaDownload, error) {
	return bridge.MediaDownload{}, nil
}

func TestRequestHumanHandoverSetsManualFence(t *testing.T) {
	st, err := state.OpenWithOptions(t.TempDir()+"/h.db", state.Options{MasterKey: bytes.Repeat([]byte{9}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	_ = st.PutEnterprise(ctx, state.Enterprise{ID: "e1", TenantID: "t", CorpID: "c", CredentialRef: "env:X"})
	_ = st.PutBinding(ctx, state.Binding{ID: "b1", EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p", Active: true})
	c, err := st.EnsureCustomer(ctx, "e1", "b1", "ext-h")
	if err != nil {
		t.Fatal(err)
	}
	ad := &handoverFakeAdapter{}
	if err := RequestHumanHandover(ctx, st, ad, bridge.Binding{ID: "b1"}, c, HandoverConfig{}, "phrase_handover"); err != nil {
		t.Fatal(err)
	}
	if len(ad.trans) != 1 || ad.trans[0] != bridge.OfficialStateWaiting {
		t.Fatalf("trans %v", ad.trans)
	}
	if len(ad.events) != 1 || ad.events[0] != "code-1|"+defaultHandoverWelcome {
		t.Fatalf("events %v", ad.events)
	}
	got, _ := st.Customer(ctx, c.ID)
	if got.FenceSource != state.FenceManual || got.State != state.CustomerWaitingHuman {
		t.Fatalf("%s/%s", got.State, got.FenceSource)
	}
}

// Inbound: no manual intent + official 2/3 → Trans(1), message delivered.
func TestInboundForceTransToAIWithoutManualIntent(t *testing.T) {
	for _, official := range []string{state.CustomerHuman, state.CustomerWaitingHuman} {
		t.Run(official, func(t *testing.T) {
			st, c := unitStore(t)
			ctx := context.Background()
			_ = st.SetCustomerAuthorized(ctx, c.ID, true)
			srv, _ := bridge.NewServer(bridge.Config{Store: st, Bindings: []bridge.Binding{{ID: "b1", CorpID: "v", CallbackToken: "t", CallbackAESKey: key43(4), Enabled: true}}})
			var posts int32
			cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&posts, 1) }))
			t.Cleanup(cc.Close)
			cur := official
			var trans []int
			d := &DeliveryWorker{
				Store: st, Server: srv, CallbackURL: map[string]string{"b1": cc.URL}, AgentID: map[string]string{},
				HTTP: cc.Client(), MaxAttempts: 3, MaxConc: 1, Logger: nopRuntimeLogger{},
				OfficialState: func(context.Context, string, state.Customer) (string, error) { return cur, nil },
				ForceTransToAI: func(_ context.Context, _ string, _ state.Customer, from string) error {
					trans = append(trans, bridge.OfficialStateAI)
					cur = state.CustomerAIEligible
					return nil
				},
			}
			// Auto fence as if a prior send saw official human (common after 加接待人).
			_ = st.SetHandoverStatus(ctx, c.ID, official, "service_state_precheck")
			before, _ := st.Customer(ctx, c.ID)
			if before.FenceSource != state.FenceAuto {
				t.Fatal(before.FenceSource)
			}
			if _, err := st.CommitSyncPage(ctx, "sc", "in-"+official, false, []state.InboxMessage{{
				BindingID: "b1", ExternalMsgID: "in-" + official, CustomerID: c.ID, Generation: before.Generation,
				Type: "text", PayloadRef: "hello", CreateTime: time.Now(),
			}}); err != nil {
				t.Fatal(err)
			}
			d.DeliverOnce(ctx)
			after, _ := st.Customer(ctx, c.ID)
			m, _ := st.InboxByExternalID(ctx, "sc", "in-"+official)
			if len(trans) != 1 || trans[0] != bridge.OfficialStateAI {
				t.Fatalf("trans %v", trans)
			}
			if after.State != state.CustomerAIEligible || after.FenceSource != "" {
				t.Fatalf("customer %+v", after)
			}
			if after.Generation != before.Generation {
				t.Fatalf("generation bumped: %d→%d", before.Generation, after.Generation)
			}
			if m.State == state.InboxHeld || atomic.LoadInt32(&posts) != 1 {
				t.Fatalf("not delivered: state=%s posts=%d", m.State, posts)
			}
		})
	}
}

// Inbound: BeginHandover(manual) → no Trans→1, message HELD.
func TestInboundManualHandoverBlocksWithoutForceTrans(t *testing.T) {
	st, c := unitStore(t)
	ctx := context.Background()
	_ = st.SetCustomerAuthorized(ctx, c.ID, true)
	srv, _ := bridge.NewServer(bridge.Config{Store: st, Bindings: []bridge.Binding{{ID: "b1", CorpID: "v", CallbackToken: "t", CallbackAESKey: key43(4), Enabled: true}}})
	var posts int32
	cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&posts, 1) }))
	t.Cleanup(cc.Close)
	var trans int32
	d := &DeliveryWorker{
		Store: st, Server: srv, CallbackURL: map[string]string{"b1": cc.URL}, AgentID: map[string]string{},
		HTTP: cc.Client(), MaxAttempts: 3, MaxConc: 1, Logger: nopRuntimeLogger{},
		OfficialState: func(context.Context, string, state.Customer) (string, error) {
			return state.CustomerHuman, nil
		},
		ForceTransToAI: func(context.Context, string, state.Customer, string) error {
			atomic.AddInt32(&trans, 1)
			return nil
		},
	}
	if _, err := st.BeginHandover(ctx, c.ID, "admin_handover"); err != nil {
		t.Fatal(err)
	}
	cu, _ := st.Customer(ctx, c.ID)
	if _, err := st.CommitSyncPage(ctx, "sc", "man1", false, []state.InboxMessage{{
		BindingID: "b1", ExternalMsgID: "man1", CustomerID: c.ID, Generation: cu.Generation,
		Type: "text", PayloadRef: "hi", CreateTime: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	d.DeliverOnce(ctx)
	m, _ := st.InboxByExternalID(ctx, "sc", "man1")
	if atomic.LoadInt32(&trans) != 0 {
		t.Fatal("must not force-trans when manual")
	}
	if m.State != state.InboxHeld || atomic.LoadInt32(&posts) != 0 {
		t.Fatalf("must HELD: state=%s posts=%d", m.State, posts)
	}
}

// Inbound: local still AI-eligible (no fence) but official 3 → still Trans→1 then deliver.
func TestInboundForceTransWhenLocalStillAIEligible(t *testing.T) {
	st, c := unitStore(t)
	ctx := context.Background()
	_ = st.SetCustomerAuthorized(ctx, c.ID, true)
	srv, _ := bridge.NewServer(bridge.Config{Store: st, Bindings: []bridge.Binding{{ID: "b1", CorpID: "v", CallbackToken: "t", CallbackAESKey: key43(4), Enabled: true}}})
	var posts int32
	cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&posts, 1) }))
	t.Cleanup(cc.Close)
	cur := state.CustomerHuman
	var trans []int
	d := &DeliveryWorker{
		Store: st, Server: srv, CallbackURL: map[string]string{"b1": cc.URL}, AgentID: map[string]string{},
		HTTP: cc.Client(), MaxAttempts: 3, MaxConc: 1, Logger: nopRuntimeLogger{},
		OfficialState: func(context.Context, string, state.Customer) (string, error) { return cur, nil },
		ForceTransToAI: func(_ context.Context, _ string, _ state.Customer, _ string) error {
			trans = append(trans, bridge.OfficialStateAI)
			cur = state.CustomerAIEligible
			return nil
		},
	}
	before, _ := st.Customer(ctx, c.ID)
	if before.State != state.CustomerAIEligible || before.FenceSource != "" {
		t.Fatalf("precondition %+v", before)
	}
	if _, err := st.CommitSyncPage(ctx, "sc", "nofence", false, []state.InboxMessage{{
		BindingID: "b1", ExternalMsgID: "nofence", CustomerID: c.ID, Generation: before.Generation,
		Type: "text", PayloadRef: "q", CreateTime: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	d.DeliverOnce(ctx)
	m, _ := st.InboxByExternalID(ctx, "sc", "nofence")
	if len(trans) != 1 {
		t.Fatalf("trans %v", trans)
	}
	if m.State == state.InboxHeld || atomic.LoadInt32(&posts) != 1 {
		t.Fatalf("state=%s posts=%d", m.State, posts)
	}
}

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

// emptyMsgCodeAdapter: Trans succeeds with empty msg_code → RequestHumanHandover
// must skip send_msg_on_event but still BeginHandover(manual).
type emptyMsgCodeAdapter struct {
	handoverFakeAdapter
	eventN int
}

func (a *emptyMsgCodeAdapter) TransServiceState(_ context.Context, _ bridge.Binding, _ bridge.Customer, st int, _ string) (string, error) {
	a.trans = append(a.trans, st)
	return "", nil
}
func (a *emptyMsgCodeAdapter) SendMsgOnEvent(_ context.Context, _ bridge.Binding, _, _ string) error {
	a.eventN++
	return nil
}

func TestRequestHumanHandoverEmptyMsgCodeSkipsEvent(t *testing.T) {
	st, err := state.OpenWithOptions(t.TempDir()+"/h-empty.db", state.Options{MasterKey: bytes.Repeat([]byte{9}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	_ = st.PutEnterprise(ctx, state.Enterprise{ID: "e1", TenantID: "t", CorpID: "c", CredentialRef: "env:X"})
	_ = st.PutBinding(ctx, state.Binding{ID: "b1", EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p", Active: true})
	c, err := st.EnsureCustomer(ctx, "e1", "b1", "ext-empty")
	if err != nil {
		t.Fatal(err)
	}
	ad := &emptyMsgCodeAdapter{}
	if err := RequestHumanHandover(ctx, st, ad, bridge.Binding{ID: "b1", ServicerUserID: "WenHaiYan"}, c, HandoverConfig{Welcome: "自定义欢迎"}, "phrase"); err != nil {
		t.Fatal(err)
	}
	if len(ad.trans) != 1 || ad.trans[0] != bridge.OfficialStateHuman {
		t.Fatalf("want trans→3, got %v", ad.trans)
	}
	if ad.eventN != 0 {
		t.Fatalf("empty msg_code must skip send_msg_on_event, got %d", ad.eventN)
	}
	got, _ := st.Customer(ctx, c.ID)
	if got.FenceSource != state.FenceManual || got.State != state.CustomerHuman {
		t.Fatalf("want manual HUMAN, got %s/%s", got.State, got.FenceSource)
	}
}

func TestRequestHumanHandoverCustomWelcome(t *testing.T) {
	st, err := state.OpenWithOptions(t.TempDir()+"/h-wel.db", state.Options{MasterKey: bytes.Repeat([]byte{9}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	_ = st.PutEnterprise(ctx, state.Enterprise{ID: "e1", TenantID: "t", CorpID: "c", CredentialRef: "env:X"})
	_ = st.PutBinding(ctx, state.Binding{ID: "b1", EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p", Active: true})
	c, err := st.EnsureCustomer(ctx, "e1", "b1", "ext-wel")
	if err != nil {
		t.Fatal(err)
	}
	ad := &handoverFakeAdapter{}
	cfg := HandoverConfig{Welcome: "  请稍等人工  "}
	if err := RequestHumanHandover(ctx, st, ad, bridge.Binding{ID: "b1", ServicerUserID: "WenHaiYan"}, c, cfg, "phrase"); err != nil {
		t.Fatal(err)
	}
	if len(ad.events) != 1 || ad.events[0] != "code-1|请稍等人工" {
		t.Fatalf("events %v", ad.events)
	}
}

// Phrase then a follow-up message: fence_source=manual must block pull-back
// (no ForceTransToAI) and keep inbox HELD — scenario 2 → 3 chain.
func TestInboundPhraseThenManualFenceBlocksPullBack(t *testing.T) {
	st, c := unitStore(t)
	ctx := context.Background()
	_ = st.SetCustomerAuthorized(ctx, c.ID, true)
	srv, _ := bridge.NewServer(bridge.Config{Store: st, Bindings: []bridge.Binding{{ID: "b1", CorpID: "v", CallbackToken: "t", CallbackAESKey: key43(4), Enabled: true}}})
	var posts int32
	cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&posts, 1) }))
	t.Cleanup(cc.Close)
	var forceN int32
	ad := &handoverFakeAdapter{}
	d := &DeliveryWorker{
		Store: st, Server: srv, CallbackURL: map[string]string{"b1": cc.URL}, AgentID: map[string]string{},
		HTTP: cc.Client(), MaxAttempts: 3, MaxConc: 1, Logger: nopRuntimeLogger{},
		HandoverPhrase: "转人工",
		OnHandoverPhrase: func(ctx context.Context, cu state.Customer, m state.InboxMessage) error {
			return RequestHumanHandover(ctx, st, ad, bridge.Binding{ID: "b1", ServicerUserID: "WenHaiYan"}, cu, HandoverConfig{}, "phrase_handover")
		},
		OfficialState: func(context.Context, string, state.Customer) (string, error) {
			return state.CustomerHuman, nil
		},
		ForceTransToAI: func(context.Context, string, state.Customer, string) error {
			atomic.AddInt32(&forceN, 1)
			return nil
		},
	}

	before, _ := st.Customer(ctx, c.ID)
	if _, err := st.CommitSyncPage(ctx, "sc", "ph1", false, []state.InboxMessage{{
		BindingID: "b1", ExternalMsgID: "ph1", CustomerID: c.ID, Generation: before.Generation,
		Type: "text", PayloadRef: "  转人工  ", CreateTime: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	d.DeliverOnce(ctx)
	m1, _ := st.InboxByExternalID(ctx, "sc", "ph1")
	if m1.State != state.InboxHeld || atomic.LoadInt32(&posts) != 0 {
		t.Fatalf("phrase: state=%s posts=%d", m1.State, posts)
	}
	if len(ad.trans) != 1 || ad.trans[0] != bridge.OfficialStateHuman {
		t.Fatalf("phrase must trans→3, got %v", ad.trans)
	}
	cu, _ := st.Customer(ctx, c.ID)
	if cu.FenceSource != state.FenceManual {
		t.Fatal(cu.FenceSource)
	}
	if atomic.LoadInt32(&forceN) != 0 {
		t.Fatal("phrase path must not ForceTransToAI")
	}

	// Follow-up while manual-fenced: official still 2/3 → no pull-back, HELD.
	if _, err := st.CommitSyncPage(ctx, "sc", "ph2", false, []state.InboxMessage{{
		BindingID: "b1", ExternalMsgID: "ph2", CustomerID: c.ID, Generation: cu.Generation,
		Type: "text", PayloadRef: "还有人吗", CreateTime: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	d.DeliverOnce(ctx)
	m2, _ := st.InboxByExternalID(ctx, "sc", "ph2")
	if atomic.LoadInt32(&forceN) != 0 {
		t.Fatal("manual fence must not ForceTransToAI on follow-up")
	}
	if m2.State != state.InboxHeld || atomic.LoadInt32(&posts) != 0 {
		t.Fatalf("follow-up must HELD: state=%s posts=%d", m2.State, posts)
	}
}

// Nil OfficialState/ForceTransToAI hooks → pullBack is a no-op (legacy/tests).
func TestPullBackNilHooksNoOp(t *testing.T) {
	st, c := unitStore(t)
	d := &DeliveryWorker{Store: st, Logger: nopRuntimeLogger{}}
	got, ok := d.pullBackOfficialHumanIfNeeded(context.Background(), c, state.InboxMessage{BindingID: "b1"})
	if !ok || got.ID != c.ID {
		t.Fatalf("ok=%v got=%+v", ok, got)
	}
}

// Official neither WAITING nor HUMAN → no ForceTrans; AI-eligible delivers.
func TestInboundOfficialAISkipsForceTrans(t *testing.T) {
	st, c := unitStore(t)
	ctx := context.Background()
	_ = st.SetCustomerAuthorized(ctx, c.ID, true)
	srv, _ := bridge.NewServer(bridge.Config{Store: st, Bindings: []bridge.Binding{{ID: "b1", CorpID: "v", CallbackToken: "t", CallbackAESKey: key43(4), Enabled: true}}})
	var posts int32
	cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&posts, 1) }))
	t.Cleanup(cc.Close)
	var forceN int32
	d := &DeliveryWorker{
		Store: st, Server: srv, CallbackURL: map[string]string{"b1": cc.URL}, AgentID: map[string]string{},
		HTTP: cc.Client(), MaxAttempts: 3, MaxConc: 1, Logger: nopRuntimeLogger{},
		OfficialState:  func(context.Context, string, state.Customer) (string, error) { return state.CustomerAIEligible, nil },
		ForceTransToAI: func(context.Context, string, state.Customer, string) error { atomic.AddInt32(&forceN, 1); return nil },
	}
	before, _ := st.Customer(ctx, c.ID)
	if _, err := st.CommitSyncPage(ctx, "sc", "ai-ok", false, []state.InboxMessage{{
		BindingID: "b1", ExternalMsgID: "ai-ok", CustomerID: c.ID, Generation: before.Generation,
		Type: "text", PayloadRef: "hi", CreateTime: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	d.DeliverOnce(ctx)
	if atomic.LoadInt32(&forceN) != 0 {
		t.Fatal("official AI must not ForceTrans")
	}
	m, _ := st.InboxByExternalID(ctx, "sc", "ai-ok")
	if m.State == state.InboxHeld || atomic.LoadInt32(&posts) != 1 {
		t.Fatalf("state=%s posts=%d", m.State, posts)
	}
}

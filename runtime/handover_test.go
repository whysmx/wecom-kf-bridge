package runtime

import (
	"bytes"
	"context"
	"errors"
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

func TestHandoverWelcomeCustomAndDefault(t *testing.T) {
	if (HandoverConfig{}).welcome() != defaultHandoverWelcome {
		t.Fatal("default")
	}
	if (HandoverConfig{Welcome: "  hi  "}).welcome() != "hi" {
		t.Fatal("custom")
	}
}

func TestRequestHumanHandoverTransFailure(t *testing.T) {
	st, err := state.OpenWithOptions(t.TempDir()+"/h2.db", state.Options{MasterKey: bytes.Repeat([]byte{9}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	_ = st.PutEnterprise(ctx, state.Enterprise{ID: "e1", TenantID: "t", CorpID: "c", CredentialRef: "env:X"})
	_ = st.PutBinding(ctx, state.Binding{ID: "b1", EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p", Active: true})
	c, err := st.EnsureCustomer(ctx, "e1", "b1", "ext-h2")
	if err != nil {
		t.Fatal(err)
	}
	ad := &handoverFailTransAdapter{err: errBoomRT}
	if err := RequestHumanHandover(ctx, st, ad, bridge.Binding{ID: "b1"}, c, HandoverConfig{}, "phrase"); err == nil {
		t.Fatal("want trans err")
	}
}

type handoverFailTransAdapter struct {
	bridge.Adapter
	err error
}

func (a *handoverFailTransAdapter) TransServiceState(context.Context, bridge.Binding, bridge.Customer, int) (string, error) {
	return "", a.err
}
func (a *handoverFailTransAdapter) SendMsgOnEvent(context.Context, bridge.Binding, string, string) error {
	return nil
}
func (a *handoverFailTransAdapter) GetUser(_ context.Context, _ bridge.Binding, c bridge.Customer) (bridge.Customer, error) {
	return c, nil
}
func (a *handoverFailTransAdapter) SendText(context.Context, bridge.SendRequest) (string, error) {
	return "", nil
}
func (a *handoverFailTransAdapter) GetMedia(context.Context, bridge.Binding, string) (bridge.MediaDownload, error) {
	return bridge.MediaDownload{}, nil
}
func (a *handoverFailTransAdapter) ServiceState(context.Context, bridge.Binding, bridge.Customer) (string, error) {
	return state.CustomerAIEligible, nil
}

var errBoomRT = errors.New("boom")

func TestInboundForceTransFailureHeld(t *testing.T) {
	st, c := unitStore(t)
	ctx := context.Background()
	_ = st.SetCustomerAuthorized(ctx, c.ID, true)
	srv, _ := bridge.NewServer(bridge.Config{Store: st, Bindings: []bridge.Binding{{ID: "b1", CorpID: "v", CallbackToken: "t", CallbackAESKey: key43(4), Enabled: true}}})
	var posts int32
	cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&posts, 1) }))
	t.Cleanup(cc.Close)
	l := &recLogger{}
	d := &DeliveryWorker{
		Store: st, Server: srv, CallbackURL: map[string]string{"b1": cc.URL}, AgentID: map[string]string{},
		HTTP: cc.Client(), MaxAttempts: 3, MaxConc: 1, Logger: l,
		OfficialState: func(context.Context, string, state.Customer) (string, error) {
			return state.CustomerHuman, nil
		},
		ForceTransToAI: func(context.Context, string, state.Customer, string) error {
			return errBoomRT
		},
	}
	before, _ := st.Customer(ctx, c.ID)
	if _, err := st.CommitSyncPage(ctx, "sc", "ftfail", false, []state.InboxMessage{{
		BindingID: "b1", ExternalMsgID: "ftfail", CustomerID: c.ID, Generation: before.Generation,
		Type: "text", PayloadRef: "q", CreateTime: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	d.DeliverOnce(ctx)
	m, _ := st.InboxByExternalID(ctx, "sc", "ftfail")
	if m.State != state.InboxHeld || atomic.LoadInt32(&posts) != 0 {
		t.Fatalf("state=%s posts=%d", m.State, posts)
	}
	if !l.has("handover_inbound_force_trans_failed") {
		t.Fatal("log")
	}
}

func TestInboundForceTransClearFenceFailureHeld(t *testing.T) {
	st, c := unitStore(t)
	ctx := context.Background()
	_ = st.SetCustomerAuthorized(ctx, c.ID, true)
	srv, _ := bridge.NewServer(bridge.Config{Store: st, Bindings: []bridge.Binding{{ID: "b1", CorpID: "v", CallbackToken: "t", CallbackAESKey: key43(4), Enabled: true}}})
	var posts int32
	cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&posts, 1) }))
	t.Cleanup(cc.Close)
	l := &recLogger{}
	d := &DeliveryWorker{
		Store: st, Server: srv, CallbackURL: map[string]string{"b1": cc.URL}, AgentID: map[string]string{},
		HTTP: cc.Client(), MaxAttempts: 3, MaxConc: 1, Logger: l,
		OfficialState: func(context.Context, string, state.Customer) (string, error) {
			return state.CustomerHuman, nil
		},
		ForceTransToAI: func(context.Context, string, state.Customer, string) error { return nil },
	}
	_ = st.SetHandoverStatus(ctx, c.ID, state.CustomerHuman, "service_state_precheck")
	// Break ClearAutoFenceInPlace via trigger on customers update of state
	st.DB().Exec(`CREATE TRIGGER caf_fail BEFORE UPDATE OF state ON customers BEGIN SELECT RAISE(ABORT,'x'); END`)
	before, _ := st.Customer(ctx, c.ID)
	if _, err := st.CommitSyncPage(ctx, "sc", "cafail", false, []state.InboxMessage{{
		BindingID: "b1", ExternalMsgID: "cafail", CustomerID: c.ID, Generation: before.Generation,
		Type: "text", PayloadRef: "q", CreateTime: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	d.DeliverOnce(ctx)
	m, _ := st.InboxByExternalID(ctx, "sc", "cafail")
	if m.State != state.InboxHeld || atomic.LoadInt32(&posts) != 0 {
		t.Fatalf("state=%s posts=%d", m.State, posts)
	}
	if !l.has("handover_inbound_clear_fence_failed") {
		t.Fatal("log")
	}
}

func TestInboundHandoverPhraseHolds(t *testing.T) {
	st, c := unitStore(t)
	ctx := context.Background()
	_ = st.SetCustomerAuthorized(ctx, c.ID, true)
	srv, _ := bridge.NewServer(bridge.Config{Store: st, Bindings: []bridge.Binding{{ID: "b1", CorpID: "v", CallbackToken: "t", CallbackAESKey: key43(4), Enabled: true}}})
	var posts int32
	cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&posts, 1) }))
	t.Cleanup(cc.Close)
	var called int32
	d := &DeliveryWorker{
		Store: st, Server: srv, CallbackURL: map[string]string{"b1": cc.URL}, AgentID: map[string]string{},
		HTTP: cc.Client(), MaxAttempts: 3, MaxConc: 1, Logger: nopRuntimeLogger{},
		HandoverPhrase: "转人工",
		OnHandoverPhrase: func(ctx context.Context, c state.Customer, m state.InboxMessage) error {
			atomic.AddInt32(&called, 1)
			_, err := st.BeginHandover(ctx, c.ID, "phrase_handover")
			return err
		},
	}
	before, _ := st.Customer(ctx, c.ID)
	if _, err := st.CommitSyncPage(ctx, "sc", "phrase1", false, []state.InboxMessage{{
		BindingID: "b1", ExternalMsgID: "phrase1", CustomerID: c.ID, Generation: before.Generation,
		Type: "text", PayloadRef: "转人工", CreateTime: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	d.DeliverOnce(ctx)
	m, _ := st.InboxByExternalID(ctx, "sc", "phrase1")
	if atomic.LoadInt32(&called) != 1 || m.State != state.InboxHeld || atomic.LoadInt32(&posts) != 0 {
		t.Fatalf("called=%d state=%s posts=%d", called, m.State, posts)
	}
	cu, _ := st.Customer(ctx, c.ID)
	if cu.FenceSource != state.FenceManual {
		t.Fatal(cu.FenceSource)
	}
}

func TestInboundHandoverPhraseFailureHeld(t *testing.T) {
	st, c := unitStore(t)
	ctx := context.Background()
	_ = st.SetCustomerAuthorized(ctx, c.ID, true)
	srv, _ := bridge.NewServer(bridge.Config{Store: st, Bindings: []bridge.Binding{{ID: "b1", CorpID: "v", CallbackToken: "t", CallbackAESKey: key43(4), Enabled: true}}})
	cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(cc.Close)
	d := &DeliveryWorker{
		Store: st, Server: srv, CallbackURL: map[string]string{"b1": cc.URL}, AgentID: map[string]string{},
		HTTP: cc.Client(), MaxAttempts: 3, MaxConc: 1, Logger: nopRuntimeLogger{},
		HandoverPhrase:   "转人工",
		OnHandoverPhrase: func(context.Context, state.Customer, state.InboxMessage) error { return errBoomRT },
	}
	before, _ := st.Customer(ctx, c.ID)
	if _, err := st.CommitSyncPage(ctx, "sc", "phrase2", false, []state.InboxMessage{{
		BindingID: "b1", ExternalMsgID: "phrase2", CustomerID: c.ID, Generation: before.Generation,
		Type: "text", PayloadRef: "转人工", CreateTime: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	d.DeliverOnce(ctx)
	m, _ := st.InboxByExternalID(ctx, "sc", "phrase2")
	if m.State != state.InboxHeld {
		t.Fatal(m.State)
	}
}

func TestRequestHumanHandoverBeginHandoverFails(t *testing.T) {
	st, err := state.OpenWithOptions(t.TempDir()+"/h3.db", state.Options{MasterKey: bytes.Repeat([]byte{9}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	_ = st.PutEnterprise(ctx, state.Enterprise{ID: "e1", TenantID: "t", CorpID: "c", CredentialRef: "env:X"})
	_ = st.PutBinding(ctx, state.Binding{ID: "b1", EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p", Active: true})
	c, err := st.EnsureCustomer(ctx, "e1", "b1", "ext-h3")
	if err != nil {
		t.Fatal(err)
	}
	st.DB().Exec(`CREATE TRIGGER bh_fail BEFORE UPDATE OF state ON customers BEGIN SELECT RAISE(ABORT,'x'); END`)
	ad := &handoverFakeAdapter{}
	if err := RequestHumanHandover(ctx, st, ad, bridge.Binding{ID: "b1"}, c, HandoverConfig{}, "phrase"); err == nil {
		t.Fatal("want begin fail")
	}
}

func TestInboundOfficialStateErrorContinues(t *testing.T) {
	st, c := unitStore(t)
	ctx := context.Background()
	_ = st.SetCustomerAuthorized(ctx, c.ID, true)
	srv, _ := bridge.NewServer(bridge.Config{Store: st, Bindings: []bridge.Binding{{ID: "b1", CorpID: "v", CallbackToken: "t", CallbackAESKey: key43(4), Enabled: true}}})
	var posts int32
	cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&posts, 1) }))
	t.Cleanup(cc.Close)
	d := &DeliveryWorker{
		Store: st, Server: srv, CallbackURL: map[string]string{"b1": cc.URL}, AgentID: map[string]string{},
		HTTP: cc.Client(), MaxAttempts: 3, MaxConc: 1, Logger: nopRuntimeLogger{},
		OfficialState:  func(context.Context, string, state.Customer) (string, error) { return "", errBoomRT },
		ForceTransToAI: func(context.Context, string, state.Customer, string) error { t.Fatal("no force"); return nil },
	}
	before, _ := st.Customer(ctx, c.ID)
	if _, err := st.CommitSyncPage(ctx, "sc", "oserr", false, []state.InboxMessage{{
		BindingID: "b1", ExternalMsgID: "oserr", CustomerID: c.ID, Generation: before.Generation,
		Type: "text", PayloadRef: "q", CreateTime: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	d.DeliverOnce(ctx)
	if atomic.LoadInt32(&posts) != 1 {
		t.Fatalf("posts=%d", posts)
	}
}

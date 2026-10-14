package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	bridge "github.com/whysmx/wecom-kf-bridge"
	"github.com/whysmx/wecom-kf-bridge/state"
	"github.com/whysmx/wecom-kf-bridge/wecom"
)

// Inbound manual fence + live get ∈ {0,4,3}: clear on 0/4 (no trans), keep on 3.
func TestInboundManualFenceProbeLive0_4_3(t *testing.T) {
	cases := []struct {
		name    string
		live    string
		clear   bool
		deliver bool
	}{
		{"live0_AI", state.CustomerAIEligible, true, true},
		{"live4_CLOSED", state.CustomerClosed, true, true},
		{"live3_HUMAN", state.CustomerHuman, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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
					return tc.live, nil
				},
				ForceTransToAI: func(context.Context, string, state.Customer, string) error {
					atomic.AddInt32(&trans, 1)
					return nil
				},
			}
			cu, _ := st.Customer(ctx, c.ID)
			ext := "probe-" + tc.name
			if _, err := st.CommitSyncPage(ctx, "sc", ext, false, []state.InboxMessage{{
				BindingID: "b1", ExternalMsgID: ext, CustomerID: c.ID, Generation: cu.Generation,
				Type: "text", PayloadRef: "hi", CreateTime: time.Now(),
			}}); err != nil {
				t.Fatal(err)
			}
			d.DeliverOnce(ctx)
			if atomic.LoadInt32(&trans) != 0 {
				t.Fatal("manual fence probe must never ForceTransToAI")
			}
			after, _ := st.Customer(ctx, c.ID)
			m, _ := st.InboxByExternalID(ctx, "sc", ext)
			if tc.clear {
				if after.FenceSource != "" || after.State != state.CustomerAIEligible {
					t.Fatalf("want fence cleared: %+v", after)
				}
				if !tc.deliver {
					t.Fatal("misconfigured case")
				}
				if m.State == state.InboxHeld || atomic.LoadInt32(&posts) != 1 {
					t.Fatalf("want AI deliver: state=%s posts=%d", m.State, posts)
				}
			} else {
				if after.FenceSource != state.FenceManual || after.State != state.CustomerHuman {
					t.Fatalf("live==3 must keep HUMAN fence: %+v", after)
				}
				if m.State != state.InboxHeld || atomic.LoadInt32(&posts) != 0 {
					t.Fatalf("live==3 must HELD: state=%s posts=%d", m.State, posts)
				}
			}
		})
	}
}

func TestPullBackManualFenceNilOfficialState(t *testing.T) {
	st, c := unitStore(t)
	ctx := context.Background()
	_, _ = st.BeginHandover(ctx, c.ID, "phrase")
	_ = st.SetHandoverStatus(ctx, c.ID, state.CustomerHuman, "phrase")
	cu, _ := st.Customer(ctx, c.ID)
	d := &DeliveryWorker{Store: st, Logger: nopRuntimeLogger{}}
	got, ok := d.pullBackOfficialHumanIfNeeded(ctx, cu, state.InboxMessage{BindingID: "b1"})
	if !ok || got.FenceSource != state.FenceManual {
		t.Fatalf("nil OfficialState must keep fence: ok=%v %+v", ok, got)
	}
}

func TestPullBackManualFenceOfficialStateError(t *testing.T) {
	st, c := unitStore(t)
	ctx := context.Background()
	_, _ = st.BeginHandover(ctx, c.ID, "phrase")
	_ = st.SetHandoverStatus(ctx, c.ID, state.CustomerHuman, "phrase")
	cu, _ := st.Customer(ctx, c.ID)
	d := &DeliveryWorker{
		Store: st, Logger: nopRuntimeLogger{},
		OfficialState: func(context.Context, string, state.Customer) (string, error) {
			return "", errors.New("get failed")
		},
	}
	got, ok := d.pullBackOfficialHumanIfNeeded(ctx, cu, state.InboxMessage{BindingID: "b1"})
	if !ok || got.FenceSource != state.FenceManual {
		t.Fatalf("get error must keep fence: ok=%v %+v", ok, got)
	}
}

func TestPullBackManualFenceAlignError(t *testing.T) {
	st, c := unitStore(t)
	ctx := context.Background()
	_, _ = st.BeginHandover(ctx, c.ID, "phrase")
	_ = st.SetHandoverStatus(ctx, c.ID, state.CustomerHuman, "phrase")
	cu, _ := st.Customer(ctx, c.ID)
	st.Close() // force ClearManualFenceAfterOfficialEnd to fail
	d := &DeliveryWorker{
		Store: st, Logger: nopRuntimeLogger{},
		OfficialState: func(context.Context, string, state.Customer) (string, error) {
			return state.CustomerClosed, nil
		},
	}
	got, ok := d.pullBackOfficialHumanIfNeeded(ctx, cu, state.InboxMessage{BindingID: "b1"})
	if !ok || got.ID != cu.ID {
		t.Fatalf("align error must keep prior customer: ok=%v %+v", ok, got)
	}
}

func TestAlignAfterOfficialSessionEndErrorsAndAIEligible(t *testing.T) {
	st, c := unitStore(t)
	ctx := context.Background()
	_, cleared, err := AlignAfterOfficialSessionEnd(ctx, st, "missing-id", state.CustomerClosed, "t")
	if err == nil || cleared {
		t.Fatalf("missing customer: cleared=%v err=%v", cleared, err)
	}
	_, _ = st.BeginHandover(ctx, c.ID, "phrase")
	_ = st.SetHandoverStatus(ctx, c.ID, state.CustomerHuman, "phrase")
	nc, cleared, err := AlignAfterOfficialSessionEnd(ctx, st, c.ID, state.CustomerAIEligible, "inbound0")
	if err != nil || !cleared || nc.FenceSource != "" || nc.State != state.CustomerAIEligible {
		t.Fatalf("AI_ELIGIBLE(0) must clear: cleared=%v %+v %v", cleared, nc, err)
	}
	got, _ := st.Customer(ctx, c.ID)
	if got.OfficialStatus != state.CustomerAIEligible {
		t.Fatal(got.OfficialStatus)
	}
	st.Close()
	_, cleared, err = AlignAfterOfficialSessionEnd(ctx, st, c.ID, state.CustomerClosed, "closed-store")
	if err == nil || cleared {
		t.Fatalf("closed store: cleared=%v err=%v", cleared, err)
	}
}

// Exercises Gateway Sync.OnSessionEnd (session_status_change change_type=3).
func TestGatewayOnSessionEndAlign(t *testing.T) {
	fw := &fakeWeCom{}
	wx := httptest.NewServer(fw.handler(t))
	t.Cleanup(wx.Close)

	t.Setenv("SE_MASTER", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	t.Setenv("SE_SECRET", "real-secret")
	t.Setenv("SE_CBTOK", "realtok")
	t.Setenv("SE_CBAES", key43(1))
	t.Setenv("SE_VSECRET", "vsecret")
	t.Setenv("SE_VTOK", "vtok")
	t.Setenv("SE_VAES", key43(2))
	raw := fmt.Sprintf(`{
	 "storage":{"database":%q},
	 "security":{"master_key_env":"SE_MASTER","callback_targets":[{"host":"127.0.0.1","port":9,"allowed_cidrs":["127.0.0.0/8"]}]},
	 "wecom":{"allow_insecure_http":true,"api_base_url":%q,"customer_origins":[3],"service_state_map":{"0":"AI_ELIGIBLE","1":"AI_ELIGIBLE","2":"WAITING_HUMAN","3":"HUMAN","4":"CLOSED"},"handover_phrase":"转人工","handover_welcome":"请稍候","handover_scripts":{"end":"会话已结束"}},
	 "enterprises":[{"id":"e1","tenant_key":"acme","corp_id":"wwcorp","secret_env":"SE_SECRET","callback_token_env":"SE_CBTOK","callback_aes_key_env":"SE_CBAES"}],
	 "bindings":[{"id":"b1","enterprise_id":"e1","open_kfid":"kf1","project_id":"p1","virtual_corp_id":"bridge_a","agent_id":"1000002","virtual_secret_env":"SE_VSECRET","callback_token_env":"SE_VTOK","callback_aes_key_env":"SE_VAES","callback_url":"http://127.0.0.1:9/cb","servicer_userid":"WenHaiYan"}]}`,
		t.TempDir()+"/se.db", wx.URL)
	cfg, err := ParseConfig([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	log := &fieldLogger{}
	g, err := Build(context.Background(), cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	if g.Sync.OnSessionEnd == nil {
		t.Fatal("OnSessionEnd not wired")
	}
	ctx := context.Background()
	st := g.Store

	// missing customer
	g.Sync.OnSessionEnd(ctx, wecom.SessionEndEvent{BindingID: "b1", CustomerID: "nope", MsgID: "m0"})

	cu, err := st.EnsureCustomer(ctx, "e1", "b1", "ext-end")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = st.BeginHandover(ctx, cu.ID, "phrase")
	_ = st.SetHandoverStatus(ctx, cu.ID, state.CustomerHuman, "phrase")

	// live still HUMAN → align skipped (fence kept)
	fw.mu.Lock()
	fw.stateGet = `{"errcode":0,"service_state":3}`
	fw.mu.Unlock()
	g.Sync.OnSessionEnd(ctx, wecom.SessionEndEvent{BindingID: "b1", CustomerID: cu.ID, MsgID: "m-skip", MsgCode: "CODE"})
	got, _ := st.Customer(ctx, cu.ID)
	if got.FenceSource != state.FenceManual {
		t.Fatalf("skipped must keep fence: %+v", got)
	}

	// get fails → treat as CLOSED, clear fence; tip send fails (non-blocking)
	fw.mu.Lock()
	fw.stateGet = `{"errcode":40001,"errmsg":"fail"}`
	fw.mu.Unlock()
	// break tip path: override send_msg_on_event via fake returning error — fake always OK;
	// use empty binding so SendMsgOnEvent fails on missing route after align with code.
	// Instead: tip with code on known binding succeeds; tip failure covered via adapter after
	// we force SendMsgOnEvent to fail by using bad binding id after align with CLOSED get.
	fw.mu.Lock()
	fw.stateGet = `{"errcode":0,"service_state":4}`
	fw.mu.Unlock()
	g.Sync.OnSessionEnd(ctx, wecom.SessionEndEvent{BindingID: "b1", CustomerID: cu.ID, MsgID: "m-ok", MsgCode: "ENDCODE"})
	got, _ = st.Customer(ctx, cu.ID)
	if got.FenceSource != "" || got.State != state.CustomerAIEligible {
		t.Fatalf("CLOSED must clear: %+v", got)
	}
	fw.mu.Lock()
	if fw.eventN < 1 {
		t.Fatalf("want end tip send, eventN=%d texts=%v", fw.eventN, fw.eventTexts)
	}
	fw.mu.Unlock()

	// empty msg_code: align skipped path already cleared; re-fence and clear with no tip
	_, _ = st.BeginHandover(ctx, cu.ID, "phrase2")
	_ = st.SetHandoverStatus(ctx, cu.ID, state.CustomerHuman, "phrase2")
	fw.mu.Lock()
	beforeEv := fw.eventN
	fw.stateGet = `{"errcode":0,"service_state":0}`
	fw.mu.Unlock()
	g.Sync.OnSessionEnd(ctx, wecom.SessionEndEvent{BindingID: "b1", CustomerID: cu.ID, MsgID: "m-empty", MsgCode: "  "})
	got, _ = st.Customer(ctx, cu.ID)
	if got.FenceSource != "" {
		t.Fatalf("live0 must clear: %+v", got)
	}
	fw.mu.Lock()
	if fw.eventN != beforeEv {
		t.Fatalf("empty msg_code must skip tip: %d→%d", beforeEv, fw.eventN)
	}
	fw.mu.Unlock()

	// get error → CLOSED fallback clear; tip failure via unknown binding (no route)
	cu2, err := st.EnsureCustomer(ctx, "e1", "b1", "ext-end2")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = st.BeginHandover(ctx, cu2.ID, "p")
	_ = st.SetHandoverStatus(ctx, cu2.ID, state.CustomerHuman, "p")
	fw.mu.Lock()
	fw.stateGet = `{"errcode":500,"errmsg":"boom"}`
	fw.mu.Unlock()
	g.Sync.OnSessionEnd(ctx, wecom.SessionEndEvent{BindingID: "missing-b", CustomerID: cu2.ID, MsgID: "m-getfail", MsgCode: "TIP"})
	got, _ = st.Customer(ctx, cu2.ID)
	if got.FenceSource != "" {
		t.Fatalf("get-fail fallback CLOSED must clear: %+v", got)
	}
}

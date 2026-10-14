package runtime

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bridge "github.com/whysmx/wecom-kf-bridge"
	"github.com/whysmx/wecom-kf-bridge/state"
)

// After an automatic fence (official 2/3 seen at send time), the customer
// writes again: if the official state is AI (0/1), delivery recovers the
// customer with a new generation and delivers the message under the new UID.
// Manual fence + official HUMAN stays held; manual + official AI/CLOSED clears (seat-end sync).
func TestDeliveryAutoRecoversAutomaticFence(t *testing.T) {
	st, c := unitStore(t)
	ctx := context.Background()
	_ = st.SetCustomerAuthorized(ctx, c.ID, true)
	srv, _ := bridge.NewServer(bridge.Config{Store: st, Bindings: []bridge.Binding{{ID: "b1", CorpID: "v", CallbackToken: "t", CallbackAESKey: key43(4), Enabled: true}}})
	var posts int32
	cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&posts, 1) }))
	defer cc.Close()
	official := state.CustomerHuman
	var queries int32
	l := &recLogger{}
	d := &DeliveryWorker{Store: st, Server: srv, CallbackURL: map[string]string{"b1": cc.URL}, AgentID: map[string]string{}, HTTP: cc.Client(), MaxAttempts: 3, MaxConc: 1, Logger: l,
		OfficialState: func(context.Context, string, state.Customer) (string, error) {
			atomic.AddInt32(&queries, 1)
			if official == "err" {
				return "", errors.New("boom")
			}
			return official, nil
		}}
	n := 0
	add := func() string {
		n++
		cu, _ := st.Customer(ctx, c.ID)
		id := "m" + string(rune('0'+n))
		if _, err := st.CommitSyncPage(ctx, "sc", id, false, []state.InboxMessage{{BindingID: "b1", ExternalMsgID: id, CustomerID: c.ID, Generation: cu.Generation, Type: "text", PayloadRef: "q", CreateTime: time.Now()}}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	get := func(id string) state.InboxMessage { m, _ := st.InboxByExternalID(ctx, "sc", id); return m }

	// automatic fence from an official human observation
	_ = st.SetHandoverStatus(ctx, c.ID, state.CustomerHuman, "service_state_precheck")
	// official still human -> held, no recovery
	m1 := add()
	d.DeliverOnce(ctx)
	if get(m1).State != state.InboxHeld || atomic.LoadInt32(&posts) != 0 {
		t.Fatalf("delivered while officially human: %+v", get(m1))
	}
	// official query error -> held, fail closed
	official = "err"
	m2 := add()
	d.DeliverOnce(ctx)
	if get(m2).State != state.InboxHeld {
		t.Fatal("delivered on query error")
	}
	// official back to AI -> new generation, delivered
	official = state.CustomerAIEligible
	m3 := add()
	d.DeliverOnce(ctx)
	cu, _ := st.Customer(ctx, c.ID)
	if cu.State != state.CustomerAIEligible || cu.Generation != c.Generation+1 || cu.FenceSource != "" {
		t.Fatalf("not recovered: %+v", cu)
	}
	if m := get(m3); m.Generation != cu.Generation || m.State == state.InboxHeld || atomic.LoadInt32(&posts) != 1 {
		t.Fatalf("m3 %+v posts=%d", m, posts)
	}
	if get(m1).State != state.InboxHeld || get(m1).Generation != c.Generation {
		t.Fatal("messages held during the human period must stay held")
	}

	// manual fence + official still HUMAN → keep stay (HELD); get is probed.
	official = state.CustomerHuman
	_, _ = st.BeginHandover(ctx, c.ID, "admin_handover")
	q := atomic.LoadInt32(&queries)
	m4 := add()
	d.DeliverOnce(ctx)
	if atomic.LoadInt32(&queries) <= q {
		t.Fatal("manual fence must probe official get")
	}
	if cu2, _ := st.Customer(ctx, c.ID); cu2.FenceSource != state.FenceManual || get(m4).State != state.InboxHeld {
		t.Fatalf("live HUMAN must keep manual fence: %+v m4=%s", cu2, get(m4).State)
	}
	// manual fence + official left human (0/4→AI) → clear fence, deliver
	official = state.CustomerAIEligible
	m4b := add()
	postsBefore := atomic.LoadInt32(&posts)
	d.DeliverOnce(ctx)
	if cu3, _ := st.Customer(ctx, c.ID); cu3.FenceSource != "" || cu3.State != state.CustomerAIEligible {
		t.Fatalf("live AI must clear manual fence: %+v", cu3)
	}
	if get(m4b).State == state.InboxHeld || atomic.LoadInt32(&posts) != postsBefore+1 {
		t.Fatalf("after seat-end align want deliver: %+v posts=%d", get(m4b), posts)
	}
	// no hook -> held (auto fence, OfficialState nil)
	_ = st.SetHandoverStatus(ctx, c.ID, state.CustomerHuman, "service_state_precheck")
	d.OfficialState = nil
	m5 := add()
	d.DeliverOnce(ctx)
	if get(m5).State != state.InboxHeld {
		t.Fatal("no hook")
	}
	// recovery store failure is logged and held
	d.OfficialState = func(context.Context, string, state.Customer) (string, error) { return state.CustomerAIEligible, nil }
	m6 := add()
	st.DB().Exec(`CREATE TRIGGER af BEFORE INSERT ON customer_uids BEGIN SELECT RAISE(ABORT,'x'); END`)
	d.DeliverOnce(ctx)
	if get(m6).State != state.InboxHeld {
		t.Fatal("held on recovery failure")
	}
	if !l.has("auto_recover_failed") {
		t.Fatal("recovery failure not logged")
	}
}

type recLogger struct {
	mu   sync.Mutex
	evts []string
}

func (r *recLogger) Log(ev string, _ map[string]any) {
	r.mu.Lock()
	r.evts = append(r.evts, ev)
	r.mu.Unlock()
}
func (r *recLogger) has(ev string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.evts {
		if e == ev {
			return true
		}
	}
	return false
}

// P1: treat_48002_as_ai_eligible must not let inbound auto-recover clear an
// automatic fence. OfficialState returning the 48002 bypass sentinel is not
// a real AI observation.
func TestAutoRecoverIgnores48002Bypass(t *testing.T) {
	st, c := unitStore(t)
	ctx := context.Background()
	_ = st.SetCustomerAuthorized(ctx, c.ID, true)
	srv, _ := bridge.NewServer(bridge.Config{Store: st, Bindings: []bridge.Binding{{ID: "b1", CorpID: "v", CallbackToken: "t", CallbackAESKey: key43(4), Enabled: true}}})
	var posts int32
	cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&posts, 1) }))
	defer cc.Close()
	d := &DeliveryWorker{Store: st, Server: srv, CallbackURL: map[string]string{"b1": cc.URL}, AgentID: map[string]string{}, HTTP: cc.Client(), MaxAttempts: 3, MaxConc: 1, Logger: nopRuntimeLogger{},
		OfficialState: func(context.Context, string, state.Customer) (string, error) {
			return state.CustomerAIEligibleBypass48002, nil
		}}
	_ = st.SetHandoverStatus(ctx, c.ID, state.CustomerHuman, "service_state_precheck")
	before, _ := st.Customer(ctx, c.ID)
	if before.FenceSource != state.FenceAuto {
		t.Fatal(before.FenceSource)
	}
	if _, err := st.CommitSyncPage(ctx, "sc", "in1", false, []state.InboxMessage{{BindingID: "b1", ExternalMsgID: "in1", CustomerID: c.ID, Generation: before.Generation, Type: "text", PayloadRef: "q", CreateTime: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	d.DeliverOnce(ctx)
	after, _ := st.Customer(ctx, c.ID)
	m, _ := st.InboxByExternalID(ctx, "sc", "in1")
	if after.State != state.CustomerHuman || after.FenceSource != state.FenceAuto || after.Generation != before.Generation {
		t.Fatalf("48002 bypass cleared fence: %+v", after)
	}
	if m.State != state.InboxHeld || atomic.LoadInt32(&posts) != 0 {
		t.Fatalf("delivered to AI under bypass: state=%s posts=%d", m.State, posts)
	}

	// WAITING_HUMAN auto fence likewise
	_, _ = st.RecoverCustomer(ctx, c.ID, "admin_recover") // need AI first to set waiting via SetHandover
	_ = st.SetHandoverStatus(ctx, c.ID, state.CustomerWaitingHuman, "service_state_precheck")
	cu, _ := st.Customer(ctx, c.ID)
	if _, err := st.CommitSyncPage(ctx, "sc", "in2", false, []state.InboxMessage{{BindingID: "b1", ExternalMsgID: "in2", CustomerID: c.ID, Generation: cu.Generation, Type: "text", PayloadRef: "q", CreateTime: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	d.DeliverOnce(ctx)
	cu2, _ := st.Customer(ctx, c.ID)
	m2, _ := st.InboxByExternalID(ctx, "sc", "in2")
	if cu2.State != state.CustomerWaitingHuman || cu2.FenceSource != state.FenceAuto || m2.State != state.InboxHeld || atomic.LoadInt32(&posts) != 0 {
		t.Fatalf("WAITING fence cleared: %+v msg=%s posts=%d", cu2, m2.State, posts)
	}
}

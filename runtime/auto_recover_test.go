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
// Manual takeovers and official human states stay held.
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

	// manual takeover is sticky even if the official state is AI
	_, _ = st.BeginHandover(ctx, c.ID, "admin_handover")
	q := atomic.LoadInt32(&queries)
	m4 := add()
	d.DeliverOnce(ctx)
	if cu2, _ := st.Customer(ctx, c.ID); cu2.State == state.CustomerAIEligible || get(m4).State != state.InboxHeld {
		t.Fatal("manual takeover auto-cleared")
	}
	if atomic.LoadInt32(&queries) != q {
		t.Fatal("official state queried for a manual takeover")
	}
	// no hook -> held
	_, _ = st.RecoverCustomer(ctx, c.ID, "admin_recover")
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

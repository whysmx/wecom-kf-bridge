package runtime

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	bridge "github.com/whysmx/wecom-kf-bridge"
	"github.com/whysmx/wecom-kf-bridge/state"
)

// #28: each callback hostname may only reach its own CIDRs. Target A and
// target B share a port; A's name resolving into B's network is refused.
func TestCallbackDialPolicyIsPerTarget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	c := Config{}
	c.Security.CallbackTargets = []CallbackTarget{
		{Host: "a.internal", Port: mustAtoi(port), AllowedCIDRs: []string{"10.1.0.0/16"}},
		{Host: "b.internal", Port: mustAtoi(port), AllowedCIDRs: []string{"127.0.0.0/8"}},
	}
	lookup := func(_ context.Context, host string) ([]net.IPAddr, error) {
		switch host {
		case "a.internal", "b.internal":
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
		}
		return nil, errors.New("nxdomain")
	}
	cl := newCallbackClient(c, lookup)
	if _, err := cl.Get("http://a.internal:" + port + "/cb"); !errors.Is(err, errTargetNotAllowed) {
		t.Fatalf("A reached B's network: %v", err)
	}
	if r, err := cl.Get("http://b.internal:" + port + "/cb"); err != nil || r.StatusCode != 200 {
		t.Fatalf("B refused: %v", err)
	}
	if _, err := cl.Get("http://c.internal:" + port + "/cb"); !errors.Is(err, errTargetNotAllowed) {
		t.Fatalf("unconfigured host: %v", err)
	}
	// configured name that does not resolve: provably not delivered
	c.Security.CallbackTargets = append(c.Security.CallbackTargets, CallbackTarget{Host: "gone.internal", Port: 80, AllowedCIDRs: []string{"0.0.0.0/0"}})
	_, err := newCallbackClient(c, lookup).Get("http://gone.internal/cb")
	if err == nil || !provablyNotDelivered(err) {
		t.Fatalf("dns failure: %v", err)
	}
	// link-local is never dialled even if inside the allowed CIDR
	c.Security.CallbackTargets = []CallbackTarget{{Host: "169.254.169.254", Port: 80, AllowedCIDRs: []string{"169.254.0.0/16"}}}
	if _, err := newCallbackClient(c, lookup).Get("http://169.254.169.254/latest"); !errors.Is(err, errTargetNotAllowed) {
		t.Fatalf("metadata address dialled: %v", err)
	}
}

// #26: a second process (or Build) on the same database is refused.
func TestSingleInstanceLock(t *testing.T) {
	cfg := gatewayConfig(t)
	g, err := Build(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), cfg, nil); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second instance: %v", err)
	}
	g.Close()
	g2, err := Build(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("lock not released on close: %v", err)
	}
	g2.Close()
	if _, err := AcquireInstanceLock(t.TempDir() + "/no/such/dir/db"); err == nil {
		t.Fatal("unwritable lock path")
	}
	var nl *InstanceLock
	if nl.Release() != nil {
		t.Fatal("nil release")
	}
}

// #24: reusing an ID with a different enterprise/open_kfid/tenant refuses
// to start and leaves stored identity unchanged.
func TestConfigIdentityChangeRefusedAtStartup(t *testing.T) {
	cfg := gatewayConfig(t)
	g, err := Build(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := g.Store.EnsureCustomer(context.Background(), "e1", "b1", "ext")
	g.Close()
	for name, mut := range map[string]func(*Config){
		"open_kfid":  func(c *Config) { c.Bindings[0].OpenKfID = "kf-other" },
		"tenant_key": func(c *Config) { c.Enterprises[0].TenantKey = "other" },
		"corp_id":    func(c *Config) { c.Enterprises[0].CorpID = "wwother" },
	} {
		changed := cfg
		changed.Enterprises = append([]EnterpriseConfig(nil), cfg.Enterprises...)
		changed.Bindings = append([]BindingConfig(nil), cfg.Bindings...)
		mut(&changed)
		if _, err := Build(context.Background(), changed, nil); !errors.Is(err, state.ErrIdentityChanged) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	g, err = Build(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	b, _ := g.Store.Binding(context.Background(), "b1")
	got, _ := g.Store.EnsureCustomer(context.Background(), "e1", "b1", "ext")
	if b.OpenKfID != "kf1" || got.ID != c.ID {
		t.Fatalf("identity drifted: %+v %+v", b, got)
	}
}

// #29: a failed state transition is never treated as success; the
// customer's later messages wait and the visible state is unchanged.
func TestDeliveryTransitionFailuresStopAndKeepState(t *testing.T) {
	ctx := context.Background()
	type tc struct {
		prep  func(st *state.Store, c state.Customer, d *DeliveryWorker)
		msg   state.InboxMessage
		state string
	}
	cases := map[string]tc{
		"unsupported": {nil, state.InboxMessage{Type: "image", CreateTime: time.Now()}, state.InboxReceived},
		"classify":    {nil, state.InboxMessage{Type: "text", CreateTime: time.Now()}, state.InboxReceived},
		"held": {func(st *state.Store, c state.Customer, _ *DeliveryWorker) {
			st.SetCustomerState(ctx, c.ID, state.CustomerHuman)
		}, state.InboxMessage{Type: "text", CreateTime: time.Now()}, state.InboxReady},
		"no_time": {nil, state.InboxMessage{Type: "text"}, state.InboxReady},
		"no_url":  {func(_ *state.Store, _ state.Customer, d *DeliveryWorker) { d.CallbackURL["b1"] = "" }, state.InboxMessage{Type: "text", CreateTime: time.Now()}, state.InboxReady},
		"build_failed": {func(_ *state.Store, _ state.Customer, d *DeliveryWorker) {
			d.Server, _ = bridge.NewServer(bridge.Config{Store: d.Store})
		}, state.InboxMessage{Type: "text", CreateTime: time.Now()}, state.InboxReady},
		"accepted": {nil, state.InboxMessage{Type: "text", CreateTime: time.Now()}, state.InboxPosting},
	}
	for name, k := range cases {
		t.Run(name, func(t *testing.T) {
			st, c := unitStore(t)
			_ = st.SetCustomerAuthorized(ctx, c.ID, true)
			cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
			defer cc.Close()
			srv, _ := bridge.NewServer(bridge.Config{Store: st, Bindings: []bridge.Binding{{ID: "b1", CorpID: "v", CallbackToken: "t", CallbackAESKey: key43(4), Enabled: true}}})
			d := &DeliveryWorker{Store: st, Server: srv, CallbackURL: map[string]string{"b1": cc.URL}, AgentID: map[string]string{}, HTTP: cc.Client(), MaxAttempts: 3, MaxConc: 1, Logger: nopRuntimeLogger{}}
			m := k.msg
			m.BindingID, m.ExternalMsgID, m.CustomerID, m.Generation, m.PayloadRef = "b1", "x1", c.ID, 1, "q"
			if _, err := st.CommitSyncPage(ctx, "sc", "n", false, []state.InboxMessage{m}); err != nil {
				t.Fatal(err)
			}
			cur, _ := st.InboxByExternalID(ctx, "sc", "x1")
			// advance to the state just before the failing transition
			if k.state == state.InboxReady || k.state == state.InboxPosting {
				st.TransitionInbox(ctx, cur.ID, state.InboxClassified, "")
				st.TransitionInbox(ctx, cur.ID, state.InboxReady, "")
			}
			if k.state == state.InboxPosting {
				// fail exactly POSTING -> HTTP_ACCEPTED
				st.DB().Exec(`CREATE TRIGGER f BEFORE UPDATE OF state ON inbox WHEN NEW.state='HTTP_ACCEPTED' BEGIN SELECT RAISE(ABORT,'injected'); END`)
			} else {
				st.DB().Exec(`CREATE TRIGGER f BEFORE UPDATE OF state ON inbox BEGIN SELECT RAISE(ABORT,'injected'); END`)
			}
			if k.prep != nil {
				k.prep(st, c, d)
			}
			cur, _ = st.InboxByExternalID(ctx, "sc", "x1")
			if d.deliver(ctx, cur) {
				t.Fatal("failed transition reported as success")
			}
			if got, _ := st.InboxByExternalID(ctx, "sc", "x1"); got.State != k.state {
				t.Fatalf("state %s, want %s", got.State, k.state)
			}
		})
	}
}

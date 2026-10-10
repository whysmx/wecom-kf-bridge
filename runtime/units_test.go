package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	bridge "github.com/whysmx/wecom-kf-bridge"
	"github.com/whysmx/wecom-kf-bridge/state"
	"github.com/whysmx/wecom-kf-bridge/wecom"
)

func unitStore(t *testing.T) (*state.Store, state.Customer) {
	t.Helper()
	st, err := state.OpenWithOptions(t.TempDir()+"/u.db", state.Options{MasterKey: bytes.Repeat([]byte{3}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	_ = st.PutEnterprise(ctx, state.Enterprise{ID: "e1", TenantID: "t", CorpID: "c", CredentialRef: "r"})
	_ = st.PutBinding(ctx, state.Binding{ID: "b1", EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p", Active: true})
	_ = st.EnsureScope(ctx, "sc", "b1")
	c, _ := st.EnsureCustomer(ctx, "e1", "b1", "ext")
	return st, c
}

func TestAdapterTokenRefreshStateMapAndErrors(t *testing.T) {
	var tokens, sends atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/cgi-bin/gettoken", func(w http.ResponseWriter, r *http.Request) {
		n := tokens.Add(1)
		fmt.Fprintf(w, `{"errcode":0,"access_token":"T%d","expires_in":7200}`, n)
	})
	mux.HandleFunc("/cgi-bin/kf/send_msg", func(w http.ResponseWriter, r *http.Request) {
		if sends.Add(1) == 1 {
			fmt.Fprint(w, `{"errcode":42001,"errmsg":"expired"}`)
			return
		}
		if r.URL.Query().Get("access_token") != "T2" {
			t.Errorf("not refreshed")
		}
		fmt.Fprint(w, `{"errcode":0,"msgid":"m"}`)
	})
	mux.HandleFunc("/cgi-bin/kf/service_state/get", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"errcode":0,"service_state":9}`)
	})
	mux.HandleFunc("/cgi-bin/kf/customer/batchget", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"errcode":40001}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	st, c := unitStore(t)
	cl := wecom.NewClient(srv.URL, "c", "s")
	a := &WeComAdapter{clients: map[string]*wecom.Client{"e1": cl}, tokens: map[string]*tokenCache{"e1": {client: cl, now: time.Now}}, routes: map[string]bindingRoute{"b1": {"e1", "kf1"}}, states: map[int]string{1: state.CustomerAIEligible}, store: st, now: time.Now}
	ctx := context.Background()
	b := bridge.Binding{ID: "b1"}
	cust := bridge.Customer{ID: c.ID, ExternalUserID: "ext"}
	if id, err := a.SendText(ctx, bridge.SendRequest{BindingID: "b1", Customer: cust, Content: "x"}); err != nil || id != "m" {
		t.Fatalf("send after refresh %q %v", id, err)
	}
	if st, err := a.ServiceState(ctx, b, cust); err != nil || st != state.CustomerUnknown {
		t.Fatalf("unmapped state must be UNKNOWN: %q %v", st, err)
	}
	if _, err := a.GetUser(ctx, b, cust); err == nil {
		t.Fatal("profile error swallowed")
	}
	if _, err := a.SendText(ctx, bridge.SendRequest{BindingID: "nope"}); err == nil {
		t.Fatal("unknown binding")
	}
	if _, err := a.ServiceState(ctx, bridge.Binding{ID: "nope"}, cust); err == nil {
		t.Fatal("unknown binding")
	}
	if _, err := a.GetUser(ctx, bridge.Binding{ID: "nope"}, cust); err == nil {
		t.Fatal("unknown binding")
	}
	// token endpoint down -> send provably not executed
	down := wecom.NewClient("http://127.0.0.1:1", "c", "s")
	down.Backoff = time.Millisecond
	a.tokens["e1"] = &tokenCache{client: down, now: time.Now}
	_, err := a.SendText(ctx, bridge.SendRequest{BindingID: "b1", Customer: cust})
	var se *bridge.SendError
	if !errors.As(err, &se) || se.Kind != bridge.SendUnavailable {
		t.Fatalf("token failure kind: %v", err)
	}
	if _, err := a.ServiceState(ctx, b, cust); err == nil {
		t.Fatal("state with no token")
	}
	if e := classifySendError(errors.New("timeout")); !errors.As(e, &se) || se.Kind != bridge.SendUnknown {
		t.Fatal("unknown default")
	}
	tc := &tokenCache{token: "x", exp: time.Now().Add(time.Hour), now: time.Now}
	tc.invalidate("other")
	if tc.token != "x" {
		t.Fatal("invalidate wrong token")
	}
}

func TestDeliveryWorkerPaths(t *testing.T) {
	st, c := unitStore(t)
	ctx := context.Background()
	var code atomic.Int32
	code.Store(500)
	cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(int(code.Load())) }))
	defer cc.Close()
	srv, err := bridge.NewServer(bridge.Config{Store: st, Bindings: []bridge.Binding{{ID: "b1", CorpID: "v", CallbackToken: "t", CallbackAESKey: key43(4), Enabled: true}}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	_, err = st.CommitSyncPage(ctx, "sc", "c1", false, []state.InboxMessage{
		{BindingID: "b1", ExternalMsgID: "img", CustomerID: c.ID, Generation: 1, Type: "image", CreateTime: now},
		{BindingID: "b1", ExternalMsgID: "t1", CustomerID: c.ID, Generation: 1, Type: "text", PayloadRef: "q1", CreateTime: now},
		{BindingID: "b1", ExternalMsgID: "t2", CustomerID: c.ID, Generation: 1, Type: "text", PayloadRef: "q2", CreateTime: now},
	})
	if err != nil {
		t.Fatal(err)
	}
	d := &DeliveryWorker{Store: st, Server: srv, CallbackURL: map[string]string{"b1": cc.URL}, AgentID: map[string]string{}, HTTP: cc.Client(), MaxAttempts: 3, MaxConc: 2, Logger: nopRuntimeLogger{}, Interval: time.Hour}
	d.DeliverOnce(ctx)
	get := func(id string) state.InboxMessage { m, _ := st.InboxByExternalID(ctx, "sc", id); return m }
	if get("img").State != state.InboxUnsupported || get("t1").State != state.InboxDeliveryUnknown || get("t2").State != state.InboxReceived {
		t.Fatalf("%s %s %s", get("img").State, get("t1").State, get("t2").State)
	}
	// connection refused -> RETRY_WAIT (provably not delivered)
	d.CallbackURL["b1"] = "http://127.0.0.1:1/cb"
	d.HTTP = &http.Client{}
	d.DeliverOnce(ctx)
	if get("t2").State != state.InboxRetryWait {
		t.Fatalf("t2 %s", get("t2").State)
	}
	d.CallbackURL["b1"] = cc.URL
	code.Store(200)
	d.HTTP = cc.Client()
	d.DeliverOnce(ctx)
	if get("t2").State != state.InboxHTTPAccepted {
		t.Fatalf("t2 %s", get("t2").State)
	}
	// held customer and missing callback url
	_, _ = st.CommitSyncPage(ctx, "sc", "c2", false, []state.InboxMessage{{BindingID: "b1", ExternalMsgID: "t3", CustomerID: c.ID, Generation: 1, Type: "text", PayloadRef: "q", CreateTime: now}, {BindingID: "b1", ExternalMsgID: "t4", CustomerID: c.ID, Generation: 1, Type: "text", PayloadRef: "q"}})
	d.CallbackURL["b1"] = ""
	d.DeliverOnce(ctx)
	if get("t3").State != state.InboxHeld || get("t3").ErrorCategory != "no_callback_url" {
		t.Fatalf("t3 %+v", get("t3"))
	}
	d.CallbackURL["b1"] = cc.URL
	d.DeliverOnce(ctx)
	if get("t4").ErrorCategory != "missing_create_time" {
		t.Fatalf("t4 %+v", get("t4"))
	}
	_, _ = st.CommitSyncPage(ctx, "sc", "c3", false, []state.InboxMessage{{BindingID: "b1", ExternalMsgID: "t5", CustomerID: c.ID, Generation: 1, Type: "text", PayloadRef: "q", CreateTime: now}})
	_ = st.SetCustomerState(ctx, c.ID, state.CustomerHuman)
	d.Kick()
	d.Kick()
	rctx, cancel := context.WithCancel(ctx)
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	d.Run(rctx)
	if get("t5").State != state.InboxHeld {
		t.Fatalf("t5 %s", get("t5").State)
	}
}

func TestSyncWorkerAndNotifications(t *testing.T) {
	st, _ := unitStore(t)
	var calls atomic.Int32
	wx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "gettoken") {
			fmt.Fprint(w, `{"errcode":0,"access_token":"A","expires_in":7200}`)
			return
		}
		calls.Add(1)
		fmt.Fprint(w, `{"errcode":0,"next_cursor":"n","has_more":0,"msg_list":[{"msgid":"m1","open_kfid":"kf1","external_userid":"ext","origin":3,"msgtype":"text","send_time":1}]}`)
	}))
	defer wx.Close()
	cl := wecom.NewClient(wx.URL, "c", "s")
	var synced atomic.Int32
	w := &SyncWorker{Store: st, Scopes: map[string]scopeInfo{"sc": {enterpriseID: "e1", bindingID: "b1", openKfID: "kf1", client: cl, tokens: &tokenCache{client: cl, now: time.Now}}}, Origins: wecom.OriginPolicy{CustomerOrigins: []int{3}}, Interval: 20 * time.Millisecond, MaxConc: 1, Logger: nopRuntimeLogger{}, OnSynced: func() { synced.Add(1) }}
	g := &Gateway{Store: st, Sync: w, scopes: map[string]string{"e1|kf1": "sc"}}
	ctx := context.Background()
	if err := g.onNotification(ctx, "e1", wecom.Notification{Event: "other"}); err != nil {
		t.Fatal(err)
	}
	if err := g.onNotification(ctx, "e1", wecom.Notification{Event: "kf_msg_or_event", OpenKfID: "zzz"}); err == nil {
		t.Fatal("unbound kf accepted")
	}
	if err := g.onNotification(ctx, "e1", wecom.Notification{Event: "kf_msg_or_event", OpenKfID: "kf1"}); err != nil {
		t.Fatal(err)
	}
	if err := g.onNotification(ctx, "e1", wecom.Notification{Event: "kf_msg_or_event", OpenKfID: "kf1", Token: "P"}); err != nil {
		t.Fatal(err)
	}
	w.Wake("unknown-scope")
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { w.Run(rctx); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for synced.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if synced.Load() == 0 || calls.Load() == 0 {
		t.Fatal("sync did not run")
	}
	// token failure path
	bad := wecom.NewClient("http://127.0.0.1:1", "c", "s")
	bad.Backoff = time.Millisecond
	w.Scopes["sc"] = scopeInfo{enterpriseID: "e1", bindingID: "b1", client: bad, tokens: &tokenCache{client: bad, now: time.Now}}
	w.SyncOnce(ctx, "sc")
	if (&Gateway{}).Close() != nil {
		t.Fatal("close nil")
	}
	if ShutdownGrace(Config{}) != 0 {
		t.Fatal("grace")
	}
}

func TestConfigValidationErrors(t *testing.T) {
	bad := `{"storage":{"database":"d"},"wecom":{"api_base_url":"https://x","customer_origins":[3],"service_state_map":{"x":"AI_ELIGIBLE","1":"BAD"}},
	"enterprises":[{"id":"e"},{"id":"e"}],"bindings":[{"id":"b","enterprise_id":"zz","callback_url":"http://nope:1"},{"id":"b"}]}`
	_, err := ParseConfig([]byte(bad))
	for _, s := range []string{"service_state_map", "enterprise e", "duplicate enterprise", "binding b", "duplicate binding", "callback_targets"} {
		if err == nil || !strings.Contains(err.Error(), s) {
			t.Fatalf("missing %q in %v", s, err)
		}
	}
	if _, err := ParseConfig([]byte(`{"storage":{"database":"d"},"wecom":{"api_base_url":"ftp://x","customer_origins":[3]}}`)); err == nil {
		t.Fatal("bad url")
	}
	t.Setenv("BAD_MASTER", "short")
	c, _ := ParseConfig([]byte(`{"storage":{"database":"d"},"security":{"master_key_env":"BAD_MASTER"},"wecom":{"api_base_url":"https://x","customer_origins":[3]}}`))
	if _, err := Build(context.Background(), c, nil); err == nil {
		t.Fatal("bad master key")
	}
	t.Setenv("BAD_MASTER", strings.Repeat("A", 43))
	c.Enterprises = []EnterpriseConfig{{ID: "e", TenantKey: "k", CorpID: "c", SecretEnv: "UNSET_X"}}
	c.Storage.Database = t.TempDir() + "/x.db"
	if _, err := Build(context.Background(), c, nil); err == nil || !strings.Contains(err.Error(), "UNSET_X") {
		t.Fatalf("missing secret env: %v", err)
	}
}

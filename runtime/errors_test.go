package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	bridge "github.com/whysmx/wecom-kf-bridge"
	"github.com/whysmx/wecom-kf-bridge/state"
	"github.com/whysmx/wecom-kf-bridge/wecom"
)

var gatewayEnv = map[string]string{
	"G_MASTER": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)),
	"G_SECRET": "s", "G_CBTOK": "t", "G_CBAES": key43(1),
	"G_VSECRET": "v", "G_VTOK": "vt", "G_VAES": key43(2),
}

func gatewayConfig(t *testing.T) Config {
	t.Helper()
	for k, v := range gatewayEnv {
		t.Setenv(k, v)
	}
	raw := fmt.Sprintf(`{"storage":{"database":%q},"security":{"master_key_env":"G_MASTER","callback_targets":[{"host":"127.0.0.1","port":9,"allowed_cidrs":["127.0.0.0/8"]}]},
	 "wecom":{"api_base_url":"http://127.0.0.1:1","customer_origins":[3]},
	 "enterprises":[{"id":"e1","tenant_key":"acme","corp_id":"wwcorp","secret_env":"G_SECRET","callback_token_env":"G_CBTOK","callback_aes_key_env":"G_CBAES"}],
	 "bindings":[{"id":"b1","enterprise_id":"e1","open_kfid":"kf1","project_id":"p1","virtual_corp_id":"bridge_a","agent_id":"1000002","virtual_secret_env":"G_VSECRET","callback_token_env":"G_VTOK","callback_aes_key_env":"G_VAES","callback_url":"http://127.0.0.1:9/cb"}]}`, t.TempDir()+"/g.db")
	c, err := ParseConfig([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Each missing or malformed secret must abort startup and name the source.
func TestBuildFailsClosedOnEachSecret(t *testing.T) {
	g, err := Build(context.Background(), gatewayConfig(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	g.Close()
	for k := range gatewayEnv {
		if k == "G_MASTER" {
			continue
		}
		t.Run("unset_"+k, func(t *testing.T) {
			c := gatewayConfig(t)
			t.Setenv(k, "")
			if _, err := Build(context.Background(), c, nil); err == nil {
				t.Fatalf("started without %s", k)
			}
		})
	}
	for _, k := range []string{"G_CBAES", "G_VAES"} {
		c := gatewayConfig(t)
		t.Setenv(k, "short")
		if _, err := Build(context.Background(), c, nil); err == nil {
			t.Fatalf("bad aes %s accepted", k)
		}
	}
	c := gatewayConfig(t)
	c.Storage.Database = t.TempDir() + "/missing/dir/x.db"
	if _, err := Build(context.Background(), c, nil); err == nil {
		t.Fatal("unopenable database accepted")
	}
	c = gatewayConfig(t)
	c.Enterprises[0].TenantKey = "bad key/../x"
	if _, err := Build(context.Background(), c, nil); err == nil {
		t.Fatal("invalid tenant key accepted")
	}
}

func TestNotificationStoreFailure(t *testing.T) {
	st, _ := unitStore(t)
	g := &Gateway{Store: st, Sync: &SyncWorker{}, scopes: map[string]string{"e1|kf1": "sc"}}
	st.Close()
	if err := g.onNotification(context.Background(), "e1", wecom.Notification{Event: "kf_msg_or_event", OpenKfID: "kf1"}); err == nil {
		t.Fatal("store failure swallowed: WeChat must see a non-success and retry")
	}
}

type stuckWorker struct{}

func (stuckWorker) Run(context.Context) { select {} }

func TestAppEdgeCases(t *testing.T) {
	a, _ := NewApp(AppConfig{Addr: "127.0.0.1:0"})
	if a.Shutdown(context.Background()) != nil {
		t.Fatal("shutdown before serve")
	}
	rr := httptest.NewRecorder()
	a.cfg.Handler.ServeHTTP(rr, httptest.NewRequest("GET", "/x", nil))
	if rr.Code != 404 {
		t.Fatal("default handler")
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ln.Close()
	if a.Serve(ln) == nil {
		t.Fatal("closed listener")
	}
	// a worker that ignores cancellation makes shutdown report it
	b, _ := NewApp(AppConfig{Addr: "127.0.0.1:0", Workers: []Worker{stuckWorker{}}, ShutdownTimeout: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.RunSignals(ctx) }()
	<-b.ready
	cancel()
	if err := <-done; err == nil || !strings.Contains(err.Error(), "workers did not stop") {
		t.Fatalf("got %v", err)
	}
}

func TestSyncWorkerErrorPaths(t *testing.T) {
	st, _ := unitStore(t)
	wx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "gettoken") {
			fmt.Fprint(w, `{"errcode":0,"access_token":"A","expires_in":7200}`)
			return
		}
		fmt.Fprint(w, `{"errcode":95000,"errmsg":"bad"}`)
	}))
	defer wx.Close()
	cl := wecom.NewClient(wx.URL, "c", "s")
	cl.MaxRetries = 0
	var logged []string
	var mu sync.Mutex
	w := &SyncWorker{Store: st, Scopes: map[string]scopeInfo{"sc": {enterpriseID: "e1", bindingID: "b1", openKfID: "kf1", client: cl, tokens: &tokenCache{client: cl, now: time.Now}}}, Origins: wecom.OriginPolicy{CustomerOrigins: []int{3}}, Interval: time.Hour, MaxConc: 1, Logger: LoggerFunc(func(e string, _ map[string]any) { mu.Lock(); logged = append(logged, e); mu.Unlock() })}
	ctx := context.Background()
	w.SyncOnce(ctx, "sc")
	st.Close()
	w.SyncOnce(ctx, "sc")
	w.resumePending(ctx, func(string) { t.Fatal("started") })
	got := strings.Join(logged, ",")
	for _, e := range []string{"sync_failed", "sync_pull_token_unreadable", "sync_pending_query_failed"} {
		if !strings.Contains(got, e) {
			t.Fatalf("missing %s in %s", e, got)
		}
	}
	// cancelled while waiting for the concurrency slot
	w2 := &SyncWorker{Store: st, Scopes: map[string]scopeInfo{}, Interval: time.Hour, MaxConc: 1, Logger: nopRuntimeLogger{}}
	w2.init()
	w2.running["x"] = false
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	w2.Run(cctx)
}

func TestDeliveryErrorPaths(t *testing.T) {
	st, c := unitStore(t)
	ctx := context.Background()
	srv, _ := bridge.NewServer(bridge.Config{Store: st, Bindings: []bridge.Binding{{ID: "b1", CorpID: "v", CallbackToken: "t", CallbackAESKey: key43(4), Enabled: true}}})
	_ = st.SetCustomerAuthorized(ctx, c.ID, true)
	_ = st.PutBinding(ctx, state.Binding{ID: "b9", EnterpriseID: "e1", OpenKfID: "kf9", ProjectID: "p", Active: true})
	now := time.Now()
	add := func(id, binding string) {
		if _, err := st.CommitSyncPage(ctx, "sc", id, false, []state.InboxMessage{{BindingID: binding, ExternalMsgID: id, CustomerID: c.ID, Generation: 1, Type: "text", PayloadRef: "q", CreateTime: now}}); err != nil {
			t.Fatal(err)
		}
	}
	get := func(id string) state.InboxMessage { m, _ := st.InboxByExternalID(ctx, "sc", id); return m }
	// transport error after the request was written: outcome unknown
	hj := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}))
	defer hj.Close()
	d := &DeliveryWorker{Store: st, Server: srv, CallbackURL: map[string]string{"b1": hj.URL, "b9": hj.URL}, AgentID: map[string]string{}, HTTP: hj.Client(), MaxAttempts: 3, MaxConc: 1, Logger: nopRuntimeLogger{}}
	add("u1", "b1")
	d.DeliverOnce(ctx)
	if get("u1").State != state.InboxDeliveryUnknown {
		t.Fatalf("u1 %s", get("u1").State)
	}
	// binding unknown to the compat server: cannot build callback
	add("u2", "b9")
	d.DeliverOnce(ctx)
	if get("u2").ErrorCategory != "build_failed" {
		t.Fatalf("u2 %+v", get("u2"))
	}
	// malformed target URL
	add("u3", "b1")
	d.CallbackURL["b1"] = "http://bad host\x7f"
	d.DeliverOnce(ctx)
	if get("u3").State != state.InboxRetryWait {
		t.Fatalf("u3 %s", get("u3").State)
	}
	// customer row missing, then closed store
	m := state.InboxMessage{ID: 999, CustomerID: "nope", State: state.InboxReady}
	if d.deliver(ctx, m) {
		t.Fatal("missing customer delivered")
	}
	st.Close()
	d.DeliverOnce(ctx)
	if d.deliver(ctx, state.InboxMessage{ID: 1, State: state.InboxReceived, Type: "text"}) {
		t.Fatal("transition failure ignored")
	}
	if d.deliver(ctx, state.InboxMessage{ID: 1, State: state.InboxClassified, Type: "text"}) {
		t.Fatal("transition failure ignored")
	}
}

func TestCallbackDialerAddressChecks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	c := Config{}
	c.Security.CallbackTargets = []CallbackTarget{{Host: "127.0.0.1", Port: 1, AllowedCIDRs: []string{"127.0.0.0/8"}}}
	cl := callbackHTTPClient(c)
	if _, err := cl.Get(srv.URL); !errors.Is(err, errTargetNotAllowed) {
		t.Fatalf("port mismatch allowed: %v", err)
	}
	if _, err := cl.Get("http://0.0.0.0:" + port); !errors.Is(err, errTargetNotAllowed) {
		t.Fatalf("unspecified allowed: %v", err)
	}
	c.Security.CallbackTargets[0].Port = mustAtoi(port)
	if r, err := callbackHTTPClient(c).Get(srv.URL); err != nil || r.StatusCode != 200 {
		t.Fatalf("allowed target refused: %v", err)
	}
	if provablyNotDelivered(errors.New("eof")) {
		t.Fatal("generic error treated as not delivered")
	}
}

func mustAtoi(s string) int { var n int; fmt.Sscan(s, &n); return n }

func TestAdapterRefreshFailsAfterRejection(t *testing.T) {
	n := 0
	wx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "gettoken") {
			n++
			if n > 1 {
				fmt.Fprint(w, `{"errcode":40013}`)
				return
			}
			fmt.Fprint(w, `{"errcode":0,"access_token":"A","expires_in":7200}`)
			return
		}
		fmt.Fprint(w, `{"errcode":40014}`)
	}))
	defer wx.Close()
	cl := wecom.NewClient(wx.URL, "c", "s")
	tc := &tokenCache{client: cl, now: time.Now}
	_, err := withToken(context.Background(), tc, func(tok string) (int, error) { return 0, &wecom.APIError{Code: 40014} })
	if !errors.Is(err, errTokenFetch) {
		t.Fatalf("got %v", err)
	}
}

func TestLoggerAndHealthEdges(t *testing.T) {
	l := NewLogger(nil)
	l.Log("", map[string]any{"bad": func() {}})
	var buf bytes.Buffer
	h := NewHealth(HealthConfig{Logger: NewLogger(&buf), Database: func(context.Context) error { return errors.New("db down") }})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/readyz", nil))
	if rr.Code == 200 || !strings.Contains(buf.String(), "health_check_failed") {
		t.Fatalf("%d %s", rr.Code, buf.String())
	}
	var nh *Health
	if nh.Ready(context.Background()) {
		t.Fatal("nil health ready")
	}
	if classifyError(nil) != "" {
		t.Fatal("nil error")
	}
	c := Config{}
	c.Security.CallbackTargets = []CallbackTarget{{Host: "cc", Port: 443}, {Host: "cc", Port: 80}}
	if c.CheckCallbackURL("https://cc/x") != nil || c.CheckCallbackURL("http://cc/x") != nil {
		t.Fatal("default ports")
	}
	if !strings.Contains(fmt.Sprint(Config{}.Validate()), "storage.database") {
		t.Fatal("storage required")
	}
}

type LoggerFunc func(string, map[string]any)

func (f LoggerFunc) Log(e string, m map[string]any) { f(e, m) }

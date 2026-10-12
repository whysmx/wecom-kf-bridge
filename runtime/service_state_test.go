package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
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

type fieldLogger struct {
	mu  sync.Mutex
	evs []string
	fs  []map[string]any
}

func (l *fieldLogger) Log(ev string, f map[string]any) {
	l.mu.Lock()
	l.evs = append(l.evs, ev)
	l.fs = append(l.fs, f)
	l.mu.Unlock()
}
func (l *fieldLogger) last() (string, map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.evs) == 0 {
		return "", nil
	}
	return l.evs[len(l.evs)-1], l.fs[len(l.fs)-1]
}

// Each service_state/get failure mode logs a distinct event with the raw
// official errcode/errmsg or state integer, and never the token or user id.
func TestServiceStateDiagnostics(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "gettoken") {
			fmt.Fprint(w, `{"errcode":0,"access_token":"REALTOKEN","expires_in":7200}`)
			return
		}
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	cl := wecom.NewClient(srv.URL, "c", "s")
	cl.MaxRetries = 0
	l := &fieldLogger{}
	a := &WeComAdapter{clients: map[string]*wecom.Client{"e1": cl}, tokens: map[string]*tokenCache{"e1": {client: cl, now: time.Now}}, routes: map[string]bindingRoute{"b1": {"e1", "kf1"}}, states: map[int]string{1: state.CustomerAIEligible, 3: state.CustomerHuman}, now: time.Now, logger: l}
	cust := bridge.Customer{ExternalUserID: "wm-SECRET-EXT"}
	b := bridge.Binding{ID: "b1"}
	ctx := context.Background()
	check := func(wantEv string, kv map[string]any) {
		t.Helper()
		ev, f := l.last()
		if ev != wantEv {
			t.Fatalf("event %q want %q (%v)", ev, wantEv, f)
		}
		for k, v := range kv {
			if fmt.Sprint(f[k]) != fmt.Sprint(v) {
				t.Fatalf("%s: %s=%v want %v", ev, k, f[k], v)
			}
		}
		if s := fmt.Sprint(f); strings.Contains(s, "REALTOKEN") || strings.Contains(s, "wm-SECRET-EXT") {
			t.Fatalf("leak: %s", s)
		}
	}

	body = `{"errcode":95014,"errmsg":"no permission"}`
	if _, err := a.ServiceState(ctx, b, cust); err == nil {
		t.Fatal("errcode accepted")
	}
	check("service_state_upstream_errcode", map[string]any{"errcode": 95014, "errmsg": "no permission"})

	body = `{"errcode":48002,"errmsg":"api forbidden"}`
	if _, err := a.ServiceState(ctx, b, cust); err == nil {
		t.Fatal("48002 accepted")
	}
	check("service_state_upstream_errcode", map[string]any{"errcode": 48002, "errmsg": "api forbidden", "open_kfid": "kf1"})
	if _, f := l.last(); !strings.Contains(fmt.Sprint(f["hint"]), "可调用接口的应用") {
		t.Fatal("no 48002 hint")
	}

	body = `{"errcode":0,"service_state":0}`
	if st, err := a.ServiceState(ctx, b, cust); err != nil || st != state.CustomerUnknown {
		t.Fatal(st, err)
	}
	check("service_state_unmapped", map[string]any{"service_state": 0})

	body = `{"errcode":0,"service_state":3}`
	if st, _ := a.ServiceState(ctx, b, cust); st != state.CustomerHuman {
		t.Fatal(st)
	}
	check("service_state_not_ai", map[string]any{"service_state": 3, "mapped": state.CustomerHuman})

	n := len(l.evs)
	body = `{"errcode":0,"service_state":1}`
	if st, _ := a.ServiceState(ctx, b, cust); st != state.CustomerAIEligible || len(l.evs) != n {
		t.Fatal("AI state should not log", st)
	}

	body = `not json`
	if _, err := a.ServiceState(ctx, b, cust); err == nil {
		t.Fatal("bad json accepted")
	}
	check("service_state_call_error", nil)

	if _, err := a.ServiceState(ctx, bridge.Binding{ID: "nope"}, cust); err == nil {
		t.Fatal("unknown binding")
	}
	check("service_state_route_error", map[string]any{"binding_id": "nope"})

	(&WeComAdapter{}).log("x", nil) // nil logger is safe
}

func TestServiceStateMapStartupWarning(t *testing.T) {
	for _, tc := range []struct {
		m    map[string]string
		warn bool
	}{
		{nil, true},
		{map[string]string{}, true},
		{map[string]string{"3": "HUMAN"}, true},
		{map[string]string{"0": "AI_ELIGIBLE", "1": "AI_ELIGIBLE", "2": "WAITING_HUMAN", "3": "HUMAN", "4": "CLOSED"}, false},
	} {
		l := &fieldLogger{}
		warnServiceStateMap(tc.m, l)
		if got := len(l.evs) == 1 && l.evs[0] == "WARNING_service_state_map_incomplete"; got != tc.warn {
			t.Fatalf("%v warn=%v events=%v", tc.m, got, l.evs)
		}
	}
	// Build emits it for an empty map
	l := &fieldLogger{}
	g, err := Build(context.Background(), gatewayConfig(t), l)
	if err != nil {
		t.Fatal(err)
	}
	g.Close()
	if !strings.Contains(strings.Join(l.evs, ","), "WARNING_service_state_map_incomplete") {
		t.Fatal(l.evs)
	}
}

// Treat48002AsAIEligible (default off): 48002 still fails send with the
// upstream error. When on, the same 48002 is logged as a WARNING and the
// send path sees AI_ELIGIBLE; other errcodes stay failures.
func TestTreat48002AsAIEligible(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "gettoken") {
			fmt.Fprint(w, `{"errcode":0,"access_token":"REALTOKEN","expires_in":7200}`)
			return
		}
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	cl := wecom.NewClient(srv.URL, "c", "s")
	cl.MaxRetries = 0
	l := &fieldLogger{}
	a := &WeComAdapter{clients: map[string]*wecom.Client{"e1": cl}, tokens: map[string]*tokenCache{"e1": {client: cl, now: time.Now}}, routes: map[string]bindingRoute{"b1": {"e1", "kf1"}}, states: map[int]string{1: state.CustomerAIEligible}, now: time.Now, logger: l}
	cust := bridge.Customer{ExternalUserID: "wm-SECRET-EXT"}
	b := bridge.Binding{ID: "b1"}
	ctx := context.Background()

	body = `{"errcode":48002,"errmsg":"api forbidden"}`
	if _, err := a.ServiceState(ctx, b, cust); err == nil {
		t.Fatal("default: 48002 must block")
	}
	if ev, f := l.last(); ev != "service_state_upstream_errcode" || f["errcode"] != 48002 {
		t.Fatalf("default log: %s %v", ev, f)
	}

	a.treat48002AsAI = true
	st, err := a.ServiceState(ctx, b, cust)
	if err != nil || st != state.CustomerAIEligibleBypass48002 {
		t.Fatalf("switch on: want bypass sentinel, got %s %v", st, err)
	}
	if st == state.CustomerAIEligible {
		t.Fatal("bypass must be distinguishable from real AI_ELIGIBLE")
	}
	ev, f := l.last()
	if ev != "WARNING_service_state_48002_bypassed" || f["errcode"] != 48002 {
		t.Fatalf("bypass log: %s %v", ev, f)
	}
	if !strings.Contains(fmt.Sprint(f["effect"]), "joint-debug") || !strings.Contains(fmt.Sprint(f["hint"]), "可调用接口的应用") {
		t.Fatalf("missing warning fields: %v", f)
	}
	if s := fmt.Sprint(f); strings.Contains(s, "REALTOKEN") || strings.Contains(s, "wm-SECRET-EXT") {
		t.Fatalf("leak: %s", s)
	}

	// other errcodes and transport errors still fail with the switch on
	body = `{"errcode":95014,"errmsg":"no permission"}`
	if _, err := a.ServiceState(ctx, b, cust); err == nil {
		t.Fatal("95014 must still block")
	}
	if ev, _ := l.last(); ev != "service_state_upstream_errcode" {
		t.Fatal(ev)
	}
	body = `{"errcode":48001,"errmsg":"api not found"}`
	if _, err := a.ServiceState(ctx, b, cust); err == nil {
		t.Fatal("48001 must still block")
	}
	body = `not json`
	if _, err := a.ServiceState(ctx, b, cust); err == nil {
		t.Fatal("network/parse error must still block")
	}
	if ev, _ := l.last(); ev != "service_state_call_error" {
		t.Fatal(ev)
	}
}

func TestTreat48002StartupWarning(t *testing.T) {
	t.Setenv("T_MASTER", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	t.Setenv("T_SECRET", "real-secret")
	t.Setenv("T_CBTOK", "realtok")
	t.Setenv("T_CBAES", key43(1))
	t.Setenv("T_VSECRET", "vsecret")
	t.Setenv("T_VTOK", "vtok")
	t.Setenv("T_VAES", key43(2))
	raw := fmt.Sprintf(`{
	 "server":{"public_listen":"127.0.0.1:0"},
	 "storage":{"database":%q},
	 "security":{"master_key_env":"T_MASTER","callback_targets":[{"host":"127.0.0.1","port":9,"allowed_cidrs":["127.0.0.0/8"]}]},
	 "wecom":{"allow_insecure_http":true,"api_base_url":"http://127.0.0.1:9","customer_origins":[3],"treat_48002_as_ai_eligible":true,"service_state_map":{"1":"AI_ELIGIBLE"}},
	 "enterprises":[{"id":"e1","tenant_key":"acme","corp_id":"ww","secret_env":"T_SECRET","callback_token_env":"T_CBTOK","callback_aes_key_env":"T_CBAES"}],
	 "bindings":[{"id":"b1","enterprise_id":"e1","open_kfid":"kf1","project_id":"p1","virtual_corp_id":"bridge_a","agent_id":"1","virtual_secret_env":"T_VSECRET","callback_token_env":"T_VTOK","callback_aes_key_env":"T_VAES","callback_url":"http://127.0.0.1:9/cb"}]}`,
		t.TempDir()+"/g.db")
	cfg, err := ParseConfig([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	l := &fieldLogger{}
	g, err := Build(context.Background(), cfg, l)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if !g.adapter.treat48002AsAI {
		t.Fatal("flag not wired")
	}
	found := false
	for i, ev := range l.evs {
		if ev == "WARNING_treat_48002_as_ai_eligible" {
			found = true
			if !strings.Contains(fmt.Sprint(l.fs[i]["effect"]), "never enable in production") {
				t.Fatal(l.fs[i])
			}
		}
	}
	if !found {
		t.Fatal("startup WARNING missing", l.evs)
	}
}

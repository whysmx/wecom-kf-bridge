package runtime

import (
	"context"
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

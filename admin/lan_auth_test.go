package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestLANNoLoginPrivateIPGetsSession(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.LanNoLogin = true })
	w := e.do("GET", "/admin/accounts", nil, nil, "")
	if w.Code != 200 {
		t.Fatalf("expected auto-login 200, got %d %s", w.Code, w.Body.String())
	}
	var ck string
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName {
			ck = c.Value
		}
	}
	if len(ck) != 64 {
		t.Fatalf("cookie %q", ck)
	}
	if !strings.Contains(w.Body.String(), "csrf_token") {
		t.Fatal("page should include csrf")
	}
	audits, _, _ := e.st.Audits(context.Background(), 0, 20)
	var ok bool
	for _, a := range audits {
		if a.Operation == "login" && a.Result == "ok" && a.Summary == "lan_no_login" && strings.HasPrefix(a.Actor, "admin-lan@") {
			ok = true
		}
	}
	if !ok {
		t.Fatal("missing lan_no_login audit", audits)
	}
}

func TestLANNoLoginPublicIPStillRedirects(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.LanNoLogin = true })
	r := e.doCustom("GET", "/admin/", nil, nil, "", "8.8.8.8:1234")
	if r.Code != http.StatusSeeOther || r.Header().Get("Location") != "/admin/login" {
		t.Fatalf("public IP should hit login wall: %d %s", r.Code, r.Header().Get("Location"))
	}
}

func TestLANNoLoginDisabledKeepsWall(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.LanNoLogin = false })
	w := e.do("GET", "/admin/", nil, nil, "")
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/admin/login" {
		t.Fatalf("lan_no_login=false should redirect: %d", w.Code)
	}
}

func TestLANNoLoginPOSTWithoutSessionStill401(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.LanNoLogin = true })
	w := e.do("POST", "/admin/accounts/sync", url.Values{}, map[string]string{"Origin": origin}, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("POST without session must stay 401, got %d", w.Code)
	}
}

func TestLANNoLoginCSRFStillRequired(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.LanNoLogin = true })
	w := e.do("GET", "/admin/accounts", nil, nil, "")
	var ck string
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName {
			ck = c.Value
		}
	}
	form := url.Values{"idempotency_key": {"lan-csrf-key-00000001"}}
	bad := e.do("POST", "/admin/accounts/sync", form, map[string]string{"Origin": origin}, ck)
	if bad.Code != http.StatusForbidden {
		t.Fatalf("missing csrf should 403, got %d", bad.Code)
	}
	m := csrfRe.FindStringSubmatch(w.Body.String())
	if m == nil {
		t.Fatal("no csrf on auto-login page")
	}
	form.Set("csrf_token", m[1])
	form.Set("idempotency_key", "lan-csrf-key-00000002")
	ok := e.do("POST", "/admin/accounts/sync", form, map[string]string{"Origin": origin}, ck)
	if ok.Code != http.StatusSeeOther {
		t.Fatalf("with csrf want 303, got %d %s", ok.Code, ok.Body.String())
	}
}

func TestLANNoLoginSkipsStepUp(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.LanNoLogin = true })
	w := e.do("GET", "/admin/settings", nil, nil, "")
	var ck string
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName {
			ck = c.Value
		}
	}
	m := csrfRe.FindStringSubmatch(w.Body.String())
	if m == nil {
		t.Fatal("no csrf")
	}
	cl := &client{e: e, cookie: ck, csrf: m[1]}
	// settings/save requires step-up; lanAuth session should pass without password step-up
	form := url.Values{
		"treat_48002_as_ai_eligible": {"false"},
		"service_state_map":          {`{"1":"AI_ELIGIBLE","2":"WAITING_HUMAN","3":"HUMAN"}`},
		"public_base_url":            {""},
	}
	res := cl.post("/admin/settings/save", form)
	if res.Code != http.StatusSeeOther {
		t.Fatalf("lan session should skip step-up: %d %s", res.Code, res.Body.String())
	}
}

func TestIsLANClient(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"10.0.0.5:1", true},
		{"192.168.1.1:80", true},
		{"127.0.0.1:9", true},
		{"[::1]:9", true},
		{"8.8.8.8:1", false},
		{"1.2.3.4:99", false},
		{"169.254.1.1:1", true}, // link-local
	}
	for _, tc := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = tc.addr
		if got := isLANClient(r); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.addr, got, tc.want)
		}
	}
}

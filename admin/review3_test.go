package admin

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/whysmx/wecom-kf-bridge/state"
	"github.com/whysmx/wecom-kf-bridge/wecom"
)

func rev(t *testing.T, e *env) url.Values {
	return url.Values{"revision": {itoa64(mustCustomer(t, e).Revision)}}
}

// #33: official transfer first, read back, then local state.
func TestHandoverRequiresOfficialConfirmation(t *testing.T) {
	e := newEnv(t)
	cl := e.login()
	id := e.cust.ID
	path := "/admin/customers/" + id

	e.kf.transErr = &wecom.APIError{Code: 95014, Message: "no permission"}
	cl.post(path+"/handover", rev(t, e))
	if c := mustCustomer(t, e); c.State != state.CustomerAIEligible {
		t.Fatal("local state changed after official rejection", c.State)
	}
	if !strings.Contains(cl.flash(), "官方未变更") {
		t.Fatal("rejection not shown")
	}

	e.kf.transErr = errLost
	cl.post(path+"/handover", rev(t, e))
	c := mustCustomer(t, e)
	if c.State != state.CustomerUnknown || c.OfficialStatus != state.CustomerUnknown {
		t.Fatal("uncertain handover not UNKNOWN / AI not paused", c.State, c.OfficialStatus)
	}
	if !strings.Contains(cl.get("/admin/customers").Body.String(), "UNKNOWN") {
		t.Fatal("UNKNOWN not shown")
	}
	if w := cl.post(path+"/recover", rev(t, e)); w.Code != http.StatusConflict {
		t.Fatal("recover from UNKNOWN allowed")
	}

	// readback mismatch (trans ok but state unchanged) is uncertain too
	e.kf.transErr, e.kf.stuck = nil, true
	e.kf.states["ext-secret-id"] = 1
	cl.post(path+"/handover", rev(t, e))
	if c := mustCustomer(t, e); c.State != state.CustomerUnknown {
		t.Fatal("readback mismatch accepted", c.State)
	}
	e.kf.stuck = false
	e.kf.getErr = errors.New("get failed")
	cl.post(path+"/handover", rev(t, e))
	if c := mustCustomer(t, e); c.State != state.CustomerUnknown {
		t.Fatal("readback failure accepted")
	}
	e.kf.getErr = nil
	cl.post(path+"/handover", rev(t, e))
	c = mustCustomer(t, e)
	if c.State != state.CustomerHuman || c.OfficialStatus != state.CustomerWaitingHuman {
		t.Fatal("confirmed handover", c.State, c.OfficialStatus)
	}
	if e.kf.count("trans:ext-secret-id:2") == 0 || e.kf.count("get:") == 0 {
		t.Fatal("official API not called", e.kf.calls)
	}

	// recover: uncertain -> UNKNOWN, no new generation
	e.kf.transErr = errLost
	cl.post(path+"/recover", rev(t, e))
	c = mustCustomer(t, e)
	if c.Generation != e.cust.Generation || c.State != state.CustomerUnknown {
		t.Fatal("uncertain recover changed generation or kept AI open", c)
	}
	e.kf.transErr = nil
	cl.post(path+"/handover", rev(t, e))
	e.kf.transErr = &wecom.APIError{Code: 1, Message: "x"}
	cl.post(path+"/recover", rev(t, e))
	if c := mustCustomer(t, e); c.Generation != e.cust.Generation || c.State != state.CustomerHuman {
		t.Fatal("rejected recover changed state")
	}
	e.kf.transErr = nil
	cl.post(path+"/recover", rev(t, e))
	c = mustCustomer(t, e)
	if c.Generation != e.cust.Generation+1 || c.State != state.CustomerAIEligible || c.OfficialStatus != state.CustomerAIEligible {
		t.Fatal("confirmed recover", c)
	}
}

func TestHandoverNeedsMappedOfficialState(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.ServiceStateMap = nil })
	cl := e.login()
	cl.post("/admin/customers/"+e.cust.ID+"/handover", rev(t, e))
	if !strings.Contains(cl.flash(), "service_state_map") || mustCustomer(t, e).State != state.CustomerAIEligible {
		t.Fatal("unmapped state")
	}
	if e.kf.count("trans") != 0 {
		t.Fatal("trans called without a mapped target")
	}
	// binding vanished
	e2 := newEnv(t)
	cl2 := e2.login()
	conn, _ := e2.st.DB().Conn(context.Background())
	conn.ExecContext(context.Background(), `PRAGMA foreign_keys=OFF`)
	if _, err := conn.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS immutable_customers`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `UPDATE customers SET binding_id='gone'`); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	cl2.post("/admin/customers/"+e2.cust.ID+"/handover", rev(t, e2))
	if !strings.Contains(cl2.flash(), "绑定不存在") {
		t.Fatal(cl2.flash())
	}
}

// #32: possibly-deleted (UNKNOWN) also freezes; freeze failures are shown.
func TestAccountDeleteFreezesBindings(t *testing.T) {
	e := newEnv(t)
	a := seedAccount(t, e, "wk1")
	cl := e.login()
	cl.stepUp()
	tk := between(cl.get("/admin/accounts?id=wk1").Body.String(), `name="ticket" value="`, `"`)
	e.kf.err = errLost
	cl.post("/admin/accounts/wk1/delete", url.Values{"revision": {itoa64(a.Revision)}, "ticket": {tk}, "confirm": {"wk1"}})
	if b, _ := e.st.Binding(context.Background(), "b1"); b.Active {
		t.Fatal("uncertain delete left binding active")
	}
	if !strings.Contains(cl.flash(), "已停用 1 个关联绑定") {
		t.Fatal("freeze not reported")
	}
	e2 := newEnv(t)
	a2 := seedAccount(t, e2, "wk1")
	cl2 := e2.login()
	cl2.stepUp()
	e2.rt.applyErr = errors.New("apply failed")
	tk = between(cl2.get("/admin/accounts?id=wk1").Body.String(), `name="ticket" value="`, `"`)
	cl2.post("/admin/accounts/wk1/delete", url.Values{"revision": {itoa64(a2.Revision)}, "ticket": {tk}, "confirm": {"wk1"}})
	if !strings.Contains(cl2.flash(), "警告") {
		t.Fatal("freeze failure not shown")
	}
}

// #34: Secure cookie unless explicitly insecure.
func TestInsecureCookieOption(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.InsecureCookie = true; c.Origin = "http://127.0.0.1:8091" })
	w := e.do("POST", "/admin/login", url.Values{"password": {password}}, map[string]string{"Origin": "http://127.0.0.1:8091"}, "")
	ck := w.Result().Cookies()[0]
	if ck.Secure || !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode {
		t.Fatal(ck)
	}
	e2 := newEnv(t)
	if !e2.do("POST", "/admin/login", url.Values{"password": {password}}, map[string]string{"Origin": origin}, "").Result().Cookies()[0].Secure {
		t.Fatal("default must be Secure")
	}
}

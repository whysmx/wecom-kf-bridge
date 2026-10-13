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

// putHuman runs confirmed handover so local is HUMAN and official WAITING (2).
func putHuman(t *testing.T, e *env, cl *client) state.Customer {
	t.Helper()
	path := "/admin/customers/" + e.cust.ID
	cl.post(path+"/handover", rev(t, e))
	cu := mustCustomer(t, e)
	if cu.State != state.CustomerHuman {
		t.Fatalf("handover precondition: state=%s", cu.State)
	}
	return cu
}

// Recover from official WAITING(2) and HUMAN(3): always Trans→4, never →1.
func TestRecoverEndsOfficial2And3To4NeverTo1(t *testing.T) {
	for _, official := range []struct {
		name string
		code int
	}{
		{"waiting2", 2},
		{"human3", 3},
	} {
		t.Run(official.name, func(t *testing.T) {
			e := newEnv(t, func(c *Config) {
				c.HandoverWelcome = "排队中请稍候"
				c.HandoverEndScript = "会话已结束，欢迎再来"
			})
			cl := e.login()
			cu := putHuman(t, e, cl)
			// After handover official is 2; optionally promote to 3.
			e.kf.mu.Lock()
			e.kf.states["ext-secret-id"] = official.code
			e.kf.mu.Unlock()

			before4 := e.kf.count("trans:ext-secret-id:4")
			before1 := e.kf.count("trans:ext-secret-id:1")
			cl.post("/admin/customers/"+e.cust.ID+"/recover", url.Values{"revision": {itoa64(cu.Revision)}})
			nc := mustCustomer(t, e)
			if nc.State != state.CustomerAIEligible {
				t.Fatalf("local state=%s", nc.State)
			}
			if nc.OfficialStatus != state.CustomerClosed {
				t.Fatalf("official=%s want CLOSED", nc.OfficialStatus)
			}
			if nc.Generation != cu.Generation+1 {
				t.Fatalf("generation %d→%d", cu.Generation, nc.Generation)
			}
			if e.kf.count("trans:ext-secret-id:4") != before4+1 {
				t.Fatalf("recover must trans→4: %v", e.kf.calls)
			}
			if e.kf.count("trans:ext-secret-id:1") != before1 {
				t.Fatalf("recover must never trans→1 (95016): %v", e.kf.calls)
			}
			if e.kf.count("event:msg-code-admin:会话已结束，欢迎再来") == 0 {
				t.Fatalf("end script not sent on recover→4: %v", e.kf.calls)
			}
			if !strings.Contains(cl.flash(), "官方已结束(4)") {
				t.Fatalf("flash=%q", cl.flash())
			}
		})
	}
}

// Official already CLOSED: skip Trans, local recover only; still never →1.
func TestRecoverWhenOfficialAlreadyClosedSkipsTrans(t *testing.T) {
	e := newEnv(t)
	cl := e.login()
	cu := putHuman(t, e, cl)
	e.kf.mu.Lock()
	e.kf.states["ext-secret-id"] = 4
	e.kf.mu.Unlock()
	beforeTrans := e.kf.count("trans:")
	cl.post("/admin/customers/"+e.cust.ID+"/recover", url.Values{"revision": {itoa64(cu.Revision)}})
	nc := mustCustomer(t, e)
	if nc.State != state.CustomerAIEligible || nc.OfficialStatus != state.CustomerClosed {
		t.Fatalf("got state=%s official=%s", nc.State, nc.OfficialStatus)
	}
	if e.kf.count("trans:") != beforeTrans {
		t.Fatalf("must skip trans when already CLOSED: %v", e.kf.calls)
	}
	if e.kf.count("trans:ext-secret-id:1") != 0 {
		t.Fatalf("never →1: %v", e.kf.calls)
	}
	if !strings.Contains(cl.flash(), "官方已结束(4)") {
		t.Fatalf("flash=%q", cl.flash())
	}
}

// Official already AI (0/1): skip Trans, local recover; AI flash; never →1.
func TestRecoverWhenOfficialAlreadyAISkipsTrans(t *testing.T) {
	for _, code := range []int{0, 1} {
		t.Run("official"+string(rune('0'+code)), func(t *testing.T) {
			e := newEnv(t, func(c *Config) {
				c.ServiceStateMap = map[int]string{
					0: state.CustomerAIEligible,
					1: state.CustomerAIEligible,
					2: state.CustomerWaitingHuman,
					3: state.CustomerHuman,
					4: state.CustomerClosed,
				}
			})
			cl := e.login()
			cu := putHuman(t, e, cl)
			e.kf.mu.Lock()
			e.kf.states["ext-secret-id"] = code
			e.kf.mu.Unlock()
			beforeTrans := e.kf.count("trans:")
			cl.post("/admin/customers/"+e.cust.ID+"/recover", url.Values{"revision": {itoa64(cu.Revision)}})
			nc := mustCustomer(t, e)
			if nc.State != state.CustomerAIEligible || nc.OfficialStatus != state.CustomerAIEligible {
				t.Fatalf("state=%s official=%s", nc.State, nc.OfficialStatus)
			}
			if e.kf.count("trans:") != beforeTrans {
				t.Fatalf("must skip trans when already AI: %v", e.kf.calls)
			}
			if e.kf.count("trans:ext-secret-id:1") != 0 {
				t.Fatalf("never →1: %v", e.kf.calls)
			}
			if !strings.Contains(cl.flash(), "官方已是 AI") {
				t.Fatalf("flash=%q", cl.flash())
			}
		})
	}
}

// ServiceState get failure before end→4 → UNKNOWN, no generation bump, no →1.
func TestRecoverServiceStateGetFailureMarksUnknown(t *testing.T) {
	e := newEnv(t)
	cl := e.login()
	cu := putHuman(t, e, cl)
	e.kf.getErr = errors.New("get failed")
	cl.post("/admin/customers/"+e.cust.ID+"/recover", url.Values{"revision": {itoa64(cu.Revision)}})
	got := mustCustomer(t, e)
	if got.Generation != cu.Generation || got.State != state.CustomerUnknown || got.OfficialStatus != state.CustomerUnknown {
		t.Fatalf("want UNKNOWN same gen: %+v", got)
	}
	if e.kf.count("trans:ext-secret-id:1") != 0 || e.kf.count("trans:ext-secret-id:4") != 0 {
		t.Fatalf("no trans on get failure: %v", e.kf.calls)
	}
	if !strings.Contains(cl.flash(), "UNKNOWN") {
		t.Fatalf("flash=%q", cl.flash())
	}
}

// Binding vanished after human → recover rejected, no →1.
func TestRecoverBindingMissingRejected(t *testing.T) {
	e := newEnv(t)
	cl := e.login()
	cu := putHuman(t, e, cl)
	conn, _ := e.st.DB().Conn(context.Background())
	_, _ = conn.ExecContext(context.Background(), `PRAGMA foreign_keys=OFF`)
	if _, err := conn.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS immutable_customers`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `UPDATE customers SET binding_id='gone'`); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	before1 := e.kf.count("trans:ext-secret-id:1")
	cl.post("/admin/customers/"+e.cust.ID+"/recover", url.Values{"revision": {itoa64(cu.Revision)}})
	if !strings.Contains(cl.flash(), "绑定不存在") {
		t.Fatalf("flash=%q", cl.flash())
	}
	if e.kf.count("trans:ext-secret-id:1") != before1 {
		t.Fatalf("never →1: %v", e.kf.calls)
	}
}

// End→4 OK but local RecoverCustomer fails → 409, no fake AI success.
func TestRecoverLocalRecoverCustomerFailure(t *testing.T) {
	e := newEnv(t)
	cl := e.login()
	cu := putHuman(t, e, cl)
	if _, err := e.st.DB().Exec(`CREATE TRIGGER block_ai_recover BEFORE UPDATE OF state ON customers WHEN NEW.state='AI_ELIGIBLE' BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	w := cl.post("/admin/customers/"+e.cust.ID+"/recover", url.Values{"revision": {itoa64(cu.Revision)}})
	if w.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d body=%s", w.Code, w.Body)
	}
	got := mustCustomer(t, e)
	if got.State == state.CustomerAIEligible {
		t.Fatal("must not claim local AI after RecoverCustomer failure")
	}
	if e.kf.count("trans:ext-secret-id:4") == 0 {
		t.Fatalf("end→4 should have run: %v", e.kf.calls)
	}
	if e.kf.count("trans:ext-secret-id:1") != 0 {
		t.Fatalf("never →1: %v", e.kf.calls)
	}
}

// Unknown / missing customer id on recover path.
func TestRecoverCustomerNotFound(t *testing.T) {
	e := newEnv(t)
	cl := e.login()
	w := cl.post("/admin/customers/no-such-customer/recover", url.Values{"revision": {"1"}})
	if w.Code != http.StatusNotFound && !strings.Contains(cl.flash()+w.Body.String(), "客户不存在") {
		// flash redirect or direct status
		if w.Code != http.StatusSeeOther && w.Code != http.StatusNotFound {
			t.Fatalf("code=%d flash=%q body=%s", w.Code, cl.flash(), w.Body)
		}
	}
}

// Unmapped official get → UNKNOWN: refuse recover (no unsafe 2→4), never →1.
func TestRecoverUnmappedOfficialRefuses(t *testing.T) {
	e := newEnv(t)
	cl := e.login()
	cu := putHuman(t, e, cl)
	e.kf.mu.Lock()
	e.kf.states["ext-secret-id"] = 9 // not in ServiceStateMap
	e.kf.mu.Unlock()
	before := len(e.kf.calls)
	cl.post("/admin/customers/"+e.cust.ID+"/recover", url.Values{"revision": {itoa64(cu.Revision)}})
	nc := mustCustomer(t, e)
	if nc.State != state.CustomerHuman || nc.Generation != cu.Generation {
		t.Fatalf("must not local-recover on UNKNOWN official: state=%s gen=%d", nc.State, nc.Generation)
	}
	if !strings.Contains(cl.flash(), "UNKNOWN") {
		t.Fatalf("flash=%q", cl.flash())
	}
	for _, c := range e.kf.calls[before:] {
		if strings.HasPrefix(c, "trans:") {
			t.Fatalf("must not trans on unmapped official: %v", e.kf.calls[before:])
		}
	}
}

// WAITING recover: definitive 2→3 reject keeps local human; no →4/→1.
func TestRecoverWaitingTrans3Rejected(t *testing.T) {
	e := newEnv(t)
	cl := e.login()
	cu := putHuman(t, e, cl) // official WAITING(2); harness servicer=WenHaiYan
	e.kf.transErr = &wecom.APIError{Code: 95014, Message: "no permission"}
	before := len(e.kf.calls)
	cl.post("/admin/customers/"+e.cust.ID+"/recover", url.Values{"revision": {itoa64(cu.Revision)}})
	got := mustCustomer(t, e)
	if got.State != state.CustomerHuman || got.Generation != cu.Generation {
		t.Fatalf("rejected recover must not change local: %+v", got)
	}
	flash := cl.flash()
	if !strings.Contains(flash, "2→3") {
		t.Fatalf("flash=%q", flash)
	}
	for _, c := range e.kf.calls[before:] {
		if c == "trans:ext-secret-id:4" || strings.HasPrefix(c, "trans:ext-secret-id:1") {
			t.Fatalf("must not →4/→1 after 2→3 reject: %v", e.kf.calls[before:])
		}
	}
}

// Handover queue tip still fires on trans→2 when welcome configured.
func TestHandoverWelcomeSentOnTransToWaiting(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.HandoverWelcome = "请稍候接入" })
	cl := e.login()
	cl.post("/admin/customers/"+e.cust.ID+"/handover", rev(t, e))
	if e.kf.count("event:msg-code-admin:请稍候接入") == 0 {
		t.Fatalf("welcome not sent: %v", e.kf.calls)
	}
}

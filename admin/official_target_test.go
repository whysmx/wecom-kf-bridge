package admin

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/whysmx/wecom-kf-bridge/state"
)

func TestOfficialTargetPrefersAI1OverZero(t *testing.T) {
	c := &Console{cfg: Config{ServiceStateMap: map[int]string{
		0: state.CustomerAIEligible,
		1: state.CustomerAIEligible,
		2: state.CustomerWaitingHuman,
		3: state.CustomerHuman,
		4: state.CustomerClosed,
	}}}
	got, ok := c.officialTarget(state.CustomerAIEligible)
	if !ok || got != 1 {
		t.Fatalf("officialTarget(AI_ELIGIBLE)=%d ok=%v; want 1, true", got, ok)
	}
	got, ok = c.officialTarget(state.CustomerWaitingHuman)
	if !ok || got != 2 {
		t.Fatalf("officialTarget(WAITING_HUMAN)=%d ok=%v; want 2, true", got, ok)
	}
}

func TestOfficialTargetOnlyIllegalZeroReturnsFalse(t *testing.T) {
	c := &Console{cfg: Config{ServiceStateMap: map[int]string{
		0: state.CustomerAIEligible,
	}}}
	got, ok := c.officialTarget(state.CustomerAIEligible)
	if ok {
		t.Fatalf("officialTarget with only key 0: got %d ok=%v; want ok=false", got, ok)
	}
}

func TestOfficialTargetPicksSmallestAmongMultipleValid(t *testing.T) {
	c := &Console{cfg: Config{ServiceStateMap: map[int]string{
		3: state.CustomerAIEligible,
		1: state.CustomerAIEligible,
		4: state.CustomerAIEligible,
	}}}
	got, ok := c.officialTarget(state.CustomerAIEligible)
	if !ok || got != 1 {
		t.Fatalf("multiple valid AI keys: got %d ok=%v; want 1, true", got, ok)
	}
}

func TestOfficialTargetNonAIInternalStates(t *testing.T) {
	c := &Console{cfg: Config{ServiceStateMap: map[int]string{
		1: state.CustomerAIEligible,
		2: state.CustomerWaitingHuman,
		3: state.CustomerHuman,
		4: state.CustomerClosed,
	}}}
	cases := []struct {
		internal string
		want     int
	}{
		{state.CustomerWaitingHuman, 2},
		{state.CustomerHuman, 3},
		{state.CustomerClosed, 4},
	}
	for _, tc := range cases {
		got, ok := c.officialTarget(tc.internal)
		if !ok || got != tc.want {
			t.Fatalf("officialTarget(%s)=%d ok=%v; want %d, true", tc.internal, got, ok, tc.want)
		}
	}
}

func TestOfficialTargetSkipsKeysOutside1To4(t *testing.T) {
	c := &Console{cfg: Config{ServiceStateMap: map[int]string{
		0:  state.CustomerAIEligible,
		-1: state.CustomerAIEligible,
		5:  state.CustomerAIEligible,
		99: state.CustomerAIEligible,
		2:  state.CustomerAIEligible, // only legal key for AI
		3:  state.CustomerHuman,
	}}}
	got, ok := c.officialTarget(state.CustomerAIEligible)
	if !ok || got != 2 {
		t.Fatalf("out-of-range keys should be skipped: got %d ok=%v; want 2, true", got, ok)
	}
	got, ok = c.officialTarget(state.CustomerHuman)
	if !ok || got != 3 {
		t.Fatalf("officialTarget(HUMAN)=%d ok=%v; want 3, true", got, ok)
	}
}

func TestOfficialTargetNoValidKeyReturnsFalse(t *testing.T) {
	c := &Console{cfg: Config{ServiceStateMap: map[int]string{
		0:  state.CustomerAIEligible,
		5:  state.CustomerWaitingHuman,
		-3: state.CustomerHuman,
	}}}
	for _, internal := range []string{
		state.CustomerAIEligible,
		state.CustomerWaitingHuman,
		state.CustomerHuman,
		state.CustomerClosed,
		state.CustomerUnknown,
	} {
		got, ok := c.officialTarget(internal)
		if ok {
			t.Fatalf("no valid 1–4 mapping for %s: got %d ok=%v; want ok=false", internal, got, ok)
		}
	}
	c2 := &Console{cfg: Config{ServiceStateMap: map[int]string{}}}
	if _, ok := c2.officialTarget(state.CustomerAIEligible); ok {
		t.Fatal("empty map should return ok=false")
	}
}

func TestOfficialTargetNeverSelectsZeroWhenValidExists(t *testing.T) {
	c := &Console{cfg: Config{ServiceStateMap: map[int]string{
		0: state.CustomerAIEligible,
		4: state.CustomerAIEligible, // only legal AI key (boundary)
	}}}
	got, ok := c.officialTarget(state.CustomerAIEligible)
	if !ok || got != 4 {
		t.Fatalf("0+4 both AI: got %d ok=%v; want 4, true (never 0)", got, ok)
	}
	if got == 0 {
		t.Fatal("must never return official state 0 (WeCom 40058)")
	}
}

func TestOfficialTargetBoundaryFourSelectable(t *testing.T) {
	c := &Console{cfg: Config{ServiceStateMap: map[int]string{
		4: state.CustomerClosed,
		1: state.CustomerAIEligible,
	}}}
	got, ok := c.officialTarget(state.CustomerClosed)
	if !ok || got != 4 {
		t.Fatalf("boundary key 4: got %d ok=%v; want 4, true", got, ok)
	}
}

// Recover must end official session with trans→4 (CLOSED), never →1/→0.
// WeCom rejects 2→1 / 3→1 with 95016; 0 is illegal for trans (40058).
func TestRecoverTransferUsesOfficialClosed4NotAI(t *testing.T) {
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
	path := "/admin/customers/" + e.cust.ID
	cl.post(path+"/handover", rev(t, e))
	cu := mustCustomer(t, e)
	if cu.State != state.CustomerHuman {
		t.Fatalf("handover: state=%s", cu.State)
	}
	if e.kf.count("trans:ext-secret-id:3:WenHaiYan") == 0 {
		t.Fatalf("handover should trans→3 with servicer: %v", e.kf.calls)
	}
	if e.kf.count("trans:ext-secret-id:2") != 0 {
		t.Fatalf("handover must not trans→2: %v", e.kf.calls)
	}
	cl.post(path+"/recover", url.Values{"revision": {itoa64(cu.Revision)}})
	nc := mustCustomer(t, e)
	if nc.State != state.CustomerAIEligible {
		t.Fatalf("recover: state=%s", nc.State)
	}
	if nc.OfficialStatus != state.CustomerClosed {
		t.Fatalf("recover official=%s; want CLOSED", nc.OfficialStatus)
	}
	// After intentional handoff official is HUMAN(3): recover only →4.
	if e.kf.count("trans:ext-secret-id:4") == 0 {
		t.Fatalf("recover must trans→4 (CLOSED), not →1: %v", e.kf.calls)
	}
	if e.kf.count("trans:ext-secret-id:1") != 0 {
		t.Fatalf("must never trans→1 on recover (95016): %v", e.kf.calls)
	}
	if e.kf.count("trans:ext-secret-id:0") != 0 {
		t.Fatalf("must never trans→0 (40058): %v", e.kf.calls)
	}
}

func TestHandoverTransferRejectsWhenOnlyIllegalZeroMapped(t *testing.T) {
	e := newEnv(t, func(c *Config) {
		c.ServiceStateMap = map[int]string{
			0: state.CustomerHuman, // illegal for HUMAN target
			1: state.CustomerAIEligible,
		}
	})
	cl := e.login()
	cl.post("/admin/customers/"+e.cust.ID+"/handover", rev(t, e))
	if !strings.Contains(cl.flash(), "service_state_map") {
		t.Fatalf("expected map error flash, got %q", cl.flash())
	}
	if mustCustomer(t, e).State != state.CustomerAIEligible {
		t.Fatal("local state must not change when target unmapped")
	}
	if e.kf.count("trans") != 0 {
		t.Fatalf("trans must not be called: %v", e.kf.calls)
	}
	if w := cl.post("/admin/customers/"+e.cust.ID+"/handover", rev(t, e)); w.Code == http.StatusInternalServerError {
		t.Fatal("unexpected 500")
	}
}

// WAITING recover without bindings[].servicer_userid fails with 未配置接待人 and never calls 2→4.
func TestRecoverWaitingWithoutServicerFails(t *testing.T) {
	e := newEnv(t)
	cl := e.login()
	path := "/admin/customers/" + e.cust.ID
	cl.post(path+"/handover", rev(t, e)) // needs servicer for →3
	cu := mustCustomer(t, e)
	if cu.State != state.CustomerHuman {
		t.Fatalf("handover precondition: %s", cu.State)
	}
	e.rt.configBindings[0].ServicerUserID = ""
	e.kf.mu.Lock()
	e.kf.states["ext-secret-id"] = 2 // simulate stuck WAITING
	e.kf.mu.Unlock()
	beforeCalls := len(e.kf.calls)
	cl.post(path+"/recover", url.Values{"revision": {itoa64(cu.Revision)}})
	if !strings.Contains(cl.flash(), "未配置接待人") {
		t.Fatalf("flash want 未配置接待人, got %q", cl.flash())
	}
	nc := mustCustomer(t, e)
	if nc.State != state.CustomerHuman || nc.Generation != cu.Generation {
		t.Fatalf("must not local-recover without servicer: %+v", nc)
	}
	for _, c := range e.kf.calls[beforeCalls:] {
		if strings.HasPrefix(c, "trans:ext-secret-id:4") || strings.HasPrefix(c, "trans:ext-secret-id:3") {
			t.Fatalf("must not trans 2→3/2→4 when servicer missing: %v", e.kf.calls[beforeCalls:])
		}
	}
}

// Official HUMAN recover still only →4 (no →3).
func TestRecoverHumanOnlyTransClosed(t *testing.T) {
	e := newEnv(t)
	cl := e.login()
	path := "/admin/customers/" + e.cust.ID
	cl.post(path+"/handover", rev(t, e))
	// Simulate official already HUMAN(3).
	e.kf.states["ext-secret-id"] = 3
	cu := mustCustomer(t, e)
	_ = e.st.SetOfficialStatus(context.Background(), cu.ID, state.CustomerHuman)
	cu = mustCustomer(t, e)
	before := append([]string(nil), e.kf.calls...)
	cl.post(path+"/recover", url.Values{"revision": {itoa64(cu.Revision)}})
	nc := mustCustomer(t, e)
	if nc.State != state.CustomerAIEligible || nc.OfficialStatus != state.CustomerClosed {
		t.Fatalf("HUMAN recover flash=%q cust=%+v", cl.flash(), nc)
	}
	var after []string
	for _, c := range e.kf.calls[len(before):] {
		after = append(after, c)
	}
	for _, c := range after {
		if strings.Contains(c, ":3:") {
			t.Fatalf("HUMAN must not trans→3: %v", after)
		}
	}
	ok4 := false
	for _, c := range after {
		if c == "trans:ext-secret-id:4" {
			ok4 = true
		}
	}
	if !ok4 {
		t.Fatalf("HUMAN recover must →4: %v", after)
	}
}

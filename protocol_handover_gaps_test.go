package bridge

import (
	"context"
	"net/http"
	"testing"

	"github.com/whysmx/wecom-kf-bridge/state"
)

// After phrase-style BeginHandover(manual), send stays 70004 even if official is AI.
func TestSendAfterPhraseManualFenceStays70004(t *testing.T) {
	e := newEnv(t)
	tok := e.token()
	if _, err := e.st.BeginHandover(context.Background(), e.cust.ID, "phrase_handover"); err != nil {
		t.Fatal(err)
	}
	e.ad.svcState = state.CustomerAIEligible
	if code, v := e.send(tok, e.cust.UID, "text", "ai-should-block"); code != 403 || errcode(v) != ErrCodeDisabled {
		t.Fatalf("want 70004: %d %v", code, v)
	}
	if got := e.ad.transed(); len(got) != 0 {
		t.Fatalf("must not force-trans: %v", got)
	}
	if len(e.ad.sent()) != 0 {
		t.Fatal("sent")
	}
}

// Auto fence + official WAITING_HUMAN → end→4, fail-closed (no send as AI).
func TestSendForceEndFromWaitingHumanFailClosed(t *testing.T) {
	e := newEnv(t)
	tok := e.token()
	_ = e.st.SetHandoverStatus(context.Background(), e.cust.ID, state.CustomerWaitingHuman, "service_state_precheck")
	before, _ := e.st.Customer(context.Background(), e.cust.ID)
	e.ad.svcState = state.CustomerWaitingHuman
	if code, v := e.send(tok, e.cust.UID, "text", "from-waiting"); code != 403 || errcode(v) != ErrCodeDisabled {
		t.Fatalf("%d %v", code, v)
	}
	after, _ := e.st.Customer(context.Background(), e.cust.ID)
	if after.FenceSource != state.FenceAuto || after.Generation != before.Generation {
		t.Fatalf("%+v gen %d→%d", after, before.Generation, after.Generation)
	}
	if got := e.ad.transed(); len(got) != 2 || got[0] != OfficialStateHuman || got[1] != OfficialStateClosed {
		t.Fatalf("WAITING trans %v want →3 then →4", got)
	}
	if len(e.ad.sent()) != 0 {
		t.Fatal("sent")
	}
}

// WAITING without bindings[].servicer_userid → fail 未配置接待人, never 2→4.
func TestSendForceEndWaitingWithoutServicer(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Bindings[0].ServicerUserID = "" })
	tok := e.token()
	_ = e.st.SetHandoverStatus(context.Background(), e.cust.ID, state.CustomerWaitingHuman, "service_state_precheck")
	e.ad.svcState = state.CustomerWaitingHuman
	if code, v := e.send(tok, e.cust.UID, "text", "from-waiting"); code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 without servicer: %d %v", code, v)
	}
	if got := e.ad.transed(); len(got) != 0 {
		t.Fatalf("must not call 2→4 when servicer missing: %v", got)
	}
	if len(e.ad.sent()) != 0 {
		t.Fatal("sent")
	}
}

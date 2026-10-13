package bridge

import (
	"context"
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

// Auto fence + official WAITING_HUMAN → force Trans→1, clear fence, send.
func TestSendForceTransFromWaitingHumanClearsAutoFence(t *testing.T) {
	e := newEnv(t)
	tok := e.token()
	_ = e.st.SetHandoverStatus(context.Background(), e.cust.ID, state.CustomerWaitingHuman, "service_state_precheck")
	before, _ := e.st.Customer(context.Background(), e.cust.ID)
	e.ad.svcState = state.CustomerWaitingHuman
	if code, v := e.send(tok, e.cust.UID, "text", "from-waiting"); code != 200 || errcode(v) != 0 {
		t.Fatalf("%d %v", code, v)
	}
	after, _ := e.st.Customer(context.Background(), e.cust.ID)
	if after.State != state.CustomerAIEligible || after.FenceSource != "" || after.Generation != before.Generation {
		t.Fatalf("%+v gen %d→%d", after, before.Generation, after.Generation)
	}
	if got := e.ad.transed(); len(got) != 1 || got[0] != OfficialStateAI {
		t.Fatalf("trans %v", got)
	}
}

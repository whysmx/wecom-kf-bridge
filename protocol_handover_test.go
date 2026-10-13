package bridge

import (
	"context"
	"net/http"
	"testing"

	"github.com/whysmx/wecom-kf-bridge/state"
)

// No manual fence + official HUMAN/WAITING → Trans(1), message sent, no manual fence.
func TestSendForceTransToAIWithoutManualIntent(t *testing.T) {
	for _, official := range []string{state.CustomerHuman, state.CustomerWaitingHuman} {
		t.Run(official, func(t *testing.T) {
			e := newEnv(t)
			tok := e.token()
			e.ad.svcState = official
			if code, v := e.send(tok, e.cust.UID, "text", "ai-reply"); code != 200 || errcode(v) != 0 {
				t.Fatalf("send: %d %v", code, v)
			}
			if got := e.ad.transed(); len(got) != 1 || got[0] != OfficialStateAI {
				t.Fatalf("trans calls %v", got)
			}
			if len(e.ad.sent()) != 1 {
				t.Fatalf("sends %d", len(e.ad.sent()))
			}
			c, _ := e.st.Customer(context.Background(), e.cust.ID)
			if c.FenceSource == state.FenceManual || c.State != state.CustomerAIEligible {
				t.Fatalf("state %s/%s", c.State, c.FenceSource)
			}
		})
	}
}

// Manual BeginHandover → keep 70004, never force Trans→1.
func TestSendManualHandoverBlocksWithoutForceTrans(t *testing.T) {
	e := newEnv(t)
	tok := e.token()
	if _, err := e.st.BeginHandover(context.Background(), e.cust.ID, "admin_handover"); err != nil {
		t.Fatal(err)
	}
	e.ad.svcState = state.CustomerHuman
	if code, v := e.send(tok, e.cust.UID, "text", "late"); code != 403 || errcode(v) != ErrCodeDisabled {
		t.Fatalf("manual hold: %d %v", code, v)
	}
	if got := e.ad.transed(); len(got) != 0 {
		t.Fatalf("must not force-trans when manual: %v", got)
	}
	if len(e.ad.sent()) != 0 {
		t.Fatal("sent while manual")
	}
}

// Force-trans failure fails closed (503), does not send.
func TestSendForceTransFailureFailClosed(t *testing.T) {
	e := newEnv(t)
	tok := e.token()
	e.ad.svcState = state.CustomerHuman
	e.ad.transErr = errBoom
	if code, v := e.send(tok, e.cust.UID, "text", "x"); code != http.StatusServiceUnavailable || errcode(v) != ErrCodeUnavailable {
		t.Fatalf("fail closed: %d %v", code, v)
	}
	if len(e.ad.sent()) != 0 {
		t.Fatal("sent after failed force-trans")
	}
}

// Auto fence + official HUMAN → force-trans, clear auto fence in place, send.
func TestSendForceTransClearsAutoFenceInPlace(t *testing.T) {
	e := newEnv(t)
	tok := e.token()
	_ = e.st.SetHandoverStatus(context.Background(), e.cust.ID, state.CustomerHuman, "service_state_precheck")
	before, _ := e.st.Customer(context.Background(), e.cust.ID)
	if before.FenceSource != state.FenceAuto {
		t.Fatal(before.FenceSource)
	}
	e.ad.svcState = state.CustomerHuman
	if code, v := e.send(tok, e.cust.UID, "text", "after-pullback"); code != 200 {
		t.Fatalf("%d %v", code, v)
	}
	after, _ := e.st.Customer(context.Background(), e.cust.ID)
	if after.State != state.CustomerAIEligible || after.FenceSource != "" {
		t.Fatalf("%s/%s", after.State, after.FenceSource)
	}
	if after.Generation != before.Generation || after.UID != before.UID {
		t.Fatalf("generation/UID must stay: before=%d/%s after=%d/%s", before.Generation, before.UID, after.Generation, after.UID)
	}
	if got := e.ad.transed(); len(got) != 1 || got[0] != OfficialStateAI {
		t.Fatalf("trans %v", got)
	}
}

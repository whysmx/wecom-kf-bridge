package bridge

import (
	"context"
	"net/http"
	"testing"

	"github.com/whysmx/wecom-kf-bridge/state"
)

// No manual fence + official HUMAN/WAITING → Trans(4) end, fail-closed (no send).
// WeCom forbids 2→1 / 3→1 (95016); AI resumes only after customer re-entry.
func TestSendForceEndWithoutManualIntent(t *testing.T) {
	for _, official := range []string{state.CustomerHuman, state.CustomerWaitingHuman} {
		t.Run(official, func(t *testing.T) {
			e := newEnv(t)
			tok := e.token()
			e.ad.svcState = official
			if code, v := e.send(tok, e.cust.UID, "text", "ai-reply"); code != 403 || errcode(v) != ErrCodeDisabled {
				t.Fatalf("send: %d %v", code, v)
			}
			if got := e.ad.transed(); len(got) != 1 || got[0] != OfficialStateClosed {
				t.Fatalf("trans calls %v want →4", got)
			}
			if len(e.ad.sent()) != 0 {
				t.Fatalf("must not send after end→4: %d", len(e.ad.sent()))
			}
		})
	}
}

// Manual BeginHandover → keep 70004, never force Trans→4.
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

// Force-end failure fails closed (503), does not send.
func TestSendForceEndFailureFailClosed(t *testing.T) {
	e := newEnv(t)
	tok := e.token()
	e.ad.svcState = state.CustomerHuman
	e.ad.transErr = errBoom
	if code, v := e.send(tok, e.cust.UID, "text", "x"); code != http.StatusServiceUnavailable || errcode(v) != ErrCodeUnavailable {
		t.Fatalf("fail closed: %d %v", code, v)
	}
	if len(e.ad.sent()) != 0 {
		t.Fatal("sent after failed force-end")
	}
}

// Auto fence + official HUMAN → end→4, fail-closed (fence kept; no send as AI).
func TestSendForceEndFromAutoFenceFailClosed(t *testing.T) {
	e := newEnv(t)
	tok := e.token()
	_ = e.st.SetHandoverStatus(context.Background(), e.cust.ID, state.CustomerHuman, "service_state_precheck")
	before, _ := e.st.Customer(context.Background(), e.cust.ID)
	if before.FenceSource != state.FenceAuto {
		t.Fatal(before.FenceSource)
	}
	e.ad.svcState = state.CustomerHuman
	if code, v := e.send(tok, e.cust.UID, "text", "after-end"); code != 403 || errcode(v) != ErrCodeDisabled {
		t.Fatalf("%d %v", code, v)
	}
	after, _ := e.st.Customer(context.Background(), e.cust.ID)
	if after.FenceSource != state.FenceAuto {
		t.Fatalf("auto fence should remain until re-entry: %s/%s", after.State, after.FenceSource)
	}
	if got := e.ad.transed(); len(got) != 1 || got[0] != OfficialStateClosed {
		t.Fatalf("trans %v want →4", got)
	}
	if len(e.ad.sent()) != 0 {
		t.Fatal("sent")
	}
}

// Force-end rejected by platform → 502 ErrCodePlatform.
func TestSendForceEndRejected(t *testing.T) {
	e := newEnv(t)
	tok := e.token()
	e.ad.svcState = state.CustomerHuman
	e.ad.transErr = &SendError{Kind: SendRejected, Err: errBoom}
	if code, v := e.send(tok, e.cust.UID, "text", "x"); code != http.StatusBadGateway || errcode(v) != ErrCodePlatform {
		t.Fatalf("rejected: %d %v", code, v)
	}
	if len(e.ad.sent()) != 0 {
		t.Fatal("sent")
	}
}

// Trans OK but reread fails → 503 fail-closed.
func TestSendForceEndRereadFailure(t *testing.T) {
	e := newEnv(t)
	tok := e.token()
	e.ad.svcSeq = []any{state.CustomerHuman, errBoom}
	e.ad.transNoPromote = true
	if code, v := e.send(tok, e.cust.UID, "text", "x"); code != http.StatusServiceUnavailable || errcode(v) != ErrCodeUnavailable {
		t.Fatalf("reread fail: %d %v", code, v)
	}
	if got := e.ad.transed(); len(got) != 1 || got[0] != OfficialStateClosed {
		t.Fatalf("trans %v", got)
	}
	if len(e.ad.sent()) != 0 {
		t.Fatal("sent")
	}
}

// Trans OK but reread still HUMAN → 503 not confirmed CLOSED.
func TestSendForceEndNotConfirmed(t *testing.T) {
	e := newEnv(t)
	tok := e.token()
	e.ad.svcState = state.CustomerHuman
	e.ad.transNoPromote = true
	if code, v := e.send(tok, e.cust.UID, "text", "x"); code != http.StatusServiceUnavailable || errcode(v) != ErrCodeUnavailable {
		t.Fatalf("not confirmed: %d %v", code, v)
	}
	if len(e.ad.sent()) != 0 {
		t.Fatal("sent")
	}
}

// ensureAIEligible: local auto fence + ServiceState error → 503 before CreateOutbox.
func TestEnsureAIEligibleServiceStateError(t *testing.T) {
	e := newEnv(t)
	tok := e.token()
	_ = e.st.SetHandoverStatus(context.Background(), e.cust.ID, state.CustomerHuman, "service_state_precheck")
	e.ad.svcErr = errBoom
	if code, v := e.send(tok, e.cust.UID, "text", "x"); code != http.StatusServiceUnavailable || errcode(v) != ErrCodeUnavailable {
		t.Fatalf("svc err: %d %v", code, v)
	}
	if len(e.ad.transed()) != 0 {
		t.Fatal("trans on svc err")
	}
}

// unavailableAdapter methods exist for interface completeness.
func TestUnavailableAdapterHandoverMethods(t *testing.T) {
	var a unavailableAdapter
	ctx := context.Background()
	if _, err := a.TransServiceState(ctx, Binding{}, Customer{}, OfficialStateAI); err == nil {
		t.Fatal("expected err")
	}
	if err := a.SendMsgOnEvent(ctx, Binding{}, "c", "t"); err == nil {
		t.Fatal("expected err")
	}
}

// ensureAIEligible force-end fails while local auto-fenced.
func TestEnsureAIEligibleForceEndFails(t *testing.T) {
	e := newEnv(t)
	tok := e.token()
	_ = e.st.SetHandoverStatus(context.Background(), e.cust.ID, state.CustomerHuman, "service_state_precheck")
	e.ad.svcState = state.CustomerHuman
	e.ad.transErr = errBoom
	if code, v := e.send(tok, e.cust.UID, "text", "x"); code != http.StatusServiceUnavailable {
		t.Fatalf("%d %v", code, v)
	}
}

// ensureAIEligible: official AI + auto fence → clear in place; clear failure → 503.
func TestEnsureAIEligibleClearFenceFails(t *testing.T) {
	e := newEnv(t)
	tok := e.token()
	_ = e.st.SetHandoverStatus(context.Background(), e.cust.ID, state.CustomerHuman, "service_state_precheck")
	e.ad.svcState = state.CustomerAIEligible
	e.st.DB().Exec(`CREATE TRIGGER caf_send BEFORE UPDATE OF state ON customers BEGIN SELECT RAISE(ABORT,'x'); END`)
	if code, v := e.send(tok, e.cust.UID, "text", "x"); code != http.StatusServiceUnavailable {
		t.Fatalf("%d %v", code, v)
	}
}

// ensureAIEligible: official CLOSED while auto-fenced → held (no force-end).
func TestEnsureAIEligibleNonHumanOfficialHeld(t *testing.T) {
	e := newEnv(t)
	tok := e.token()
	_ = e.st.SetHandoverStatus(context.Background(), e.cust.ID, state.CustomerHuman, "service_state_precheck")
	e.ad.svcState = state.CustomerClosed
	if code, v := e.send(tok, e.cust.UID, "text", "x"); code != 403 || errcode(v) != ErrCodeDisabled {
		t.Fatalf("%d %v", code, v)
	}
	if len(e.ad.transed()) != 0 {
		t.Fatal("trans")
	}
}

package bridge

import (
	"context"
	"testing"

	"github.com/whysmx/wecom-kf-bridge/state"
)

// Official 4 (已结束/未开始 → CLOSED) blocks this send only; no persistent
// fence. Human (3) fences automatically.
func TestSendOfficialClosedBlocksOnlyThisSend(t *testing.T) {
	e := newEnv(t)
	tok := e.token()
	e.ad.svcState = state.CustomerClosed
	if code, v := e.send(tok, e.cust.UID, "text", "x"); code != 403 || errcode(v) != ErrCodeDisabled {
		t.Fatalf("closed: %d %v", code, v)
	}
	c, _ := e.st.Customer(context.Background(), e.cust.ID)
	if c.State != state.CustomerAIEligible || c.FenceSource != "" {
		t.Fatalf("CLOSED wrote a fence: %s/%s", c.State, c.FenceSource)
	}
	e.ad.svcState = state.CustomerAIEligible
	if code, v := e.send(tok, e.cust.UID, "text", "y"); code != 200 {
		t.Fatalf("send after reopen: %d %v", code, v)
	}
	e.ad.svcState = state.CustomerHuman
	e.send(tok, e.cust.UID, "text", "z")
	if c, _ = e.st.Customer(context.Background(), e.cust.ID); c.State != state.CustomerHuman || c.FenceSource != state.FenceAuto {
		t.Fatalf("human: %s/%s", c.State, c.FenceSource)
	}
}

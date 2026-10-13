package admin

import (
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

package runtime

import (
	"context"
	"strings"

	"github.com/whysmx/wecom-kf-bridge/state"
)

// officialLeftHuman reports whether a mapped service_state means the seat has
// left human reception. Architect: live ∈ {0,4} → AI_ELIGIBLE or CLOSED.
// 48002 bypass must not count (treat_48002 untouched).
func officialLeftHuman(mapped string) bool {
	return mapped == state.CustomerAIEligible || mapped == state.CustomerClosed
}

// AlignAfterOfficialSessionEnd clears manual/HUMAN fence after the seat ended
// the session (or inbound probe sees live 0/4). Updates official cache from
// the mapped get result. Does not call trans 3→4.
//
// Returns the post-align customer. If mapped is still HUMAN/WAITING, leaves
// the fence untouched (still intentional human stay).
func AlignAfterOfficialSessionEnd(ctx context.Context, store *state.Store, customerID, mapped, reason string) (state.Customer, bool, error) {
	c, err := store.Customer(ctx, customerID)
	if err != nil {
		return state.Customer{}, false, err
	}
	if !officialLeftHuman(mapped) {
		return c, false, nil
	}
	nc, err := store.ClearManualFenceAfterOfficialEnd(ctx, customerID, reason)
	if err != nil {
		return c, false, err
	}
	// Cache official from get: 4→CLOSED; 0/1→AI_ELIGIBLE (SetOfficialStatus allows it).
	_ = store.SetOfficialStatus(ctx, customerID, mapped)
	return nc, true, nil
}

// sessionEndMsgCode returns a non-empty msg_code for optional end tip.
func sessionEndMsgCode(code string) string {
	return strings.TrimSpace(code)
}

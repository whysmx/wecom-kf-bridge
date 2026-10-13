package runtime

import (
	"context"
	"errors"
	"strings"

	bridge "github.com/whysmx/wecom-kf-bridge"
	"github.com/whysmx/wecom-kf-bridge/state"
)

const (
	defaultHandoverPhrase = "转人工"
	// defaultHandoverWelcome kept as alias of defaultHandoverQueueScript for tests.
	defaultHandoverWelcome = defaultHandoverQueueScript
)

// HandoverConfig is the phrase + transfer scripts used by 口令 and admin paths.
// Button and phrase share the same manual fence via RequestHumanHandover.
type HandoverConfig struct {
	Phrase  string          // exact trim match; default 转人工
	Welcome string          // legacy queue tip; Scripts.Queue wins when set
	Scripts HandoverScripts // three configurable send_msg_on_event phrases
}

func (h HandoverConfig) phrase() string {
	p := strings.TrimSpace(h.Phrase)
	if p == "" {
		return defaultHandoverPhrase
	}
	return p
}

// welcome returns the transfer tip used with trans→3 msg_code → send_msg_on_event.
func (h HandoverConfig) welcome() string {
	return h.Scripts.queueText(h.Welcome)
}

// MatchHandoverPhrase reports whether content is an exact (trim) phrase hit.
func MatchHandoverPhrase(cfg HandoverConfig, content string) bool {
	return strings.TrimSpace(content) == cfg.phrase()
}

// RequestHumanHandover performs the intentional handoff:
// official trans → HUMAN (3) with bindings[].servicer_userid, optional
// send_msg_on_event (queue tip), then local BeginHandover (manual fence).
// Stops at 3 — does not auto 3→4. Missing servicer → 未配置接待人 (no silent
// fallback to WAITING/2→4). Does not delete servicers.
//
// P1: if official trans→3 succeeded but BeginHandover fails, compensate with
// MarkManualFence so inbound pullBack will not ForceTrans→4 and AI stays paused.
func RequestHumanHandover(ctx context.Context, store *state.Store, ad bridge.Adapter, b bridge.Binding, c state.Customer, cfg HandoverConfig, reason string) error {
	sid := strings.TrimSpace(b.ServicerUserID)
	if sid == "" {
		return errors.New("未配置接待人")
	}
	cust := bridge.Customer{ID: c.ID, BindingID: c.BindingID, ExternalUserID: c.ExternalUserID, UserID: c.UID}
	msgCode, err := ad.TransServiceState(ctx, b, cust, bridge.OfficialStateHuman, sid)
	if err != nil {
		return err
	}
	if msgCode != "" {
		_ = ad.SendMsgOnEvent(ctx, b, msgCode, cfg.welcome())
	}
	if _, err := store.BeginHandover(ctx, c.ID, reason); err != nil {
		// Official already HUMAN: must plant manual intent before returning.
		if _, cerr := store.MarkManualFence(ctx, c.ID, reason+"_compensate"); cerr != nil {
			return err
		}
		_ = store.SetOfficialStatus(ctx, c.ID, state.CustomerHuman)
		return nil
	}
	_ = store.SetHandoverStatus(ctx, c.ID, state.CustomerHuman, reason)
	_ = store.SetOfficialStatus(ctx, c.ID, state.CustomerHuman)
	return nil
}

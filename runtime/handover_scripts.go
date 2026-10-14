package runtime

import "strings"

// Default transfer / event phrases for send_msg_on_event (WeCom: queue tip,
// human-join tip, end tip). Queue tip is wired on intentional trans→3; end is
// sent on admin recover trans→4 when HandoverEndScript is configured.
const (
	defaultHandoverQueueScript = "正在为您转接人工客服，请稍候…"
	defaultHandoverHumanScript = "人工客服已接入，请描述您的问题。"
	defaultHandoverEndScript   = "本次人工服务已结束，后续将由智能助手继续为您服务。"
)

// HandoverScripts are the three configurable send_msg_on_event copy slots.
// Official msg_code from kf/service_state/trans (or event callback) is the
// only delivery channel — never a parallel send path.
type HandoverScripts struct {
	// Queue is sent after intentional trans→3 (human assigned / transfer tip).
	Queue string `json:"queue"`
	// Human is reserved for pool-accept tip (config stored; minimal path still sends Queue).
	Human string `json:"human"`
	// End is sent on admin recover after confirmed trans→4 (send_msg_on_event).
	End string `json:"end"`
}

func (s HandoverScripts) queueText(legacyWelcome string) string {
	if q := strings.TrimSpace(s.Queue); q != "" {
		return q
	}
	if w := strings.TrimSpace(legacyWelcome); w != "" {
		return w
	}
	return defaultHandoverQueueScript
}

func (s HandoverScripts) humanText() string {
	if h := strings.TrimSpace(s.Human); h != "" {
		return h
	}
	return defaultHandoverHumanScript
}

func (s HandoverScripts) endText() string {
	if e := strings.TrimSpace(s.End); e != "" {
		return e
	}
	return defaultHandoverEndScript
}

// NormalizeHandoverScripts fills empty slots with defaults (and maps legacy
// handover_welcome into queue when queue is blank).
func NormalizeHandoverScripts(s HandoverScripts, legacyWelcome string) HandoverScripts {
	return HandoverScripts{
		Queue: s.queueText(legacyWelcome),
		Human: s.humanText(),
		End:   s.endText(),
	}
}

// HandoverContextConfig is the local chat-draft window used when transferring
// to a human (admin detail + optional send_msg_on_event summary).
type HandoverContextConfig struct {
	MaxTurns        int `json:"max_turns"`
	WindowMinutes   int `json:"window_minutes"`
	TTLDays         int `json:"ttl_days"`
	SummaryMaxChars int `json:"summary_max_chars"`
}

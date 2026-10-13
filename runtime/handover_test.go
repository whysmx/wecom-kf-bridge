package runtime

import (
	"bytes"
	"context"
	"testing"

	bridge "github.com/whysmx/wecom-kf-bridge"
	"github.com/whysmx/wecom-kf-bridge/state"
)

func TestMatchHandoverPhraseExactTrim(t *testing.T) {
	cfg := HandoverConfig{}
	if !MatchHandoverPhrase(cfg, "转人工") || !MatchHandoverPhrase(cfg, "  转人工  ") {
		t.Fatal("default phrase")
	}
	if MatchHandoverPhrase(cfg, "请转人工谢谢") || MatchHandoverPhrase(cfg, "转人工吧") {
		t.Fatal("must be exact")
	}
	cfg.Phrase = "人工"
	if !MatchHandoverPhrase(cfg, "人工") || MatchHandoverPhrase(cfg, "转人工") {
		t.Fatal("custom phrase")
	}
}

type handoverFakeAdapter struct {
	bridge.Adapter
	trans []int
	events []string
	svc    string
}

func (a *handoverFakeAdapter) ServiceState(context.Context, bridge.Binding, bridge.Customer) (string, error) {
	if a.svc == "" {
		return state.CustomerAIEligible, nil
	}
	return a.svc, nil
}
func (a *handoverFakeAdapter) TransServiceState(_ context.Context, _ bridge.Binding, _ bridge.Customer, st int) (string, error) {
	a.trans = append(a.trans, st)
	if st == bridge.OfficialStateWaiting {
		a.svc = state.CustomerWaitingHuman
	}
	return "code-1", nil
}
func (a *handoverFakeAdapter) SendMsgOnEvent(_ context.Context, _ bridge.Binding, code, text string) error {
	a.events = append(a.events, code+"|"+text)
	return nil
}
func (a *handoverFakeAdapter) GetUser(_ context.Context, _ bridge.Binding, c bridge.Customer) (bridge.Customer, error) {
	return c, nil
}
func (a *handoverFakeAdapter) SendText(context.Context, bridge.SendRequest) (string, error) {
	return "", nil
}
func (a *handoverFakeAdapter) GetMedia(context.Context, bridge.Binding, string) (bridge.MediaDownload, error) {
	return bridge.MediaDownload{}, nil
}

func TestRequestHumanHandoverSetsManualFence(t *testing.T) {
	st, err := state.OpenWithOptions(t.TempDir()+"/h.db", state.Options{MasterKey: bytes.Repeat([]byte{9}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	_ = st.PutEnterprise(ctx, state.Enterprise{ID: "e1", TenantID: "t", CorpID: "c", CredentialRef: "env:X"})
	_ = st.PutBinding(ctx, state.Binding{ID: "b1", EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p", Active: true})
	c, err := st.EnsureCustomer(ctx, "e1", "b1", "ext-h")
	if err != nil {
		t.Fatal(err)
	}
	ad := &handoverFakeAdapter{}
	if err := RequestHumanHandover(ctx, st, ad, bridge.Binding{ID: "b1"}, c, HandoverConfig{}, "phrase_handover"); err != nil {
		t.Fatal(err)
	}
	if len(ad.trans) != 1 || ad.trans[0] != bridge.OfficialStateWaiting {
		t.Fatalf("trans %v", ad.trans)
	}
	if len(ad.events) != 1 || ad.events[0] != "code-1|"+defaultHandoverWelcome {
		t.Fatalf("events %v", ad.events)
	}
	got, _ := st.Customer(ctx, c.ID)
	if got.FenceSource != state.FenceManual || got.State != state.CustomerWaitingHuman {
		t.Fatalf("%s/%s", got.State, got.FenceSource)
	}
}

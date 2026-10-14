package runtime

import (
	"bytes"
	"context"
	"strings"
	"testing"

	bridge "github.com/whysmx/wecom-kf-bridge"
	"github.com/whysmx/wecom-kf-bridge/state"
)

func TestRequestHumanHandoverMergesChatSummary(t *testing.T) {
	st, err := state.OpenWithOptions(t.TempDir()+"/hc.db", state.Options{MasterKey: bytes.Repeat([]byte{9}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	_ = st.PutEnterprise(ctx, state.Enterprise{ID: "e1", TenantID: "t", CorpID: "c", CredentialRef: "env:X"})
	_ = st.PutBinding(ctx, state.Binding{ID: "b1", EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p", Active: true})
	c, err := st.EnsureCustomer(ctx, "e1", "b1", "ext-hc")
	if err != nil {
		t.Fatal(err)
	}
	_ = st.AppendChatTurn(ctx, c.ID, state.ChatRoleUser, "text", "我想退款", 0)
	_ = st.AppendChatTurn(ctx, c.ID, state.ChatRoleAI, "text", "请稍等，正在查询", 0)

	ad := &handoverFakeAdapter{}
	cfg := HandoverConfig{
		Welcome:                "正在为您转接人工客服，请稍候…",
		ContextMaxTurns:        20,
		ContextWindowMinutes:   30,
		ContextSummaryMaxChars: 600,
	}
	if err := RequestHumanHandover(ctx, st, ad, bridge.Binding{ID: "b1", ServicerUserID: "WenHaiYan"}, c, cfg, "phrase"); err != nil {
		t.Fatal(err)
	}
	if len(ad.events) != 1 {
		t.Fatalf("events %v", ad.events)
	}
	ev := ad.events[0]
	if !strings.Contains(ev, "code-1|") || !strings.Contains(ev, "正在为您转接") || !strings.Contains(ev, "我想退款") || !strings.Contains(ev, "请稍等") {
		t.Fatalf("event %q", ev)
	}
	got, _ := st.Customer(ctx, c.ID)
	if got.FenceSource != state.FenceManual || got.State != state.CustomerHuman {
		t.Fatalf("%s/%s", got.State, got.FenceSource)
	}
}

func TestRequestHumanHandoverNoMsgCodeSkipsSummary(t *testing.T) {
	st, err := state.OpenWithOptions(t.TempDir()+"/hc2.db", state.Options{MasterKey: bytes.Repeat([]byte{9}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	_ = st.PutEnterprise(ctx, state.Enterprise{ID: "e1", TenantID: "t", CorpID: "c", CredentialRef: "env:X"})
	_ = st.PutBinding(ctx, state.Binding{ID: "b1", EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p", Active: true})
	c, err := st.EnsureCustomer(ctx, "e1", "b1", "ext-hc2")
	if err != nil {
		t.Fatal(err)
	}
	_ = st.AppendChatTurn(ctx, c.ID, state.ChatRoleUser, "text", "hello", 0)

	ad := &emptyMsgCodeAdapter{}
	if err := RequestHumanHandover(ctx, st, ad, bridge.Binding{ID: "b1", ServicerUserID: "WenHaiYan"}, c, HandoverConfig{Welcome: "tip"}, "phrase"); err != nil {
		t.Fatal(err)
	}
	if ad.eventN != 0 {
		t.Fatalf("no msg_code must skip event, eventN=%d", ad.eventN)
	}
	got, _ := st.Customer(ctx, c.ID)
	if got.State != state.CustomerHuman {
		t.Fatalf("state %s", got.State)
	}
}

func TestRequestHumanHandoverDraftFailureDoesNotBlock(t *testing.T) {
	// Closed store → MergeHandoverTipWithContext fails internally but tip falls back;
	// BeginHandover would also fail — instead use valid store with no turns (empty merge).
	st, err := state.OpenWithOptions(t.TempDir()+"/hc3.db", state.Options{MasterKey: bytes.Repeat([]byte{9}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	_ = st.PutEnterprise(ctx, state.Enterprise{ID: "e1", TenantID: "t", CorpID: "c", CredentialRef: "env:X"})
	_ = st.PutBinding(ctx, state.Binding{ID: "b1", EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p", Active: true})
	c, err := st.EnsureCustomer(ctx, "e1", "b1", "ext-hc3")
	if err != nil {
		t.Fatal(err)
	}
	ad := &handoverFakeAdapter{}
	if err := RequestHumanHandover(ctx, st, ad, bridge.Binding{ID: "b1", ServicerUserID: "WenHaiYan"}, c, HandoverConfig{Welcome: "仅提示"}, "phrase"); err != nil {
		t.Fatal(err)
	}
	if len(ad.events) != 1 || ad.events[0] != "code-1|仅提示" {
		t.Fatalf("empty draft keeps tip only: %v", ad.events)
	}
}

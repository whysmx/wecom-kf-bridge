package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/whysmx/wecom-kf-bridge/admin"
	"github.com/whysmx/wecom-kf-bridge/state"
)

// P1 regression: OnHandoverPhrase must read liveCfg after SaveSettings so the
// 口令 path picks up the new queue script the same way the admin button does.
func TestOnHandoverPhraseUsesHotReloadedScripts(t *testing.T) {
	fw := &fakeWeCom{}
	wx := httptest.NewServer(fw.handler(t))
	t.Cleanup(wx.Close)

	t.Setenv("HR_MASTER", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	t.Setenv("HR_SECRET", "real-secret")
	t.Setenv("HR_CBTOK", "realtok")
	t.Setenv("HR_CBAES", key43(1))
	t.Setenv("HR_VSECRET", "vsecret")
	t.Setenv("HR_VTOK", "vtok")
	t.Setenv("HR_VAES", key43(2))
	raw := fmt.Sprintf(`{
	 "storage":{"database":%q},
	 "security":{"master_key_env":"HR_MASTER","callback_targets":[{"host":"127.0.0.1","port":9,"allowed_cidrs":["127.0.0.0/8"]}]},
	 "wecom":{"allow_insecure_http":true,"api_base_url":%q,"customer_origins":[3],"service_state_map":{"0":"AI_ELIGIBLE","1":"AI_ELIGIBLE","2":"WAITING_HUMAN","3":"HUMAN","4":"CLOSED"},"handover_phrase":"转人工","handover_welcome":"旧排队话术"},
	 "enterprises":[{"id":"e1","tenant_key":"acme","corp_id":"wwcorp","secret_env":"HR_SECRET","callback_token_env":"HR_CBTOK","callback_aes_key_env":"HR_CBAES"}],
	 "bindings":[{"id":"b1","enterprise_id":"e1","open_kfid":"kf1","project_id":"p1","virtual_corp_id":"bridge_a","agent_id":"1000002","virtual_secret_env":"HR_VSECRET","callback_token_env":"HR_VTOK","callback_aes_key_env":"HR_VAES","callback_url":"http://127.0.0.1:9/cb"}]}`,
		t.TempDir()+"/hr.db", wx.URL)
	cfg, err := ParseConfig([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	g, err := Build(context.Background(), cfg, &fieldLogger{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	if g.Delivery.OnHandoverPhrase == nil {
		t.Fatal("OnHandoverPhrase not wired")
	}

	ctx := context.Background()
	st := g.Store
	_ = st.PutEnterprise(ctx, state.Enterprise{ID: "e1", TenantID: "t", CorpID: "c", CredentialRef: "r"})

	fw.mu.Lock()
	fw.transBody = `{"errcode":0,"msg_code":"mc-old"}`
	fw.mu.Unlock()
	cu1, err := st.EnsureCustomer(ctx, "e1", "b1", "ext-hr-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Delivery.OnHandoverPhrase(ctx, cu1, state.InboxMessage{BindingID: "b1"}); err != nil {
		t.Fatal(err)
	}
	fw.mu.Lock()
	if len(fw.eventTexts) != 1 || fw.eventTexts[0] != "旧排队话术" {
		fw.mu.Unlock()
		t.Fatalf("before save: eventTexts=%v want [旧排队话术]", fw.eventTexts)
	}
	fw.eventTexts = nil
	fw.transBody = `{"errcode":0,"msg_code":"mc-new"}`
	fw.mu.Unlock()

	g.applyEditableSettings(admin.EditableSettings{
		Treat48002AsAIEligible: cfg.WeCom.Treat48002AsAIEligible,
		ServiceStateMap: map[string]string{
			"0": "AI_ELIGIBLE", "1": "AI_ELIGIBLE",
			"2": "WAITING_HUMAN", "3": "HUMAN", "4": "CLOSED",
		},
		PublicBaseURL:       cfg.Server.PublicBaseURL,
		HandoverQueueScript: "新排队话术",
		HandoverHumanScript: "接入自定义",
		HandoverEndScript:   "结束自定义",
	})

	cu2, err := st.EnsureCustomer(ctx, "e1", "b1", "ext-hr-2")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Delivery.OnHandoverPhrase(ctx, cu2, state.InboxMessage{BindingID: "b1"}); err != nil {
		t.Fatal(err)
	}
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if len(fw.eventTexts) != 1 || fw.eventTexts[0] != "新排队话术" {
		t.Fatalf("after SaveSettings hot-reload: eventTexts=%v want [新排队话术]", fw.eventTexts)
	}
}

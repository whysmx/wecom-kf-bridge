package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/whysmx/wecom-kf-bridge/state"
)

// Exercises the ForceTransToAI (end→4) / OnHandoverPhrase closures wired in Build.
func TestGatewayForceTransToAIClosure(t *testing.T) {
	fw := &fakeWeCom{}
	wx := httptest.NewServer(fw.handler(t))
	t.Cleanup(wx.Close)

	t.Setenv("FT_MASTER", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	t.Setenv("FT_SECRET", "real-secret")
	t.Setenv("FT_CBTOK", "realtok")
	t.Setenv("FT_CBAES", key43(1))
	t.Setenv("FT_VSECRET", "vsecret")
	t.Setenv("FT_VTOK", "vtok")
	t.Setenv("FT_VAES", key43(2))
	raw := fmt.Sprintf(`{
	 "storage":{"database":%q},
	 "security":{"master_key_env":"FT_MASTER","callback_targets":[{"host":"127.0.0.1","port":9,"allowed_cidrs":["127.0.0.0/8"]}]},
	 "wecom":{"allow_insecure_http":true,"api_base_url":%q,"customer_origins":[3],"service_state_map":{"0":"AI_ELIGIBLE","1":"AI_ELIGIBLE","2":"WAITING_HUMAN","3":"HUMAN","4":"CLOSED"},"handover_phrase":"转人工","handover_welcome":"请稍候"},
	 "enterprises":[{"id":"e1","tenant_key":"acme","corp_id":"wwcorp","secret_env":"FT_SECRET","callback_token_env":"FT_CBTOK","callback_aes_key_env":"FT_CBAES"}],
	 "bindings":[{"id":"b1","enterprise_id":"e1","open_kfid":"kf1","project_id":"p1","virtual_corp_id":"bridge_a","agent_id":"1000002","virtual_secret_env":"FT_VSECRET","callback_token_env":"FT_VTOK","callback_aes_key_env":"FT_VAES","callback_url":"http://127.0.0.1:9/cb","servicer_userid":"WenHaiYan"}]}`,
		t.TempDir()+"/ft.db", wx.URL)
	cfg, err := ParseConfig([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	g, err := Build(context.Background(), cfg, &fieldLogger{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	if g.Delivery.ForceTransToAI == nil || g.Delivery.OnHandoverPhrase == nil {
		t.Fatal("hooks not wired")
	}
	c := state.Customer{ID: "cid", BindingID: "b1", ExternalUserID: "ext-ft", UID: "u1"}
	ctx := context.Background()

	// end→4 success: get returns CLOSED; hook still fail-closes (re-entry required)
	fw.mu.Lock()
	fw.stateGet = `{"errcode":0,"service_state":4}`
	fw.mu.Unlock()
	err = g.Delivery.ForceTransToAI(ctx, "b1", c, state.CustomerHuman)
	if err == nil {
		t.Fatal("want fail-closed after end→4")
	}
	if !strings.Contains(err.Error(), "re-entry") {
		t.Fatalf("want re-entry hint, got %v", err)
	}

	// trans rejected
	fw.mu.Lock()
	fw.transBody = `{"errcode":48002,"errmsg":"no"}`
	fw.mu.Unlock()
	if err := g.Delivery.ForceTransToAI(ctx, "b1", c, state.CustomerHuman); err == nil {
		t.Fatal("want reject")
	}

	// trans OK but reread not CLOSED
	fw.mu.Lock()
	fw.transBody = `{"errcode":0,"msg_code":"mc"}`
	fw.stateGet = `{"errcode":0,"service_state":3}`
	fw.mu.Unlock()
	if err := g.Delivery.ForceTransToAI(ctx, "missing-binding", c, state.CustomerHuman); err == nil {
		t.Fatal("want not confirmed")
	}

	// OnHandoverPhrase with known binding
	fw.mu.Lock()
	fw.transBody = `{"errcode":0,"msg_code":"mc-h"}`
	fw.stateGet = `{"errcode":0,"service_state":1}`
	fw.mu.Unlock()
	// need a real customer row for BeginHandover
	st := g.Store
	_ = st.PutEnterprise(ctx, state.Enterprise{ID: "e1", TenantID: "t", CorpID: "c", CredentialRef: "r"})
	// binding already from Build
	cu, err := st.EnsureCustomer(ctx, "e1", "b1", "ext-phrase")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Delivery.OnHandoverPhrase(ctx, cu, state.InboxMessage{BindingID: "b1"}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Customer(ctx, cu.ID)
	if got.FenceSource != state.FenceManual {
		t.Fatal(got.FenceSource)
	}
}

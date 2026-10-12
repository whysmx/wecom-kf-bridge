package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bridge "github.com/whysmx/wecom-kf-bridge"
	"github.com/whysmx/wecom-kf-bridge/admin"
	"github.com/whysmx/wecom-kf-bridge/state"
	"github.com/whysmx/wecom-kf-bridge/wecom"
)

func writeGatewayConfig(t *testing.T, c Config) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	c.ConfigPath = ""
	if err := WriteConfigFile(path, c); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSaveBindingConfigPersistsAndHotApplies(t *testing.T) {
	c := gatewayConfig(t)
	path := writeGatewayConfig(t, c)
	c.ConfigPath = path
	g, err := Build(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	rt := consoleRuntime{g: g, cfg: c}
	list := rt.ConfigBindings()
	if len(list) != 1 || list[0].ID != "b1" {
		t.Fatalf("ConfigBindings %#v", list)
	}

	patch := admin.BindingConfigPatch{
		EnterpriseID:  "e1",
		OpenKfID:      "kf1",
		ProjectID:     "p-hot",
		VirtualCorpID: "bridge_a",
		AgentID:       "1000099",
		CallbackURL:   "http://127.0.0.1:9/hot",
		Disabled:      false,
	}
	if err := rt.SaveBindingConfig(context.Background(), "b1", patch); err != nil {
		t.Fatal(err)
	}

	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Bindings[0].ProjectID != "p-hot" || got.Bindings[0].AgentID != "1000099" || got.Bindings[0].CallbackURL != "http://127.0.0.1:9/hot" {
		t.Fatalf("file %+v", got.Bindings[0])
	}

	b, err := g.Store.Binding(context.Background(), "b1")
	if err != nil {
		t.Fatal(err)
	}
	if b.ProjectID != "p-hot" || b.CallbackURL != "http://127.0.0.1:9/hot" || !b.Active {
		t.Fatalf("sqlite %+v", b)
	}
	url, agent := g.Delivery.target("b1")
	if url != "http://127.0.0.1:9/hot" || agent != "1000099" {
		t.Fatalf("delivery %q %q", url, agent)
	}
	ed := rt.ConfigBindings()[0]
	if ed.ProjectID != "p-hot" || ed.AgentID != "1000099" {
		t.Fatalf("live %#v", ed)
	}
}

func TestSaveBindingConfigDisabledHotApplies(t *testing.T) {
	c := gatewayConfig(t)
	path := writeGatewayConfig(t, c)
	c.ConfigPath = path
	g, err := Build(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	rt := consoleRuntime{g: g, cfg: c}
	patch := admin.BindingConfigPatch{
		EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p1",
		VirtualCorpID: "bridge_a", AgentID: "1000002",
		CallbackURL: "http://127.0.0.1:9/cb", Disabled: true,
	}
	if err := rt.SaveBindingConfig(context.Background(), "b1", patch); err != nil {
		t.Fatal(err)
	}
	b, err := g.Store.Binding(context.Background(), "b1")
	if err != nil || b.Active {
		t.Fatalf("expected inactive: %+v %v", b, err)
	}
	got, _ := LoadConfig(path)
	if !got.Bindings[0].Disabled {
		t.Fatal("file disabled not set")
	}
}

func TestSaveBindingConfigRejectsIdentityChange(t *testing.T) {
	c := gatewayConfig(t)
	path := writeGatewayConfig(t, c)
	c.ConfigPath = path
	g, err := Build(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	rt := consoleRuntime{g: g, cfg: c}
	err = rt.SaveBindingConfig(context.Background(), "b1", admin.BindingConfigPatch{
		EnterpriseID: "e-x", OpenKfID: "kf1", ProjectID: "p1",
		VirtualCorpID: "bridge_a", AgentID: "1000002",
		CallbackURL: "http://127.0.0.1:9/cb",
	})
	if err == nil || !strings.Contains(err.Error(), "不可修改") {
		t.Fatalf("got %v", err)
	}
}

func TestSaveBindingConfigRejectsUnknownAndBadURL(t *testing.T) {
	c := gatewayConfig(t)
	path := writeGatewayConfig(t, c)
	c.ConfigPath = path
	g, err := Build(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	rt := consoleRuntime{g: g, cfg: c}
	err = rt.SaveBindingConfig(context.Background(), "missing", admin.BindingConfigPatch{
		EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p1",
		VirtualCorpID: "bridge_a", AgentID: "1000002",
		CallbackURL: "http://127.0.0.1:9/cb",
	})
	if err == nil || !strings.Contains(err.Error(), "config.json") {
		t.Fatalf("got %v", err)
	}
	err = rt.SaveBindingConfig(context.Background(), "b1", admin.BindingConfigPatch{
		EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p1",
		VirtualCorpID: "bridge_a", AgentID: "1000002",
		CallbackURL: "http://evil.example/x",
	})
	if err == nil {
		t.Fatal("expected validate/url error")
	}
}

func TestSaveBindingConfigMissingPath(t *testing.T) {
	rt := consoleRuntime{g: &Gateway{liveCfg: Config{}, configPath: ""}}
	err := rt.SaveBindingConfig(context.Background(), "b1", admin.BindingConfigPatch{
		EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p1",
		VirtualCorpID: "bridge_a", AgentID: "1000002",
		CallbackURL: "http://127.0.0.1:9/cb",
	})
	if err == nil || !strings.Contains(err.Error(), "config path") {
		t.Fatalf("got %v", err)
	}
}

func TestSaveBindingConfigEmptyFields(t *testing.T) {
	c := gatewayConfig(t)
	path := writeGatewayConfig(t, c)
	c.ConfigPath = path
	g, err := Build(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	rt := consoleRuntime{g: g, cfg: c}
	err = rt.SaveBindingConfig(context.Background(), "b1", admin.BindingConfigPatch{
		EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "  ",
		VirtualCorpID: "bridge_a", AgentID: "1000002",
		CallbackURL: "http://127.0.0.1:9/cb",
	})
	if err == nil || !strings.Contains(err.Error(), "不能为空") {
		t.Fatalf("got %v", err)
	}
}

func TestWriteConfigFilePreservesBindingsRoundTrip(t *testing.T) {
	c := gatewayConfig(t)
	path := writeGatewayConfig(t, c)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	binds := probe["bindings"].([]any)
	b0 := binds[0].(map[string]any)
	if _, ok := b0["virtual_secret"]; ok {
		t.Fatal("secret plaintext must not appear")
	}
	if b0["virtual_secret_env"] == nil || b0["virtual_secret_env"] == "" {
		t.Fatal("env missing")
	}
}

func TestSaveBindingConfigLoadFailure(t *testing.T) {
	rt := consoleRuntime{g: &Gateway{configPath: filepath.Join(t.TempDir(), "missing.json"), liveCfg: Config{}}}
	err := rt.SaveBindingConfig(context.Background(), "b1", admin.BindingConfigPatch{
		EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p1",
		VirtualCorpID: "bridge_a", AgentID: "1000002",
		CallbackURL: "http://127.0.0.1:9/cb",
	})
	if err == nil {
		t.Fatal("expected load failure")
	}
}

func TestApplyBindingConfigAppendsMissingLiveAndNilServer(t *testing.T) {
	c := gatewayConfig(t)
	st, err := state.OpenWithOptions(t.TempDir()+"/a.db", state.Options{MasterKey: bytes.Repeat([]byte{9}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err := st.PutEnterprise(ctx, state.Enterprise{ID: "e1", TenantID: "t", CorpID: "ww", CredentialRef: "env:X", Status: "ACTIVE"}); err != nil {
		t.Fatal(err)
	}
	bc := c.Bindings[0]
	g := &Gateway{
		Store:    st,
		liveCfg:  Config{Bindings: nil}, // force !found append
		Delivery: &DeliveryWorker{CallbackURL: map[string]string{}, AgentID: map[string]string{}},
		adapter:  &WeComAdapter{clients: map[string]*wecom.Client{"e1": {}}, tokens: map[string]*tokenCache{}, routes: map[string]bindingRoute{}, states: map[int]string{}},
		Sync:     &SyncWorker{Scopes: map[string]scopeInfo{}},
		scopes:   map[string]string{},
		vbs:      map[string]bridge.Binding{},
		Server:   nil, // cover nil Server early return
	}
	// Seed sealed credentials so withStoredCredentials succeeds without env first-init.
	raw := `{"corp_id":"bridge_a","secret":"v","token":"vt","aes_key":"` + gatewayEnv["G_VAES"] + `","agent_id":"1000002"}`
	if err := st.PutBinding(ctx, state.Binding{ID: bc.ID, EnterpriseID: bc.EnterpriseID, OpenKfID: bc.OpenKfID, ProjectID: "old", CallbackURL: "http://127.0.0.1:9/cb", Active: true, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveBindingSecret(ctx, bc.ID, raw, 1); err != nil {
		t.Fatal(err)
	}
	bc.ProjectID = "p-append"
	bc.CallbackURL = "http://127.0.0.1:9/new"
	bc.AgentID = "1000011"
	if err := g.applyBindingConfig(ctx, bc); err != nil {
		t.Fatal(err)
	}
	g.cfgMu.RLock()
	n := len(g.liveCfg.Bindings)
	g.cfgMu.RUnlock()
	if n != 1 || g.liveCfg.Bindings[0].ProjectID != "p-append" {
		t.Fatalf("liveCfg %#v", g.liveCfg.Bindings)
	}
	url, agent := g.Delivery.target(bc.ID)
	if url != "http://127.0.0.1:9/new" || agent != "1000011" {
		t.Fatalf("delivery %q %q", url, agent)
	}
}

func TestApplyBindingConfigBootstrapIdentityError(t *testing.T) {
	st, err := state.OpenWithOptions(t.TempDir()+"/a.db", state.Options{MasterKey: bytes.Repeat([]byte{9}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err := st.PutEnterprise(ctx, state.Enterprise{ID: "e1", TenantID: "t", CorpID: "ww", CredentialRef: "env:X", Status: "ACTIVE"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutBinding(ctx, state.Binding{ID: "b1", EnterpriseID: "e1", OpenKfID: "kf-old", ProjectID: "p1", CallbackURL: "http://127.0.0.1:9/cb", Active: true, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	g := &Gateway{Store: st, liveCfg: Config{Bindings: []BindingConfig{{ID: "b1", EnterpriseID: "e1", OpenKfID: "kf1"}}}}
	err = g.applyBindingConfig(ctx, BindingConfig{
		ID: "b1", EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p1",
		VirtualCorpID: "bridge_a", AgentID: "1000002", CallbackURL: "http://127.0.0.1:9/cb",
	})
	if err == nil {
		t.Fatal("expected identity change error")
	}
}

func TestApplyBindingConfigCredentialsFromEnv(t *testing.T) {
	c := gatewayConfig(t)
	st, err := state.OpenWithOptions(t.TempDir()+"/a.db", state.Options{MasterKey: bytes.Repeat([]byte{9}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err := st.PutEnterprise(ctx, state.Enterprise{ID: "e1", TenantID: "t", CorpID: "ww", CredentialRef: "env:X", Status: "ACTIVE"}); err != nil {
		t.Fatal(err)
	}
	bc := c.Bindings[0]
	if err := st.PutBinding(ctx, state.Binding{ID: bc.ID, EnterpriseID: bc.EnterpriseID, OpenKfID: bc.OpenKfID, ProjectID: bc.ProjectID, CallbackURL: bc.CallbackURL, Active: true, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	g := &Gateway{
		Store:    st,
		liveCfg:  Config{Bindings: []BindingConfig{bc}},
		Delivery: &DeliveryWorker{CallbackURL: map[string]string{}, AgentID: map[string]string{}},
		adapter:  &WeComAdapter{clients: map[string]*wecom.Client{"e1": {}}, tokens: map[string]*tokenCache{}, routes: map[string]bindingRoute{}, states: map[int]string{}},
		Sync:     &SyncWorker{Scopes: map[string]scopeInfo{}, Logger: nopRuntimeLogger{}},
		scopes:   map[string]string{},
		vbs:      map[string]bridge.Binding{},
		Server:   nil,
	}
	// No sealed secret → bindingCredentials path via env.
	if err := g.applyBindingConfig(ctx, bc); err != nil {
		t.Fatal(err)
	}
}

func TestApplyBindingConfigCredentialsFailure(t *testing.T) {
	st, err := state.OpenWithOptions(t.TempDir()+"/a.db", state.Options{MasterKey: bytes.Repeat([]byte{9}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err := st.PutEnterprise(ctx, state.Enterprise{ID: "e1", TenantID: "t", CorpID: "ww", CredentialRef: "env:X", Status: "ACTIVE"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutBinding(ctx, state.Binding{ID: "b1", EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p1", CallbackURL: "http://127.0.0.1:9/cb", Active: true, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MISSING_VSECRET", "")
	g := &Gateway{Store: st, liveCfg: Config{Bindings: []BindingConfig{{ID: "b1"}}}, scopes: map[string]string{}, vbs: map[string]bridge.Binding{}}
	err = g.applyBindingConfig(ctx, BindingConfig{
		ID: "b1", EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p1",
		VirtualCorpID: "bridge_a", AgentID: "1000002",
		VirtualSecretEnv: "MISSING_VSECRET", CallbackTokenEnv: "MISSING_VTOK", CallbackAESKeyEnv: "MISSING_VAES",
		CallbackURL: "http://127.0.0.1:9/cb",
	})
	if err == nil {
		t.Fatal("expected credentials failure")
	}
}

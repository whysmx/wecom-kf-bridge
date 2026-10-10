package runtime

import (
	"context"
	"strings"
	"testing"
)

// #41: binding env vars are needed only for the first initialisation.
func TestRestartWithoutLegacyBindingEnv(t *testing.T) {
	c := gatewayConfig(t)
	g, err := Build(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if gettoken(t, g.Handler, "bridge_a", gatewayEnv["G_VSECRET"]) != 0 {
		t.Fatal("first start")
	}
	if _, exported, err := g.Store.BindingSecret(context.Background(), "b1"); err != nil || !exported {
		t.Fatal("env credentials not persisted as already-exported", err)
	}
	g.Close()
	for _, k := range []string{"G_VSECRET", "G_VTOK", "G_VAES"} {
		t.Setenv(k, "")
	}
	g, err = Build(context.Background(), c, nil)
	if err != nil {
		t.Fatal("restart required legacy env:", err)
	}
	if gettoken(t, g.Handler, "bridge_a", gatewayEnv["G_VSECRET"]) != 0 {
		t.Fatal("stored credentials not used after restart")
	}
	g.Close()
	// a changed env value is ignored once stored (DB is the source of truth)
	t.Setenv("G_VSECRET", "different-secret-value-xxxxxxxx")
	g, err = Build(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if gettoken(t, g.Handler, "bridge_a", "different-secret-value-xxxxxxxx") == 0 {
		t.Fatal("env overrode stored credentials")
	}
	g.Close()
	// fresh DB without env fails clearly
	c2 := gatewayConfig(t)
	t.Setenv("G_VTOK", "")
	_, err = Build(context.Background(), c2, nil)
	if err == nil || !strings.Contains(err.Error(), "first start") || !strings.Contains(err.Error(), "G_VTOK") {
		t.Fatal("fresh DB error unclear:", err)
	}
}

// #42: api_base_url must not carry userinfo/query/fragment; logs and the
// settings page show it redacted.
func TestAPIBaseURLStrict(t *testing.T) {
	for _, u := range []string{"https://u:p@qyapi.weixin.qq.com", "https://qyapi.weixin.qq.com?x=1", "https://qyapi.weixin.qq.com/#f", "https://qyapi.weixin.qq.com?"} {
		c := gatewayConfig(t)
		c.WeCom.APIBaseURL = u
		if c.Validate() == nil {
			t.Errorf("%s accepted", u)
		}
	}
}

func TestPublicBaseURLValidation(t *testing.T) {
	for u, ok := range map[string]bool{"https://bridge.example.com": true, "": true, "ftp://x": false, "https://u:p@x": false, "https://x?a=1": false, "https://x/cgi-bin": false, "https://x/#f": false} {
		c := gatewayConfig(t)
		c.Server.PublicBaseURL = u
		if (c.Validate() == nil) != ok {
			t.Errorf("%q", u)
		}
	}
}

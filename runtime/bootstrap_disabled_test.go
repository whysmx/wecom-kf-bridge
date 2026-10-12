package runtime

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/whysmx/wecom-kf-bridge/state"
)

// #18: config disabled:true must deactivate an already-inserted binding on
// restart (bootstrap used to return the SQLite row and ignore Disabled).
func TestConfigDisabledDeactivatesExistingBinding(t *testing.T) {
	c := gatewayConfig(t)
	g, err := Build(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if gettoken(t, g.Handler, "bridge_a", gatewayEnv["G_VSECRET"]) != 0 {
		t.Fatal("precondition: token")
	}
	b, err := g.Store.Binding(context.Background(), "b1")
	if err != nil || !b.Active {
		t.Fatalf("precondition active: %+v %v", b, err)
	}
	g.Close()

	var logBuf bytes.Buffer
	logger := NewLogger(&logBuf)
	c.Bindings[0].Disabled = true
	g, err = Build(context.Background(), c, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	b, err = g.Store.Binding(context.Background(), "b1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Active {
		t.Fatalf("disabled:true left binding active: %+v", b)
	}
	if gettoken(t, g.Handler, "bridge_a", gatewayEnv["G_VSECRET"]) == 0 {
		t.Fatal("disabled binding still issued a token")
	}
	if !strings.Contains(logBuf.String(), "binding_config_disabled") {
		t.Fatalf("missing binding_config_disabled log: %s", logBuf.String())
	}
}

// #18: disabled:false on a config-listed binding re-enables after a console
// disable (config wins for listed bindings on start).
func TestConfigEnabledReactivatesExistingBinding(t *testing.T) {
	c := gatewayConfig(t)
	g, err := Build(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := g.Store.Binding(context.Background(), "b1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Store.UpdateBinding(context.Background(), b.ID, b.Revision, func(x *state.Binding) {
		x.Active = false
	}); err != nil {
		t.Fatal(err)
	}
	g.Close()

	c.Bindings[0].Disabled = false
	g, err = Build(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	b, err = g.Store.Binding(context.Background(), "b1")
	if err != nil || !b.Active {
		t.Fatalf("disabled:false did not re-enable: %+v %v", b, err)
	}
	if gettoken(t, g.Handler, "bridge_a", gatewayEnv["G_VSECRET"]) != 0 {
		t.Fatal("re-enabled binding refused token")
	}
}

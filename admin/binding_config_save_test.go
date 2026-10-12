package admin

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestBindingConfigSaveRequiresStepUp(t *testing.T) {
	e := newEnv(t)
	cl := e.login()
	w := cl.post("/admin/bindings/b1/config-save", url.Values{
		"enterprise_id":   {"e1"},
		"open_kfid":       {"wk1"},
		"project_id":      {"p2"},
		"virtual_corp_id": {"bridge_b1"},
		"agent_id":        {"1000002"},
		"callback_url":    {"http://cc.internal:9000/wecom"},
	})
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "二次认证") {
		t.Fatalf("expected step-up, got %d %q", w.Code, w.Body.String())
	}
	e.rt.mu.Lock()
	n := len(e.rt.savedBindings)
	e.rt.mu.Unlock()
	if n != 0 {
		t.Fatal("must not save without step-up")
	}
}

func TestBindingConfigSaveSuccessAndAudit(t *testing.T) {
	e := newEnv(t)
	cl := e.login()
	cl.stepUp()
	w := cl.post("/admin/bindings/b1/config-save", url.Values{
		"enterprise_id":   {"e1"},
		"open_kfid":       {"wk1"},
		"project_id":      {"p-new"},
		"virtual_corp_id": {"bridge_new"},
		"agent_id":        {"1000099"},
		"callback_url":    {"http://cc.internal:9000/hot"},
		"disabled":        {"1"},
	})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("save %d %s", w.Code, w.Body.String())
	}
	e.rt.mu.Lock()
	saved := append([]BindingConfigPatch(nil), e.rt.savedBindings...)
	ids := append([]string(nil), e.rt.savedBindingID...)
	e.rt.mu.Unlock()
	if len(saved) != 1 || ids[0] != "b1" {
		t.Fatalf("saved %#v ids %#v", saved, ids)
	}
	p := saved[0]
	if p.ProjectID != "p-new" || p.AgentID != "1000099" || !p.Disabled || p.VirtualCorpID != "bridge_new" {
		t.Fatalf("patch %+v", p)
	}
	flash := cl.flash()
	if !strings.Contains(flash, "热加载") {
		t.Fatalf("flash %q", flash)
	}
	body := cl.get("/admin/audit").Body.String()
	if !strings.Contains(body, "config_save") || !strings.Contains(body, "p-new") {
		t.Fatalf("audit missing: %s", body)
	}
}

func TestBindingConfigSaveRejectsIdentityChange(t *testing.T) {
	e := newEnv(t)
	cl := e.login()
	cl.stepUp()
	w := cl.post("/admin/bindings/b1/config-save", url.Values{
		"enterprise_id":   {"e-other"},
		"open_kfid":       {"wk1"},
		"project_id":      {"p1"},
		"virtual_corp_id": {"bridge_b1"},
		"agent_id":        {"1000002"},
		"callback_url":    {"http://cc.internal:9000/wecom"},
	})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "不可修改") {
		t.Fatalf("got %d %q", w.Code, w.Body.String())
	}
	e.rt.mu.Lock()
	n := len(e.rt.savedBindings)
	e.rt.mu.Unlock()
	if n != 0 {
		t.Fatal("must not persist identity change")
	}
}

func TestBindingConfigSaveRejectsBadURLAndFormat(t *testing.T) {
	e := newEnv(t)
	cl := e.login()
	cl.stepUp()
	w := cl.post("/admin/bindings/b1/config-save", url.Values{
		"enterprise_id":   {"e1"},
		"open_kfid":       {"wk1"},
		"project_id":      {"p1"},
		"virtual_corp_id": {"bridge_b1"},
		"agent_id":        {"1000002"},
		"callback_url":    {"http://evil.example/x"},
	})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "allowlist") {
		t.Fatalf("got %d %q", w.Code, w.Body.String())
	}
	w = cl.post("/admin/bindings/b1/config-save", url.Values{
		"enterprise_id":   {"e1"},
		"open_kfid":       {"wk1"},
		"project_id":      {"bad id!"},
		"virtual_corp_id": {"bridge_b1"},
		"agent_id":        {"1000002"},
		"callback_url":    {"http://cc.internal:9000/wecom"},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad project: %d %q", w.Code, w.Body.String())
	}
}

func TestBindingConfigSaveRuntimeErrorAudited(t *testing.T) {
	e := newEnv(t)
	e.rt.bindingSaveErr = errors.New("disk full")
	cl := e.login()
	cl.stepUp()
	w := cl.post("/admin/bindings/b1/config-save", url.Values{
		"enterprise_id":   {"e1"},
		"open_kfid":       {"wk1"},
		"project_id":      {"p1"},
		"virtual_corp_id": {"bridge_b1"},
		"agent_id":        {"1000002"},
		"callback_url":    {"http://cc.internal:9000/wecom"},
	})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "保存失败") {
		t.Fatalf("got %d %q", w.Code, w.Body.String())
	}
	body := cl.get("/admin/audit").Body.String()
	if !strings.Contains(body, "failed") || !strings.Contains(body, "disk full") {
		t.Fatalf("audit missing failed: %s", body)
	}
}

func TestBindingsPageShowsConfigForm(t *testing.T) {
	e := newEnv(t)
	cl := e.login()
	body := cl.get("/admin/bindings").Body.String()
	if !strings.Contains(body, "/admin/bindings/b1/config-save") {
		t.Fatal(body)
	}
	if !strings.Contains(body, "B1_SECRET") || !strings.Contains(body, "virtual_corp_id") {
		t.Fatal("env names / editable fields missing")
	}
	// SQLite-only binding "other" is other enterprise — not shown.
	if strings.Contains(body, "/admin/bindings/other/config-save") {
		t.Fatal("other enterprise must not appear")
	}
}

func TestBindingConfigSaveUnknownConfigBinding(t *testing.T) {
	e := newEnv(t)
	e.rt.mu.Lock()
	e.rt.configBindings = nil
	e.rt.mu.Unlock()
	cl := e.login()
	cl.stepUp()
	w := cl.post("/admin/bindings/b1/config-save", url.Values{
		"enterprise_id":   {"e1"},
		"open_kfid":       {"wk1"},
		"project_id":      {"p1"},
		"virtual_corp_id": {"bridge_b1"},
		"agent_id":        {"1000002"},
		"callback_url":    {"http://cc.internal:9000/wecom"},
	})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "config.json") {
		t.Fatalf("got %d %q", w.Code, w.Body.String())
	}
}

package admin

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestSettingsSaveRequiresStepUp(t *testing.T) {
	e := newEnv(t)
	cl := e.login()
	w := cl.post("/admin/settings/save", url.Values{
		"service_state_map": {`{"1":"AI_ELIGIBLE"}`},
		"public_base_url":   {"https://bridge.example.com"},
	})
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "二次认证") {
		t.Fatalf("expected step-up, got %d %q", w.Code, w.Body.String())
	}
	e.rt.mu.Lock()
	n := len(e.rt.saved)
	e.rt.mu.Unlock()
	if n != 0 {
		t.Fatal("must not save without step-up")
	}
}

func TestSettingsSaveSuccessAndAudit(t *testing.T) {
	e := newEnv(t)
	cl := e.login()
	cl.stepUp()
	w := cl.post("/admin/settings/save", url.Values{
		"treat_48002_as_ai_eligible": {"1"},
		"service_state_map":          {`{"1":"AI_ELIGIBLE","2":"WAITING_HUMAN"}`},
		"public_base_url":            {"https://bridge.example.com"},
	})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("save %d %s", w.Code, w.Body.String())
	}
	e.rt.mu.Lock()
	saved := append([]EditableSettings(nil), e.rt.saved...)
	e.rt.mu.Unlock()
	if len(saved) != 1 {
		t.Fatalf("saved %d", len(saved))
	}
	s := saved[0]
	if !s.Treat48002AsAIEligible || s.PublicBaseURL != "https://bridge.example.com" || s.ServiceStateMap["2"] != "WAITING_HUMAN" {
		t.Fatalf("saved %+v", s)
	}
	flash := cl.flash()
	if !strings.Contains(flash, "热加载") || !strings.Contains(flash, "treat_48002") {
		t.Fatalf("flash %q", flash)
	}
	body := cl.get("/admin/audit").Body.String()
	if !strings.Contains(body, "save") || !strings.Contains(body, "joint-debug") {
		t.Fatalf("audit missing: %s", body)
	}
}

func TestSettingsSaveValidationFailure(t *testing.T) {
	e := newEnv(t)
	cl := e.login()
	cl.stepUp()
	w := cl.post("/admin/settings/save", url.Values{
		"service_state_map": {"not-json"},
		"public_base_url":   {""},
	})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "service_state_map") {
		t.Fatalf("got %d %q", w.Code, w.Body.String())
	}
	e.rt.mu.Lock()
	n := len(e.rt.saved)
	e.rt.mu.Unlock()
	if n != 0 {
		t.Fatal("must not save invalid JSON")
	}
}

func TestSettingsSaveRuntimeErrorAudited(t *testing.T) {
	e := newEnv(t)
	e.rt.saveErr = errors.New("public_base_url invalid")
	cl := e.login()
	cl.stepUp()
	w := cl.post("/admin/settings/save", url.Values{
		"service_state_map": {`{"1":"AI_ELIGIBLE"}`},
		"public_base_url":   {"ftp://bad"},
	})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "保存失败") {
		t.Fatalf("got %d %q", w.Code, w.Body.String())
	}
	body := cl.get("/admin/audit").Body.String()
	if !strings.Contains(body, "failed") {
		t.Fatalf("audit missing failed: %s", body)
	}
}

func TestSettingsPageShowsEditableForm(t *testing.T) {
	e := newEnv(t)
	cl := e.login()
	body := cl.get("/admin/settings").Body.String()
	if !strings.Contains(body, "/admin/settings/save") || !strings.Contains(body, "treat_48002_as_ai_eligible") {
		t.Fatal(body)
	}
	if !strings.Contains(body, "WECOM_SECRET") {
		t.Fatal("secrets still shown as env names")
	}
}

func TestApplyEditableSettingsSkipsBadKeys(t *testing.T) {
	e := newEnv(t)
	e.c.ApplyEditableSettings(EditableSettings{
		Treat48002AsAIEligible: false,
		ServiceStateMap:        map[string]string{"1": "AI_ELIGIBLE", "bad": "HUMAN"},
		PublicBaseURL:          "https://pub.example",
	})
	if e.c.cfg.GatewayBaseURL != "https://pub.example" {
		t.Fatal(e.c.cfg.GatewayBaseURL)
	}
	if _, ok := e.c.cfg.ServiceStateMap[1]; !ok {
		t.Fatal("missing mapped state")
	}
	if len(e.c.cfg.ServiceStateMap) != 1 {
		t.Fatalf("bad key should be skipped: %#v", e.c.cfg.ServiceStateMap)
	}
}

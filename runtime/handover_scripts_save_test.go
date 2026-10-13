package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/whysmx/wecom-kf-bridge/admin"
)

func TestSaveSettingsPersistsHandoverScripts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	raw, err := os.ReadFile("../config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConfigPath = path
	g := &Gateway{liveCfg: cfg, configPath: path, adapter: &WeComAdapter{states: map[int]string{}}}
	rt := consoleRuntime{g: g}
	s := admin.EditableSettings{
		Treat48002AsAIEligible: cfg.WeCom.Treat48002AsAIEligible,
		ServiceStateMap: map[string]string{
			"0": "AI_ELIGIBLE", "1": "AI_ELIGIBLE",
			"2": "WAITING_HUMAN", "3": "HUMAN", "4": "CLOSED",
		},
		PublicBaseURL:       cfg.Server.PublicBaseURL,
		HandoverQueueScript: "排队自定义",
		HandoverHumanScript: "接入自定义",
		HandoverEndScript:   "结束自定义",
	}
	if err := rt.SaveSettings(nil, s); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.WeCom.HandoverScripts.Queue != "排队自定义" || got.WeCom.HandoverScripts.Human != "接入自定义" || got.WeCom.HandoverScripts.End != "结束自定义" {
		t.Fatalf("scripts=%+v", got.WeCom.HandoverScripts)
	}
	if got.WeCom.HandoverWelcome != "排队自定义" {
		t.Fatalf("legacy welcome not aligned: %q", got.WeCom.HandoverWelcome)
	}
	ed := rt.EditableSettings()
	if ed.HandoverQueueScript != "排队自定义" {
		b, _ := json.Marshal(ed)
		t.Fatalf("editable=%s", b)
	}
}

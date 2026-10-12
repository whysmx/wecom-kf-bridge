package runtime

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whysmx/wecom-kf-bridge/admin"
)

func TestWriteConfigFileAndValidateEditable(t *testing.T) {
	if err := ValidateEditableSettings(false, map[string]string{"1": "AI_ELIGIBLE"}, "https://bridge.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateEditableSettings(true, map[string]string{"x": "AI_ELIGIBLE"}, ""); err == nil {
		t.Fatal("expected invalid map key")
	}
	if err := ValidateEditableSettings(false, nil, ""); err == nil {
		t.Fatal("expected empty map reject")
	}
	if err := ValidateEditableSettings(false, map[string]string{}, ""); err == nil {
		t.Fatal("expected empty object reject")
	}
	if err := ValidateEditableSettings(false, map[string]string{"1": "AI_ELIGIBLE"}, "ftp://x"); err == nil {
		t.Fatal("expected bad url")
	}
	if err := ValidateEditableSettings(false, map[string]string{"1": "AI_ELIGIBLE"}, "https://x/cgi-bin"); err == nil {
		t.Fatal("expected cgi-bin reject")
	}

	dir := t.TempDir()
	raw, err := os.ReadFile(filepath.Join("..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.WeCom.Treat48002AsAIEligible = true
	cfg.Server.PublicBaseURL = "https://bridge.example.com"
	cfg.WeCom.ServiceStateMap = map[string]string{"1": "AI_ELIGIBLE", "2": "WAITING_HUMAN", "3": "HUMAN"}
	if err := WriteConfigFile(path, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.WeCom.Treat48002AsAIEligible || got.Server.PublicBaseURL != "https://bridge.example.com" {
		t.Fatalf("%+v / %q", got.WeCom, got.Server.PublicBaseURL)
	}
	var probe map[string]any
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatal(err)
	}
	ents := probe["enterprises"].([]any)
	e0 := ents[0].(map[string]any)
	if _, ok := e0["secret"]; ok {
		t.Fatal("secret plaintext must not appear")
	}
	if e0["secret_env"] == nil || e0["secret_env"] == "" {
		t.Fatal("secret_env missing")
	}
}

func TestApplyEditableSettingsHotReload(t *testing.T) {
	g := &Gateway{
		liveCfg: Config{},
		adapter: &WeComAdapter{states: map[int]string{1: "AI_ELIGIBLE"}},
	}
	g.liveCfg.WeCom.ServiceStateMap = map[string]string{"1": "AI_ELIGIBLE"}
	s := admin.EditableSettings{
		Treat48002AsAIEligible: true,
		ServiceStateMap:        map[string]string{"1": "AI_ELIGIBLE", "2": "WAITING_HUMAN"},
		PublicBaseURL:          "https://pub.example",
	}
	g.applyEditableSettings(s)
	if !g.adapter.treat48002Enabled() {
		t.Fatal("treat not hot-applied")
	}
	if st, ok := g.adapter.mapServiceState(2); !ok || st != "WAITING_HUMAN" {
		t.Fatalf("states %#v", g.adapter.states)
	}
	g.cfgMu.RLock()
	defer g.cfgMu.RUnlock()
	if g.liveCfg.Server.PublicBaseURL != "https://pub.example" {
		t.Fatal(g.liveCfg.Server.PublicBaseURL)
	}
}

func TestSaveSettingsPersistsAndHotApplies(t *testing.T) {
	dir := t.TempDir()
	raw, err := os.ReadFile(filepath.Join("..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConfigPath = path
	g := &Gateway{liveCfg: cfg, configPath: path, adapter: &WeComAdapter{states: map[int]string{}}}
	for k, v := range cfg.WeCom.ServiceStateMap {
		// leave empty; SaveSettings will fill
		_ = k
		_ = v
	}
	rt := consoleRuntime{g: g, cfg: cfg}
	s := admin.EditableSettings{
		Treat48002AsAIEligible: true,
		ServiceStateMap:        map[string]string{"1": "AI_ELIGIBLE", "2": "WAITING_HUMAN", "3": "HUMAN"},
		PublicBaseURL:          "https://hot.example",
	}
	if err := rt.SaveSettings(nil, s); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.WeCom.Treat48002AsAIEligible || got.Server.PublicBaseURL != "https://hot.example" {
		t.Fatalf("file not updated: %+v", got.Server)
	}
	ed := rt.EditableSettings()
	if !ed.Treat48002AsAIEligible || ed.PublicBaseURL != "https://hot.example" {
		t.Fatalf("live %#v", ed)
	}
	if !g.adapter.treat48002Enabled() {
		t.Fatal("adapter not hot-applied")
	}
}

func TestWriteConfigFileEmptyPath(t *testing.T) {
	if err := WriteConfigFile("", Config{}); err == nil {
		t.Fatal("expected empty path error")
	}
	if err := WriteConfigFile("   ", Config{}); err == nil {
		t.Fatal("expected blank path error")
	}
}

func TestSaveSettingsRejectsBadInputAndMissingPath(t *testing.T) {
	rt := consoleRuntime{g: &Gateway{liveCfg: Config{}, configPath: ""}}
	err := rt.SaveSettings(nil, admin.EditableSettings{ServiceStateMap: map[string]string{"x": "AI_ELIGIBLE"}})
	if err == nil {
		t.Fatal("expected validation error")
	}
	err = rt.SaveSettings(nil, admin.EditableSettings{ServiceStateMap: map[string]string{"1": "AI_ELIGIBLE"}})
	if err == nil || !strings.Contains(err.Error(), "config path") {
		t.Fatalf("expected missing path, got %v", err)
	}
	ed := rt.EditableSettings()
	if ed.Treat48002AsAIEligible {
		t.Fatal("default should be false")
	}
}

func TestSaveSettingsLoadFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing.json")
	rt := consoleRuntime{g: &Gateway{configPath: path, liveCfg: Config{}}}
	err := rt.SaveSettings(nil, admin.EditableSettings{ServiceStateMap: map[string]string{"1": "AI_ELIGIBLE"}})
	if err == nil {
		t.Fatal("expected load failure")
	}
}

func TestWriteConfigFileIOErrors(t *testing.T) {
	dir := t.TempDir()
	if err := WriteConfigFile(filepath.Join(dir, "missing-parent", "c.json"), Config{}); err == nil {
		t.Fatal("expected write failure for missing parent")
	}
	destDir := filepath.Join(dir, "as-dir")
	if err := os.Mkdir(destDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destDir, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteConfigFile(destDir, Config{}); err == nil {
		t.Fatal("expected failure when destination is a non-empty directory")
	}
}

func TestApplyEditableSettingsNilAdapterAndConsole(t *testing.T) {
	g := &Gateway{liveCfg: Config{}}
	g.applyEditableSettings(admin.EditableSettings{
		ServiceStateMap: map[string]string{"1": "AI_ELIGIBLE"},
		PublicBaseURL:   "https://x.example",
	})
	g.cfgMu.RLock()
	defer g.cfgMu.RUnlock()
	if g.liveCfg.Server.PublicBaseURL != "https://x.example" {
		t.Fatal(g.liveCfg.Server.PublicBaseURL)
	}
}

func TestWriteConfigFileReplacesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	raw, err := os.ReadFile(filepath.Join("..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Server.PublicBaseURL = "https://replaced.example"
	if err := WriteConfigFile(path, loaded); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Server.PublicBaseURL != "https://replaced.example" {
		t.Fatalf("got %q", got.Server.PublicBaseURL)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("tmp should be gone after successful replace")
	}
}

// TestWriteConfigFileWindowsStyleOverwrite stubs rename to fail when the
// destination already exists (Windows behaviour), then expects Remove+Rename.
func TestWriteConfigFileWindowsStyleOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	raw, err := os.ReadFile(filepath.Join("..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Server.PublicBaseURL = "https://win-replace.example"

	orig := renameFile
	t.Cleanup(func() { renameFile = orig })
	calls := 0
	renameFile = func(oldpath, newpath string) error {
		calls++
		if _, err := os.Stat(newpath); err == nil {
			// Simulate Windows: cannot rename over existing destination.
			return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: errWindowsExist}
		}
		return orig(oldpath, newpath)
	}

	if err := WriteConfigFile(path, loaded); err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Fatalf("expected fallback rename path, calls=%d", calls)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Server.PublicBaseURL != "https://win-replace.example" {
		t.Fatalf("got %q", got.Server.PublicBaseURL)
	}
}

var errWindowsExist = errors.New("The file exists")

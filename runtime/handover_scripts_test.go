package runtime

import "testing"

func TestNormalizeHandoverScriptsDefaults(t *testing.T) {
	got := NormalizeHandoverScripts(HandoverScripts{}, "")
	if got.Queue != defaultHandoverQueueScript {
		t.Fatalf("queue default: %q", got.Queue)
	}
	if got.Human != defaultHandoverHumanScript {
		t.Fatalf("human default: %q", got.Human)
	}
	if got.End != defaultHandoverEndScript {
		t.Fatalf("end default: %q", got.End)
	}
}

func TestNormalizeHandoverScriptsLegacyWelcome(t *testing.T) {
	got := NormalizeHandoverScripts(HandoverScripts{}, "旧欢迎语")
	if got.Queue != "旧欢迎语" {
		t.Fatalf("legacy welcome → queue: %q", got.Queue)
	}
}

func TestNormalizeHandoverScriptsExplicitQueueWins(t *testing.T) {
	got := NormalizeHandoverScripts(HandoverScripts{Queue: "排队句", Human: "接入句", End: "结束句"}, "旧欢迎语")
	if got.Queue != "排队句" || got.Human != "接入句" || got.End != "结束句" {
		t.Fatalf("got %+v", got)
	}
}

func TestHandoverConfigWelcomeUsesScriptsQueue(t *testing.T) {
	cfg := HandoverConfig{Welcome: "legacy", Scripts: HandoverScripts{Queue: "from-scripts"}}
	if cfg.welcome() != "from-scripts" {
		t.Fatalf("welcome=%q", cfg.welcome())
	}
	cfg2 := HandoverConfig{Welcome: "legacy-only"}
	if cfg2.welcome() != "legacy-only" {
		t.Fatalf("legacy welcome=%q", cfg2.welcome())
	}
	cfg3 := HandoverConfig{}
	if cfg3.welcome() != defaultHandoverQueueScript {
		t.Fatalf("default welcome=%q", cfg3.welcome())
	}
}

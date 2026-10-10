package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	bridgeRuntime "github.com/whysmx/wecom-kf-bridge/runtime"
)

type stringWriter struct{ b *strings.Builder }

func (w *stringWriter) Write(p []byte) (int, error) { return w.b.Write(p) }

func writeConfig(t *testing.T, listen string) string {
	t.Helper()
	t.Setenv("M_MASTER", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)))
	p := t.TempDir() + "/c.json"
	cfg := `{"server":{"public_listen":"` + listen + `"},"storage":{"database":"` + t.TempDir() + `/d.db"},"security":{"master_key_env":"M_MASTER"},"wecom":{"api_base_url":"https://qyapi.weixin.qq.com","customer_origins":[3]}}`
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEnv(t *testing.T) {
	t.Setenv("BRIDGE_TEST_ENV", "")
	if env("BRIDGE_TEST_ENV", "fallback") != "fallback" {
		t.Fatal("fallback")
	}
	t.Setenv("BRIDGE_TEST_ENV", "value")
	if env("BRIDGE_TEST_ENV", "fallback") != "value" {
		t.Fatal("value")
	}
}

func TestRunContextMissingConfigIsFatal(t *testing.T) {
	var b strings.Builder
	if got := runContext(context.Background(), "/no/such/config.json", &stringWriter{&b}); got != 1 || !strings.Contains(b.String(), "config_invalid") {
		t.Fatalf("runContext=%d %s", got, b.String())
	}
}

func TestRunContextStartsAndStops(t *testing.T) {
	// Stop only once initialisation is done, so a slow CI machine cannot
	// turn this into an init-timeout test.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	orig := newApp
	defer func() { newApp = orig }()
	newApp = func(c bridgeRuntime.AppConfig) (*bridgeRuntime.App, error) {
		a, err := orig(c)
		go func() { time.Sleep(200 * time.Millisecond); cancel() }()
		return a, err
	}
	var b strings.Builder
	if got := runContext(ctx, writeConfig(t, "127.0.0.1:0"), &stringWriter{&b}); got != 0 {
		t.Fatalf("runContext=%d %s", got, b.String())
	}
}

func TestRunContextListenError(t *testing.T) {
	var b strings.Builder
	if got := runContext(context.Background(), writeConfig(t, "bad address"), &stringWriter{&b}); got != 1 {
		t.Fatalf("runContext=%d", got)
	}
}

func TestRunContextBuildAndAppErrors(t *testing.T) {
	ob, oa := build, newApp
	defer func() { build, newApp = ob, oa }()
	p := writeConfig(t, "127.0.0.1:0")
	build = func(context.Context, bridgeRuntime.Config, bridgeRuntime.Logger) (*bridgeRuntime.Gateway, error) {
		return nil, errors.New("x")
	}
	var b strings.Builder
	if runContext(context.Background(), p, &stringWriter{&b}) != 1 {
		t.Fatal("build error")
	}
	build = ob
	newApp = func(bridgeRuntime.AppConfig) (*bridgeRuntime.App, error) { return nil, errors.New("init") }
	if runContext(context.Background(), p, &stringWriter{&b}) != 1 {
		t.Fatal("app error")
	}
}

func TestRunAndMain(t *testing.T) {
	origRun, origExit := runContextFn, exit
	defer func() { runContextFn, exit = origRun, origExit }()
	runContextFn = func(context.Context, string, interface{ Write([]byte) (int, error) }) int { return 7 }
	if run() != 7 {
		t.Fatal("run")
	}
	called := -1
	exit = func(c int) { called = c }
	main()
	if called != 7 {
		t.Fatal("exit")
	}
}

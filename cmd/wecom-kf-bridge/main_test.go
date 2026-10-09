package main

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"

	bridgeRuntime "github.com/whysmx/wecom-kf-bridge/runtime"
)

func TestEnv(t *testing.T) {
	os.Unsetenv("BRIDGE_TEST_ENV")
	if env("BRIDGE_TEST_ENV", "fallback") != "fallback" {
		t.Fatal("fallback")
	}
	os.Setenv("BRIDGE_TEST_ENV", "value")
	defer os.Unsetenv("BRIDGE_TEST_ENV")
	if env("BRIDGE_TEST_ENV", "fallback") != "value" {
		t.Fatal("value")
	}
}

func TestRunContextInvalidAddress(t *testing.T) {
	var b strings.Builder
	if got := runContext(context.Background(), "bad address", &stringWriter{b: &b}); got != 1 {
		t.Fatalf("runContext=%d", got)
	}
}

func TestRunContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var b strings.Builder
	if got := runContext(ctx, "127.0.0.1:0", &stringWriter{b: &b}); got != 0 {
		t.Fatalf("runContext cancelled=%d", got)
	}
}

func TestRunContextListenError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var b strings.Builder
	if got := runContext(context.Background(), ln.Addr().String(), &stringWriter{b: &b}); got != 1 {
		t.Fatalf("runContext listen error=%d", got)
	}
}

func TestRunContextInitError(t *testing.T) {
	orig := newApp
	defer func() { newApp = orig }()
	newApp = func(bridgeRuntime.AppConfig) (*bridgeRuntime.App, error) { return nil, errors.New("init failed") }
	var b strings.Builder
	if got := runContext(context.Background(), ":0", &stringWriter{b: &b}); got != 1 {
		t.Fatalf("runContext init error=%d", got)
	}
}

func TestRunAndMain(t *testing.T) {
	origRun := runContextFn
	origExit := exit
	defer func() { runContextFn, exit = origRun, origExit }()
	runContextFn = func(context.Context, string, interface{ Write([]byte) (int, error) }) int { return 7 }
	if got := run(); got != 7 {
		t.Fatalf("run=%d", got)
	}
	called := -1
	exit = func(code int) { called = code }
	main()
	if called != 7 {
		t.Fatalf("exit code=%d", called)
	}
}

type stringWriter struct{ b *strings.Builder }

func (w *stringWriter) Write(p []byte) (int, error) { return w.b.Write(p) }

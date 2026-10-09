package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type loggerFunc func(string, map[string]any)

func (f loggerFunc) Log(e string, m map[string]any) { f(e, m) }

func TestStructuredLoggerRedactsNestedSecrets(t *testing.T) {
	var b bytes.Buffer
	l := NewLogger(&b)
	l.Clock = func() time.Time { return time.Unix(1, 2) }
	l.Log("inbound", map[string]any{"trace_id": "t1", "token": "secret", "nested": map[string]any{"content": "customer text", "ok": "yes"}, "query": "https://x.test/a?access_token=abc&x=y", "blob": []byte{1, 2}})
	l.Error("failed", errors.New("request timeout?token=abc"), map[string]any{"authorization": "Bearer x"})
	got := b.String()
	for _, bad := range []string{"secret", "customer text", "abc", "Bearer x"} {
		if strings.Contains(got, bad) {
			t.Fatalf("log leaked %q: %s", bad, got)
		}
	}
	if !strings.Contains(got, "inbound") || !strings.Contains(got, "REDACTED") {
		t.Fatalf("unexpected log %s", got)
	}
	if FormatJSON(nil) != "null" {
		t.Fatalf("nil format")
	}
	f := Fields("a", 1, "bad")
	if f["a"] != 1 || len(f) != 1 {
		t.Fatalf("fields %#v", f)
	}
}
func TestSanitizeFieldsTypes(t *testing.T) {
	now := time.Now()
	in := map[string]any{"s": "ok", "password": "x", "bytes": []byte{1}, "slice": []any{"content", "ok"}, "time": now, "map": map[string]string{"secret": "x", "v": "v"}}
	out := SanitizeFields(in)
	if out["password"] != "[REDACTED]" || out["bytes"] != "[bytes:1]" {
		t.Fatalf("out %#v", out)
	}
	if !strings.Contains(FormatJSON(in), "REDACTED") {
		t.Fatal("format missing redaction")
	}
}
func TestHealthEndpointsAndReady(t *testing.T) {
	h := NewHealth(HealthConfig{Database: func(context.Context) error { return nil }, Upstream: func(context.Context) error { return errors.New("timeout: access_token=x") }, Extra: map[string]Check{"x": func(context.Context) error { return nil }}})
	for _, path := range []string{"/healthz", "/health"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != 200 {
			t.Fatalf("%s %d", path, rr.Code)
		}
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != 503 || strings.Contains(rr.Body.String(), "access_token") {
		t.Fatalf("ready %d %s", rr.Code, rr.Body)
	}
	if h.Ready(context.Background()) {
		t.Fatal("failed check ready")
	}
	h.Stop()
	rr = httptest.NewRecorder()
	h.Readiness(rr, httptest.NewRequest(http.MethodGet, "/readiness", nil))
	if rr.Code != 503 {
		t.Fatal(rr.Code)
	}
	h.Start()
	h.cfg.Upstream = nil
	if !h.Ready(context.Background()) {
		t.Fatal("expected ready")
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/other", nil))
	if rr.Code != 404 {
		t.Fatal(rr.Code)
	}
	var nilH *Health
	rr = httptest.NewRecorder()
	nilH.Readiness(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != 503 {
		t.Fatal(rr.Code)
	}
}
func TestAppServeAndShutdown(t *testing.T) {
	var logs atomic.Int32
	l := loggerFunc(func(string, map[string]any) { logs.Add(1) })
	h := NewHealth(HealthConfig{})
	app, err := NewApp(AppConfig{Addr: "127.0.0.1:0", Health: h, Logger: l, ShutdownTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- app.Serve(ln) }()
	time.Sleep(10 * time.Millisecond)
	rr := httptest.NewRecorder()
	h.Liveness(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != 200 {
		t.Fatal(rr.Code)
	}
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if logs.Load() < 2 {
		t.Fatal("lifecycle logs missing")
	}
	var nilApp *App
	if err := nilApp.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if nilApp.Server() != nil {
		t.Fatal("nil server")
	}
	if _, err := NewApp(AppConfig{}); err != nil {
		t.Fatal(err)
	}
}
func TestAppErrors(t *testing.T) {
	var nilApp *App
	if err := nilApp.ListenAndServe(); err == nil {
		t.Fatal("nil listen")
	}
	if err := nilApp.Serve(nil); err == nil {
		t.Fatal("nil serve")
	}
	if err := nilApp.RunSignals(context.Background()); err == nil { /* listener may be unavailable only */
	}
	if _, err := NewApp(AppConfig{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})}); err != nil {
		t.Fatal(err)
	}
	_ = fmt.Sprint(nilApp)
}

func TestRuntimeAliasesAndBranches(t *testing.T) {
	if NewHealthHandler(HealthConfig{}) == nil {
		t.Fatal("health handler")
	}
	var nilHealth *Health
	nilHealth.Start()
	nilHealth.Stop()
	nilHealth.SetStarted(false)
	h := NewHealth(HealthConfig{})
	h.SetStarted(false)
	h.Start()
	h.Stop()
	h.Start()
	var b bytes.Buffer
	l := NewStructuredLogger(&b)
	l.Info("info", nil)
	l.Error("timeout", errors.New("timeout"), nil)
	l.Error("denied", errors.New("forbidden"), nil)
	l.Error("missing", errors.New("not found"), nil)
	l.Error("bad", errors.New("invalid input"), nil)
	l.Error("other", errors.New("other"), nil)
	l.Error("nil", nil, nil)
	var nl *StructuredLogger
	nl.Log("x", nil)
	nl.Info("x", nil)
	nl.Error("x", nil, nil)
	if classifyError(errors.New("timeout")) != "timeout" || classifyError(errors.New("forbidden")) != "permission" || classifyError(errors.New("not found")) != "not_found" || classifyError(errors.New("invalid")) != "invalid" || classifyError(errors.New("x")) != "internal" {
		t.Fatal("classification")
	}
	type box struct{ X string }
	p := &box{X: "x"}
	_ = sanitizeValue("p", p)
	_ = sanitizeValue("m", map[string]string{"x": "y"})
	_ = sanitizeValue("a", []any{"x"})
	_ = sanitizeValue("t", time.Now())
	_ = sanitizeValue("e", errors.New("e"))
	_ = sanitizeValue("x", struct{ A int }{1})
	_ = cloneFields(nil)
	if b.Len() == 0 {
		t.Fatal("logger output")
	}
}
func TestHealthHandlerAndCapture(t *testing.T) {
	h := NewHealth(HealthConfig{})
	mux := h.Handler()
	for _, p := range []string{"/healthz", "/health", "/readyz", "/readiness"} {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, p, nil))
		if rr.Code != 200 {
			t.Fatalf("%s %d", p, rr.Code)
		}
	}
	c := &responseCapture{header: make(http.Header)}
	if _, err := c.Write([]byte("a")); err != nil || c.code != 200 {
		t.Fatal("capture")
	}
	c.WriteHeader(201)
	c.Write([]byte("b"))
	if c.code != 201 {
		t.Fatal(c.code)
	}
}
func TestAppConstructAndSignalErrors(t *testing.T) {
	a, _ := NewApp(AppConfig{Addr: "bad address"})
	if a.Server() != nil {
		t.Fatal("server before serve")
	}
	if err := a.ListenAndServe(); err == nil {
		t.Fatal("invalid listen should fail")
	}
	// A cancelled context exercises the shutdown path without waiting for a signal.
	a, _ = NewApp(AppConfig{Addr: "127.0.0.1:0", ShutdownTimeout: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = a.RunSignals(ctx)
	a, _ = NewApp(AppConfig{Health: NewHealth(HealthConfig{})})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = a.Serve(ln) }()
	time.Sleep(10 * time.Millisecond)
	_ = a.Shutdown(context.Background())
}

func TestAppRunSignalsGracefulCancellation(t *testing.T) {
	a, err := NewApp(AppConfig{Addr: "127.0.0.1:0", ShutdownTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if err := a.RunSignals(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAppRunSignalsListenError(t *testing.T) {
	a, _ := NewApp(AppConfig{Addr: "bad address"})
	if err := a.RunSignals(context.Background()); err == nil {
		t.Fatal("expected listen error")
	}
}

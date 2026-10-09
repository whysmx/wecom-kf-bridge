package main

import (
	"context"
	"os"
	stdruntime "runtime"

	bridgeRuntime "github.com/whysmx/wecom-kf-bridge/runtime"
)

var exit = os.Exit
var runContextFn = runContext
var newApp = bridgeRuntime.NewApp

func main() { exit(run()) }
func run() int {
	return runContextFn(context.Background(), env("LISTEN_ADDR", ":8080"), os.Stdout)
}

func runContext(ctx context.Context, addr string, out interface{ Write([]byte) (int, error) }) int {
	logger := bridgeRuntime.NewLogger(out)
	health := bridgeRuntime.NewHealth(bridgeRuntime.HealthConfig{Logger: logger})
	app, err := newApp(bridgeRuntime.AppConfig{Addr: addr, Health: health, Logger: logger})
	if err != nil {
		logger.Error("runtime_init_failed", err, nil)
		return 1
	}
	if err := app.RunSignals(ctx); err != nil {
		logger.Error("runtime_failed", err, map[string]any{"go_version": stdruntime.Version()})
		return 1
	}
	return 0
}
func env(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

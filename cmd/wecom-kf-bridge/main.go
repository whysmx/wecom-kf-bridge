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
var build = bridgeRuntime.Build

func main() { exit(run()) }
func run() int {
	return runContextFn(context.Background(), env("WECOM_KF_BRIDGE_CONFIG", "./config.json"), os.Stdout)
}

// runContext loads the config, opens SQLite, mounts the compatible API,
// the tenant callback router and health, and starts the workers. A missing
// or invalid config is fatal: the gateway never starts health-only.
func runContext(ctx context.Context, configPath string, out interface{ Write([]byte) (int, error) }) int {
	logger := bridgeRuntime.NewLogger(out)
	cfg, err := bridgeRuntime.LoadConfig(configPath)
	if err != nil {
		logger.Error("config_invalid", err, nil)
		return 1
	}
	gw, err := build(ctx, cfg, logger)
	if err != nil {
		logger.Error("runtime_init_failed", err, nil)
		return 1
	}
	defer gw.Close()
	app, err := newApp(bridgeRuntime.AppConfig{Addr: cfg.Server.PublicListen, Handler: gw.Handler, Health: gw.Health, Logger: logger, Workers: gw.Workers, ShutdownTimeout: bridgeRuntime.ShutdownGrace(cfg), AdminAddr: cfg.Admin.Listen, AdminHandler: gw.Admin})
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

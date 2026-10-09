package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

type AppConfig struct {
	Addr            string
	Handler         http.Handler
	Health          *Health
	Logger          Logger
	ShutdownTimeout time.Duration
}
type App struct {
	cfg       AppConfig
	mu        sync.RWMutex
	server    *http.Server
	ready     chan struct{}
	readyOnce sync.Once
}

func NewApp(cfg AppConfig) (*App, error) {
	if strings.TrimSpace(cfg.Addr) == "" {
		cfg.Addr = ":8080"
	}
	if cfg.Handler == nil {
		mux := http.NewServeMux()
		if cfg.Health != nil {
			mux.Handle("/", cfg.Health)
		} else {
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
		}
		cfg.Handler = mux
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = 10 * time.Second
	}
	return &App{cfg: cfg, ready: make(chan struct{})}, nil
}
func (a *App) Server() *http.Server {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.server
}
func (a *App) ListenAndServe() error {
	if a == nil {
		return errors.New("runtime: nil app")
	}
	ln, err := net.Listen("tcp", a.cfg.Addr)
	if err != nil {
		return err
	}
	return a.Serve(ln)
}
func (a *App) Serve(ln net.Listener) error {
	if a == nil || ln == nil {
		return errors.New("runtime: nil app/listener")
	}
	server := &http.Server{Handler: a.cfg.Handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	a.mu.Lock()
	a.server = server
	a.mu.Unlock()
	a.readyOnce.Do(func() { close(a.ready) })
	if a.cfg.Health != nil {
		a.cfg.Health.Start()
	}
	if a.cfg.Logger != nil {
		a.cfg.Logger.Log("runtime_started", map[string]any{"addr": ln.Addr().String()})
	}
	err := server.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
func (a *App) Shutdown(ctx context.Context) error {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	server := a.server
	a.mu.RUnlock()
	if server == nil {
		return nil
	}
	if a.cfg.Health != nil {
		a.cfg.Health.Stop()
	}
	err := server.Shutdown(ctx)
	if a.cfg.Logger != nil {
		a.cfg.Logger.Log("runtime_stopped", map[string]any{"error": err})
	}
	return err
}
func (a *App) RunSignals(ctx context.Context) error {
	if a == nil {
		return errors.New("runtime: nil app")
	}
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- a.ListenAndServe() }()
	// Wait until Serve has installed the server before handling cancellation;
	// otherwise a stop arriving during net.Listen could leave an untracked
	// listener running.
	select {
	case err := <-errc:
		return err
	case <-a.ready:
	case <-ctx.Done():
		select {
		case err := <-errc:
			return err
		case <-a.ready:
		}
	}
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		stop, c := context.WithTimeout(context.Background(), a.cfg.ShutdownTimeout)
		defer c()
		if err := a.Shutdown(stop); err != nil {
			return fmt.Errorf("runtime shutdown: %w", err)
		}
		return nil
	}
}

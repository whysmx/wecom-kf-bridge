package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Check is an optional readiness dependency. A nil check is ignored.
type Check func(context.Context) error

type HealthConfig struct {
	Database Check
	Upstream Check
	Callback Check
	Extra    map[string]Check
	Timeout  time.Duration
	Logger   Logger
}

type Health struct {
	cfg      HealthConfig
	mu       sync.RWMutex
	started  bool
	stopping bool
}

func NewHealth(cfg HealthConfig) *Health {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Second
	}
	return &Health{cfg: cfg, started: true}
}
func NewHealthHandler(cfg HealthConfig) *Health { return NewHealth(cfg) }
func (h *Health) Start() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.started = true
	h.stopping = false
	h.mu.Unlock()
}
func (h *Health) Stop() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.stopping = true
	h.mu.Unlock()
}
func (h *Health) SetStarted(v bool) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.started = v
	h.mu.Unlock()
}
func (h *Health) Liveness(w http.ResponseWriter, r *http.Request)  { h.write(w, r, false) }
func (h *Health) Readiness(w http.ResponseWriter, r *http.Request) { h.write(w, r, true) }
func (h *Health) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", h.Liveness)
	mux.HandleFunc("/health", h.Liveness)
	mux.HandleFunc("/readyz", h.Readiness)
	mux.HandleFunc("/readiness", h.Readiness)
	return mux
}
func (h *Health) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz", "/health":
		h.Liveness(w, r)
	case "/readyz", "/readiness":
		h.Readiness(w, r)
	default:
		http.NotFound(w, r)
	}
}

type checkResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}
type healthResponse struct {
	Status string                 `json:"status"`
	Checks map[string]checkResult `json:"checks,omitempty"`
	Time   string                 `json:"time"`
}

func (h *Health) write(w http.ResponseWriter, r *http.Request, ready bool) {
	if h == nil {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	h.mu.RLock()
	started, stopping := h.started, h.stopping
	cfg := h.cfg
	h.mu.RUnlock()
	out := healthResponse{Status: "ok", Time: time.Now().UTC().Format(time.RFC3339Nano)}
	if ready {
		out.Checks = map[string]checkResult{}
		if !started || stopping {
			out.Checks["process"] = checkResult{Error: "not_started"}
		} else {
			out.Checks["process"] = checkResult{OK: true}
		}
		checks := map[string]Check{"database": cfg.Database, "upstream": cfg.Upstream, "callback": cfg.Callback}
		for k, c := range cfg.Extra {
			checks[k] = c
		}
		ctx := r.Context()
		cancel := func() {}
		if cfg.Timeout > 0 {
			ctx, cancel = context.WithTimeout(ctx, cfg.Timeout)
		}
		defer cancel()
		for name, c := range checks {
			if c == nil {
				continue
			}
			err := c(ctx)
			if err != nil {
				out.Checks[name] = checkResult{Error: classifyError(err)}
				if cfg.Logger != nil {
					cfg.Logger.Log("health_check_failed", map[string]any{"check": name, "error": err})
				}
			} else {
				out.Checks[name] = checkResult{OK: true}
			}
		}
		for _, v := range out.Checks {
			if !v.OK {
				out.Status = "not_ready"
				break
			}
		}
	}
	code := http.StatusOK
	if out.Status != "ok" {
		code = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(out)
}

// Ready reports readiness without exposing dependency error text to callers.
func (h *Health) Ready(ctx context.Context) bool {
	if h == nil {
		return false
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://runtime/readyz", nil)
	rr := &responseCapture{header: make(http.Header)}
	h.Readiness(rr, req)
	return rr.code == http.StatusOK
}

type responseCapture struct {
	header http.Header
	code   int
	body   strings.Builder
}

func (r *responseCapture) Header() http.Header { return r.header }
func (r *responseCapture) WriteHeader(c int)   { r.code = c }
func (r *responseCapture) Write(p []byte) (int, error) {
	if r.code == 0 {
		r.code = 200
	}
	return r.body.Write(p)
}

var _ http.Handler = (*Health)(nil)

package wecom

import (
	"net/http"
	"regexp"
	"strings"
	"sync"
)

// RouterPrefix is the public callback path from docs/04 §3.
const RouterPrefix = "/webhooks/wechat-kf/"

var tenantKeyRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Router selects exactly one tenant's callback Webhook (its own Token, AES
// key and receiver) from the {tenant_key} path segment. It never tries the
// keys of other tenants, so a callback can only be verified by the tenant
// it was addressed to.
type Router struct {
	mu    sync.RWMutex
	hooks map[string]*Webhook
}

func NewRouter() *Router { return &Router{hooks: map[string]*Webhook{}} }

// Handle registers the Webhook for tenantKey.
func (r *Router) Handle(tenantKey string, w *Webhook) error {
	if !tenantKeyRE.MatchString(tenantKey) || w == nil {
		return ErrInvalidArgument
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.hooks[tenantKey]; dup {
		return ErrInvalidArgument
	}
	r.hooks[tenantKey] = w
	return nil
}

func (r *Router) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	rw.Header().Set("Cache-Control", "no-store")
	key, ok := strings.CutPrefix(req.URL.Path, RouterPrefix)
	if !ok || !tenantKeyRE.MatchString(key) {
		http.NotFound(rw, req)
		return
	}
	r.mu.RLock()
	w := r.hooks[key]
	r.mu.RUnlock()
	if w == nil {
		http.NotFound(rw, req)
		return
	}
	w.ServeHTTP(rw, req)
}

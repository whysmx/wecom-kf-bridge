package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	bridge "github.com/whysmx/wecom-kf-bridge"
	"github.com/whysmx/wecom-kf-bridge/state"
	"github.com/whysmx/wecom-kf-bridge/wecom"
)

// Gateway is the fully wired process: SQLite store, cc-connect compatible
// API, tenant-routed WeChat callbacks, health and background workers.
type Gateway struct {
	Store    *state.Store
	Server   *bridge.Server
	Router   *wecom.Router
	Handler  http.Handler
	Health   *Health
	Sync     *SyncWorker
	Delivery *DeliveryWorker
	Workers  []Worker
	// Admin is the console handler (nil unless admin.listen is set).
	Admin http.Handler
	lock  *InstanceLock

	adapter *WeComAdapter
	vmu     sync.Mutex
	vbs     map[string]bridge.Binding
	kfmu    sync.RWMutex
	scopes  map[string]string // enterprise|open_kfid -> scope
}

func (g *Gateway) Close() error {
	if g == nil || g.Store == nil {
		return nil
	}
	err := g.Store.Close()
	_ = g.lock.Release()
	return err
}

// Build wires every component from cfg. Secrets come from the environment.
func Build(ctx context.Context, cfg Config, logger Logger) (*Gateway, error) {
	if logger == nil {
		logger = nopRuntimeLogger{}
	}
	// #37: Build is exported, so it applies defaults and validation itself.
	cfg.defaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.WeCom.AllowInsecureHTTP && strings.HasPrefix(cfg.WeCom.APIBaseURL, "http://") {
		logger.Log("SECURITY_WARNING_insecure_wecom_api", map[string]any{"api_base_url": cfg.WeCom.APIBaseURL, "warning": "corp secret and access_token are sent in cleartext; use only with local fakes"})
	}
	mk, err := secretEnv(cfg.Security.MasterKeyEnv)
	if err != nil {
		return nil, err
	}
	key, err := state.ParseMasterKey(mk)
	if err != nil {
		return nil, err
	}
	lock, err := AcquireInstanceLock(cfg.Storage.Database)
	if err != nil {
		return nil, err
	}
	st, err := state.OpenWithOptions(cfg.Storage.Database, state.Options{MasterKey: key, Policy: state.SendPolicy{Window: time.Duration(cfg.SendPolicy.WindowHours) * time.Hour, MaxSends: cfg.SendPolicy.MaxSends}})
	if err != nil {
		lock.Release()
		return nil, err
	}
	g := &Gateway{Store: st, lock: lock}
	if err := g.wire(ctx, cfg, logger); err != nil {
		g.Close()
		return nil, err
	}
	return g, nil
}

func (g *Gateway) wire(ctx context.Context, cfg Config, logger Logger) error {
	st := g.Store
	if in, out, err := st.RecoverInFlight(ctx); err != nil {
		return err
	} else if in+out > 0 {
		logger.Log("recovered_in_flight", map[string]any{"inbox": in, "outbox": out})
	}
	adapter := &WeComAdapter{clients: map[string]*wecom.Client{}, tokens: map[string]*tokenCache{}, routes: map[string]bindingRoute{}, states: map[int]string{}, store: st, now: time.Now}
	for k, v := range cfg.WeCom.ServiceStateMap {
		n, _ := strconv.Atoi(k)
		adapter.states[n] = v
	}
	g.Router = wecom.NewRouter()
	g.Sync = &SyncWorker{Store: st, Scopes: map[string]scopeInfo{}, Origins: wecom.OriginPolicy{CustomerOrigins: cfg.WeCom.CustomerOrigins}, Interval: time.Duration(cfg.Workers.SyncIntervalSeconds) * time.Second, MaxPages: cfg.Workers.MaxSyncPages, MaxConc: cfg.Workers.MaxConcurrency, Logger: logger}
	g.adapter, g.vbs, g.scopes = adapter, map[string]bridge.Binding{}, map[string]string{}
	for _, e := range cfg.Enterprises {
		secret, err := secretEnv(e.SecretEnv)
		if err != nil {
			return err
		}
		if err := st.PutEnterprise(ctx, state.Enterprise{ID: e.ID, TenantID: e.TenantKey, CorpID: e.CorpID, CredentialRef: "env:" + e.SecretEnv}); err != nil {
			return err
		}
		cl := wecom.NewClient(cfg.WeCom.APIBaseURL, e.CorpID, secret)
		cl.Logger = logger
		adapter.clients[e.ID] = cl
		adapter.tokens[e.ID] = &tokenCache{client: cl, now: time.Now}
	}
	g.Delivery = &DeliveryWorker{Store: st, CallbackURL: map[string]string{}, AgentID: map[string]string{}, HTTP: callbackHTTPClient(cfg), Interval: time.Duration(cfg.Workers.DeliveryIntervalMillis) * time.Millisecond, MaxAttempts: cfg.Workers.MaxDeliveryAttempts, MaxConc: cfg.Workers.MaxConcurrency, Logger: logger}
	configured := map[string]bool{}
	for _, b := range cfg.Bindings {
		configured[b.ID] = true
		rev := b.CredentialRevision
		if rev <= 0 {
			rev = 1
		}
		sb, err := g.bootstrapBinding(ctx, state.Binding{ID: b.ID, EnterpriseID: b.EnterpriseID, OpenKfID: b.OpenKfID, ProjectID: b.ProjectID, CallbackURL: b.CallbackURL, Active: !b.Disabled, Revision: rev})
		if err != nil {
			return err
		}
		vb, err := g.bindingCredentials(ctx, b, sb)
		if err != nil {
			return err
		}
		if err := g.register(ctx, sb, vb); err != nil {
			return err
		}
	}
	// Bindings created in the console exist only in SQLite.
	all, err := st.Bindings(ctx)
	if err != nil {
		return err
	}
	for _, b := range all {
		if configured[b.ID] || adapter.clients[b.EnterpriseID] == nil {
			continue
		}
		vb := g.withStoredCredentials(ctx, bridge.Binding{ID: b.ID})
		if vb.CorpID == "" {
			logger.Log("binding_without_credentials", map[string]any{"binding_id": b.ID})
			continue
		}
		if err := g.register(ctx, b, vb); err != nil {
			return err
		}
	}
	var vbs []bridge.Binding
	for _, vb := range g.vbs {
		vbs = append(vbs, vb)
	}
	for _, e := range cfg.Enterprises {
		tok, err := secretEnv(e.CallbackTokenEnv)
		if err != nil {
			return err
		}
		aes, err := secretEnv(e.CallbackAESKeyEnv)
		if err != nil {
			return err
		}
		wh, err := wecom.NewWebhook(tok, aes, e.CorpID)
		if err != nil {
			return fmt.Errorf("enterprise %s: %w", e.ID, err)
		}
		wh.Logger = logger
		entID := e.ID
		wh.OnNotification = func(ctx context.Context, n wecom.Notification) error {
			return g.onNotification(ctx, entID, n)
		}
		if err := g.Router.Handle(e.TenantKey, wh); err != nil {
			return fmt.Errorf("enterprise %s: invalid tenant_key", e.ID)
		}
	}
	srv, err := bridge.NewServer(bridge.Config{Bindings: vbs, Store: st, Adapter: adapter, TokenTTL: time.Duration(cfg.Security.TokenTTLSeconds) * time.Second, MaxTokensPerBinding: cfg.Security.MaxTokensPerBind, Logger: logger})
	if err != nil {
		return err
	}
	g.Server = srv
	g.Delivery.Server = srv
	g.Sync.OnSynced = g.Delivery.Kick
	g.Health = NewHealth(HealthConfig{Logger: logger, Database: func(ctx context.Context) error { return st.DB().PingContext(ctx) }})
	mux := http.NewServeMux()
	mux.Handle("/cgi-bin/", srv.Handler())
	mux.Handle(wecom.RouterPrefix, g.Router)
	mux.Handle("/", g.Health)
	g.Handler = mux
	g.Workers = []Worker{g.Sync, g.Delivery}
	if cfg.Admin.Listen != "" {
		h, err := g.buildAdmin(cfg)
		if err != nil {
			return err
		}
		g.Admin = h
	}
	return nil
}

// onNotification runs inside the callback request: a short transaction
// stores the sealed pull token / pending flag, then the worker is woken.
// It does not wait for sync or AI (docs/04 §3).
func (g *Gateway) onNotification(ctx context.Context, entID string, n wecom.Notification) error {
	if n.Event != "kf_msg_or_event" {
		return nil
	}
	g.kfmu.RLock()
	scope, ok := g.scopes[entID+"|"+n.OpenKfID]
	g.kfmu.RUnlock()
	if !ok {
		return errors.New("runtime: notification for unbound open_kfid")
	}
	var err error
	if n.Token != "" {
		err = g.Store.SaveSyncToken(ctx, scope, n.Token, time.Now().Add(10*time.Minute))
	} else {
		err = g.Store.MarkSyncPending(ctx, scope, true)
	}
	if err != nil {
		return err
	}
	g.Sync.Wake(scope)
	return nil
}

type nopRuntimeLogger struct{}

func (nopRuntimeLogger) Log(string, map[string]any) {}

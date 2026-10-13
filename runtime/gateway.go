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
	"github.com/whysmx/wecom-kf-bridge/admin"
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

	// liveCfg / configPath support admin settings save + hot-reload (#25).
	cfgMu      sync.RWMutex
	liveCfg    Config
	configPath string
	console    *admin.Console
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
		logger.Log("SECURITY_WARNING_insecure_wecom_api", map[string]any{"api_base_url": wecom.RedactURL(cfg.WeCom.APIBaseURL), "warning": "corp secret and access_token are sent in cleartext; use only with local fakes"})
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
	g := &Gateway{Store: st, lock: lock, liveCfg: cfg, configPath: cfg.ConfigPath}
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
	adapter := &WeComAdapter{clients: map[string]*wecom.Client{}, tokens: map[string]*tokenCache{}, routes: map[string]bindingRoute{}, states: map[int]string{}, store: st, now: time.Now, treat48002AsAI: cfg.WeCom.Treat48002AsAIEligible}
	adapter.logger = logger
	for k, v := range cfg.WeCom.ServiceStateMap {
		n, _ := strconv.Atoi(k)
		adapter.states[n] = v
	}
	warnServiceStateMap(cfg.WeCom.ServiceStateMap, logger)
	if cfg.WeCom.Treat48002AsAIEligible {
		logger.Log("WARNING_treat_48002_as_ai_eligible", map[string]any{"effect": "kf/service_state/get errcode 48002 will be treated as AI_ELIGIBLE for send; joint-debug only, never enable in production"})
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
		sb, err := g.bootstrapBinding(ctx, state.Binding{ID: b.ID, EnterpriseID: b.EnterpriseID, OpenKfID: b.OpenKfID, ProjectID: b.ProjectID, CallbackURL: b.CallbackURL, Active: !b.Disabled, Revision: rev}, logger)
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
	// OfficialState uses ServiceState as-is. A 48002 bypass returns
	// AI_ELIGIBLE_48002_BYPASS (not AI_ELIGIBLE), so auto-recover will not
	// clear fences when the joint-debug switch is on.
	g.Delivery.OfficialState = func(ctx context.Context, bindingID string, c state.Customer) (string, error) {
		return adapter.ServiceState(ctx, bridge.Binding{ID: bindingID}, bridge.Customer{ExternalUserID: c.ExternalUserID})
	}
	// Inbound force-pullback mirrors protocol.forceTransToAI (trans→1 + re-read).
	g.Delivery.ForceTransToAI = func(ctx context.Context, bindingID string, c state.Customer, fromMapped string) error {
		b := bridge.Binding{ID: bindingID}
		if vb, ok := g.vbs[bindingID]; ok {
			b = vb
		}
		cust := bridge.Customer{ID: c.ID, BindingID: c.BindingID, ExternalUserID: c.ExternalUserID, UserID: c.UID}
		logger.Log("handover_force_trans_ai", map[string]any{
			"binding_id": bindingID, "customer_id": c.ID, "from_mapped": fromMapped,
			"external_userid_prefix": bridge.TruncateID(c.ExternalUserID, 8), "target": bridge.OfficialStateAI,
			"path": "inbound",
		})
		if _, err := adapter.TransServiceState(ctx, b, cust, bridge.OfficialStateAI); err != nil {
			logger.Log("handover_force_trans_ai_failed", map[string]any{
				"binding_id": bindingID, "customer_id": c.ID, "from_mapped": fromMapped,
				"external_userid_prefix": bridge.TruncateID(c.ExternalUserID, 8), "error": err.Error(), "path": "inbound",
			})
			return err
		}
		got, err := adapter.ServiceState(ctx, b, cust)
		if err != nil {
			logger.Log("handover_force_trans_ai_reread_failed", map[string]any{
				"binding_id": bindingID, "customer_id": c.ID, "error": err.Error(), "path": "inbound",
			})
			return err
		}
		if got != state.CustomerAIEligible && got != state.CustomerAIEligibleBypass48002 {
			logger.Log("handover_force_trans_ai_not_confirmed", map[string]any{
				"binding_id": bindingID, "customer_id": c.ID, "got": got, "from_mapped": fromMapped, "path": "inbound",
			})
			return fmt.Errorf("force trans not confirmed as AI: %s", got)
		}
		logger.Log("handover_force_trans_ai_ok", map[string]any{
			"binding_id": bindingID, "customer_id": c.ID, "from_mapped": fromMapped, "path": "inbound",
		})
		return nil
	}
	hcfg := HandoverConfig{Phrase: cfg.WeCom.HandoverPhrase, Welcome: cfg.WeCom.HandoverWelcome}
	g.Delivery.HandoverPhrase = hcfg.phrase()
	g.Delivery.OnHandoverPhrase = func(ctx context.Context, c state.Customer, m state.InboxMessage) error {
		vb, ok := g.vbs[m.BindingID]
		if !ok {
			vb = bridge.Binding{ID: m.BindingID}
		}
		return RequestHumanHandover(ctx, st, adapter, vb, c, hcfg, "phrase_handover")
	}
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

// warnServiceStateMap flags a service_state_map the send path cannot work
// with: replies need some state mapped to AI_ELIGIBLE (every unmapped value
// is treated as UNKNOWN and held), and console handover/recover need
// WAITING_HUMAN and AI_ELIGIBLE targets.
func warnServiceStateMap(m map[string]string, logger Logger) {
	have := map[string]bool{}
	for _, v := range m {
		have[v] = true
	}
	var missing []string
	for _, need := range []string{state.CustomerAIEligible, state.CustomerWaitingHuman, state.CustomerHuman} {
		if !have[need] {
			missing = append(missing, need)
		}
	}
	if len(m) == 0 || len(missing) > 0 {
		logger.Log("WARNING_service_state_map_incomplete", map[string]any{"entries": len(m), "missing": missing, "effect": "unmapped official service_state values are UNKNOWN: replies are held (70006); without AI_ELIGIBLE no reply can be sent"})
	}
}

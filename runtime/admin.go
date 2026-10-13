package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	bridge "github.com/whysmx/wecom-kf-bridge"
	"github.com/whysmx/wecom-kf-bridge/admin"
	"github.com/whysmx/wecom-kf-bridge/state"
	"github.com/whysmx/wecom-kf-bridge/wecom"
)

// bootstrapBinding inserts a configured binding the first time. When the
// row already exists, identity (enterprise_id / open_kfid) stays immutable
// (#24); mutable fields from config — active (!disabled), project_id,
// callback_url — are applied and revision bumps when anything changes (#18).
// disabled:true → Active=false is the kill switch (logged). disabled:false
// also writes Active=true, so a console disable of a config-listed binding
// is overwritten on the next start (config wins for listed bindings).
func (g *Gateway) bootstrapBinding(ctx context.Context, b state.Binding, logger Logger) (state.Binding, error) {
	if logger == nil {
		logger = nopRuntimeLogger{}
	}
	cur, err := g.Store.Binding(ctx, b.ID)
	switch {
	case err == nil:
		if cur.EnterpriseID != b.EnterpriseID || cur.OpenKfID != b.OpenKfID {
			return cur, fmt.Errorf("binding %s: %w", b.ID, state.ErrIdentityChanged)
		}
		if cur.ProjectID == b.ProjectID && cur.CallbackURL == b.CallbackURL && cur.Active == b.Active {
			return cur, nil
		}
		wasActive := cur.Active
		nb, err := g.Store.UpdateBinding(ctx, cur.ID, cur.Revision, func(x *state.Binding) {
			x.ProjectID = b.ProjectID
			x.CallbackURL = b.CallbackURL
			x.Active = b.Active
		})
		if err != nil {
			return cur, err
		}
		if wasActive && !nb.Active {
			logger.Log("binding_config_disabled", map[string]any{
				"binding_id": nb.ID,
				"revision":   nb.Revision,
			})
		}
		return nb, nil
	case errors.Is(err, state.ErrNotFound):
		if err := g.Store.PutBinding(ctx, b); err != nil {
			return b, err
		}
		return g.Store.Binding(ctx, b.ID)
	default:
		return b, err
	}
}

// bindingCredentials resolves a configured binding's virtual credentials
// (#41): once SQLite holds them, the *_env variables are no longer needed
// (and ignored if set). On first initialisation they are read from the
// environment and persisted sealed, marked as already exported.
func (g *Gateway) bindingCredentials(ctx context.Context, b BindingConfig, sb state.Binding) (bridge.Binding, error) {
	stored := g.withStoredCredentials(ctx, bridge.Binding{ID: b.ID, AgentID: b.AgentID})
	if stored.CorpID != "" {
		return stored, nil
	}
	cr := admin.Credentials{CorpID: b.VirtualCorpID, AgentID: b.AgentID}
	for _, f := range []struct {
		env string
		dst *string
	}{{b.VirtualSecretEnv, &cr.Secret}, {b.CallbackTokenEnv, &cr.Token}, {b.CallbackAESKeyEnv, &cr.AESKey}} {
		v, err := secretEnv(f.env)
		if err != nil {
			return bridge.Binding{}, fmt.Errorf("binding %s: no credentials stored in the database yet, first start needs the environment: %w", b.ID, err)
		}
		*f.dst = v
	}
	if _, err := wecom.DecodeAESKey(cr.AESKey); err != nil {
		return bridge.Binding{}, fmt.Errorf("binding %s: %w", b.ID, err)
	}
	raw, _ := json.Marshal(cr)
	if err := g.Store.SaveBindingSecret(ctx, b.ID, string(raw), sb.Revision); err != nil {
		return bridge.Binding{}, err
	}
	if _, err := g.Store.ExportBindingSecret(ctx, b.ID); err != nil {
		return bridge.Binding{}, err
	}
	return bridge.Binding{ID: b.ID, CorpID: cr.CorpID, CorpSecret: cr.Secret, AgentID: cr.AgentID, CallbackToken: cr.Token, CallbackAESKey: cr.AESKey}, nil
}

// withStoredCredentials overlays console-generated virtual credentials.
func (g *Gateway) withStoredCredentials(ctx context.Context, vb bridge.Binding) bridge.Binding {
	raw, _, err := g.Store.BindingSecret(ctx, vb.ID)
	if err != nil {
		return vb
	}
	var cr admin.Credentials
	if json.Unmarshal([]byte(raw), &cr) != nil || cr.CorpID == "" {
		return vb
	}
	vb.CorpID, vb.CorpSecret, vb.CallbackToken, vb.CallbackAESKey = cr.CorpID, cr.Secret, cr.Token, cr.AESKey
	if cr.AgentID != "" {
		vb.AgentID = cr.AgentID
	}
	return vb
}

// register wires a binding into sync, routing and delivery.
func (g *Gateway) register(ctx context.Context, b state.Binding, vb bridge.Binding) error {
	scope := "scope:" + b.EnterpriseID + ":" + b.OpenKfID
	if err := g.Store.EnsureScope(ctx, scope, b.ID); err != nil {
		return err
	}
	vb.Enabled, vb.Revision = b.Active, b.Revision
	g.kfmu.Lock()
	g.scopes[b.EnterpriseID+"|"+b.OpenKfID] = scope
	g.kfmu.Unlock()
	g.Sync.SetScope(scope, scopeInfo{enterpriseID: b.EnterpriseID, bindingID: b.ID, openKfID: b.OpenKfID, client: g.adapter.clients[b.EnterpriseID], tokens: g.adapter.tokens[b.EnterpriseID]})
	g.adapter.setRoute(b.ID, bindingRoute{enterpriseID: b.EnterpriseID, openKfID: b.OpenKfID})
	g.Delivery.SetTarget(b.ID, b.CallbackURL, vb.AgentID)
	g.vmu.Lock()
	g.vbs[b.ID] = vb
	g.vmu.Unlock()
	return nil
}

// consoleRuntime is the admin.Runtime view of the running gateway.
type consoleRuntime struct {
	g   *Gateway
	cfg Config
}

func (r consoleRuntime) ApplyBinding(ctx context.Context, b state.Binding, cr *admin.Credentials) error {
	r.g.vmu.Lock()
	vb := r.g.vbs[b.ID]
	r.g.vmu.Unlock()
	vb.ID = b.ID
	if cr != nil {
		vb.CorpID, vb.CorpSecret, vb.CallbackToken, vb.CallbackAESKey = cr.CorpID, cr.Secret, cr.Token, cr.AESKey
		if cr.AgentID != "" {
			vb.AgentID = cr.AgentID
		}
	}
	if err := r.g.register(ctx, b, vb); err != nil {
		return err
	}
	vb.Enabled, vb.Revision = b.Active, b.Revision
	return r.g.Server.ApplyBinding(vb)
}

func (r consoleRuntime) CheckURL(raw string) error { return r.cfg.CheckCallbackURL(raw) }

func (r consoleRuntime) get(ctx context.Context, raw string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := r.g.Delivery.HTTP.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, strings.TrimSpace(string(body)), nil
}

// TestURL connects through the same per-target SSRF client used for
// delivery (redirects are not followed).
func (r consoleRuntime) TestURL(ctx context.Context, raw string) error {
	code, _, err := r.get(ctx, raw)
	if err != nil {
		return errors.New("连接失败")
	}
	if code >= 300 && code < 400 {
		return errors.New("拒绝重定向")
	}
	return nil
}

// Verify sends the WeCom URL-verification challenge to cc-connect.
func (r consoleRuntime) Verify(ctx context.Context, bindingID string) error {
	target, _ := r.g.Delivery.target(bindingID)
	if err := r.cfg.CheckCallbackURL(target); err != nil {
		return err
	}
	q, echo, err := r.g.Server.VerifyChallenge(bindingID)
	if err != nil {
		return err
	}
	sep := "?"
	if strings.Contains(target, "?") {
		sep = "&"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	code, body, err := r.get(ctx, target+sep+q.Encode())
	if err != nil {
		return errors.New("连接失败")
	}
	if code != http.StatusOK || body != echo {
		return fmt.Errorf("回显不匹配（HTTP %d）", code)
	}
	return nil
}

func (r consoleRuntime) Status(ctx context.Context) map[string]string {
	out := map[string]string{"gateway": "运行中", "sqlite": "正常", "sync_worker": "运行中", "delivery_worker": "运行中", "wecom_api": "未确认", "last_sync": "—"}
	var last int64
	if err := r.g.Store.DB().QueryRowContext(ctx, `SELECT COALESCE(MAX(last_success_at),0) FROM sync_scopes`).Scan(&last); err != nil {
		out["sqlite"] = "异常"
		return out
	}
	if last > 0 {
		t := time.Unix(0, last)
		out["last_sync"] = t.Format("2006-01-02 15:04:05")
		if time.Since(t) < 10*time.Minute {
			out["wecom_api"] = "正常"
		}
	}
	return out
}

// consoleKF calls the official /cgi-bin/kf/* API with the enterprise's
// real token (kept server-side in the token cache).
type consoleKF struct {
	client *wecom.Client
	tokens *tokenCache
}

func (k consoleKF) ListAccounts(ctx context.Context, offset, limit int) ([]wecom.Account, error) {
	tok, err := k.tokens.get(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := k.client.ListAccounts(ctx, tok, wecom.AccountListRequest{Offset: offset, Limit: limit})
	return resp.Accounts, err
}

func (k consoleKF) AddAccount(ctx context.Context, name, mediaID string) (string, error) {
	tok, err := k.tokens.get(ctx)
	if err != nil {
		return "", err
	}
	resp, err := k.client.AddAccount(ctx, tok, wecom.AccountAddRequest{Name: name, MediaID: mediaID})
	return resp.OpenKfID, err
}

func (k consoleKF) UpdateAccount(ctx context.Context, id, name, mediaID string) error {
	tok, err := k.tokens.get(ctx)
	if err != nil {
		return err
	}
	_, err = k.client.UpdateAccount(ctx, tok, wecom.AccountUpdateRequest{OpenKfID: id, Name: name, MediaID: mediaID})
	return err
}

func (k consoleKF) DeleteAccount(ctx context.Context, id string) error {
	tok, err := k.tokens.get(ctx)
	if err != nil {
		return err
	}
	return k.client.DeleteAccount(ctx, tok, id)
}

func (k consoleKF) ContactURL(ctx context.Context, id, scene string) (string, error) {
	tok, err := k.tokens.get(ctx)
	if err != nil {
		return "", err
	}
	resp, err := k.client.AddContactWay(ctx, tok, map[string]any{"open_kfid": id, "scene": scene})
	return resp.URL, err
}

func (k consoleKF) TransServiceState(ctx context.Context, id, ext string, st int) (string, error) {
	tok, err := k.tokens.get(ctx)
	if err != nil {
		return "", err
	}
	resp, err := k.client.TransServiceState(ctx, tok, wecom.ServiceStateRequest{OpenKfID: id, ExternalUserID: ext, ServiceState: st})
	if err != nil {
		return "", err
	}
	return resp.MsgCode, nil
}
func (k consoleKF) SendMsgOnEvent(ctx context.Context, code, text string) error {
	code, text = strings.TrimSpace(code), strings.TrimSpace(text)
	if code == "" || text == "" {
		return nil
	}
	tok, err := k.tokens.get(ctx)
	if err != nil {
		return err
	}
	_, err = k.client.SendMsgOnEvent(ctx, tok, wecom.EventMsgRequest{Code: code, MsgType: "text", Text: &wecom.SendText{Content: text}})
	return err
}

func (k consoleKF) ServiceState(ctx context.Context, id, ext string) (int, error) {
	tok, err := k.tokens.get(ctx)
	if err != nil {
		return 0, err
	}
	resp, err := k.client.GetServiceState(ctx, tok, wecom.ServiceStateRequest{OpenKfID: id, ExternalUserID: ext})
	return resp.ServiceState, err
}

func (r consoleRuntime) EditableSettings() admin.EditableSettings {
	r.g.cfgMu.RLock()
	defer r.g.cfgMu.RUnlock()
	cfg := r.g.liveCfg
	m := map[string]string{}
	for k, v := range cfg.WeCom.ServiceStateMap {
		m[k] = v
	}
	scripts := NormalizeHandoverScripts(cfg.WeCom.HandoverScripts, cfg.WeCom.HandoverWelcome)
	return admin.EditableSettings{
		Treat48002AsAIEligible: cfg.WeCom.Treat48002AsAIEligible,
		ServiceStateMap:        m,
		PublicBaseURL:          cfg.Server.PublicBaseURL,
		HandoverQueueScript:    scripts.Queue,
		HandoverHumanScript:    scripts.Human,
		HandoverEndScript:      scripts.End,
	}
}

func (r consoleRuntime) SaveSettings(_ context.Context, s admin.EditableSettings) error {
	if err := ValidateEditableSettings(s.Treat48002AsAIEligible, s.ServiceStateMap, s.PublicBaseURL); err != nil {
		return err
	}
	path := r.g.configPath
	if path == "" {
		return errors.New("config path unknown; cannot persist settings")
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		return err
	}
	cfg.WeCom.Treat48002AsAIEligible = s.Treat48002AsAIEligible
	cfg.WeCom.ServiceStateMap = s.ServiceStateMap
	cfg.Server.PublicBaseURL = s.PublicBaseURL
	cfg.WeCom.HandoverScripts = HandoverScripts{
		Queue: strings.TrimSpace(s.HandoverQueueScript),
		Human: strings.TrimSpace(s.HandoverHumanScript),
		End:   strings.TrimSpace(s.HandoverEndScript),
	}
	// Keep legacy welcome aligned with queue so older readers stay consistent.
	if cfg.WeCom.HandoverScripts.Queue != "" {
		cfg.WeCom.HandoverWelcome = cfg.WeCom.HandoverScripts.Queue
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := WriteConfigFile(path, cfg); err != nil {
		return err
	}
	r.g.applyEditableSettings(s)
	return nil
}

func (g *Gateway) applyEditableSettings(s admin.EditableSettings) {
	states := map[int]string{}
	for k, v := range s.ServiceStateMap {
		n, _ := strconv.Atoi(k)
		states[n] = v
	}
	g.cfgMu.Lock()
	g.liveCfg.WeCom.Treat48002AsAIEligible = s.Treat48002AsAIEligible
	g.liveCfg.WeCom.ServiceStateMap = map[string]string{}
	for k, v := range s.ServiceStateMap {
		g.liveCfg.WeCom.ServiceStateMap[k] = v
	}
	g.liveCfg.Server.PublicBaseURL = s.PublicBaseURL
	g.liveCfg.WeCom.HandoverScripts = HandoverScripts{
		Queue: strings.TrimSpace(s.HandoverQueueScript),
		Human: strings.TrimSpace(s.HandoverHumanScript),
		End:   strings.TrimSpace(s.HandoverEndScript),
	}
	if g.liveCfg.WeCom.HandoverScripts.Queue != "" {
		g.liveCfg.WeCom.HandoverWelcome = g.liveCfg.WeCom.HandoverScripts.Queue
	}
	g.cfgMu.Unlock()
	if g.adapter != nil {
		g.adapter.setEditableWeCom(s.Treat48002AsAIEligible, states)
	}
	if g.console != nil {
		g.console.ApplyEditableSettings(s)
	}
}

func editableBindingFrom(b BindingConfig) admin.EditableBindingConfig {
	return admin.EditableBindingConfig{
		ID: b.ID, EnterpriseID: b.EnterpriseID, OpenKfID: b.OpenKfID,
		ProjectID: b.ProjectID, VirtualCorpID: b.VirtualCorpID, AgentID: b.AgentID,
		CallbackURL: b.CallbackURL, Disabled: b.Disabled,
		VirtualSecretEnv: b.VirtualSecretEnv, CallbackTokenEnv: b.CallbackTokenEnv,
		CallbackAESKeyEnv: b.CallbackAESKeyEnv, CredentialRevision: b.CredentialRevision,
	}
}

func (r consoleRuntime) ConfigBindings() []admin.EditableBindingConfig {
	r.g.cfgMu.RLock()
	defer r.g.cfgMu.RUnlock()
	out := make([]admin.EditableBindingConfig, 0, len(r.g.liveCfg.Bindings))
	for _, b := range r.g.liveCfg.Bindings {
		out = append(out, editableBindingFrom(b))
	}
	return out
}

func (r consoleRuntime) SaveBindingConfig(ctx context.Context, id string, patch admin.BindingConfigPatch) error {
	path := r.g.configPath
	if path == "" {
		return errors.New("config path unknown; cannot persist binding")
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		return err
	}
	idx := -1
	for i, b := range cfg.Bindings {
		if b.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("binding %s not in config.json", id)
	}
	cur := cfg.Bindings[idx]
	if patch.EnterpriseID != cur.EnterpriseID || patch.OpenKfID != cur.OpenKfID {
		return errors.New("enterprise_id/open_kfid 不可修改")
	}
	project := strings.TrimSpace(patch.ProjectID)
	vcorp := strings.TrimSpace(patch.VirtualCorpID)
	agent := strings.TrimSpace(patch.AgentID)
	cb := strings.TrimSpace(patch.CallbackURL)
	if project == "" || vcorp == "" || agent == "" || cb == "" {
		return errors.New("project_id、virtual_corp_id、agent_id、callback_url 不能为空")
	}
	cur.ProjectID = project
	cur.VirtualCorpID = vcorp
	cur.AgentID = agent
	cur.CallbackURL = cb
	cur.Disabled = patch.Disabled
	cfg.Bindings[idx] = cur
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := WriteConfigFile(path, cfg); err != nil {
		return err
	}
	return r.g.applyBindingConfig(ctx, cur)
}

// applyBindingConfig hot-reloads one config binding: liveCfg, SQLite via
// bootstrapBinding, then register/ApplyBinding for delivery + virtual API (#29).
// Sealed virtual corp_id stays as stored until rotate; agent_id/callback/project/disabled apply immediately.
func (g *Gateway) applyBindingConfig(ctx context.Context, bc BindingConfig) error {
	g.cfgMu.Lock()
	found := false
	for i := range g.liveCfg.Bindings {
		if g.liveCfg.Bindings[i].ID == bc.ID {
			g.liveCfg.Bindings[i] = bc
			found = true
			break
		}
	}
	if !found {
		g.liveCfg.Bindings = append(g.liveCfg.Bindings, bc)
	}
	g.cfgMu.Unlock()

	var logger Logger = nopRuntimeLogger{}
	if g.Sync != nil && g.Sync.Logger != nil {
		logger = g.Sync.Logger
	}
	sb, err := g.bootstrapBinding(ctx, state.Binding{
		ID: bc.ID, EnterpriseID: bc.EnterpriseID, OpenKfID: bc.OpenKfID,
		ProjectID: bc.ProjectID, CallbackURL: bc.CallbackURL, Active: !bc.Disabled,
	}, logger)
	if err != nil {
		return err
	}
	vb := g.withStoredCredentials(ctx, bridge.Binding{ID: bc.ID, AgentID: bc.AgentID})
	if vb.CorpID == "" {
		vb, err = g.bindingCredentials(ctx, bc, sb)
		if err != nil {
			return err
		}
	} else {
		vb.AgentID = bc.AgentID
	}
	if err := g.register(ctx, sb, vb); err != nil {
		return err
	}
	if g.Server == nil {
		return nil
	}
	vb.Enabled, vb.Revision = sb.Active, sb.Revision
	return g.Server.ApplyBinding(vb)
}

func (g *Gateway) buildAdmin(cfg Config) (http.Handler, error) {
	hash, err := secretEnv(cfg.Admin.PasswordHashEnv)
	if err != nil {
		return nil, err
	}
	e := cfg.Enterprises[0]
	origin := cfg.Admin.Origin
	if origin == "" {
		origin = "http://" + cfg.Admin.Listen
	}
	var targets []string
	for _, t := range cfg.Security.CallbackTargets {
		targets = append(targets, fmt.Sprintf("%s:%d %v", t.Host, t.Port, t.AllowedCIDRs))
	}
	settings := []admin.Setting{
		{Group: "企业", Name: "企业名称", Value: cfg.Admin.CompanyName},
		{Group: "企业", Name: "CorpID", Value: maskID(e.CorpID)},
		{Group: "企业", Name: "Secret 环境变量", Value: e.SecretEnv},
		{Group: "企业", Name: "回调 Token 环境变量", Value: e.CallbackTokenEnv},
		{Group: "企业", Name: "回调 AES 环境变量", Value: e.CallbackAESKeyEnv},
		{Group: "微信客服", Name: "API 地址", Value: wecom.RedactURL(cfg.WeCom.APIBaseURL)},
		{Group: "微信客服", Name: "customer origins", Value: fmt.Sprint(cfg.WeCom.CustomerOrigins)},
		{Group: "微信客服", Name: "service_state_map", Value: fmt.Sprint(cfg.WeCom.ServiceStateMap)},
		{Group: "微信客服", Name: "treat_48002_as_ai_eligible", Value: fmt.Sprint(cfg.WeCom.Treat48002AsAIEligible)},
		{Group: "服务", Name: "public_base_url", Value: cfg.Server.PublicBaseURL},
		{Group: "发送", Name: "窗口（小时）", Value: fmt.Sprint(cfg.SendPolicy.WindowHours)},
		{Group: "发送", Name: "发送预算", Value: fmt.Sprint(cfg.SendPolicy.MaxSends)},
		{Group: "发送", Name: "分块大小（字节）", Value: fmt.Sprint(bridge.DefaultChunkBytes)},
		{Group: "发送", Name: "最大投递次数", Value: fmt.Sprint(cfg.Workers.MaxDeliveryAttempts)},
		{Group: "Worker", Name: "同步周期（秒）", Value: fmt.Sprint(cfg.Workers.SyncIntervalSeconds)},
		{Group: "Worker", Name: "投递周期（毫秒）", Value: fmt.Sprint(cfg.Workers.DeliveryIntervalMillis)},
		{Group: "Worker", Name: "并发", Value: fmt.Sprint(cfg.Workers.MaxConcurrency)},
		{Group: "Worker", Name: "关闭宽限（秒）", Value: fmt.Sprint(cfg.Workers.ShutdownGraceSeconds)},
		{Group: "安全", Name: "callback allowlist", Value: strings.Join(targets, "; ")},
	}
	c, err := admin.New(admin.Config{
		Store: g.Store, KF: consoleKF{client: g.adapter.clients[e.ID], tokens: g.adapter.tokens[e.ID]}, Runtime: consoleRuntime{g: g, cfg: cfg},
		PasswordHash: []byte(hash), Origin: origin, EnterpriseID: e.ID, CompanyName: cfg.Admin.CompanyName, GatewayBaseURL: cfg.Server.PublicBaseURL, CorpID: e.CorpID, Settings: settings,
		InsecureCookie:  cfg.Admin.InsecureCookie,
		LanNoLogin:      cfg.Admin.LanNoLoginEnabled(),
		ServiceStateMap: g.adapter.states,
		HandoverWelcome: HandoverConfig{Welcome: cfg.WeCom.HandoverWelcome, Scripts: cfg.WeCom.HandoverScripts}.welcome(),
		SessionTTL:      time.Duration(cfg.Admin.SessionTTLMinutes) * time.Minute, MaxSessions: cfg.Admin.MaxSessions,
	})
	if err != nil {
		return nil, err
	}
	g.console = c
	return c.Handler(), nil
}

func maskID(v string) string {
	if len(v) <= 6 {
		return strings.Repeat("*", len(v))
	}
	return v[:3] + strings.Repeat("*", len(v)-5) + v[len(v)-2:]
}

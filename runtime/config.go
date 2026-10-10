package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the process configuration (docs/09 §3). It is JSON rather than
// the YAML sketch in the docs because adding a YAML dependency needs review
// (docs/11 §5); the keys follow the documented layout. Secrets are never
// written in the file: *_env fields name environment variables.
//
// Enterprises/bindings are seeded into SQLite on first start-up; once a
// record exists, SQLite (changed through the admin console) wins.
type Config struct {
	Server struct {
		PublicListen string `json:"public_listen"`
	} `json:"server"`
	// Admin is the single-enterprise console (docs/17). It is disabled
	// unless listen is set, and never shares the public listener.
	Admin   AdminConfig `json:"admin"`
	Storage struct {
		Database string `json:"database"`
	} `json:"storage"`
	Security struct {
		MasterKeyEnv     string           `json:"master_key_env"`
		CallbackTargets  []CallbackTarget `json:"callback_targets"`
		TokenTTLSeconds  int              `json:"token_ttl_seconds"`
		MaxTokensPerBind int              `json:"max_tokens_per_binding"`
	} `json:"security"`
	Workers struct {
		MaxConcurrency         int `json:"max_concurrency"`
		ShutdownGraceSeconds   int `json:"shutdown_grace_seconds"`
		SyncIntervalSeconds    int `json:"sync_interval_seconds"`
		DeliveryIntervalMillis int `json:"delivery_interval_millis"`
		MaxDeliveryAttempts    int `json:"max_delivery_attempts"`
		MaxSyncPages           int `json:"max_sync_pages"`
	} `json:"workers"`
	SendPolicy struct {
		WindowHours int `json:"window_hours"`
		MaxSends    int `json:"max_sends"`
	} `json:"send_policy"`
	WeCom struct {
		APIBaseURL string `json:"api_base_url"`
		// CustomerOrigins: sync_msg origin values treated as customer
		// content. Must be set explicitly (AT-019); WxJava notes 3.
		CustomerOrigins []int `json:"customer_origins"`
		// ServiceStateMap maps numeric service_state to internal states;
		// unmapped values are UNKNOWN and block AI sends.
		ServiceStateMap map[string]string `json:"service_state_map"`
	} `json:"wecom"`
	Enterprises []EnterpriseConfig `json:"enterprises"`
	Bindings    []BindingConfig    `json:"bindings"`
}

type AdminConfig struct {
	Listen string `json:"listen"`
	// Origin is the exact browser origin, e.g. https://admin.example:8443
	// (default http://<listen>).
	Origin string `json:"origin"`
	// PasswordHashEnv names the env var holding the bcrypt password hash.
	PasswordHashEnv   string `json:"password_hash_env"`
	CompanyName       string `json:"company_name"`
	SessionTTLMinutes int    `json:"session_ttl_minutes"`
	MaxSessions       int    `json:"max_sessions"`
}

type CallbackTarget struct {
	Host         string   `json:"host"`
	Port         int      `json:"port"`
	AllowedCIDRs []string `json:"allowed_cidrs"`
}

type EnterpriseConfig struct {
	ID                string `json:"id"`
	TenantKey         string `json:"tenant_key"`
	CorpID            string `json:"corp_id"`
	SecretEnv         string `json:"secret_env"`
	CallbackTokenEnv  string `json:"callback_token_env"`
	CallbackAESKeyEnv string `json:"callback_aes_key_env"`
}

type BindingConfig struct {
	ID                 string `json:"id"`
	EnterpriseID       string `json:"enterprise_id"`
	OpenKfID           string `json:"open_kfid"`
	ProjectID          string `json:"project_id"`
	VirtualCorpID      string `json:"virtual_corp_id"`
	AgentID            string `json:"agent_id"`
	VirtualSecretEnv   string `json:"virtual_secret_env"`
	CallbackTokenEnv   string `json:"callback_token_env"`
	CallbackAESKeyEnv  string `json:"callback_aes_key_env"`
	CallbackURL        string `json:"callback_url"`
	CredentialRevision int64  `json:"credential_revision"`
	Disabled           bool   `json:"disabled"`
}

// LoadConfig reads and validates a config file. Unknown fields are errors.
func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	return ParseConfig(raw)
}

func ParseConfig(raw []byte) (Config, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	c.defaults()
	return c, c.Validate()
}

func (c *Config) defaults() {
	if c.Server.PublicListen == "" {
		c.Server.PublicListen = "127.0.0.1:8090"
	}
	if c.Security.MasterKeyEnv == "" {
		c.Security.MasterKeyEnv = "WECOM_KF_BRIDGE_MASTER_KEY"
	}
	if c.Security.TokenTTLSeconds <= 0 {
		c.Security.TokenTTLSeconds = 3600
	}
	w := &c.Workers
	if w.MaxConcurrency <= 0 {
		w.MaxConcurrency = 2
	}
	if w.ShutdownGraceSeconds <= 0 {
		w.ShutdownGraceSeconds = 20
	}
	if w.SyncIntervalSeconds <= 0 {
		w.SyncIntervalSeconds = 30
	}
	if w.DeliveryIntervalMillis <= 0 {
		w.DeliveryIntervalMillis = 500
	}
	if w.MaxDeliveryAttempts <= 0 {
		w.MaxDeliveryAttempts = 3
	}
	if w.MaxSyncPages <= 0 {
		w.MaxSyncPages = 100
	}
}

func (c Config) Validate() error {
	var errs []string
	if c.Admin.Listen != "" {
		if len(c.Enterprises) != 1 {
			errs = append(errs, "admin console requires exactly one enterprise (single-enterprise deployment)")
		}
		if c.Admin.PasswordHashEnv == "" {
			errs = append(errs, "admin.password_hash_env required")
		}
		if c.Admin.Listen == c.Server.PublicListen {
			errs = append(errs, "admin.listen must differ from server.public_listen")
		}
	}
	if c.Storage.Database == "" {
		errs = append(errs, "storage.database required")
	}
	if u, err := url.Parse(c.WeCom.APIBaseURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		errs = append(errs, "wecom.api_base_url must be an http(s) URL")
	}
	if len(c.WeCom.CustomerOrigins) == 0 {
		errs = append(errs, "wecom.customer_origins required (origin values are not guessed)")
	}
	for k, v := range c.WeCom.ServiceStateMap {
		if _, err := strconv.Atoi(k); err != nil || !validInternalState(v) {
			errs = append(errs, "wecom.service_state_map invalid entry "+k)
		}
	}
	ents := map[string]bool{}
	tenants := map[string]bool{}
	for _, e := range c.Enterprises {
		if e.ID == "" || e.TenantKey == "" || e.CorpID == "" || e.SecretEnv == "" || e.CallbackTokenEnv == "" || e.CallbackAESKeyEnv == "" {
			errs = append(errs, "enterprise "+e.ID+": id, tenant_key, corp_id and *_env fields required")
		}
		if ents[e.ID] || tenants[e.TenantKey] {
			errs = append(errs, "duplicate enterprise id/tenant_key "+e.ID)
		}
		ents[e.ID], tenants[e.TenantKey] = true, true
	}
	ids := map[string]bool{}
	for _, b := range c.Bindings {
		if b.ID == "" || !ents[b.EnterpriseID] || b.OpenKfID == "" || b.ProjectID == "" || b.VirtualCorpID == "" || b.AgentID == "" || b.VirtualSecretEnv == "" || b.CallbackTokenEnv == "" || b.CallbackAESKeyEnv == "" {
			errs = append(errs, "binding "+b.ID+": all fields required and enterprise must exist")
		}
		if ids[b.ID] {
			errs = append(errs, "duplicate binding "+b.ID)
		}
		ids[b.ID] = true
		if b.CallbackURL != "" {
			if err := c.CheckCallbackURL(b.CallbackURL); err != nil {
				errs = append(errs, "binding "+b.ID+": "+err.Error())
			}
		}
	}
	if len(errs) > 0 {
		return errors.New("config: " + strings.Join(errs, "; "))
	}
	return nil
}

// CheckCallbackURL applies the static part of the SSRF policy (docs/09 §5):
// http(s), no userinfo, host:port must be an allowed callback target.
func (c Config) CheckCallbackURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("callback_url must be http(s)")
	}
	if u.User != nil {
		return errors.New("callback_url must not contain credentials")
	}
	if _, ok := c.callbackTarget(u); !ok {
		return errors.New("callback_url host:port not in security.callback_targets")
	}
	return nil
}

func (c Config) callbackTarget(u *url.URL) (CallbackTarget, bool) {
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	for _, t := range c.Security.CallbackTargets {
		if strings.EqualFold(t.Host, u.Hostname()) && strconv.Itoa(t.Port) == port {
			return t, true
		}
	}
	return CallbackTarget{}, false
}

func validInternalState(v string) bool {
	switch v {
	case "AI_ELIGIBLE", "WAITING_HUMAN", "HUMAN", "CLOSED", "UNKNOWN":
		return true
	}
	return false
}

func (c Config) grace() time.Duration {
	return time.Duration(c.Workers.ShutdownGraceSeconds) * time.Second
}

// secretEnv reads a required secret from the environment.
func secretEnv(name string) (string, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return "", fmt.Errorf("config: environment variable %s is empty", name)
	}
	return v, nil
}

// ShutdownGrace is the configured worker/HTTP shutdown grace.
func ShutdownGrace(c Config) time.Duration { return c.grace() }

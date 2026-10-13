// Package admin is the single-enterprise management console
// (docs/17-single-enterprise-admin-design.md): server-rendered HTML forms on
// a separate listener, one administrator, server-side sessions, CSRF,
// step-up re-authentication, idempotent POST-Redirect-GET writes and audit.
package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/whysmx/wecom-kf-bridge/state"
	"github.com/whysmx/wecom-kf-bridge/wecom"
	"golang.org/x/crypto/bcrypt"
)

// KF is the official WeChat KF account API, called server-side through the
// runtime's token cache. The browser never sees a real access_token.
type KF interface {
	ListAccounts(ctx context.Context, offset, limit int) ([]wecom.Account, error)
	AddAccount(ctx context.Context, name, mediaID string) (string, error)
	UpdateAccount(ctx context.Context, openKfID, name, mediaID string) error
	DeleteAccount(ctx context.Context, openKfID string) error
	ContactURL(ctx context.Context, openKfID, scene string) (string, error)
	// TransServiceState / ServiceState: official reception state
	// (/cgi-bin/kf/service_state/trans and /get). Trans returns msg_code
	// for send_msg_on_event (may be empty). servicerUserID required for →3.
	TransServiceState(ctx context.Context, openKfID, externalUserID string, state int, servicerUserID string) (msgCode string, err error)
	ServiceState(ctx context.Context, openKfID, externalUserID string) (int, error)
	// SendMsgOnEvent sends a one-shot queue/welcome phrase; no-op if code empty.
	SendMsgOnEvent(ctx context.Context, msgCode, text string) error
}

// Credentials are the virtual cc-connect credentials of a binding.
type Credentials struct {
	CorpID string `json:"corp_id"`
	Secret string `json:"secret"`
	Token  string `json:"token"`
	AESKey string `json:"aes_key"`
	// AgentID is the cc-connect agent_id (optional; kept on rotation).
	AgentID string `json:"agent_id,omitempty"`
}

// Runtime is the running gateway the console reconfigures.
type Runtime interface {
	// ApplyBinding installs the binding (and new credentials when non-nil)
	// into the compatible API, sync and delivery workers.
	ApplyBinding(ctx context.Context, b state.Binding, creds *Credentials) error
	// CheckURL validates a callback URL against the SSRF allowlist.
	CheckURL(raw string) error
	// TestURL performs a connection test with the SSRF-checked client.
	TestURL(ctx context.Context, raw string) error
	// Verify runs the WeCom URL-verification challenge against cc-connect.
	Verify(ctx context.Context, bindingID string) error
	// Status reports component status (no secrets).
	Status(ctx context.Context) map[string]string
	// EditableSettings returns the live values of admin-editable runtime knobs (#25).
	EditableSettings() EditableSettings
	// SaveSettings persists editable knobs (config file) and hot-applies them (#25).
	SaveSettings(ctx context.Context, s EditableSettings) error
	// ConfigBindings returns bindings present in config.json (live view) (#29).
	ConfigBindings() []EditableBindingConfig
	// SaveBindingConfig writes editable binding fields to config.json and hot-applies (#29).
	SaveBindingConfig(ctx context.Context, id string, patch BindingConfigPatch) error
}

// EditableSettings are the knobs the system-settings page may change (#25).
// Secrets stay out of this struct.
type EditableSettings struct {
	Treat48002AsAIEligible bool
	ServiceStateMap        map[string]string // official int as string → internal state
	PublicBaseURL          string
	// Transfer scripts for send_msg_on_event (queue on trans→2; end on recover→4).
	HandoverQueueScript string
	HandoverHumanScript string
	HandoverEndScript   string
}

// EditableBindingConfig is the config.json binding surface shown on the bindings page (#29).
// Env names are display-only; secrets are never included.
type EditableBindingConfig struct {
	ID                 string
	EnterpriseID       string
	OpenKfID           string
	ProjectID          string
	VirtualCorpID      string
	AgentID            string
	CallbackURL        string
	Disabled           bool
	VirtualSecretEnv   string
	CallbackTokenEnv   string
	CallbackAESKeyEnv  string
	CredentialRevision int64
	// ServicerUserID is bindings[].servicer_userid (e.g. WenHaiYan); needed for WAITING recover 2→3→4.
	ServicerUserID string
}

// BindingConfigPatch is the mutable subset a bindings form may submit (#29).
// EnterpriseID/OpenKfID are echoed for immutability checks and must match config.
type BindingConfigPatch struct {
	EnterpriseID    string
	OpenKfID        string
	ProjectID       string
	VirtualCorpID   string
	AgentID         string
	CallbackURL     string
	Disabled        bool
	ServicerUserID  string
}

// Setting is one displayed configuration value (never a secret value).
type Setting struct{ Group, Name, Value string }

type Config struct {
	Store   *state.Store
	KF      KF
	Runtime Runtime
	// PasswordHash is the bcrypt hash of the administrator password.
	PasswordHash []byte
	// Origin is the exact scheme://host[:port] browsers use for the console;
	// POSTs whose Origin/Referer differ are refused.
	Origin string
	// InsecureCookie omits the Secure flag (plain-http; runtime config
	// requires it for http:// origins).
	InsecureCookie bool
	// LanNoLogin lets private/loopback/link-local clients skip the login
	// wall (CSRF and write protections remain).
	LanNoLogin   bool
	EnterpriseID string
	CompanyName  string
	CorpID       string
	Settings     []Setting
	// ServiceStateMap maps official service_state values to internal states
	// (wecom.service_state_map). Handover transfers to WAITING_HUMAN;
	// recover ends WAITING via 2→3→4 (or HUMAN →4) then restores local AI.
	ServiceStateMap map[int]string
	// HandoverWelcome is sent via send_msg_on_event after admin/phrase trans→2.
	HandoverWelcome string
	// HandoverEndScript is sent via send_msg_on_event after admin recover trans→4.
	HandoverEndScript string
	// GatewayBaseURL is the public base URL cc-connect uses as api_base_url
	// (server.public_base_url). Empty exports a placeholder to fill in.
	GatewayBaseURL string
	SessionTTL     time.Duration
	StepUpTTL      time.Duration
	MaxSessions    int
	Clock          func() time.Time
	Random         io.Reader
}

type Console struct {
	cfg      Config
	mu       sync.Mutex
	sessions map[string]*session
	fails    []time.Time
	seq      uint64
	mux      *http.ServeMux
}

type session struct {
	id, csrf      string
	created, exp  time.Time
	seq           uint64
	stepUpUntil   time.Time
	flash         string
	pendingExport string
	tickets       map[string]ticket
	// lanAuth marks a session issued via LanNoLogin (no password); step-up
	// password checks are skipped for these sessions.
	lanAuth bool
}

type ticket struct {
	object, action string
	revision       int64
	exp            time.Time
}

func New(cfg Config) (*Console, error) {
	if cfg.Store == nil || cfg.KF == nil || cfg.Runtime == nil {
		return nil, errors.New("admin: store, kf and runtime are required")
	}
	if _, err := bcrypt.Cost(cfg.PasswordHash); err != nil {
		return nil, errors.New("admin: password hash must be bcrypt")
	}
	if !strings.HasPrefix(cfg.Origin, "http://") && !strings.HasPrefix(cfg.Origin, "https://") {
		return nil, errors.New("admin: origin must be http(s)://host[:port]")
	}
	if cfg.EnterpriseID == "" {
		return nil, errors.New("admin: enterprise id required")
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 8 * time.Hour
	}
	if cfg.StepUpTTL <= 0 {
		cfg.StepUpTTL = 5 * time.Minute
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = 8
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Random == nil {
		cfg.Random = rand.Reader
	}
	c := &Console{cfg: cfg, sessions: map[string]*session{}}
	c.routes()
	return c, nil
}

func (c *Console) randomHex(n int) string {
	b := make([]byte, n)
	if _, err := io.ReadFull(c.cfg.Random, b); err != nil {
		panic("admin: no randomness: " + err.Error())
	}
	return hex.EncodeToString(b)
}

const cookieName = "wkb_admin"

func (c *Console) newSession() *session {
	return c.newSessionAuth(false)
}

func (c *Console) newSessionAuth(lan bool) *session {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.cfg.Clock()
	for k, s := range c.sessions {
		if !now.Before(s.exp) {
			delete(c.sessions, k)
		}
	}
	for len(c.sessions) >= c.cfg.MaxSessions {
		var oldest string
		for k, s := range c.sessions {
			if oldest == "" || s.seq < c.sessions[oldest].seq {
				oldest = k
			}
		}
		delete(c.sessions, oldest)
	}
	c.seq++
	s := &session{seq: c.seq, id: c.randomHex(32), csrf: c.randomHex(32), created: now, exp: now.Add(c.cfg.SessionTTL), tickets: map[string]ticket{}, lanAuth: lan}
	c.sessions[s.id] = s
	return s
}

func (c *Console) lookup(r *http.Request) *session {
	ck, err := r.Cookie(cookieName)
	if err != nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.sessions[ck.Value]
	if !ok {
		return nil
	}
	if !c.cfg.Clock().Before(s.exp) {
		delete(c.sessions, ck.Value)
		return nil
	}
	return s
}

func (c *Console) drop(s *session) {
	c.mu.Lock()
	delete(c.sessions, s.id)
	c.mu.Unlock()
}

func (c *Console) setCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: value, Path: "/admin", HttpOnly: true, Secure: !c.cfg.InsecureCookie, SameSite: http.SameSiteStrictMode, MaxAge: maxAge})
}

// checkPassword compares in constant time (bcrypt) and throttles: after 5
// failures within 15 minutes all attempts are refused for the window.
func (c *Console) checkPassword(pw string) (bool, bool) {
	c.mu.Lock()
	now := c.cfg.Clock()
	var recent []time.Time
	for _, t := range c.fails {
		if now.Sub(t) < 15*time.Minute {
			recent = append(recent, t)
		}
	}
	c.fails = recent
	locked := len(recent) >= 5
	c.mu.Unlock()
	if locked {
		return false, true
	}
	ok := bcrypt.CompareHashAndPassword(c.cfg.PasswordHash, []byte(pw)) == nil
	if !ok {
		c.mu.Lock()
		c.fails = append(c.fails, now)
		c.mu.Unlock()
	}
	return ok, false
}

func (c *Console) setFlash(s *session, msg string) {
	c.mu.Lock()
	s.flash = msg
	c.mu.Unlock()
}

func (c *Console) takeFlash(s *session) (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, e := s.flash, s.pendingExport
	s.flash, s.pendingExport = "", ""
	return f, e
}

func (c *Console) steppedUp(s *session) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s.lanAuth {
		return true
	}
	return c.cfg.Clock().Before(s.stepUpUntil)
}

// isLANClient reports whether the TCP peer is loopback, RFC1918/ULA private,
// or link-local. X-Forwarded-For is ignored (docs/17 §2).
func isLANClient(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// issueTicket creates a single-use danger confirmation bound to object,
// action and revision with a short lifetime.
func (c *Console) issueTicket(s *session, object, action string, rev int64) string {
	id := c.randomHex(16)
	c.mu.Lock()
	s.tickets[id] = ticket{object: object, action: action, revision: rev, exp: c.cfg.Clock().Add(2 * time.Minute)}
	c.mu.Unlock()
	return id
}

func (c *Console) useTicket(s *session, id, object, action string, rev int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := s.tickets[id]
	delete(s.tickets, id)
	return ok && t.object == object && t.action == action && t.revision == rev && c.cfg.Clock().Before(t.exp)
}

// mask shows only the edges of an identifier.
func mask(v string) string {
	r := []rune(v)
	if len(r) <= 6 {
		return strings.Repeat("*", len(r))
	}
	return string(r[:3]) + strings.Repeat("*", len(r)-5) + string(r[len(r)-2:])
}

// ApplyEditableSettings hot-updates the console view of editable knobs after
// Runtime.SaveSettings succeeds (#25).
func (c *Console) ApplyEditableSettings(s EditableSettings) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg.GatewayBaseURL = s.PublicBaseURL
	m := map[int]string{}
	for k, v := range s.ServiceStateMap {
		n, err := strconv.Atoi(k)
		if err != nil {
			continue
		}
		m[n] = v
	}
	c.cfg.ServiceStateMap = m
	if q := strings.TrimSpace(s.HandoverQueueScript); q != "" {
		c.cfg.HandoverWelcome = q
	}
	c.cfg.HandoverEndScript = strings.TrimSpace(s.HandoverEndScript)
	// Refresh displayed rows that mirror editable fields; keep secret env names.
	out := make([]Setting, 0, len(c.cfg.Settings)+6)
	for _, row := range c.cfg.Settings {
		switch row.Name {
		case "service_state_map", "public_base_url", "treat_48002_as_ai_eligible",
			"handover_scripts.queue", "handover_scripts.human", "handover_scripts.end":
			continue
		default:
			out = append(out, row)
		}
	}
	out = append(out,
		Setting{Group: "微信客服", Name: "service_state_map", Value: fmt.Sprint(s.ServiceStateMap)},
		Setting{Group: "微信客服", Name: "treat_48002_as_ai_eligible", Value: fmt.Sprint(s.Treat48002AsAIEligible)},
		Setting{Group: "服务", Name: "public_base_url", Value: s.PublicBaseURL},
		Setting{Group: "转接话术", Name: "handover_scripts.queue", Value: s.HandoverQueueScript},
		Setting{Group: "转接话术", Name: "handover_scripts.human", Value: s.HandoverHumanScript},
		Setting{Group: "转接话术", Name: "handover_scripts.end", Value: s.HandoverEndScript},
	)
	c.cfg.Settings = out
}

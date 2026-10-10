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
	"io"
	"net/http"
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
	// (/cgi-bin/kf/service_state/trans and /get).
	TransServiceState(ctx context.Context, openKfID, externalUserID string, state int) error
	ServiceState(ctx context.Context, openKfID, externalUserID string) (int, error)
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
	// InsecureCookie omits the Secure flag (plain-http loopback only; the
	// runtime config enforces that).
	InsecureCookie bool
	EnterpriseID   string
	CompanyName    string
	CorpID         string
	Settings       []Setting
	// ServiceStateMap maps official service_state values to internal states
	// (wecom.service_state_map). Handover transfers to the value mapped to
	// WAITING_HUMAN, recovery to AI_ELIGIBLE.
	ServiceStateMap map[int]string
	SessionTTL      time.Duration
	StepUpTTL       time.Duration
	MaxSessions     int
	Clock           func() time.Time
	Random          io.Reader
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
	s := &session{seq: c.seq, id: c.randomHex(32), csrf: c.randomHex(32), created: now, exp: now.Add(c.cfg.SessionTTL), tickets: map[string]ticket{}}
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
	return c.cfg.Clock().Before(s.stepUpUntil)
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

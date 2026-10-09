package bridge

import (
	"context"
	crand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	ErrCodeOK          = 0
	ErrCodeAuth        = 70001
	ErrCodeForbidden   = 70002
	ErrCodeParameter   = 70003
	ErrCodeDisabled    = 70004
	ErrCodeUnsupported = 70005
	ErrCodePlatform    = 70006
	ErrCodeUnavailable = 70007
	ErrCodeUnknown     = 70008
)

type Binding struct {
	ID             string
	CorpID         string
	CorpSecret     string
	AgentID        string
	CallbackToken  string
	CallbackAESKey string
	Enabled        bool
	Revision       int64
}
type Customer struct {
	BindingID  string
	UserID     string
	Name       string
	Authorized bool
	Generation int64
	State      string
}
type SendRequest struct {
	BindingID string
	ToUser    string
	UserID    string
	AgentID   string
	MsgType   string
	Content   string
	Customer  Customer
	Chunk     int
	Chunks    int
}

// Adapter is the narrow boundary to the real customer API and cc-connect
// transport. It intentionally contains no model, queue, or credential APIs.
type Adapter interface {
	GetUser(context.Context, Binding, string) (Customer, error)
	SendText(context.Context, SendRequest) error
}

// MemoryAdapter is a harmless adapter useful for protocol tests and examples.
type MemoryAdapter struct{ Users map[string]Customer }

func (a MemoryAdapter) GetUser(_ context.Context, _ Binding, id string) (Customer, error) {
	if a.Users != nil {
		if u, ok := a.Users[id]; ok {
			return u, nil
		}
	}
	return Customer{UserID: id}, nil
}
func (MemoryAdapter) SendText(_ context.Context, _ SendRequest) error { return nil }

type Config struct {
	Bindings      []Binding
	Customers     []Customer
	Adapter       Adapter
	TokenTTL      time.Duration
	Random        io.Reader
	Clock         func() time.Time
	Logger        Logger
	MaxBodyBytes  int64
	MaxChunkBytes int
	OnInbound     func(context.Context, Binding, InboundMessage) error
}
type accessToken struct {
	BindingID string
	ExpiresAt time.Time
	Revision  int64
}
type Server struct {
	cfg       Config
	mu        sync.RWMutex
	bindings  map[string]Binding
	byCorp    map[string]string
	customers map[string]Customer
	tokens    map[string]accessToken
	randMu    sync.Mutex
}

func NewServer(cfg Config) *Server {
	if cfg.Adapter == nil {
		cfg.Adapter = MemoryAdapter{}
	}
	if cfg.TokenTTL <= 0 {
		cfg.TokenTTL = time.Hour
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Random == nil {
		cfg.Random = crand.Reader
	}
	if cfg.Logger == nil {
		cfg.Logger = nopLogger{}
	}
	if cfg.MaxChunkBytes <= 0 {
		cfg.MaxChunkBytes = DefaultChunkBytes
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 1 << 20
	}
	s := &Server{cfg: cfg, bindings: map[string]Binding{}, byCorp: map[string]string{}, customers: map[string]Customer{}, tokens: map[string]accessToken{}}
	for _, b := range cfg.Bindings {
		s.bindings[b.ID] = b
		s.byCorp[b.CorpID] = b.ID
	}
	for _, c := range cfg.Customers {
		if c.BindingID != "" && c.UserID != "" {
			s.customers[c.BindingID+"\x00"+c.UserID] = c
		}
	}
	return s
}
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }
func (s *Server) binding(id string) (Binding, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.bindings[id]
	return b, ok
}
func (s *Server) findByToken(v string) (Binding, error) {
	s.mu.RLock()
	t, ok := s.tokens[v]
	if !ok {
		s.mu.RUnlock()
		return Binding{}, errAPI{ErrCodeAuth, "invalid access_token", http.StatusUnauthorized}
	}
	b, bok := s.bindings[t.BindingID]
	s.mu.RUnlock()
	if !bok || !b.Enabled || t.Revision != b.Revision || (!t.ExpiresAt.IsZero() && !s.cfg.Clock().Before(t.ExpiresAt)) {
		return Binding{}, errAPI{ErrCodeAuth, "invalid access_token", http.StatusUnauthorized}
	}
	return b, nil
}
func (s *Server) customer(b Binding, id string) (Customer, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.customers[b.ID+"\x00"+id]
	if ok && c.BindingID == "" {
		c.BindingID = b.ID
	}
	return c, ok
}
func (s *Server) issueToken(b Binding) (string, time.Time, error) {
	raw := make([]byte, 32)
	s.randMu.Lock()
	_, e := io.ReadFull(s.cfg.Random, raw)
	s.randMu.Unlock()
	if e != nil {
		return "", time.Time{}, e
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	exp := s.cfg.Clock().Add(s.cfg.TokenTTL)
	s.mu.Lock()
	s.tokens[tok] = accessToken{BindingID: b.ID, ExpiresAt: exp, Revision: b.Revision}
	s.mu.Unlock()
	return tok, exp, nil
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch r.URL.Path {
	case "/cgi-bin/gettoken":
		s.handleGetToken(w, r)
	case "/cgi-bin/user/get":
		s.handleUserGet(w, r)
	case "/cgi-bin/message/send":
		s.handleMessageSend(w, r)
	case "/wecom/callback", "/wecom/callback/":
		s.handleCallback(w, r)
	default:
		writeAPIError(w, errAPI{ErrCodeParameter, "not found", http.StatusNotFound})
	}
}
func (s *Server) handleGetToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, errAPI{ErrCodeParameter, "method not allowed", http.StatusMethodNotAllowed})
		return
	}
	corp, secret := r.URL.Query().Get("corpid"), r.URL.Query().Get("corpsecret")
	s.mu.RLock()
	id := s.byCorp[corp]
	b, ok := s.bindings[id]
	s.mu.RUnlock()
	if !ok || !b.Enabled || secret != b.CorpSecret {
		writeAPIError(w, errAPI{ErrCodeAuth, "invalid credentials", http.StatusUnauthorized})
		return
	}
	tok, _, e := s.issueToken(b)
	if e != nil {
		writeAPIError(w, errAPI{ErrCodeUnavailable, "token unavailable", http.StatusServiceUnavailable})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"errcode": 0, "errmsg": "ok", "access_token": tok, "expires_in": int64(s.cfg.TokenTTL / time.Second)})
}
func (s *Server) handleUserGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, errAPI{ErrCodeParameter, "method not allowed", http.StatusMethodNotAllowed})
		return
	}
	b, e := s.findByToken(r.URL.Query().Get("access_token"))
	if e != nil {
		writeAPIError(w, e)
		return
	}
	id := r.URL.Query().Get("userid")
	if !validUserID(id) {
		writeAPIError(w, errAPI{ErrCodeParameter, "invalid userid", http.StatusBadRequest})
		return
	}
	c, ok := s.customer(b, id)
	if !ok || (!c.Authorized && c.Authorized == false) { /* an explicitly absent customer is always denied */
		if !ok {
			writeAPIError(w, errAPI{ErrCodeForbidden, "unknown userid", http.StatusForbidden})
			return
		}
	}
	fetched, err := s.cfg.Adapter.GetUser(r.Context(), b, id)
	if err != nil && err != io.EOF {
		writeAPIError(w, errAPI{ErrCodeUnavailable, "user unavailable", http.StatusBadGateway})
		return
	}
	if err == nil {
		if fetched.UserID != "" && fetched.UserID != id {
			writeAPIError(w, errAPI{ErrCodeForbidden, "userid mismatch", http.StatusForbidden})
			return
		}
		if fetched.Name != "" {
			c.Name = fetched.Name
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"errcode": 0, "errmsg": "ok", "userid": id, "name": c.Name})
}

type sendPayload struct {
	ToUser  string          `json:"touser"`
	MsgType string          `json:"msgtype"`
	AgentID json.RawMessage `json:"agentid"`
	Text    struct {
		Content string `json:"content"`
	} `json:"text"`
	Markdown struct {
		Content string `json:"content"`
	} `json:"markdown"`
}

func (s *Server) handleMessageSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, errAPI{ErrCodeParameter, "method not allowed", http.StatusMethodNotAllowed})
		return
	}
	b, e := s.findByToken(r.URL.Query().Get("access_token"))
	if e != nil {
		writeAPIError(w, e)
		return
	}
	var p sendPayload
	dec := json.NewDecoder(io.LimitReader(r.Body, s.cfg.MaxBodyBytes))
	if err := dec.Decode(&p); err != nil {
		writeAPIError(w, errAPI{ErrCodeParameter, "invalid JSON", http.StatusBadRequest})
		return
	}
	if !validUserID(p.ToUser) {
		writeAPIError(w, errAPI{ErrCodeParameter, "invalid touser", http.StatusBadRequest})
		return
	}
	agent, err := parseAgentID(p.AgentID)
	if err != nil || !sameAgentID(agent, b.AgentID) {
		writeAPIError(w, errAPI{ErrCodeParameter, "agentid mismatch", http.StatusBadRequest})
		return
	}
	typ := strings.ToLower(strings.TrimSpace(p.MsgType))
	var content string
	switch typ {
	case "text":
		content = p.Text.Content
	case "markdown":
		content = p.Markdown.Content
	default:
		writeAPIError(w, errAPI{ErrCodeUnsupported, "unsupported msgtype", http.StatusBadRequest})
		return
	}
	if content == "" {
		writeAPIError(w, errAPI{ErrCodeParameter, "empty content", http.StatusBadRequest})
		return
	}
	c, ok := s.customer(b, p.ToUser)
	if !ok || !c.Authorized {
		writeAPIError(w, errAPI{ErrCodeForbidden, "unknown or unauthorized userid", http.StatusForbidden})
		return
	}
	chunks, err := ChunkUTF8(content, s.cfg.MaxChunkBytes)
	if err != nil {
		writeAPIError(w, errAPI{ErrCodeParameter, err.Error(), http.StatusBadRequest})
		return
	}
	for i, ch := range chunks {
		if err := s.cfg.Adapter.SendText(r.Context(), SendRequest{BindingID: b.ID, ToUser: p.ToUser, UserID: p.ToUser, AgentID: agent, MsgType: "text", Content: ch, Customer: c, Chunk: i + 1, Chunks: len(chunks)}); err != nil {
			writeAPIError(w, errAPI{ErrCodeUnknown, "send result unknown", http.StatusBadGateway})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"errcode": 0, "errmsg": "ok"})
}

func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sig, ts, nonce := q.Get("msg_signature"), q.Get("timestamp"), q.Get("nonce")
	if sig == "" {
		sig = q.Get("signature")
	}
	if sig == "" || ts == "" || nonce == "" {
		writeAPIError(w, errAPI{ErrCodeParameter, "missing callback parameters", http.StatusBadRequest})
		return
	}
	if r.Method == http.MethodGet {
		enc := strings.ReplaceAll(q.Get("echostr"), " ", "+")
		if enc == "" {
			writeAPIError(w, errAPI{ErrCodeParameter, "missing echostr", http.StatusBadRequest})
			return
		}
		for _, b := range s.allBindings() {
			c := WeComCrypto{Token: b.CallbackToken, AESKey: b.CallbackAESKey, CorpID: b.CorpID}
			if c.VerifySignature(sig, ts, nonce, enc) {
				plain, e := c.Decrypt(enc)
				if e == nil {
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write(plain)
					return
				}
			}
		}
		writeAPIError(w, errAPI{ErrCodeAuth, "invalid callback", http.StatusForbidden})
		return
	}
	if r.Method != http.MethodPost {
		writeAPIError(w, errAPI{ErrCodeParameter, "method not allowed", http.StatusMethodNotAllowed})
		return
	}
	body, e := io.ReadAll(io.LimitReader(r.Body, s.cfg.MaxBodyBytes+1))
	if e != nil || int64(len(body)) > s.cfg.MaxBodyBytes {
		writeAPIError(w, errAPI{ErrCodeParameter, "callback body too large", http.StatusRequestEntityTooLarge})
		return
	}
	env, e := unmarshalEnvelope(body)
	if e != nil {
		writeAPIError(w, errAPI{ErrCodeParameter, "invalid callback", http.StatusBadRequest})
		return
	}
	for _, b := range s.allBindings() {
		c := WeComCrypto{Token: b.CallbackToken, AESKey: b.CallbackAESKey, CorpID: b.CorpID}
		if !c.VerifySignature(sig, ts, nonce, env.Encrypt) {
			continue
		}
		plain, e := c.Decrypt(env.Encrypt)
		if e != nil {
			continue
		}
		m, e := unmarshalInbound(plain)
		if e != nil {
			writeAPIError(w, errAPI{ErrCodeParameter, "invalid callback XML", http.StatusBadRequest})
			return
		}
		if s.cfg.OnInbound != nil {
			if e = s.cfg.OnInbound(r.Context(), b, m); e != nil {
				writeAPIError(w, errAPI{ErrCodeUnavailable, "callback unavailable", http.StatusServiceUnavailable})
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("success"))
		return
	}
	writeAPIError(w, errAPI{ErrCodeAuth, "invalid callback", http.StatusForbidden})
}
func (s *Server) allBindings() []Binding {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Binding, 0, len(s.bindings))
	for _, b := range s.bindings {
		out = append(out, b)
	}
	return out
}
func (s *Server) BuildCallback(bindingID string, m InboundMessage) (Callback, error) {
	b, ok := s.binding(bindingID)
	if !ok {
		return Callback{}, fmt.Errorf("bridge: binding not found")
	}
	if !b.Enabled {
		return Callback{}, fmt.Errorf("bridge: binding disabled")
	}
	if m.ToUserName == "" {
		m.ToUserName = b.CorpID
	}
	plain, e := marshalInbound(m)
	if e != nil {
		return Callback{}, e
	}
	c := WeComCrypto{Token: b.CallbackToken, AESKey: b.CallbackAESKey, CorpID: b.CorpID, Rand: s.cfg.Random}
	enc, e := c.Encrypt(plain)
	if e != nil {
		return Callback{}, e
	}
	ts := fmt.Sprintf("%d", s.cfg.Clock().Unix())
	nonce, e := s.randomNonce()
	if e != nil {
		return Callback{}, e
	}
	sig := c.Signature(ts, nonce, enc)
	body, e := marshalEnvelope(CallbackEnvelope{ToUserName: b.CorpID, AgentID: b.AgentID, Encrypt: enc})
	if e != nil {
		return Callback{}, e
	}
	return Callback{Signature: sig, Timestamp: ts, Nonce: nonce, Body: body, Encrypted: enc}, nil
}
func (s *Server) randomNonce() (string, error) {
	b := make([]byte, 8)
	s.randMu.Lock()
	defer s.randMu.Unlock()
	if _, e := io.ReadFull(s.cfg.Random, b); e != nil {
		return "", e
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

type errAPI struct {
	Code    int
	Message string
	Status  int
}

func (e errAPI) Error() string { return e.Message }
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeAPIError(w http.ResponseWriter, e error) {
	var a errAPI
	if x, ok := e.(errAPI); ok {
		a = x
	} else {
		a = errAPI{ErrCodeUnavailable, e.Error(), http.StatusInternalServerError}
	}
	if a.Status == 0 {
		a.Status = http.StatusBadRequest
	}
	writeJSON(w, a.Status, map[string]any{"errcode": a.Code, "errmsg": a.Message})
}

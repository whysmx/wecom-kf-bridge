package bridge

import (
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/whysmx/wecom-kf-bridge/state"
)

// Gateway error codes (docs/03 §3). They are not official WeChat codes.
const (
	ErrCodeOK          = 0
	ErrCodeAuth        = 70001 // authentication
	ErrCodeForbidden   = 70002 // cross-binding / unknown / unauthorized
	ErrCodeParameter   = 70003
	ErrCodeDisabled    = 70004 // disabled, human takeover, stale generation
	ErrCodeUnsupported = 70005
	ErrCodePlatform    = 70006 // platform (or its locally enforced policy) rejected; not executed
	ErrCodeUnavailable = 70007 // temporarily unavailable; provably not sent
	ErrCodeUnknown     = 70008 // result unknown; stored as UNKNOWN, never auto-resent
)

// Binding holds the virtual credentials one cc-connect platform instance
// uses. ID must equal the durable state binding ID.
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

// Customer is the protocol view of a durable state.Customer.
type Customer struct {
	ID             string
	BindingID      string
	UserID         string // compatible UID of the current generation
	ExternalUserID string
	Name           string
	Authorized     bool
	Generation     int64
	State          string
}

func customerFrom(c state.Customer) Customer {
	return Customer{ID: c.ID, BindingID: c.BindingID, UserID: c.UID, ExternalUserID: c.ExternalUserID, Name: c.Nickname, Authorized: c.Authorized, Generation: c.Generation, State: c.State}
}

type SendRequest struct {
	BindingID string
	OutboxID  string
	ToUser    string
	AgentID   string
	MsgType   string
	Content   string
	Customer  Customer
	Chunk     int
	Chunks    int
}

// SendErrorKind classifies an upstream send failure.
type SendErrorKind int

const (
	// SendUnknown: the request may have executed (timeout, lost response).
	SendUnknown SendErrorKind = iota
	// SendRejected: the platform explicitly refused; nothing was sent.
	SendRejected
	// SendUnavailable: the request provably never reached the platform.
	SendUnavailable
)

// SendError lets an Adapter state what is known about a failure. Any other
// error is treated as SendUnknown, the safe default.
type SendError struct {
	Kind SendErrorKind
	Err  error
}

func (e *SendError) Error() string {
	if e.Err == nil {
		return "bridge: send failed"
	}
	return e.Err.Error()
}
func (e *SendError) Unwrap() error { return e.Err }

func sendKind(err error) SendErrorKind {
	var se *SendError
	if errors.As(err, &se) {
		return se.Kind
	}
	return SendUnknown
}

// Adapter is the narrow boundary to the real WeChat customer service API.
// It intentionally contains no model, queue, or credential APIs.
type Adapter interface {
	// GetUser refreshes the profile (nickname) of a customer.
	GetUser(context.Context, Binding, Customer) (Customer, error)
	// ServiceState returns the internal handover state (state.Customer*)
	// from the official service_state API. Errors block the send.
	ServiceState(context.Context, Binding, Customer) (string, error)
	// SendText sends one chunk and returns the upstream msgid.
	SendText(context.Context, SendRequest) (string, error)
}

// Store is the durable state the protocol layer needs; *state.Store
// implements it. There is no in-memory customer table.
type Store interface {
	CustomerByUID(context.Context, string, string) (state.Customer, error)
	CreateOutbox(context.Context, state.OutboxMessage) (state.OutboxMessage, error)
	BlockOutbox(context.Context, string, string) (state.OutboxMessage, error)
	MarkOutboxSending(context.Context, string) (state.OutboxMessage, error)
	RecordOutboxChunk(context.Context, string, int, string) error
	TransitionOutbox(context.Context, string, string, string) (state.OutboxMessage, error)
	MarkOutboxUnknown(context.Context, string, string) (state.OutboxMessage, error)
	ReleaseBudget(context.Context, string, int) error
	SetHandoverStatus(context.Context, string, string, string) error
}

// unavailableAdapter fails closed when no real adapter is configured; there
// is no "always succeeds" adapter in production code.
type unavailableAdapter struct{}

var errNoAdapter = errors.New("bridge: no WeChat adapter configured")

func (unavailableAdapter) GetUser(_ context.Context, _ Binding, c Customer) (Customer, error) {
	return c, errNoAdapter
}
func (unavailableAdapter) ServiceState(context.Context, Binding, Customer) (string, error) {
	return "", errNoAdapter
}
func (unavailableAdapter) SendText(context.Context, SendRequest) (string, error) {
	return "", &SendError{Kind: SendUnavailable, Err: errNoAdapter}
}

type Config struct {
	Bindings      []Binding
	Store         Store
	Adapter       Adapter
	TokenTTL      time.Duration
	Random        io.Reader
	Clock         func() time.Time
	Logger        Logger
	MaxBodyBytes  int64
	MaxChunkBytes int
	// SendDeadline bounds state query + all chunk sends (docs/07 §4: 15s,
	// below the client's 30s HTTP timeout).
	SendDeadline time.Duration
	// ProfileTimeout bounds the nickname refresh in user/get.
	ProfileTimeout time.Duration
	// MaxTokensPerBinding / MaxTokens bound the virtual token table.
	MaxTokensPerBinding int
	MaxTokens           int
}
type accessToken struct {
	BindingID string
	IssuedAt  time.Time
	ExpiresAt time.Time
	Revision  int64
}
type Server struct {
	cfg      Config
	mu       sync.RWMutex
	bindings map[string]Binding
	byCorp   map[string]string
	tokens   map[string]accessToken
	randMu   sync.Mutex
}

func NewServer(cfg Config) (*Server, error) {
	if cfg.Store == nil {
		return nil, errors.New("bridge: a persistent Store is required")
	}
	if cfg.Adapter == nil {
		cfg.Adapter = unavailableAdapter{}
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
	if cfg.SendDeadline <= 0 {
		cfg.SendDeadline = 15 * time.Second
	}
	if cfg.ProfileTimeout <= 0 {
		cfg.ProfileTimeout = 3 * time.Second
	}
	if cfg.MaxTokensPerBinding <= 0 {
		cfg.MaxTokensPerBinding = 8
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 1024
	}
	s := &Server{cfg: cfg, bindings: map[string]Binding{}, byCorp: map[string]string{}, tokens: map[string]accessToken{}}
	for _, b := range cfg.Bindings {
		if b.ID == "" || b.CorpID == "" {
			return nil, fmt.Errorf("bridge: binding id and corp_id required")
		}
		if _, dup := s.byCorp[b.CorpID]; dup {
			return nil, fmt.Errorf("bridge: duplicate virtual corp_id %q", b.CorpID)
		}
		s.bindings[b.ID] = b
		s.byCorp[b.CorpID] = b.ID
	}
	return s, nil
}
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }
func (s *Server) binding(id string) (Binding, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.bindings[id]
	return b, ok
}
func (s *Server) findByToken(v string) (Binding, error) {
	deny := errAPI{ErrCodeAuth, "invalid access_token", http.StatusUnauthorized}
	s.mu.RLock()
	t, ok := s.tokens[v]
	b, bok := s.bindings[t.BindingID]
	s.mu.RUnlock()
	if !ok {
		return Binding{}, deny
	}
	if !s.cfg.Clock().Before(t.ExpiresAt) {
		s.mu.Lock()
		delete(s.tokens, v)
		s.mu.Unlock()
		return Binding{}, deny
	}
	if !bok || !b.Enabled || t.Revision != b.Revision {
		return Binding{}, deny
	}
	return b, nil
}

// issueToken stores a new virtual token. Expired tokens are purged and the
// table is bounded per binding and globally (oldest evicted first), so
// repeated gettoken calls cannot grow memory without limit.
func (s *Server) issueToken(b Binding) (string, time.Time, error) {
	raw := make([]byte, 32)
	s.randMu.Lock()
	_, e := io.ReadFull(s.cfg.Random, raw)
	s.randMu.Unlock()
	if e != nil {
		return "", time.Time{}, e
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	now := s.cfg.Clock()
	exp := now.Add(s.cfg.TokenTTL)
	s.mu.Lock()
	defer s.mu.Unlock()
	perBinding := 0
	for k, t := range s.tokens {
		if !now.Before(t.ExpiresAt) {
			delete(s.tokens, k)
		} else if t.BindingID == b.ID {
			perBinding++
		}
	}
	for perBinding >= s.cfg.MaxTokensPerBinding {
		s.evictOldestLocked(b.ID)
		perBinding--
	}
	for len(s.tokens) >= s.cfg.MaxTokens {
		s.evictOldestLocked("")
	}
	s.tokens[tok] = accessToken{BindingID: b.ID, IssuedAt: now, ExpiresAt: exp, Revision: b.Revision}
	return tok, exp, nil
}
func (s *Server) evictOldestLocked(bindingID string) {
	var oldest string
	var at time.Time
	for k, t := range s.tokens {
		if bindingID != "" && t.BindingID != bindingID {
			continue
		}
		if oldest == "" || t.IssuedAt.Before(at) || (t.IssuedAt.Equal(at) && k < oldest) {
			oldest, at = k, t.IssuedAt
		}
	}
	if oldest != "" {
		delete(s.tokens, oldest)
	}
}
func (s *Server) tokenCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.tokens)
}

// serveHTTP only serves the cc-connect compatible /cgi-bin API. Official
// WeChat callbacks are handled by wecom.Router at /webhooks/wechat-kf/{tenant_key}.
func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch r.URL.Path {
	case "/cgi-bin/gettoken":
		s.handleGetToken(w, r)
	case "/cgi-bin/user/get":
		s.handleUserGet(w, r)
	case "/cgi-bin/message/send":
		s.handleMessageSend(w, r)
	default:
		writeAPIError(w, errAPI{ErrCodeParameter, "not found", http.StatusNotFound})
	}
}

// secretEqual compares fixed-length digests in constant time so neither the
// content nor the length of the configured secret leaks through timing.
func secretEqual(a, b string) bool {
	x, y := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
}

func (s *Server) handleGetToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, errAPI{ErrCodeParameter, "method not allowed", http.StatusMethodNotAllowed})
		return
	}
	corp, secret := r.URL.Query().Get("corpid"), r.URL.Query().Get("corpsecret")
	s.mu.RLock()
	id, known := s.byCorp[corp]
	b := s.bindings[id]
	s.mu.RUnlock()
	match := secretEqual(secret, b.CorpSecret)
	if !known || !b.Enabled || b.CorpSecret == "" || !match {
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

// resolveCustomer maps a compatible UID to the durable customer and applies
// the identity rules: unknown → 70002, old generation → 70004, revoked
// entry authorization → 70002.
func (s *Server) resolveCustomer(ctx context.Context, b Binding, uid string) (state.Customer, error) {
	c, err := s.cfg.Store.CustomerByUID(ctx, b.ID, uid)
	switch {
	case errors.Is(err, state.ErrNotFound):
		return c, errAPI{ErrCodeForbidden, "unknown userid", http.StatusForbidden}
	case errors.Is(err, state.ErrStaleGeneration):
		s.cfg.Logger.Log("stale_generation", map[string]any{"binding_id": b.ID})
		return c, errAPI{ErrCodeDisabled, "stale generation", http.StatusForbidden}
	case err != nil:
		return c, errAPI{ErrCodeUnavailable, "state unavailable", http.StatusServiceUnavailable}
	case !c.Authorized:
		return c, errAPI{ErrCodeForbidden, "unauthorized userid", http.StatusForbidden}
	}
	return c, nil
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
	sc, err := s.resolveCustomer(r.Context(), b, id)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	c := customerFrom(sc)
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.ProfileTimeout)
	defer cancel()
	// A profile failure falls back to the cached nickname (or empty name so
	// the client falls back to the UID); it never blocks or borrows data.
	if fetched, err := s.cfg.Adapter.GetUser(ctx, b, c); err == nil && fetched.Name != "" && (fetched.ID == "" || fetched.ID == c.ID) {
		c.Name = fetched.Name
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

func outboxRejection(err error) errAPI {
	switch {
	case errors.Is(err, state.ErrStaleGeneration):
		return errAPI{ErrCodeDisabled, "stale generation", http.StatusForbidden}
	case errors.Is(err, state.ErrHeld):
		return errAPI{ErrCodeDisabled, "customer held for human service", http.StatusForbidden}
	case errors.Is(err, state.ErrBindingInactive):
		return errAPI{ErrCodeDisabled, "binding disabled", http.StatusForbidden}
	case errors.Is(err, state.ErrUnauthorized):
		return errAPI{ErrCodeForbidden, "unauthorized userid", http.StatusForbidden}
	case errors.Is(err, state.ErrWindowClosed):
		return errAPI{ErrCodePlatform, "send window closed", http.StatusForbidden}
	case errors.Is(err, state.ErrBudgetExceeded):
		return errAPI{ErrCodePlatform, "send budget exhausted", http.StatusTooManyRequests}
	}
	return errAPI{ErrCodeUnavailable, "state unavailable", http.StatusServiceUnavailable}
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
		content = StripMarkdown(p.Markdown.Content)
	default:
		writeAPIError(w, errAPI{ErrCodeUnsupported, "unsupported msgtype", http.StatusBadRequest})
		return
	}
	if strings.TrimSpace(content) == "" {
		writeAPIError(w, errAPI{ErrCodeParameter, "empty content", http.StatusBadRequest})
		return
	}
	chunks, err := ChunkUTF8(content, s.cfg.MaxChunkBytes)
	if err != nil {
		writeAPIError(w, errAPI{ErrCodeParameter, err.Error(), http.StatusBadRequest})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.SendDeadline)
	defer cancel()
	sc, err := s.resolveCustomer(ctx, b, p.ToUser)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeAPIErrorOrOK(w, s.send(ctx, b, sc, agent, content, chunks))
}

// send implements docs/03 §5 + docs/07 §4: register outbox (with generation,
// fence, authorization, binding and window/budget checked atomically), query
// the official servicing state, then send chunk by chunk, recording each
// accepted chunk. Errors are mapped to 70004/70006/70007/70008.
func (s *Server) send(ctx context.Context, b Binding, sc state.Customer, agent, content string, chunks []string) error {
	o, err := s.cfg.Store.CreateOutbox(ctx, state.OutboxMessage{CustomerID: sc.ID, Generation: sc.Generation, UID: sc.UID, Body: content, BudgetUnits: len(chunks), ChunksTotal: len(chunks)})
	if err != nil {
		return outboxRejection(err)
	}
	c := customerFrom(sc)
	st, err := s.cfg.Adapter.ServiceState(ctx, b, c)
	if err != nil {
		_, _ = s.cfg.Store.BlockOutbox(context.WithoutCancel(ctx), o.ID, state.BlockStateQuery)
		return errAPI{ErrCodeUnavailable, "service state unavailable", http.StatusServiceUnavailable}
	}
	if st != state.CustomerAIEligible {
		// Fence locally first; an official non-AI state is never overridden.
		if st == "" {
			st = state.CustomerUnknown
		}
		_ = s.cfg.Store.SetHandoverStatus(context.WithoutCancel(ctx), sc.ID, st, "service_state_precheck")
		_, _ = s.cfg.Store.BlockOutbox(context.WithoutCancel(ctx), o.ID, state.BlockHeld)
		return errAPI{ErrCodeDisabled, "customer held for human service", http.StatusForbidden}
	}
	if _, err = s.cfg.Store.MarkOutboxSending(ctx, o.ID); err != nil {
		_, _ = s.cfg.Store.BlockOutbox(context.WithoutCancel(ctx), o.ID, state.BlockHeld)
		return outboxRejection(err)
	}
	for i, ch := range chunks {
		msgID, err := s.cfg.Adapter.SendText(ctx, SendRequest{BindingID: b.ID, OutboxID: o.ID, ToUser: sc.UID, AgentID: agent, MsgType: "text", Content: ch, Customer: c, Chunk: i + 1, Chunks: len(chunks)})
		if err == nil {
			if e := s.cfg.Store.RecordOutboxChunk(context.WithoutCancel(ctx), o.ID, i+1, msgID); e != nil {
				// Sent upstream but not recorded: the overall result is unknown.
				_, _ = s.cfg.Store.MarkOutboxUnknown(context.WithoutCancel(ctx), o.ID, fmt.Sprintf("record_failed:%d/%d", i+1, len(chunks)))
				return errAPI{ErrCodeUnknown, "send result unknown", http.StatusBadGateway}
			}
			continue
		}
		return s.sendFailed(ctx, o, sc.ID, i, len(chunks), err)
	}
	if _, err = s.cfg.Store.TransitionOutbox(context.WithoutCancel(ctx), o.ID, state.OutboxUpstreamAccepted, ""); err != nil {
		return errAPI{ErrCodeUnknown, "send result unknown", http.StatusBadGateway}
	}
	return nil
}

// sendFailed stops at the failing chunk (no further chunks, no background
// resend), keeps the record of chunks already accepted, and releases only
// the budget of chunks that provably were never sent.
func (s *Server) sendFailed(ctx context.Context, o state.OutboxMessage, customerID string, sent, total int, err error) error {
	bg := context.WithoutCancel(ctx)
	kind := sendKind(err)
	if ctx.Err() != nil && kind != SendRejected && kind != SendUnavailable {
		kind = SendUnknown
	}
	progress := fmt.Sprintf("%d/%d", sent, total)
	suffix := ""
	if sent > 0 {
		suffix = fmt.Sprintf(" (partial: %s chunks sent)", progress)
	}
	s.cfg.Logger.Log("outbox_send_failed", map[string]any{"outbox_id": o.ID, "chunks": progress, "kind": int(kind)})
	switch kind {
	case SendRejected:
		_, _ = s.cfg.Store.TransitionOutbox(bg, o.ID, state.OutboxRejected, "platform_rejected:"+progress)
		_ = s.cfg.Store.ReleaseBudget(bg, customerID, total-sent)
		return errAPI{ErrCodePlatform, "platform rejected" + suffix, http.StatusBadGateway}
	case SendUnavailable:
		_, _ = s.cfg.Store.TransitionOutbox(bg, o.ID, state.OutboxRejected, "unavailable:"+progress)
		_ = s.cfg.Store.ReleaseBudget(bg, customerID, total-sent)
		return errAPI{ErrCodeUnavailable, "platform unavailable" + suffix, http.StatusServiceUnavailable}
	default:
		_, _ = s.cfg.Store.MarkOutboxUnknown(bg, o.ID, "unknown:"+progress)
		_ = s.cfg.Store.ReleaseBudget(bg, customerID, total-sent-1)
		return errAPI{ErrCodeUnknown, "send result unknown" + suffix, http.StatusBadGateway}
	}
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
func writeAPIErrorOrOK(w http.ResponseWriter, e error) {
	if e == nil {
		writeJSON(w, http.StatusOK, map[string]any{"errcode": 0, "errmsg": "ok"})
		return
	}
	writeAPIError(w, e)
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

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
	// ServicerUserID is bindings[].servicer_userid from config; required to
	// end WAITING via 2→3→4. Empty → recover/force-end fails 未配置接待人.
	ServicerUserID string
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

// Official WeCom service_state integers (documented; map names via config).
const (
	OfficialStateAI      = 1 // 由智能助手接待
	OfficialStateWaiting = 2 // 待接入池排队中
	OfficialStateHuman   = 3 // 由人工接待
	OfficialStateClosed  = 4 // 已结束/未开始
)

// Adapter is the narrow boundary to the real WeChat customer service API.
// It intentionally contains no model, queue, or credential APIs.
type Adapter interface {
	// GetUser refreshes the profile (nickname) of a customer.
	GetUser(context.Context, Binding, Customer) (Customer, error)
	// ServiceState returns the internal handover state (state.Customer*)
	// from the official service_state API. Errors block the send.
	ServiceState(context.Context, Binding, Customer) (string, error)
	// TransServiceState calls kf/service_state/trans and returns msg_code
	// (empty when the target state does not issue one). state is the
	// official integer (1 AI / 2 waiting / 3 human / 4 closed).
	// servicerUserID is required for →3 (HUMAN); omit (empty) for →1/→2/→4.
	TransServiceState(ctx context.Context, b Binding, c Customer, officialState int, servicerUserID string) (msgCode string, err error)
	// SendMsgOnEvent sends a one-shot event response (welcome/queue/end).
	SendMsgOnEvent(ctx context.Context, b Binding, msgCode, text string) error
	// SendText sends one chunk and returns the upstream msgid.
	SendText(context.Context, SendRequest) (string, error)
	// GetMedia proxies WeCom /cgi-bin/media/get with the real corp token.
	// Callers must enforce MaxMediaBytes; implementations never log body bytes.
	GetMedia(ctx context.Context, b Binding, mediaID string) (MediaDownload, error)
}

// Store is the durable state the protocol layer needs; *state.Store
// implements it. There is no in-memory customer table.
type Store interface {
	CustomerByUID(context.Context, string, string) (state.Customer, error)
	Customer(context.Context, string) (state.Customer, error)
	CreateOutbox(context.Context, state.OutboxMessage) (state.OutboxMessage, error)
	BlockOutbox(context.Context, string, string) (state.OutboxMessage, error)
	MarkOutboxSending(context.Context, string) (state.OutboxMessage, error)
	SendGuarded(context.Context, string, func() error) error
	RecordOutboxChunk(context.Context, string, int, string) error
	TransitionOutbox(context.Context, string, string, string) (state.OutboxMessage, error)
	MarkOutboxUnknown(context.Context, string, string) (state.OutboxMessage, error)
	ReleaseBudget(context.Context, string, int) error
	SetHandoverStatus(context.Context, string, string, string) error
	BeginHandover(context.Context, string, string) (state.Customer, error)
	ClearAutoFenceInPlace(context.Context, string, string) (state.Customer, error)
	// AppendChatTurn stores a local handover draft turn (user/AI); best-effort.
	AppendChatTurn(ctx context.Context, customerID, role, msgType, content string, ttlDays int) error
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
func (unavailableAdapter) TransServiceState(context.Context, Binding, Customer, int, string) (string, error) {
	return "", errNoAdapter
}
func (unavailableAdapter) SendMsgOnEvent(context.Context, Binding, string, string) error {
	return errNoAdapter
}
func (unavailableAdapter) SendText(context.Context, SendRequest) (string, error) {
	return "", &SendError{Kind: SendUnavailable, Err: errNoAdapter}
}
func (unavailableAdapter) GetMedia(context.Context, Binding, string) (MediaDownload, error) {
	return MediaDownload{}, errNoAdapter
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
	case "/cgi-bin/media/get":
		s.handleMediaGet(w, r)
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
//
// Human-handover: WeCom forbids 2→1 / 3→1 (95016). When official is
// WAITING_HUMAN/HUMAN without manual intent, end the session (trans→4) and
// fail-closed this send — AI resumes only after the customer re-enters (0)
// on a later message. Manual fences keep returning 70004 and are never auto-pulled.
func (s *Server) send(ctx context.Context, b Binding, sc state.Customer, agent, content string, chunks []string) error {
	var err error
	sc, err = s.ensureAIEligibleForSend(ctx, b, sc)
	if err != nil {
		return err
	}
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
	if st == state.CustomerWaitingHuman || st == state.CustomerHuman {
		if sc.FenceSource == state.FenceManual {
			_, _ = s.cfg.Store.BlockOutbox(context.WithoutCancel(ctx), o.ID, state.BlockHeld)
			return errAPI{ErrCodeDisabled, "customer held for human service", http.StatusForbidden}
		}
		// End→4 (never →1); fail-closed for this send regardless of end outcome.
		if err := s.forceEndHumanSession(ctx, b, sc, st); err != nil {
			_, _ = s.cfg.Store.BlockOutbox(context.WithoutCancel(ctx), o.ID, state.BlockStateQuery)
			return err
		}
		_, _ = s.cfg.Store.BlockOutbox(context.WithoutCancel(ctx), o.ID, state.BlockHeld)
		return errAPI{ErrCodeDisabled, "official session ended; wait for customer re-entry", http.StatusForbidden}
	} else if st != state.CustomerAIEligible && st != state.CustomerAIEligibleBypass48002 {
		// CLOSED / UNKNOWN: keep prior fail-closed behaviour. CLOSED only
		// blocks this send (no persistent fence). UNKNOWN auto-fences.
		if st == "" {
			st = state.CustomerUnknown
		}
		if st != state.CustomerClosed {
			_ = s.cfg.Store.SetHandoverStatus(context.WithoutCancel(ctx), sc.ID, st, "service_state_precheck")
		}
		_, _ = s.cfg.Store.BlockOutbox(context.WithoutCancel(ctx), o.ID, state.BlockHeld)
		return errAPI{ErrCodeDisabled, "customer held for human service", http.StatusForbidden}
	}
	if _, err = s.cfg.Store.MarkOutboxSending(ctx, o.ID); err != nil {
		_, _ = s.cfg.Store.BlockOutbox(context.WithoutCancel(ctx), o.ID, state.BlockHeld)
		return outboxRejection(err)
	}
	for i, ch := range chunks {
		var msgID string
		var sendErr error
		// Re-verify generation, fence, binding active and revision right
		// before every outbound call (#25, #27).
		if gerr := s.cfg.Store.SendGuarded(ctx, o.ID, func() error {
			msgID, sendErr = s.cfg.Adapter.SendText(ctx, SendRequest{BindingID: b.ID, OutboxID: o.ID, ToUser: sc.UID, AgentID: agent, MsgType: "text", Content: ch, Customer: c, Chunk: i + 1, Chunks: len(chunks)})
			return nil
		}); gerr != nil {
			return s.sendAborted(ctx, o, sc.ID, i, len(chunks), gerr)
		}
		err := sendErr
		if err == nil {
			if e := s.cfg.Store.RecordOutboxChunk(context.WithoutCancel(ctx), o.ID, i+1, msgID); e != nil {
				// Sent upstream but not recorded: the overall result is unknown.
				_, _ = s.cfg.Store.MarkOutboxUnknown(context.WithoutCancel(ctx), o.ID, fmt.Sprintf("record_failed:%d/%d", i+1, len(chunks)))
				return errAPI{ErrCodeUnknown, "send result unknown", http.StatusBadGateway}
			}
			// Live-smoke visibility: msgid + binding + errcode, never body/PII.
			s.cfg.Logger.Log("wecom_send_msg_ok", map[string]any{
				"binding_id": b.ID,
				"msgid":      msgID,
				"outbox_id":  o.ID,
				"chunk":      i + 1,
				"chunks":     len(chunks),
				"errcode":    0,
			})
			continue
		}
		return s.sendFailed(ctx, o, sc.ID, i, len(chunks), err)
	}
	if _, err = s.cfg.Store.TransitionOutbox(context.WithoutCancel(ctx), o.ID, state.OutboxUpstreamAccepted, ""); err != nil {
		return errAPI{ErrCodeUnknown, "send result unknown", http.StatusBadGateway}
	}
	// Local handover draft: AI outbound (best-effort; never fail the send).
	s.recordAIChatTurn(context.WithoutCancel(ctx), sc.ID, content)
	return nil
}

// recordAIChatTurn appends an AI outbound draft (best-effort; never fails the send).
func (s *Server) recordAIChatTurn(ctx context.Context, customerID, content string) {
	if err := s.cfg.Store.AppendChatTurn(ctx, customerID, state.ChatRoleAI, "text", content, 0); err != nil && s.cfg.Logger != nil {
		s.cfg.Logger.Log("chat_context_append_failed", map[string]any{"customer_id": customerID, "role": state.ChatRoleAI, "error": err.Error()})
	}
}

// ensureAIEligibleForSend: WeCom forbids forcing 2/3→1. When official is
// WAITING/HUMAN without manual intent, end→4 and fail-closed (do not clear
// fence / send as AI in this request). Manual handoff is never cleared here.
func (s *Server) ensureAIEligibleForSend(ctx context.Context, b Binding, sc state.Customer) (state.Customer, error) {
	if sc.State == state.CustomerAIEligible {
		return sc, nil
	}
	if sc.FenceSource == state.FenceManual {
		// Let CreateOutbox record BLOCKED for audit; do not force-end.
		return sc, nil
	}
	c := customerFrom(sc)
	st, err := s.cfg.Adapter.ServiceState(ctx, b, c)
	if err != nil {
		return sc, errAPI{ErrCodeUnavailable, "service state unavailable", http.StatusServiceUnavailable}
	}
	if st == state.CustomerWaitingHuman || st == state.CustomerHuman {
		// End official human session; still fail-closed for this send.
		if err := s.forceEndHumanSession(ctx, b, sc, st); err != nil {
			return sc, err
		}
		return sc, errAPI{ErrCodeDisabled, "official session ended; wait for customer re-entry", http.StatusForbidden}
	}
	// Official already AI: drop an automatic fence in place so this UID can send.
	// The 48002 bypass is not a real AI observation and must not clear fences.
	if st == state.CustomerAIEligible && sc.FenceSource == state.FenceAuto {
		nc, err := s.cfg.Store.ClearAutoFenceInPlace(ctx, sc.ID, "official_ai_observed")
		if err != nil {
			return sc, errAPI{ErrCodeUnavailable, "local fence clear failed", http.StatusServiceUnavailable}
		}
		return nc, nil
	}
	if sc.State != state.CustomerAIEligible {
		return sc, errAPI{ErrCodeDisabled, "customer held for human service", http.StatusForbidden}
	}
	return sc, nil
}

// forceEndHumanSession ends official WAITING/HUMAN.
// WAITING(2): fixed 2→3(servicer required)→4; no servicer → fail with 未配置接待人 (never 2→4).
// HUMAN(3): only →4. WeCom rejects 2→1 / 3→1 (95016); never call OfficialStateAI.
func (s *Server) forceEndHumanSession(ctx context.Context, b Binding, sc state.Customer, fromMapped string) error {
	c := customerFrom(sc)
	s.cfg.Logger.Log("handover_force_end", map[string]any{
		"binding_id":             b.ID,
		"customer_id":            sc.ID,
		"from_mapped":            fromMapped,
		"external_userid_prefix": TruncateID(sc.ExternalUserID, 8),
		"note":                   "waiting_via_3_then_4",
	})
	err := EndOfficialHumanSession(ctx, s.cfg.Adapter, b, c, fromMapped)
	if err != nil {
		s.cfg.Logger.Log("handover_force_end_failed", map[string]any{
			"binding_id":             b.ID,
			"customer_id":            sc.ID,
			"from_mapped":            fromMapped,
			"external_userid_prefix": TruncateID(sc.ExternalUserID, 8),
			"error":                  err.Error(),
		})
		if strings.Contains(err.Error(), "未配置接待人") {
			return errAPI{ErrCodeUnavailable, err.Error(), http.StatusServiceUnavailable}
		}
		var se *SendError
		if errors.As(err, &se) && se.Kind == SendRejected {
			return errAPI{ErrCodePlatform, "force end session rejected", http.StatusBadGateway}
		}
		return errAPI{ErrCodeUnavailable, "force end session failed", http.StatusServiceUnavailable}
	}
	got, err := s.cfg.Adapter.ServiceState(ctx, b, c)
	if err != nil {
		s.cfg.Logger.Log("handover_force_end_reread_failed", map[string]any{
			"binding_id": b.ID, "customer_id": sc.ID, "error": err.Error(),
		})
		return errAPI{ErrCodeUnavailable, "force end re-read failed", http.StatusServiceUnavailable}
	}
	if got != state.CustomerClosed {
		s.cfg.Logger.Log("handover_force_end_not_confirmed", map[string]any{
			"binding_id": b.ID, "customer_id": sc.ID, "got": got, "from_mapped": fromMapped,
		})
		return errAPI{ErrCodeUnavailable, "force end not confirmed as CLOSED", http.StatusServiceUnavailable}
	}
	s.cfg.Logger.Log("handover_force_end_ok", map[string]any{
		"binding_id": b.ID, "customer_id": sc.ID, "from_mapped": fromMapped,
	})
	return nil
}

// EndOfficialHumanSession ends WAITING via 2→3(servicer_userid)→4 or HUMAN via →4.
// Servicer comes from b.ServicerUserID (bindings[].servicer_userid). Empty →
// error containing 未配置接待人 and must not call 2→4 (冒烟 95016).
func EndOfficialHumanSession(ctx context.Context, ad Adapter, b Binding, c Customer, fromMapped string) error {
	if fromMapped == state.CustomerWaitingHuman {
		sid := strings.TrimSpace(b.ServicerUserID)
		if sid == "" {
			return errors.New("未配置接待人")
		}
		if _, err := ad.TransServiceState(ctx, b, c, OfficialStateHuman, sid); err != nil {
			return err
		}
	}
	_, err := ad.TransServiceState(ctx, b, c, OfficialStateClosed, "")
	return err
}

// sendAborted handles a send whose preconditions stopped holding between
// chunks: nothing more goes out, the row is recorded REJECTED with the
// reason and progress, and the budget of unsent chunks is released.
func (s *Server) sendAborted(ctx context.Context, o state.OutboxMessage, customerID string, sent, total int, err error) error {
	bg := context.WithoutCancel(ctx)
	progress := fmt.Sprintf("%d/%d", sent, total)
	s.cfg.Logger.Log("outbox_send_aborted", map[string]any{"outbox_id": o.ID, "chunks": progress, "error": err})
	if _, e := s.cfg.Store.TransitionOutbox(bg, o.ID, state.OutboxRejected, "precondition_lost:"+progress); e != nil {
		return errAPI{ErrCodeUnknown, "send state unknown", http.StatusBadGateway}
	}
	_ = s.cfg.Store.ReleaseBudget(bg, customerID, total-sent)
	return outboxRejection(err)
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

// handleMediaGet authenticates the virtual access_token and proxies
// WeCom /cgi-bin/media/get with the real corp token. Bodies over MaxMediaBytes
// are rejected. Logs never include tokens, full media_id, or binary content.
func (s *Server) handleMediaGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, errAPI{ErrCodeParameter, "method not allowed", http.StatusMethodNotAllowed})
		return
	}
	tok := r.URL.Query().Get("access_token")
	b, e := s.findByToken(tok)
	if e != nil {
		writeAPIError(w, e)
		return
	}
	mediaID := strings.TrimSpace(r.URL.Query().Get("media_id"))
	if mediaID == "" {
		writeAPIError(w, errAPI{ErrCodeParameter, "media_id required", http.StatusBadRequest})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	md, err := s.cfg.Adapter.GetMedia(ctx, b, mediaID)
	if err != nil {
		s.cfg.Logger.Log("media_get_failed", map[string]any{
			"binding_id":      b.ID,
			"media_id_prefix": TruncateID(mediaID, 8),
			"media_id_len":    len(mediaID),
			"error":           err.Error(),
		})
		var ae errAPI
		if x, ok := err.(errAPI); ok {
			ae = x
		} else {
			ae = errAPI{ErrCodeUnavailable, "media unavailable", http.StatusBadGateway}
		}
		writeAPIError(w, ae)
		return
	}
	if int64(len(md.Data)) > MaxMediaBytes {
		s.cfg.Logger.Log("media_get_too_large", map[string]any{
			"binding_id":      b.ID,
			"media_id_prefix": TruncateID(mediaID, 8),
			"media_id_len":    len(mediaID),
			"size":            len(md.Data),
		})
		writeAPIError(w, errAPI{ErrCodeParameter, "media too large", http.StatusRequestEntityTooLarge})
		return
	}
	s.cfg.Logger.Log("media_get_ok", map[string]any{
		"binding_id":      b.ID,
		"media_id_prefix": TruncateID(mediaID, 8),
		"media_id_len":    len(mediaID),
		"size":            len(md.Data),
		"has_disposition": md.ContentDisposition != "",
	})
	if md.ContentType != "" {
		w.Header().Set("Content-Type", md.ContentType)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	if md.ContentDisposition != "" {
		w.Header().Set("Content-Disposition", md.ContentDisposition)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(md.Data)
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

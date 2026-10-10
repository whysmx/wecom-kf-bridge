// Package wecom implements the small subset of the WeCom customer-service
// HTTP API used by the bridge.  Methods intentionally do not hide upstream
// errcodes: callers can inspect APIError and decide whether a request is
// retryable or has an unknown outcome.
package wecom

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var (
	ErrSignature       = errors.New("wecom: invalid signature")
	ErrInvalidArgument = errors.New("wecom: invalid argument")
	ErrResponse        = errors.New("wecom: invalid API response")
)

// Logger is deliberately tiny so applications can connect their structured
// logger without pulling a logging dependency into this package.
type Logger interface{ Log(string, map[string]any) }
type nopLogger struct{}

func (nopLogger) Log(string, map[string]any) {}

// Client is safe for concurrent use. HTTPClient may be nil, in which case a
// client with a bounded timeout is used. BaseURL is the real WeCom API host;
// methods append /cgi-bin paths and never log credentials.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
	CorpID     string
	CorpSecret string
	Logger     Logger
	Timeout    time.Duration
	// RatePerSecond/Burst bound outbound calls from this client (0 = off).
	RatePerSecond float64
	Burst         int
	// MaxRetries bounds retries. Read-only calls retry on transport errors,
	// HTTP 429 and 5xx; calls with side effects (send_msg, account writes,
	// service_state/trans) retry only when the connection was never
	// established, because a lost response there is an UNKNOWN result
	// (docs/07 §6) and must not be blindly re-sent.
	MaxRetries int
	Backoff    time.Duration
	MaxBackoff time.Duration

	once    sync.Once
	shared  *http.Client
	limiter *tokenBucket
	sleep   func(context.Context, time.Duration) error
}

func NewClient(baseURL, corpID, corpSecret string) *Client {
	return &Client{BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"), CorpID: corpID, CorpSecret: corpSecret, Timeout: 30 * time.Second, Logger: nopLogger{}, RatePerSecond: 20, Burst: 10, MaxRetries: 2, Backoff: 200 * time.Millisecond, MaxBackoff: 2 * time.Second}
}

// httpClient returns one reused *http.Client per Client so connections are
// pooled instead of creating a new client (and transport) per request.
func (c *Client) httpClient() *http.Client {
	if c == nil {
		return defaultHTTPClient
	}
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	c.init()
	return c.shared
}
var defaultHTTPClient = &http.Client{Timeout: 30 * time.Second}

func (c *Client) init() {
	c.once.Do(func() {
		timeout := 30 * time.Second
		if c.Timeout > 0 {
			timeout = c.Timeout
		}
		c.shared = &http.Client{Timeout: timeout}
		if c.RatePerSecond > 0 {
			c.limiter = newTokenBucket(c.RatePerSecond, c.Burst)
		}
		if c.sleep == nil {
			c.sleep = sleepCtx
		}
	})
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// sideEffectPaths are never retried after a request may have reached WeCom.
var sideEffectPaths = map[string]bool{
	"/cgi-bin/kf/send_msg": true, "/cgi-bin/kf/account/add": true, "/cgi-bin/kf/account/update": true,
	"/cgi-bin/kf/account/del": true, "/cgi-bin/kf/add_contact_way": true, "/cgi-bin/kf/service_state/trans": true,
	"/cgi-bin/media/upload": true,
}

// notConnected reports a failure where the request provably never left:
// dial/DNS failures. Anything else may have been processed upstream.
func notConnected(err error) bool {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	var dns *net.DNSError
	return errors.As(err, &dns)
}

// do sends a request built by mk, applying rate limiting and bounded,
// exponential backoff retries according to the side-effect rules above.
func (c *Client) do(ctx context.Context, path string, mk func() (*http.Request, error), limit int64) (int, []byte, error) {
	hc := c.httpClient()
	c.init()
	backoff := c.Backoff
	if backoff <= 0 {
		backoff = 200 * time.Millisecond
	}
	maxB := c.MaxBackoff
	if maxB <= 0 {
		maxB = 2 * time.Second
	}
	for attempt := 0; ; attempt++ {
		if c.limiter != nil {
			if err := c.limiter.wait(ctx, c.sleep); err != nil {
				return 0, nil, err
			}
		}
		req, err := mk()
		if err != nil {
			return 0, nil, err
		}
		resp, err := hc.Do(req)
		retry := false
		var status int
		var raw []byte
		if err != nil {
			retry = notConnected(err) || !sideEffectPaths[path]
		} else {
			status = resp.StatusCode
			raw, err = io.ReadAll(io.LimitReader(resp.Body, limit))
			resp.Body.Close()
			if err == nil && (status == http.StatusTooManyRequests || status >= 500) && !sideEffectPaths[path] {
				retry = true
			}
		}
		if !retry || attempt >= c.MaxRetries || ctx.Err() != nil {
			return status, raw, err
		}
		if c.Logger != nil {
			c.Logger.Log("wecom_api_retry", map[string]any{"path": path, "attempt": attempt + 1, "status": status})
		}
		if e := c.sleep(ctx, backoff); e != nil {
			if err == nil {
				return status, raw, nil
			}
			return status, raw, err
		}
		backoff *= 2
		if backoff > maxB {
			backoff = maxB
		}
	}
}

type tokenBucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
	now    func() time.Time
}

func newTokenBucket(rate float64, burst int) *tokenBucket {
	if burst < 1 {
		burst = 1
	}
	return &tokenBucket{rate: rate, burst: float64(burst), tokens: float64(burst), now: time.Now}
}
func (b *tokenBucket) wait(ctx context.Context, sleep func(context.Context, time.Duration) error) error {
	for {
		b.mu.Lock()
		n := b.now()
		if !b.last.IsZero() {
			b.tokens += n.Sub(b.last).Seconds() * b.rate
			if b.tokens > b.burst {
				b.tokens = b.burst
			}
		}
		b.last = n
		if b.tokens >= 1 {
			b.tokens--
			b.mu.Unlock()
			return nil
		}
		d := time.Duration((1 - b.tokens) / b.rate * float64(time.Second))
		b.mu.Unlock()
		if err := sleep(ctx, d); err != nil {
			return err
		}
	}
}
func (c *Client) endpoint(path string) (string, error) {
	if c == nil || strings.TrimSpace(c.BaseURL) == "" {
		return "", fmt.Errorf("%w: empty base URL", ErrInvalidArgument)
	}
	base, err := url.Parse(c.BaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return "", fmt.Errorf("%w: invalid base URL", ErrInvalidArgument)
	}
	if path == "" || path[0] != '/' {
		path = "/" + path
	}
	base.Path = strings.TrimRight(base.Path, "/") + path
	base.RawQuery = ""
	return base.String(), nil
}

type APIError struct {
	Code       int    `json:"errcode"`
	Message    string `json:"errmsg"`
	HTTPStatus int
	Path       string
}

func (e *APIError) Error() string {
	if e == nil {
		return ""
	}
	if e.HTTPStatus > 0 {
		return fmt.Sprintf("wecom: API errcode=%d errmsg=%q http=%d", e.Code, e.Message, e.HTTPStatus)
	}
	return fmt.Sprintf("wecom: API errcode=%d errmsg=%q", e.Code, e.Message)
}
func (e *APIError) Unwrap() error { return ErrResponse }

type apiEnvelope struct {
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
}

func checkEnvelope(raw []byte, status int, path string) error {
	var e apiEnvelope
	if err := json.Unmarshal(raw, &e); err != nil {
		return fmt.Errorf("%w: %v", ErrResponse, err)
	}
	if e.ErrCode != 0 {
		return &APIError{Code: e.ErrCode, Message: e.ErrMsg, HTTPStatus: status, Path: path}
	}
	if status < 200 || status >= 300 {
		return &APIError{Code: status, Message: "http error", HTTPStatus: status, Path: path}
	}
	return nil
}
func (c *Client) doJSON(ctx context.Context, method, path, token string, query url.Values, in, out any) error {
	u, err := c.endpoint(path)
	if err != nil {
		return err
	}
	if query == nil {
		query = url.Values{}
	}
	if token != "" {
		query.Set("access_token", token)
	}
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var body io.Reader
	if in != nil {
		b, e := json.Marshal(in)
		if e != nil {
			return e
		}
		body = bytes.NewReader(b)
	}
	var payload []byte
	if body != nil {
		payload, _ = io.ReadAll(body)
	}
	status, raw, e := c.do(ctx, path, func() (*http.Request, error) {
		var rd io.Reader
		if payload != nil {
			rd = bytes.NewReader(payload)
		}
		req, e := http.NewRequestWithContext(ctx, method, u, rd)
		if e == nil && in != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		return req, e
	}, 16<<20)
	if e != nil {
		return e
	}
	if err := checkEnvelope(raw, status, path); err != nil {
		return err
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%w: %v", ErrResponse, err)
		}
	}
	return nil
}

// TokenResponse is returned by /cgi-bin/gettoken.
type TokenResponse struct {
	ErrCode     int    `json:"errcode"`
	ErrMsg      string `json:"errmsg"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

func (c *Client) GetToken(ctx context.Context) (TokenResponse, error) {
	return c.GetTokenFor(ctx, c.CorpID, c.CorpSecret)
}
func (c *Client) GetTokenFor(ctx context.Context, corpID, corpSecret string) (TokenResponse, error) {
	if strings.TrimSpace(corpID) == "" || strings.TrimSpace(corpSecret) == "" {
		return TokenResponse{}, fmt.Errorf("%w: corp credentials required", ErrInvalidArgument)
	}
	q := url.Values{}
	q.Set("corpid", corpID)
	q.Set("corpsecret", corpSecret)
	var out TokenResponse
	u, err := c.endpoint("/cgi-bin/gettoken")
	if err != nil {
		return out, err
	}
	u += "?" + q.Encode()
	status, raw, e := c.do(ctx, "/cgi-bin/gettoken", func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	}, 1<<20)
	if e != nil {
		return out, e
	}
	if e = checkEnvelope(raw, status, "/cgi-bin/gettoken"); e != nil {
		return out, e
	}
	if e = json.Unmarshal(raw, &out); e != nil {
		return out, fmt.Errorf("%w: %v", ErrResponse, e)
	}
	if out.AccessToken == "" || out.ExpiresIn <= 0 {
		return out, fmt.Errorf("%w: token response missing access_token/expires_in", ErrResponse)
	}
	return out, nil
}

type UserResponse struct {
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
	UserID  string `json:"userid"`
	Name    string `json:"name"`
}

func (c *Client) GetUser(ctx context.Context, accessToken, userID string) (UserResponse, error) {
	if strings.TrimSpace(accessToken) == "" || strings.TrimSpace(userID) == "" {
		return UserResponse{}, fmt.Errorf("%w: token and userid required", ErrInvalidArgument)
	}
	q := url.Values{}
	q.Set("userid", userID)
	var out UserResponse
	err := c.doJSON(ctx, http.MethodGet, "/cgi-bin/user/get", accessToken, q, nil, &out)
	return out, err
}

// SyncRequest/SyncResponse map the fields documented by kf/sync_msg. Unknown
// fields remain available through Raw for forward compatibility.
type SyncRequest struct {
	Cursor      string `json:"cursor,omitempty"`
	Token       string `json:"token,omitempty"`
	Limit       int    `json:"limit,omitempty"`
	VoiceFormat int    `json:"voice_format,omitempty"`
	OpenKfID    string `json:"open_kfid,omitempty"`
}
type SyncText struct {
	Content string `json:"content"`
}
type SyncMessage struct {
	MsgID          string          `json:"msgid"`
	OpenKfID       string          `json:"open_kfid"`
	ExternalUserID string          `json:"external_userid"`
	SendTime       int64           `json:"send_time"`
	Origin         int             `json:"origin"`
	MsgType        string          `json:"msgtype"`
	Event          string          `json:"event,omitempty"`
	Text           *SyncText       `json:"text,omitempty"`
	Content        string          `json:"content,omitempty"`
	MediaID        string          `json:"media_id,omitempty"`
	Raw            json.RawMessage `json:"-"`
}
type SyncResponse struct {
	ErrCode    int           `json:"errcode"`
	ErrMsg     string        `json:"errmsg"`
	NextCursor string        `json:"next_cursor"`
	HasMore    int           `json:"has_more"`
	Messages   []SyncMessage `json:"msg_list"`
	MsgList    []SyncMessage `json:"-"`
}

func (c *Client) SyncMsg(ctx context.Context, accessToken string, req SyncRequest) (SyncResponse, error) {
	if strings.TrimSpace(accessToken) == "" {
		return SyncResponse{}, fmt.Errorf("%w: access token required", ErrInvalidArgument)
	}
	if req.Limit < 0 {
		return SyncResponse{}, fmt.Errorf("%w: negative limit", ErrInvalidArgument)
	}
	var out SyncResponse
	err := c.doJSON(ctx, http.MethodPost, "/cgi-bin/kf/sync_msg", accessToken, nil, req, &out)
	if err != nil {
		return out, err
	}
	out.MsgList = out.Messages
	if out.HasMore != 0 && out.HasMore != 1 {
		return out, fmt.Errorf("wecom: invalid has_more %d", out.HasMore)
	}
	for i := range out.Messages {
		if out.Messages[i].MsgID == "" {
			continue
		}
	}
	return out, nil
}

// SendRequest supports text and image messages. Text is sent through kf/send_msg;
// callers should use SendTextChunked for the 2,000 UTF-8 byte platform limit.
type SendRequest struct {
	ToUser   string     `json:"touser"`
	OpenKfID string     `json:"open_kfid"`
	MsgType  string     `json:"msgtype"`
	Text     *SendText  `json:"text,omitempty"`
	Image    *SendImage `json:"image,omitempty"`
	MsgID    string     `json:"msgid,omitempty"`
}
type SendText struct {
	Content string `json:"content"`
}
type SendImage struct {
	MediaID string `json:"media_id"`
}
type SendResponse struct {
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
	MsgID   string `json:"msgid,omitempty"`
}

func (c *Client) SendMsg(ctx context.Context, accessToken string, req SendRequest) (SendResponse, error) {
	if strings.TrimSpace(accessToken) == "" || strings.TrimSpace(req.ToUser) == "" || strings.TrimSpace(req.OpenKfID) == "" || strings.TrimSpace(req.MsgType) == "" {
		return SendResponse{}, fmt.Errorf("%w: send identity/msgtype required", ErrInvalidArgument)
	}
	var out SendResponse
	err := c.doJSON(ctx, http.MethodPost, "/cgi-bin/kf/send_msg", accessToken, nil, req, &out)
	return out, err
}

// ChunkText splits only at UTF-8 rune boundaries and never exceeds maxBytes.
// Invalid UTF-8 is rejected rather than silently replaced.
func ChunkText(text string, maxBytes int) ([]string, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("%w: maxBytes", ErrInvalidArgument)
	}
	if !utf8.ValidString(text) {
		return nil, fmt.Errorf("%w: invalid UTF-8", ErrInvalidArgument)
	}
	if text == "" {
		return []string{""}, nil
	}
	var out []string
	start := 0
	for start < len(text) {
		end := start
		for i, r := range text[start:] {
			n := utf8.RuneLen(r)
			if n < 0 {
				return nil, fmt.Errorf("%w: invalid UTF-8", ErrInvalidArgument)
			}
			if i+n > maxBytes {
				break
			}
			end = start + i + n
		}
		if end == start {
			return nil, fmt.Errorf("%w: maxBytes smaller than a rune", ErrInvalidArgument)
		}
		out = append(out, text[start:end])
		start = end
	}
	return out, nil
}

// SendTextChunked sends each chunk synchronously. A failed or unknown request
// stops immediately; no unrequested retry is attempted.
func (c *Client) SendTextChunked(ctx context.Context, accessToken string, req SendRequest, text string) ([]SendResponse, error) {
	chunks, err := ChunkText(text, 2000)
	if err != nil {
		return nil, err
	}
	out := make([]SendResponse, 0, len(chunks))
	for _, chunk := range chunks {
		req.Text = &SendText{Content: chunk}
		req.MsgID = ""
		r, e := c.SendMsg(ctx, accessToken, req)
		if e != nil {
			return out, e
		}
		out = append(out, r)
	}
	return out, nil
}

// SendText is a concise convenience alias retained for callers that only send
// text. It uses the contractual 2,000 UTF-8 byte chunk size.
func (c *Client) SendText(ctx context.Context, accessToken, toUser, openKfID, text string) ([]SendResponse, error) {
	return c.SendTextChunked(ctx, accessToken, SendRequest{ToUser: toUser, OpenKfID: openKfID, MsgType: "text"}, text)
}

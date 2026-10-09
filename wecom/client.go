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
	"net/http"
	"net/url"
	"strings"
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
}

func NewClient(baseURL, corpID, corpSecret string) *Client {
	return &Client{BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"), CorpID: corpID, CorpSecret: corpSecret, Timeout: 30 * time.Second, Logger: nopLogger{}}
}
func (c *Client) httpClient() *http.Client {
	if c != nil && c.HTTPClient != nil {
		return c.HTTPClient
	}
	timeout := 30 * time.Second
	if c != nil && c.Timeout > 0 {
		timeout = c.Timeout
	}
	return &http.Client{Timeout: timeout}
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
	req, e := http.NewRequestWithContext(ctx, method, u, body)
	if e != nil {
		return e
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, e := c.httpClient().Do(req)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if e != nil {
		return e
	}
	if err := checkEnvelope(raw, resp.StatusCode, path); err != nil {
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
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if e != nil {
		return out, e
	}
	resp, e := c.httpClient().Do(req)
	if e != nil {
		return out, e
	}
	defer resp.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if e != nil {
		return out, e
	}
	if e = checkEnvelope(raw, resp.StatusCode, "/cgi-bin/gettoken"); e != nil {
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

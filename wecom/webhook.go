package wecom

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CallbackEnvelope is the encrypted outer WeCom callback XML.
type CallbackEnvelope struct {
	XMLName    xml.Name `xml:"xml"`
	ToUserName string   `xml:"ToUserName"`
	AgentID    string   `xml:"AgentID"`
	Encrypt    string   `xml:"Encrypt"`
}

// Notification is a decoded callback. Event and message fields intentionally
// remain strings/maps at this boundary; sync_msg is authoritative for full
// customer content and callback notifications should only wake a sync worker.
type Notification struct {
	XMLName      xml.Name `xml:"xml"`
	ToUserName   string   `xml:"ToUserName"`
	FromUserName string   `xml:"FromUserName"`
	CreateTime   int64    `xml:"CreateTime"`
	MsgType      string   `xml:"MsgType"`
	Event        string   `xml:"Event"`
	ChangeType   string   `xml:"ChangeType"`
	// Token is the short-lived sync_msg pull token. It is a secret: it is
	// never logged, never serialised to JSON, and must only be persisted
	// sealed (state.Store.SaveSyncToken). The decrypted inner XML is not
	// retained at all.
	Token    string `xml:"Token" json:"-"`
	OpenKfID string `xml:"OpenKfId"`
}

// LogFields returns a redacted view safe for structured logs.
func (n Notification) LogFields() map[string]any {
	return map[string]any{"msg_type": n.MsgType, "event": n.Event, "open_kfid": n.OpenKfID, "has_token": n.Token != "", "create_time": n.CreateTime}
}

// String never prints the pull token.
func (n Notification) String() string {
	return fmt.Sprintf("Notification{MsgType:%s Event:%s OpenKfID:%s Token:[REDACTED]}", n.MsgType, n.Event, n.OpenKfID)
}

// GoString keeps %#v from leaking the token too.
func (n Notification) GoString() string { return n.String() }

type Webhook struct {
	Token          string
	AESKey         []byte
	Receiver       string
	MaxBodyBytes   int64
	Logger         Logger
	OnNotification func(context.Context, Notification) error
	Clock          func() time.Time
}

func NewWebhook(token, encodedAESKey, receiver string) (*Webhook, error) {
	key, err := DecodeAESKey(encodedAESKey)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(token) == "" || strings.TrimSpace(receiver) == "" {
		return nil, fmt.Errorf("wecom: callback token and receiver are required")
	}
	return &Webhook{Token: token, AESKey: key, Receiver: receiver, MaxBodyBytes: 1 << 20, Logger: nopLogger{}, Clock: time.Now}, nil
}
func (w *Webhook) log(event string, fields map[string]any) {
	if w != nil && w.Logger != nil {
		w.Logger.Log(event, fields)
	}
}

func (w *Webhook) VerifyEchostr(signature, timestamp, nonce, echostr string) (string, error) {
	if !VerifySignature(w.Token, timestamp, nonce, echostr, signature) {
		return "", ErrSignature
	}
	plain, err := Decrypt(w.AESKey, echostr, w.Receiver)
	if err != nil {
		return "", err
	}
	return plain, nil
}

// DecodeNotification verifies and decrypts an outer envelope without invoking
// application code. It is useful for tests and for handlers that need to queue
// a short wake-up transaction before returning HTTP 200.
func (w *Webhook) DecodeNotification(signature, timestamp, nonce string, body []byte) (Notification, error) {
	if len(body) == 0 {
		return Notification{}, fmt.Errorf("empty callback body")
	}
	var env CallbackEnvelope
	if err := xml.Unmarshal(body, &env); err != nil {
		return Notification{}, fmt.Errorf("decode callback envelope: %w", err)
	}
	if env.Encrypt == "" {
		return Notification{}, fmt.Errorf("missing Encrypt")
	}
	if !VerifySignature(w.Token, timestamp, nonce, env.Encrypt, signature) {
		return Notification{}, ErrSignature
	}
	inner, err := Decrypt(w.AESKey, env.Encrypt, w.Receiver)
	if err != nil {
		return Notification{}, err
	}
	var n Notification
	if err := xml.Unmarshal([]byte(inner), &n); err != nil {
		return Notification{}, fmt.Errorf("decode callback notification: %w", err)
	}
	return n, nil
}

func (w *Webhook) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if w == nil {
		http.Error(rw, "not configured", http.StatusInternalServerError)
		return
	}
	max := w.MaxBodyBytes
	if max <= 0 {
		max = 1 << 20
	}
	rw.Header().Set("Cache-Control", "no-store")
	q := req.URL.Query()
	sig := q.Get("msg_signature")
	if sig == "" {
		sig = q.Get("signature")
	}
	ts := q.Get("timestamp")
	nonce := q.Get("nonce")
	switch req.Method {
	case http.MethodGet:
		if sig == "" || ts == "" || nonce == "" || q.Get("echostr") == "" {
			http.Error(rw, "missing callback parameters", http.StatusBadRequest)
			return
		}
		plain, err := w.VerifyEchostr(sig, ts, nonce, q.Get("echostr"))
		if err != nil {
			w.log("wecom_callback_rejected", map[string]any{"method": "GET", "error": err})
			http.Error(rw, "invalid callback", http.StatusForbidden)
			return
		}
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte(plain))
		return
	case http.MethodPost:
		if sig == "" || ts == "" || nonce == "" {
			http.Error(rw, "missing callback parameters", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(io.LimitReader(req.Body, max+1))
		if err != nil || int64(len(body)) > max {
			http.Error(rw, "callback body too large", http.StatusRequestEntityTooLarge)
			return
		}
		n, err := w.DecodeNotification(sig, ts, nonce, body)
		if err != nil {
			w.log("wecom_callback_rejected", map[string]any{"method": "POST", "error": err})
			http.Error(rw, "invalid callback", http.StatusForbidden)
			return
		}
		if w.OnNotification != nil {
			if err := w.OnNotification(req.Context(), n); err != nil {
				w.log("wecom_callback_handler_error", map[string]any{"error": err})
				http.Error(rw, "callback not accepted", http.StatusInternalServerError)
				return
			}
		}
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte("success"))
		return
	default:
		rw.Header().Set("Allow", "GET, POST")
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// outerEnvelope is marshalled with encoding/xml so every field is escaped;
// no value is ever concatenated into markup (a "]]>" cannot break out).
type outerEnvelope struct {
	XMLName    xml.Name `xml:"xml"`
	ToUserName string   `xml:"ToUserName"`
	AgentID    string   `xml:"AgentID,omitempty"`
	Encrypt    string   `xml:"Encrypt"`
}

func marshalEnvelope(to, agentID, enc string) ([]byte, error) {
	return xml.Marshal(outerEnvelope{ToUserName: to, AgentID: agentID, Encrypt: enc})
}

// BuildCallbackBody wraps encrypted XML in the canonical outer envelope.
func (w *Webhook) BuildCallbackBody(innerXML string) ([]byte, error) {
	enc, err := Encrypt(w.AESKey, innerXML, w.Receiver)
	if err != nil {
		return nil, err
	}
	return marshalEnvelope(w.Receiver, "", enc)
}
func (w *Webhook) Signature(timestamp, nonce, encrypted string) string {
	return Signature(w.Token, timestamp, nonce, encrypted)
}

// ParseUnixTimestamp is kept strict because callback timestamps are external
// data and should not silently become zero on malformed input.
func ParseUnixTimestamp(v string) (time.Time, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}, fmt.Errorf("invalid timestamp")
	}
	return time.Unix(n, 0), nil
}

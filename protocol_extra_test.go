package bridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type errAdapter struct{ mode string }

func (a errAdapter) GetUser(context.Context, Binding, string) (Customer, error) {
	switch a.mode {
	case "e":
		return Customer{}, errors.New("down")
	case "m":
		return Customer{UserID: "other"}, nil
	case "eof":
		return Customer{}, io.EOF
	}
	return Customer{Name: "ok"}, nil
}
func (a errAdapter) SendText(context.Context, SendRequest) error {
	if a.mode == "send" {
		return errors.New("send")
	}
	return nil
}

type badReader struct{}

func (badReader) Read([]byte) (int, error) { return 0, errors.New("random") }

func req(s *Server, method, path, body string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rr
}

func TestProtocolErrorMatrix(t *testing.T) {
	s := testServer(errAdapter{})
	tok := tokenFor(t, s)
	for _, tc := range []struct {
		m, p, b string
		code    int
	}{{http.MethodPost, "/cgi-bin/gettoken?corpid=corp-1&corpsecret=secret-1", "", 405}, {http.MethodGet, "/bad", "", 404}, {http.MethodPost, "/cgi-bin/user/get?access_token=" + tok + "&userid=u1", "", 405}, {http.MethodGet, "/cgi-bin/user/get?access_token=x&userid=u1", "", 401}, {http.MethodGet, "/cgi-bin/user/get?access_token=" + tok + "&userid=a%20b", "", 400}, {http.MethodGet, "/cgi-bin/user/get?access_token=" + tok + "&userid=missing", "", 403}, {http.MethodPost, "/cgi-bin/message/send?access_token=" + tok, "", 400}, {http.MethodPost, "/cgi-bin/message/send?access_token=x", "{}", 401}, {http.MethodPost, "/wecom/callback", "", 400}} {
		if rr := req(s, tc.m, tc.p, tc.b); rr.Code != tc.code {
			t.Fatalf("%s %s got %d", tc.m, tc.p, rr.Code)
		}
	}
	if rr := req(s, http.MethodPost, "/cgi-bin/message/send?access_token="+tok, "{"); rr.Code != 400 {
		t.Fatal(rr.Code)
	}
	for _, body := range []string{`{"touser":"u1","msgtype":"text","agentid":1002,"text":{"content":""}}`, `{"touser":"u1","msgtype":"text","agentid":1001,"text":{"content":"x"}}`, `{"touser":"u1","msgtype":"image","agentid":1002,"text":{"content":"x"}}`, `{"touser":"missing","msgtype":"text","agentid":1002,"text":{"content":"x"}}`} {
		if rr := req(s, http.MethodPost, "/cgi-bin/message/send?access_token="+tok, body); rr.Code != 400 && rr.Code != 403 {
			t.Fatal(rr.Code)
		}
	}
	if rr := req(s, http.MethodPost, "/cgi-bin/message/send?access_token="+tok, `{"touser":"u1","msgtype":"text","agentid":1002,"text":{"content":"x"}}`); rr.Code != 200 {
		t.Fatal(rr.Code)
	}
	if rr := req(s, http.MethodPost, "/cgi-bin/message/send?access_token="+tok, `{"touser":"u1","msgtype":"markdown","agentid":"1002","markdown":{"content":"x"}}`); rr.Code != 200 {
		t.Fatal(rr.Code)
	}
	if rr := req(s, http.MethodGet, "/cgi-bin/user/get?access_token="+tok+"&userid=u1", ""); rr.Code != 200 {
		t.Fatal(rr.Code)
	}
	for _, raw := range []json.RawMessage{nil, []byte(`"x"`), []byte(`1.2`), []byte(`1e2`), []byte(`true`), []byte(`"1002"`)} {
		if _, err := parseAgentID(raw); err == nil && string(raw) != "\"1002\"" {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestProtocolAdapterAndTokenFailures(t *testing.T) {
	base := testBinding()
	s := NewServer(Config{Bindings: []Binding{base}, Customers: []Customer{{BindingID: base.ID, UserID: "u", Authorized: true}}, Adapter: errAdapter{mode: "e"}})
	tok := tokenFor(t, s)
	if rr := req(s, http.MethodGet, "/cgi-bin/user/get?access_token="+tok+"&userid=u", ""); rr.Code != 502 {
		t.Fatal(rr.Code)
	}
	s = NewServer(Config{Bindings: []Binding{base}, Customers: []Customer{{BindingID: base.ID, UserID: "u", Authorized: true}}, Adapter: errAdapter{mode: "m"}})
	tok = tokenFor(t, s)
	if rr := req(s, http.MethodGet, "/cgi-bin/user/get?access_token="+tok+"&userid=u", ""); rr.Code != 403 {
		t.Fatal(rr.Code)
	}
	s = NewServer(Config{Bindings: []Binding{base}, Customers: []Customer{{BindingID: base.ID, UserID: "u", Authorized: true}}, Adapter: errAdapter{mode: "send"}})
	tok = tokenFor(t, s)
	body := `{"touser":"u","msgtype":"text","agentid":1002,"text":{"content":"x"}}`
	if rr := req(s, http.MethodPost, "/cgi-bin/message/send?access_token="+tok, body); rr.Code != 502 {
		t.Fatal(rr.Code)
	}
	bad := base
	bad.Enabled = false
	s = NewServer(Config{Bindings: []Binding{bad}})
	if rr := req(s, http.MethodGet, "/cgi-bin/gettoken?corpid=corp-1&corpsecret=secret-1", ""); rr.Code != 401 {
		t.Fatal(rr.Code)
	}
	bad = base
	s = NewServer(Config{Bindings: []Binding{bad}, Random: badReader{}})
	if rr := req(s, http.MethodGet, "/cgi-bin/gettoken?corpid=corp-1&corpsecret=secret-1", ""); rr.Code != 503 {
		t.Fatal(rr.Code)
	}
}

func TestProtocolCallbackAndBuildErrors(t *testing.T) {
	s := testServer(MemoryAdapter{})
	if _, err := s.BuildCallback("missing", InboundMessage{}); err == nil {
		t.Fatal("missing")
	}
	b := testBinding()
	b.Enabled = false
	s = NewServer(Config{Bindings: []Binding{b}})
	if _, err := s.BuildCallback("b1", InboundMessage{}); err == nil {
		t.Fatal("disabled")
	}
	s = testServer(MemoryAdapter{})
	env, err := s.BuildCallback("b1", InboundMessage{FromUserName: "u", MsgType: "text", Content: "x"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		m, p, b string
		code    int
	}{{http.MethodGet, "/wecom/callback?timestamp=1&nonce=n&msg_signature=x", "", 400}, {http.MethodGet, "/wecom/callback?timestamp=1&nonce=n&msg_signature=x&echostr=x", "", 403}, {http.MethodPut, "/wecom/callback?timestamp=1&nonce=n&msg_signature=x", "", 405}, {http.MethodPost, "/wecom/callback?timestamp=1&nonce=n&msg_signature=x", "bad", 400}} {
		if rr := req(s, tc.m, tc.p, tc.b); rr.Code != tc.code {
			t.Fatalf("%d", rr.Code)
		}
	}
	s.cfg.MaxBodyBytes = 1
	if rr := req(s, http.MethodPost, "/wecom/callback?timestamp=1&nonce=n&msg_signature=x", "xxxx"); rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatal(rr.Code)
	}
	s.cfg.MaxBodyBytes = 1 << 20
	if rr := req(s, http.MethodPost, "/wecom/callback?timestamp="+env.Timestamp+"&nonce="+env.Nonce+"&msg_signature="+env.Signature, string(env.Body)); rr.Code != 200 {
		t.Fatal(rr.Code)
	}
	if rr := req(s, http.MethodPost, "/wecom/callback?timestamp="+env.Timestamp+"&nonce="+env.Nonce+"&signature="+env.Signature, string(env.Body)); rr.Code != 200 {
		t.Fatal(rr.Code)
	}
	cc := WeComCrypto{Token: testBinding().CallbackToken, AESKey: testBinding().CallbackAESKey, CorpID: testBinding().CorpID, Rand: bytes.NewReader(bytes.Repeat([]byte{4}, 64))}
	badEnc, _ := cc.Encrypt([]byte("<bad"))
	badBody, _ := marshalEnvelope(CallbackEnvelope{Encrypt: badEnc})
	badSig := cc.Signature("1", "n", badEnc)
	if rr := req(s, http.MethodPost, "/wecom/callback?timestamp=1&nonce=n&msg_signature="+badSig, string(badBody)); rr.Code != 400 {
		t.Fatal(rr.Code)
	}
	s = NewServer(Config{Bindings: []Binding{testBinding()}, OnInbound: func(context.Context, Binding, InboundMessage) error { return errors.New("x") }})
	env, _ = s.BuildCallback("b1", InboundMessage{FromUserName: "u"})
	if rr := req(s, http.MethodPost, "/wecom/callback?timestamp="+env.Timestamp+"&nonce="+env.Nonce+"&msg_signature="+env.Signature, string(env.Body)); rr.Code != 503 {
		t.Fatal(rr.Code)
	}
	if _, err := s.BuildCallback("b1", InboundMessage{FromUserName: "u"}); err == nil { /* callback still builds */
	}
	// Expired and revision-fenced tokens are rejected.
	s = testServer(MemoryAdapter{})
	tok := tokenFor(t, s)
	s.mu.Lock()
	tkn := s.tokens[tok]
	tkn.ExpiresAt = s.cfg.Clock().Add(-time.Minute)
	s.tokens[tok] = tkn
	s.mu.Unlock()
	if rr := req(s, http.MethodGet, "/cgi-bin/user/get?access_token="+tok+"&userid=u1", ""); rr.Code != 401 {
		t.Fatal(rr.Code)
	}
	s = testServer(MemoryAdapter{})
	tok = tokenFor(t, s)
	s.mu.Lock()
	b = s.bindings["b1"]
	b.Revision++
	s.bindings["b1"] = b
	s.mu.Unlock()
	if rr := req(s, http.MethodGet, "/cgi-bin/user/get?access_token="+tok+"&userid=u1", ""); rr.Code != 401 {
		t.Fatal(rr.Code)
	}
	// random nonce failure is surfaced by BuildCallback.
	s = NewServer(Config{Bindings: []Binding{testBinding()}, Random: badReader{}})
	if _, err := s.BuildCallback("b1", InboundMessage{FromUserName: "u"}); err == nil {
		t.Fatal("nonce")
	}
	_ = base64.StdEncoding
	_ = bytes.NewBuffer(nil)
}

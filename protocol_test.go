package bridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingAdapter struct {
	mu    sync.Mutex
	users map[string]Customer
	sends []SendRequest
	err   error
}

func (a *recordingAdapter) GetUser(_ context.Context, _ Binding, id string) (Customer, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	u, ok := a.users[id]
	if !ok {
		return Customer{}, io.EOF
	}
	return u, nil
}
func (a *recordingAdapter) SendText(_ context.Context, r SendRequest) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sends = append(a.sends, r)
	return a.err
}
func testBinding() Binding {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	return Binding{ID: "b1", CorpID: "corp-1", CorpSecret: "secret-1", AgentID: "1002", CallbackToken: "callback-token", CallbackAESKey: strings.TrimSuffix(key, "="), Enabled: true, Revision: 1}
}
func testServer(a Adapter) *Server {
	b := testBinding()
	return NewServer(Config{Bindings: []Binding{b}, Customers: []Customer{{BindingID: "b1", UserID: "u1", Name: "张三😀", Authorized: true}}, Adapter: a, TokenTTL: time.Hour, Random: bytes.NewReader(bytes.Repeat([]byte{7}, 4096))})
}
func tokenFor(t *testing.T, s *Server) string {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/cgi-bin/gettoken?corpid=corp-1&corpsecret=secret-1", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("token status %d", rr.Code)
	}
	var v map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v["access_token"].(string)
}
func TestProtocolTokenUserSend(t *testing.T) {
	a := &recordingAdapter{users: map[string]Customer{"u1": {UserID: "u1", Name: "外部昵称"}}}
	s := testServer(a)
	tok := tokenFor(t, s)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/cgi-bin/user/get?access_token="+tok+"&userid=u1", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "外部昵称") {
		t.Fatalf("user response %d %s", rr.Code, rr.Body)
	}
	body := `{"touser":"u1","msgtype":"text","agentid":1002,"text":{"content":"` + strings.Repeat("界", 1001) + `"}}`
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/cgi-bin/message/send?access_token="+tok, strings.NewReader(body))
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("send status %d %s", rr.Code, rr.Body)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.sends) != 2 || len([]byte(a.sends[0].Content)) > 2000 {
		t.Fatalf("chunks %#v", a.sends)
	}
}
func TestChallengeAndXML(t *testing.T) {
	s := testServer(MemoryAdapter{})
	env, err := s.BuildCallback("b1", InboundMessage{FromUserName: "u1", Content: "a<&😀", MsgType: "text", MsgID: "m1", CreateTime: 1700000000, AgentID: "1002"})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/wecom/callback?msg_signature="+env.Signature+"&timestamp="+env.Timestamp+"&nonce="+env.Nonce, bytes.NewReader(env.Body))
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 || rr.Body.String() != "success" {
		t.Fatalf("callback %d %s", rr.Code, rr.Body)
	}
	c := WeComCrypto{Token: testBinding().CallbackToken, AESKey: testBinding().CallbackAESKey, CorpID: testBinding().CorpID, Rand: bytes.NewReader(bytes.Repeat([]byte{2}, 100))}
	enc, err := c.Encrypt([]byte("echo"))
	if err != nil {
		t.Fatal(err)
	}
	sig := c.Signature("1", "n", enc)
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/wecom/callback?msg_signature="+sig+"&timestamp=1&nonce=n&echostr="+enc, nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 || rr.Body.String() != "echo" {
		t.Fatalf("challenge %d %s", rr.Code, rr.Body)
	}
}
func TestCryptoRejects(t *testing.T) {
	c := WeComCrypto{Token: "t", AESKey: testBinding().CallbackAESKey, CorpID: "c", Rand: bytes.NewReader(bytes.Repeat([]byte{3}, 100))}
	enc, err := c.Encrypt([]byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Decrypt(enc); err != nil {
		t.Fatal(err)
	}
	if _, err := (WeComCrypto{Token: "t", AESKey: testBinding().CallbackAESKey, CorpID: "other"}).Decrypt(enc); err == nil {
		t.Fatal("wrong receiver accepted")
	}
	if c.VerifySignature("bad", "1", "n", enc) {
		t.Fatal("bad signature accepted")
	}
}

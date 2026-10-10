package wecom

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/whysmx/wecom-kf-bridge/state"
)

type testLogger struct{ n atomic.Int32 }

func (l *testLogger) Log(string, map[string]any) { l.n.Add(1) }

func validKeyString() string {
	return strings.TrimRight(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32)), "=")
}

func TestClientAllMethodsAndErrors(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("access_token") == "bad" {
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"errcode":40001,"errmsg":"bad"}`))
			return
		}
		switch r.URL.Path {
		case "/cgi-bin/gettoken":
			_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok","access_token":"t","expires_in":100}`))
		case "/cgi-bin/user/get":
			_, _ = w.Write([]byte(`{"errcode":0,"userid":"u","name":"N"}`))
		case "/cgi-bin/kf/sync_msg":
			_, _ = w.Write([]byte(`{"errcode":0,"next_cursor":"n","has_more":0,"msg_list":[]}`))
		case "/cgi-bin/kf/send_msg":
			_, _ = w.Write([]byte(`{"errcode":0,"msgid":"m"}`))
		case "/cgi-bin/kf/account/add":
			_, _ = w.Write([]byte(`{"errcode":0,"open_kfid":"kf"}`))
		case "/cgi-bin/kf/account/update":
			_, _ = w.Write([]byte(`{"errcode":0}`))
		case "/cgi-bin/kf/account/del", "/cgi-bin/kf/add_contact_way", "/cgi-bin/kf/account/list", "/cgi-bin/kf/service_state/get", "/cgi-bin/kf/service_state/trans":
			_, _ = w.Write([]byte(`{"errcode":0,"account_list":[],"service_state":1}`))
		case "/cgi-bin/kf/servicer/list":
			_, _ = w.Write([]byte(`{"errcode":0,"servicer_list":[]}`))
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"errcode":0}`))
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "corp", "secret")
	ctx := context.Background()
	if _, e := c.GetToken(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e := c.GetTokenFor(ctx, "", "x"); e == nil {
		t.Fatal("empty credentials")
	}
	if _, e := c.GetUser(ctx, "t", "u"); e != nil {
		t.Fatal(e)
	}
	if _, e := c.GetUser(ctx, "bad", "u"); e == nil {
		t.Fatal("API error")
	}
	if _, e := c.SyncMsg(ctx, "t", SyncRequest{}); e != nil {
		t.Fatal(e)
	}
	if _, e := c.SendMsg(ctx, "t", SendRequest{ToUser: "u", OpenKfID: "kf", MsgType: "text", Text: &SendText{Content: "x"}}); e != nil {
		t.Fatal(e)
	}
	if _, e := c.SendText(ctx, "t", "u", "kf", strings.Repeat("x", 2001)); e != nil {
		t.Fatal(e)
	}
	if _, e := c.AddAccount(ctx, "t", AccountAddRequest{}); e == nil {
		t.Fatal("empty account")
	}
	if _, e := c.AddAccount(ctx, "t", AccountAddRequest{Name: "n"}); e != nil {
		t.Fatal(e)
	}
	if _, e := c.UpdateAccount(ctx, "t", AccountUpdateRequest{OpenKfID: "kf", Name: "n"}); e != nil {
		t.Fatal(e)
	}
	if _, e := c.UpdateAccount(ctx, "t", AccountUpdateRequest{}); e == nil {
		t.Fatal("empty update")
	}
	if e := c.DeleteAccount(ctx, "t", ""); e == nil {
		t.Fatal("empty delete")
	}
	if e := c.DeleteAccount(ctx, "t", "kf"); e != nil {
		t.Fatal(e)
	}
	if _, e := c.ListAccounts(ctx, "t", AccountListRequest{Offset: -1}); e == nil {
		t.Fatal("negative list")
	}
	if _, e := c.ListAccounts(ctx, "t", AccountListRequest{}); e != nil {
		t.Fatal(e)
	}
	if _, e := c.AddContactWay(ctx, "t", map[string]any{}); e != nil {
		t.Fatal(e)
	}
	if _, e := c.GetServiceState(ctx, "t", ServiceStateRequest{}); e == nil {
		t.Fatal("empty state")
	}
	if _, e := c.GetServiceState(ctx, "t", ServiceStateRequest{OpenKfID: "kf", ExternalUserID: "u"}); e != nil {
		t.Fatal(e)
	}
	if _, e := c.TransServiceState(ctx, "t", ServiceStateRequest{OpenKfID: "kf", ExternalUserID: "u"}); e != nil {
		t.Fatal(e)
	}
	if _, e := c.ListServicers(ctx, "t", ""); e == nil {
		t.Fatal("empty servicer")
	}
	if _, e := c.ListServicers(ctx, "t", "kf"); e != nil {
		t.Fatal(e)
	}
	if requests.Load() < 10 {
		t.Fatalf("requests %d", requests.Load())
	}
	if _, e := ChunkText("😀", 3); e == nil {
		t.Fatal("small rune")
	}
	if _, e := ChunkText("\xff", 5); e == nil {
		t.Fatal("invalid utf8")
	}
	if _, e := c.SendMsg(ctx, "t", SendRequest{}); e == nil {
		t.Fatal("invalid send")
	}
}
func TestClientEndpointResponseFailures(t *testing.T) {
	c := NewClient("", "", "")
	if _, e := c.GetToken(context.Background()); e == nil {
		t.Fatal("empty endpoint")
	}
	if _, e := c.GetUser(context.Background(), "t", "u"); e == nil {
		t.Fatal("empty endpoint")
	}
	if _, e := c.endpoint(":bad"); e == nil {
		t.Fatal("bad endpoint")
	}
	if e := checkEnvelope([]byte("{"), 200, "x"); e == nil {
		t.Fatal("malformed")
	}
	if e := checkEnvelope([]byte(`{"errcode":0}`), 500, "x"); e == nil {
		t.Fatal("http")
	}
	var ae *APIError
	if !errors.As(&APIError{Code: 1}, &ae) {
		t.Fatal("as")
	}
}

func TestOutboundAndWebhook(t *testing.T) {
	key := validKeyString()
	w, e := NewWebhook("tok", key, "corp")
	if e != nil {
		t.Fatal(e)
	}
	w.Logger = &testLogger{}
	w.Clock = func() time.Time { return time.Unix(123, 0) }
	inner := `<xml><ToUserName>corp</ToUserName><FromUserName>u</FromUserName><CreateTime>123</CreateTime><MsgType>text</MsgType><Content>x</Content></xml>`
	body, e := w.BuildCallbackBody(inner)
	if e != nil {
		t.Fatal(e)
	}
	sig := w.Signature("1", "n", extractEncrypt(body))
	req := httptest.NewRequest(http.MethodPost, "/?msg_signature="+sig+"&timestamp=1&nonce=n", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	w.OnNotification = func(context.Context, Notification) error { return nil }
	w.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("post %d", rr.Code)
	}
	n, e := w.DecodeNotification(sig, "1", "n", body)
	if e != nil || n.FromUserName != "u" {
		t.Fatalf("notification %#v %v", n, e)
	}
	if _, e := w.VerifyEchostr("bad", "1", "n", extractEncrypt(body)); e == nil {
		t.Fatal("bad sig")
	}
	msg := ClientMessage{ToUserName: "corp", FromUserName: "u", CreateTime: 123, MsgType: "text", Content: "a<&", MsgID: 1}
	cb, e := w.BuildClientCallback(msg, "1", "n")
	if e != nil || len(cb.Body) == 0 {
		t.Fatal(e)
	}
	if _, e := msg.XML(); e != nil {
		t.Fatal(e)
	}
	if _, e := BuildClientMessage("", "u", "x", "", time.Time{}, 0); e == nil {
		t.Fatal("invalid message")
	}
	if _, e := ParseUnixTimestamp("0"); e == nil {
		t.Fatal("timestamp")
	}
	if _, e := ParseUnixTimestamp("x"); e == nil {
		t.Fatal("timestamp")
	}
	for _, tc := range []struct {
		m string
		q string
	}{{http.MethodGet, ""}, {http.MethodPost, "?msg_signature=bad&timestamp=1&nonce=n"}, {http.MethodPut, ""}} {
		r := httptest.NewRequest(tc.m, "/"+tc.q, nil)
		x := httptest.NewRecorder()
		w.ServeHTTP(x, r)
	}
	bad := httptest.NewRequest(http.MethodPost, "/?msg_signature="+sig+"&timestamp=1&nonce=n", bytes.NewReader([]byte("bad")))
	x := httptest.NewRecorder()
	w.ServeHTTP(x, bad)
}
func extractEncrypt(body []byte) string {
	var e CallbackEnvelope
	_ = xmlUnmarshal(body, &e)
	return e.Encrypt
}
func xmlUnmarshal(b []byte, e *CallbackEnvelope) error {
	return xml.Unmarshal(b, e)
}

func TestCryptoRejectBranches(t *testing.T) {
	if _, e := DecodeAESKey(""); e == nil {
		t.Fatal()
	}
	if _, e := DecodeAESKey("bad"); e == nil {
		t.Fatal()
	}
	k := bytes.Repeat([]byte{1}, 32)
	if _, e := Encrypt(k, "\xff", "c"); e == nil {
		t.Fatal()
	}
	if _, e := Encrypt(k, "x", "c"); e != nil {
		t.Fatal(e)
	}
	enc, _ := Encrypt(k, "x", "c")
	for _, v := range []string{"", "bad", "AAAA"} {
		if _, e := Decrypt(k, v, "c"); e == nil {
			t.Fatal(v)
		}
	}
	if _, e := Decrypt(k, enc, "bad"); e == nil {
		t.Fatal()
	}
	if _, e := pkcs7Unpad32([]byte{1}); e == nil {
		t.Fatal()
	}
	if utf8Valid("\xff") {
		t.Fatal()
	}
	if VerifySignature("t", "1", "n", "e", "bad") {
		t.Fatal()
	}
}

// Keep database/sql referenced in this file to ensure state dependency remains
// covered when this package is tested in isolation.
var _ *sql.DB

func TestCustomerStateBridgeInvalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"errcode":0,"customer_list":[{"external_userid":"u","nickname":"N"}]}`)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "c", "s")
	if _, _, e := c.SyncCustomerProfiles(context.Background(), "t", nil, "e", "b", []string{"u"}); e == nil {
		t.Fatal("nil store")
	}
	if _, e := c.BatchGetCustomer(context.Background(), "", nil); e == nil {
		t.Fatal("empty batch")
	}
	_ = state.Customer{}
}

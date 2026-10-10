package wecom

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/whysmx/wecom-kf-bridge/state"
	_ "modernc.org/sqlite"
)

func TestClientHelpersAndErrorPaths(t *testing.T) {
	if got := (&APIError{Code: 2, Message: "x"}).Error(); !strings.Contains(got, "errcode=2") {
		t.Fatal(got)
	}
	if got := (&APIError{Code: 2, Message: "x", HTTPStatus: 500}).Error(); !strings.Contains(got, "http=500") {
		t.Fatal(got)
	}
	var nilErr *APIError
	if nilErr.Error() != "" || !errors.Is(&APIError{}, ErrResponse) {
		t.Fatal("api error methods")
	}
	if err := checkEnvelope([]byte(`{"errcode":0}`), 500, "/x"); err == nil {
		t.Fatal("status")
	}
	if err := checkEnvelope([]byte(`{"errcode":1,"errmsg":"bad"}`), 200, "/x"); err == nil {
		t.Fatal("code")
	}
	if err := checkEnvelope([]byte("!"), 200, "/x"); err == nil {
		t.Fatal("json")
	}
	var c *Client
	if c.httpClient() == nil {
		t.Fatal("nil client")
	}
	if _, err := c.endpoint("x"); err == nil {
		t.Fatal("nil endpoint")
	}
	if _, err := NewClient("http://[bad", "", "").endpoint("/"); err == nil {
		t.Fatal("parse endpoint")
	}
	if _, err := ChunkText("", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := ChunkText("abc", 0); err == nil {
		t.Fatal("max")
	}
}

func TestClientHTTPFailuresAndValidation(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/cgi-bin/gettoken", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"errcode":0,"access_token":"","expires_in":0}`))
	})
	mux.HandleFunc("/cgi-bin/user/get", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("{")) })
	mux.HandleFunc("/cgi-bin/kf/sync_msg", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"errcode":0,"has_more":2}`)) })
	mux.HandleFunc("/cgi-bin/kf/send_msg", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503); w.Write([]byte(`{"errcode":0}`)) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := NewClient(srv.URL, "c", "s")
	if _, err := c.GetToken(context.Background()); err == nil {
		t.Fatal("missing token")
	}
	if _, err := c.GetUser(context.Background(), "t", "u"); err == nil {
		t.Fatal("malformed user")
	}
	if _, err := c.SyncMsg(context.Background(), "t", SyncRequest{}); err == nil {
		t.Fatal("has_more")
	}
	if _, err := c.SendMsg(context.Background(), "t", SendRequest{ToUser: "u", OpenKfID: "k", MsgType: "text"}); err == nil {
		t.Fatal("http")
	}
	if _, err := c.SyncMsg(context.Background(), "", SyncRequest{}); err == nil {
		t.Fatal("token")
	}
	if _, err := c.SyncMsg(context.Background(), "t", SyncRequest{Limit: -1}); err == nil {
		t.Fatal("limit")
	}
	var nilCtx context.Context
	if _, err := c.GetTokenFor(nilCtx, "c", "s"); err == nil {
		t.Fatal("nil token context")
	}
	badToken := NewClient("http://example.invalid", "c", "s")
	badToken.HTTPClient = &http.Client{Transport: failingRoundTripper{}}
	if _, err := badToken.GetTokenFor(context.Background(), "c", "s"); err == nil {
		t.Fatal("token transport")
	}
	if err := c.doJSON(context.Background(), http.MethodGet, "/x", "", nil, nil, nil); err == nil {
		t.Fatal("unknown endpoint")
	}
	bad := NewClient("http://example.invalid", "c", "s")
	bad.HTTPClient = &http.Client{Transport: failingRoundTripper{}}
	if _, err := bad.GetUser(context.Background(), "t", "u"); err == nil {
		t.Fatal("transport")
	}
	if _, err := c.GetUser(nilCtx, "t", "u"); err == nil {
		t.Fatal("nil context")
	}
}

func TestSyncAndCustomerBranches(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/cgi-bin/kf/customer/batchget", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		w.Write([]byte(`{"errcode":0,"customer_list":[{"external_userid":"u","nickname":"N"}],"invalid_external_userid":["bad"]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := NewClient(srv.URL, "c", "s")
	if _, err := c.CustomerBatchGet(context.Background(), "", CustomerBatchGetRequest{ExternalUserIDs: []string{"u"}}); err == nil {
		t.Fatal("token")
	}
	if _, err := c.CustomerBatchGet(context.Background(), "t", CustomerBatchGetRequest{ExternalUserIDs: []string{"", "u"}}); err == nil {
		t.Fatal("empty id")
	}
	got, err := c.BatchGetCustomer(context.Background(), "t", []string{"u", "u"})
	if err != nil || len(got.Customers) != 1 || len(got.InvalidExternalUserIDs) != 1 {
		t.Fatalf("%#v %v", got, err)
	}
	if _, _, err := c.SyncCustomerProfiles(context.Background(), "t", nil, "e", "b", []string{"u"}); err == nil {
		t.Fatal("nil store")
	}
}

func TestOutboundWebhookBranches(t *testing.T) {
	key := validKeyString()
	w, err := NewWebhook("tok", key, "corp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewWebhook("", key, "corp"); err == nil {
		t.Fatal("token")
	}
	if _, err := NewWebhook("tok", key, ""); err == nil {
		t.Fatal("receiver")
	}
	msg := ClientMessage{ToUserName: "corp", FromUserName: "u", CreateTime: 1, MsgType: "text", MsgID: 1}
	if _, err := w.BuildClientCallback(msg, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := msg.XML(); err != nil {
		t.Fatal(err)
	}
	for _, m := range []ClientMessage{{FromUserName: "u", CreateTime: 1, MsgID: 1}, {ToUserName: "c", CreateTime: 1, MsgID: 1}, {ToUserName: "c", FromUserName: "u", MsgID: 1}} {
		if _, err := m.XML(); err == nil {
			t.Fatal("invalid xml")
		}
	}
	if _, err := BuildClientMessage("c", "u", "x", "", time.Now(), 0); err == nil {
		t.Fatal("msg id")
	}
	if _, err := w.DecodeNotification("", "", "", nil); err == nil {
		t.Fatal("empty callback")
	}
	if _, err := w.DecodeNotification("", "1", "n", []byte("<xml></xml>")); err == nil {
		t.Fatal("missing encrypt")
	}
	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/?msg_signature=x&timestamp=1&nonce=n", strings.NewReader(strings.Repeat("x", 2<<20))))
	if rr.Code != http.StatusForbidden && rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code %d", rr.Code)
	}
	rr = httptest.NewRecorder()
	var nilW *Webhook
	nilW.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != 500 {
		t.Fatal(rr.Code)
	}
}

type failingRoundTripper struct{}

func (failingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("transport")
}






func TestCustomerProfilesWithStateStore(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	st, err := state.New(db, state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_ = st.PutEnterprise(ctx, state.Enterprise{ID: "e", TenantID: "t", CorpID: "c", CredentialRef: "r"})
	_ = st.PutBinding(ctx, state.Binding{ID: "b", EnterpriseID: "e", OpenKfID: "kf", ProjectID: "p"})
	mux := http.NewServeMux()
	mux.HandleFunc("/cgi-bin/kf/customer/batchget", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"errcode":0,"customer_list":[{"external_userid":"u","nickname":"N"},{"external_userid":"empty","nickname":""},{"external_userid":"","nickname":"skip"}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	cs := NewClient(srv.URL, "c", "s")
	out, bad, err := cs.SyncCustomerProfiles(ctx, "t", st, "e", "b", []string{"u", "empty"})
	if err != nil || len(out) != 2 || len(bad) != 0 {
		t.Fatalf("%#v %#v %v", out, bad, err)
	}
	if _, _, err := cs.SyncCustomerProfiles(ctx, "", st, "e", "b", []string{"u"}); err == nil {
		t.Fatal("token")
	}
	if _, _, err := cs.SyncCustomerProfiles(ctx, "t", st, "missing", "missing", []string{"u"}); err == nil {
		t.Fatal("store error")
	}
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500); w.Write([]byte(`{"errcode":0}`)) }))
	defer errSrv.Close()
	if _, err := NewClient(errSrv.URL, "c", "s").CustomerBatchGet(ctx, "t", CustomerBatchGetRequest{ExternalUserIDs: []string{"u"}}); err == nil {
		t.Fatal("batch error")
	}
}

func TestWebhookHTTPBranches(t *testing.T) {
	w, _ := NewWebhook("tok", validKeyString(), "corp")
	inner := `<xml><ToUserName>corp</ToUserName><FromUserName>u</FromUserName><CreateTime>1</CreateTime><MsgType>text</MsgType></xml>`
	body, _ := w.BuildCallbackBody(inner)
	enc := extractEncrypt(body)
	sig := w.Signature("1", "n", enc)
	rr := httptest.NewRecorder()
	q := url.Values{"msg_signature": []string{sig}, "timestamp": []string{"1"}, "nonce": []string{"n"}, "echostr": []string{enc}}
	w.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/?"+q.Encode(), nil))
	if rr.Code != 200 {
		t.Fatalf("get %d", rr.Code)
	}
	w.OnNotification = func(context.Context, Notification) error { return errors.New("handler") }
	rr = httptest.NewRecorder()
	w.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/?msg_signature="+sig+"&timestamp=1&nonce=n", strings.NewReader(string(body))))
	if rr.Code != 500 {
		t.Fatalf("handler %d", rr.Code)
	}
	w.MaxBodyBytes = 2
	rr = httptest.NewRecorder()
	w.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/?msg_signature=x&timestamp=1&nonce=n", strings.NewReader("abcd")))
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("size %d", rr.Code)
	}
	if _, err := w.VerifyEchostr("bad", "1", "n", enc); err == nil {
		t.Fatal("sig")
	}
	if _, err := w.DecodeNotification(sig, "1", "n", []byte("<bad")); err == nil {
		t.Fatal("xml")
	}
	if _, err := w.DecodeNotification(sig, "1", "n", []byte("<xml><Encrypt>abc</Encrypt></xml>")); err == nil {
		t.Fatal("decrypt")
	}
	badInner, _ := w.BuildCallbackBody("<bad")
	badSig := w.Signature("1", "n", extractEncrypt(badInner))
	if _, err := w.DecodeNotification(badSig, "1", "n", badInner); err == nil {
		t.Fatal("inner xml")
	}
}

func TestCryptoMalformedBranches(t *testing.T) {
	k := []byte(strings.Repeat("k", 32))
	enc, err := Encrypt(k, "hello", "r")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decrypt([]byte(strings.Repeat("x", 32)), enc, "r"); err == nil {
		t.Fatal("wrong key")
	}
	if _, err := Decrypt(k, enc, "wrong"); err == nil {
		t.Fatal("receiver")
	}
	if _, err := pkcs7Unpad32([]byte(strings.Repeat("x", 16))); err == nil {
		t.Fatal("padding")
	}
	if _, err := Decrypt(k, "AAAA", ""); err == nil {
		t.Fatal("short ciphertext")
	}
	if got := pkcs7Pad32(make([]byte, 32)); len(got) != 64 {
		t.Fatal(len(got))
	}
	for i, s := range []string{"\x80", "\xc2", "\xe0\x80", "\xf0\x80\x80", "\xe2\x28\xa1"} {
		if utf8Valid(s) {
			t.Fatalf("accepted invalid %d", i)
		}
	}
}

func TestRemainingWecomBranches(t *testing.T) {
	var l nopLogger
	l.Log("x", nil)
	if _, err := NewWebhook("tok", "bad", "corp"); err == nil {
		t.Fatal("bad key")
	}
	if ts, err := ParseUnixTimestamp(" 10 "); err != nil || ts.Unix() != 10 {
		t.Fatal(err)
	}
	w, _ := NewWebhook("tok", validKeyString(), "corp")
	if _, err := w.BuildClientCallback(ClientMessage{}, "", ""); err == nil {
		t.Fatal("invalid callback")
	}
	// malformed output and JSON marshal failures in the shared HTTP helper.
	mux := http.NewServeMux()
	mux.HandleFunc("/bad", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"errcode":0}`)) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := NewClient(srv.URL, "c", "s")
	var out struct {
		X string `json:"x"`
	}
	if err := c.doJSON(context.Background(), http.MethodGet, "/bad", "", nil, nil, &out); err != nil {
		t.Fatal(err)
	}
	if err := c.doJSON(context.Background(), http.MethodPost, "/bad", "", nil, map[string]any{"bad": func() {}}, nil); err == nil {
		t.Fatal("marshal")
	}
	// Chunked send stops after the first upstream failure.
	count := 0
	smux := http.NewServeMux()
	smux.HandleFunc("/cgi-bin/kf/send_msg", func(w http.ResponseWriter, r *http.Request) {
		count++
		if count > 1 {
			w.WriteHeader(500)
			w.Write([]byte(`{"errcode":0}`))
			return
		}
		w.Write([]byte(`{"errcode":0,"msgid":"m"}`))
	})
	ss := httptest.NewServer(smux)
	defer ss.Close()
	sc := NewClient(ss.URL, "c", "s")
	if rs, err := sc.SendTextChunked(context.Background(), "t", SendRequest{ToUser: "u", OpenKfID: "k", MsgType: "text"}, strings.Repeat("x", 3000)); err == nil || len(rs) != 1 {
		t.Fatalf("%d %v", len(rs), err)
	}
}

func TestClientTinyEdgePaths(t *testing.T) {
	var n nopLogger
	n.Log("x", nil)
	c := NewClient("http://example.com/base", "c", "s")
	if u, err := c.endpoint("x"); err != nil || !strings.Contains(u, "/base/x") {
		t.Fatal(u, err)
	}
	if _, err := c.SendTextChunked(context.Background(), "", SendRequest{ToUser: "", OpenKfID: "", MsgType: ""}, "x"); err == nil {
		t.Fatal("send validation")
	}
	if _, err := c.TransServiceState(context.Background(), "", ServiceStateRequest{OpenKfID: "k", ExternalUserID: "u"}); err == nil {
		t.Fatal("transport")
	}
	key := validKeyString()
	w, _ := NewWebhook("t", key, "c")
	w.AESKey = []byte{1}
	if _, err := w.BuildCallbackBody("x"); err == nil {
		t.Fatal("bad key")
	}
}

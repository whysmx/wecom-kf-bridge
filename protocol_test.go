package bridge

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/whysmx/wecom-kf-bridge/state"
)

func TestSendRegistersOutboxBeforeSendingAndRecordsChunks(t *testing.T) {
	e := newEnv(t)
	tok := e.token()
	text := strings.Repeat("界", 1001) // 3003 bytes -> 2 chunks
	code, v := e.send(tok, e.cust.UID, "text", text)
	if code != 200 || errcode(v) != 0 {
		t.Fatalf("%d %v", code, v)
	}
	sends := e.ad.sent()
	if len(sends) != 2 || len(sends[0].Content) > 2000 || sends[0].OutboxID == "" || sends[0].Customer.ExternalUserID != "ext-1" {
		t.Fatalf("sends %+v", sends)
	}
	obs := e.outboxes()
	if len(obs) != 1 || obs[0].State != state.OutboxUpstreamAccepted || obs[0].ChunksSent != 2 || obs[0].ChunksTotal != 2 || obs[0].Body != text || obs[0].ID != sends[0].OutboxID {
		t.Fatalf("outbox %+v", obs)
	}
	ids, _ := e.st.OutboxChunkIDs(context.Background(), obs[0].ID)
	if len(ids) != 2 || ids[0] != "wx-1" {
		t.Fatalf("chunk ids %v", ids)
	}
}

func TestSendEnforcesGenerationAndTakeoverFence(t *testing.T) {
	e := newEnv(t)
	tok := e.token()
	ctx := context.Background()
	if _, err := e.st.BeginHandover(ctx, e.cust.ID, "human"); err != nil {
		t.Fatal(err)
	}
	if code, v := e.send(tok, e.cust.UID, "text", "late AI answer"); code != 403 || errcode(v) != ErrCodeDisabled {
		t.Fatalf("held send %d %v", code, v)
	}
	if obs := e.outboxes(); len(obs) != 1 || obs[0].State != state.OutboxBlocked || obs[0].ErrorCategory != state.BlockHeld {
		t.Fatalf("held request not recorded BLOCKED: %+v", obs)
	}
	n, err := e.st.RecoverCustomer(ctx, e.cust.ID, "resume")
	if err != nil {
		t.Fatal(err)
	}
	if code, v := e.send(tok, e.cust.UID, "text", "old uid"); code != 403 || errcode(v) != ErrCodeDisabled || v["errmsg"] != "stale generation" {
		t.Fatalf("old UID regained send rights: %d %v", code, v)
	}
	if code, v := e.do(http.MethodGet, "/cgi-bin/user/get?access_token="+tok+"&userid="+e.cust.UID, ""); code != 403 || errcode(v) != ErrCodeDisabled {
		t.Fatalf("old UID user/get %d %v", code, v)
	}
	if len(e.ad.sent()) != 0 {
		t.Fatal("something was sent")
	}
	if code, v := e.send(tok, n.UID, "text", "new generation"); code != 200 {
		t.Fatalf("new UID %d %v", code, v)
	}
}

func TestSendChecksOfficialServiceState(t *testing.T) {
	e := newEnv(t)
	tok := e.token()
	e.ad.svcErr = errBoom
	if code, v := e.send(tok, e.cust.UID, "text", "x"); code != 503 || errcode(v) != ErrCodeUnavailable {
		t.Fatalf("query failure must not send: %d %v", code, v)
	}
	if o := e.outboxes()[0]; o.State != state.OutboxBlocked || o.ErrorCategory != state.BlockStateQuery {
		t.Fatalf("%+v", o)
	}
	e.ad.svcErr, e.ad.svcState = nil, state.CustomerHuman
	if code, v := e.send(tok, e.cust.UID, "text", "x"); code != 403 || errcode(v) != ErrCodeDisabled {
		t.Fatalf("human state: %d %v", code, v)
	}
	c, _ := e.st.Customer(context.Background(), e.cust.ID)
	if c.State != state.CustomerHuman {
		t.Fatalf("handover not fenced locally: %s", c.State)
	}
	if len(e.ad.sent()) != 0 {
		t.Fatal("sent while human")
	}
	if c.WindowUsed != 0 {
		t.Fatalf("budget not released for unsent requests: %d", c.WindowUsed)
	}
}

func TestSendWindowAndBudget(t *testing.T) {
	e := newEnv(t)
	tok := e.token()
	for i := 0; i < 5; i++ {
		if code, v := e.send(tok, e.cust.UID, "text", "a"); code != 200 {
			t.Fatalf("send %d: %d %v", i, code, v)
		}
	}
	if code, v := e.send(tok, e.cust.UID, "text", "a"); errcode(v) != ErrCodePlatform || code != http.StatusTooManyRequests {
		t.Fatalf("budget: %d %v", code, v)
	}
	e.inbound("m-2") // customer writes again -> window reopens
	if code, _ := e.send(tok, e.cust.UID, "text", "a"); code != 200 {
		t.Fatal("window not reopened")
	}
	e.now = e.now.Add(49 * time.Hour)
	tok = e.token()
	if code, v := e.send(tok, e.cust.UID, "text", "a"); errcode(v) != ErrCodePlatform || v["errmsg"] != "send window closed" {
		t.Fatalf("expired window: %d %v", code, v)
	}
}

func TestSendErrorCodesAndPartialChunks(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		at    int
		code  int
		state string
		sent  int
		used  int
	}{
		{"rejected", &SendError{Kind: SendRejected, Err: errBoom}, 1, ErrCodePlatform, state.OutboxRejected, 0, 0},
		{"unavailable", &SendError{Kind: SendUnavailable, Err: errBoom}, 1, ErrCodeUnavailable, state.OutboxRejected, 0, 0},
		{"unknown", errBoom, 1, ErrCodeUnknown, state.OutboxUnknown, 0, 1},
		{"partial-unknown", errBoom, 2, ErrCodeUnknown, state.OutboxUnknown, 1, 2},
		{"partial-rejected", &SendError{Kind: SendRejected}, 3, ErrCodePlatform, state.OutboxRejected, 2, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.ad.sendErrAt, e.ad.sendErr = tc.at, tc.err
			tok := e.token()
			_, v := e.send(tok, e.cust.UID, "text", strings.Repeat("a", 4500)) // 3 chunks
			if errcode(v) != tc.code {
				t.Fatalf("errcode %v", v)
			}
			if tc.sent > 0 && !strings.Contains(v["errmsg"].(string), "partial") {
				t.Fatalf("partial not reported: %v", v)
			}
			o := e.outboxes()[0]
			if o.State != tc.state || o.ChunksSent != tc.sent || len(e.ad.sent()) != tc.sent {
				t.Fatalf("outbox %+v sends=%d", o, len(e.ad.sent()))
			}
			c, _ := e.st.Customer(context.Background(), e.cust.ID)
			if c.WindowUsed != tc.used {
				t.Fatalf("budget used %d want %d", c.WindowUsed, tc.used)
			}
		})
	}
	if (&SendError{}).Error() == "" || (&SendError{Err: errBoom}).Unwrap() != errBoom {
		t.Fatal("SendError")
	}
}

func TestTokenTableBoundedAndExpiring(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.MaxTokensPerBinding = 3; c.TokenTTL = time.Minute })
	var toks []string
	for i := 0; i < 10; i++ {
		toks = append(toks, e.token())
		e.now = e.now.Add(time.Second)
	}
	if n := e.srv.tokenCount(); n != 3 {
		t.Fatalf("tokens retained: %d", n)
	}
	if code, _ := e.do(http.MethodGet, "/cgi-bin/user/get?access_token="+toks[0]+"&userid="+e.cust.UID, ""); code != 401 {
		t.Fatal("evicted token still valid")
	}
	if code, _ := e.do(http.MethodGet, "/cgi-bin/user/get?access_token="+toks[9]+"&userid="+e.cust.UID, ""); code != 200 {
		t.Fatal("newest token invalid")
	}
	e.now = e.now.Add(2 * time.Minute)
	if code, _ := e.do(http.MethodGet, "/cgi-bin/user/get?access_token="+toks[9]+"&userid="+e.cust.UID, ""); code != 401 {
		t.Fatal("expired token valid")
	}
	e.token()
	if n := e.srv.tokenCount(); n != 1 {
		t.Fatalf("expired tokens not purged: %d", n)
	}
	g := newEnv(t, func(c *Config) { c.MaxTokens = 2 })
	for i := 0; i < 5; i++ {
		g.token()
		g.now = g.now.Add(time.Second)
	}
	if n := g.srv.tokenCount(); n != 2 {
		t.Fatalf("global cap: %d", n)
	}
}

func TestGetTokenCredentialChecks(t *testing.T) {
	e := newEnv(t)
	for _, q := range []string{"corpid=corp-1&corpsecret=secret-2", "corpid=corp-1&corpsecret=", "corpid=corp-1&corpsecret=secret-1x", "corpid=nope&corpsecret=secret-1", "corpid=&corpsecret="} {
		if code, v := e.do(http.MethodGet, "/cgi-bin/gettoken?"+q, ""); code != 401 || errcode(v) != ErrCodeAuth {
			t.Fatalf("%s accepted: %d", q, code)
		}
	}
	if !secretEqual("a", "a") || secretEqual("a", "ab") || secretEqual("", "x") {
		t.Fatal("secretEqual")
	}
	if code, _ := e.do(http.MethodPost, "/cgi-bin/gettoken", ""); code != 405 {
		t.Fatal("method")
	}
	bad := newEnv(t, func(c *Config) { c.Random = bytes.NewReader(nil) })
	if code, v := bad.do(http.MethodGet, "/cgi-bin/gettoken?corpid=corp-1&corpsecret=secret-1", ""); code != 503 || errcode(v) != ErrCodeUnavailable {
		t.Fatalf("random failure %d", code)
	}
	dis := newEnv(t, func(c *Config) { c.Bindings[0].Enabled = false })
	if code, _ := dis.do(http.MethodGet, "/cgi-bin/gettoken?corpid=corp-1&corpsecret=secret-1", ""); code != 401 {
		t.Fatal("disabled binding issued token")
	}
}

func TestUserGetAuthorizationAndNickname(t *testing.T) {
	e := newEnv(t)
	tok := e.token()
	get := func(uid string) (int, map[string]any) {
		return e.do(http.MethodGet, "/cgi-bin/user/get?access_token="+tok+"&userid="+uid, "")
	}
	if code, v := get(e.cust.UID); code != 200 || v["name"] != "外部昵称" || v["userid"] != e.cust.UID {
		t.Fatalf("%d %v", code, v)
	}
	e.ad.userErr = errBoom
	if _, v := get(e.cust.UID); v["name"] != "缓存昵称" {
		t.Fatalf("cached fallback %v", v)
	}
	if err := e.st.SetCustomerAuthorized(context.Background(), e.cust.ID, false); err != nil {
		t.Fatal(err)
	}
	code, v := get(e.cust.UID)
	if code != 403 || errcode(v) != ErrCodeForbidden || v["name"] != nil {
		t.Fatalf("unauthorized customer got profile: %d %v", code, v)
	}
	if code, v := e.send(tok, e.cust.UID, "text", "x"); code != 403 || errcode(v) != ErrCodeForbidden {
		t.Fatalf("unauthorized send %d %v", code, v)
	}
	if code, v := get("bcu_unknown_g1"); code != 403 || errcode(v) != ErrCodeForbidden {
		t.Fatalf("unknown %d %v", code, v)
	}
	for _, bad := range []string{"", "a@all", "a,b"} {
		if code, _ := get(bad); code != 400 {
			t.Fatalf("%q accepted", bad)
		}
	}
	if code, _ := e.do(http.MethodPost, "/cgi-bin/user/get", ""); code != 405 {
		t.Fatal("method")
	}
	if code, _ := e.do(http.MethodGet, "/cgi-bin/user/get?access_token=bad&userid=x", ""); code != 401 {
		t.Fatal("bad token")
	}
}

func TestMarkdownIsStrippedBeforeSending(t *testing.T) {
	e := newEnv(t)
	tok := e.token()
	if code, v := e.send(tok, e.cust.UID, "markdown", "## 结果\n**完成** 见 [文档](https://e.com)"); code != 200 {
		t.Fatalf("%d %v", code, v)
	}
	s := e.ad.sent()
	if len(s) != 1 || s[0].Content != "结果\n完成 见 文档 (https://e.com)" || s[0].MsgType != "text" {
		t.Fatalf("%+v", s)
	}
	if code, v := e.send(tok, e.cust.UID, "markdown", "```\n```"); code != 400 || errcode(v) != ErrCodeParameter {
		t.Fatalf("empty after strip %d %v", code, v)
	}
}

func TestSendValidation(t *testing.T) {
	e := newEnv(t)
	tok := e.token()
	p := "/cgi-bin/message/send?access_token=" + tok
	for body, code := range map[string]int{
		`{`: ErrCodeParameter,
		`{"touser":"a,b","msgtype":"text","agentid":1002,"text":{"content":"x"}}`:  ErrCodeParameter,
		`{"touser":"u","msgtype":"text","agentid":1003,"text":{"content":"x"}}`:    ErrCodeParameter,
		`{"touser":"u","msgtype":"text","agentid":1002.0,"text":{"content":"x"}}`:  ErrCodeParameter,
		`{"touser":"u","msgtype":"image","agentid":1002,"image":{"media_id":"m"}}`: ErrCodeUnsupported,
		`{"touser":"u","msgtype":"text","agentid":1002,"text":{"content":""}}`:     ErrCodeParameter,
		`{"touser":"u","msgtype":"text","agentid":"1002","text":{"content":"x"}}`:  ErrCodeForbidden,
	} {
		if _, v := e.do(http.MethodPost, p, body); errcode(v) != code {
			t.Fatalf("%s -> %v want %d", body, v, code)
		}
	}
	if code, _ := e.do(http.MethodGet, p, ""); code != 405 {
		t.Fatal("method")
	}
	if code, _ := e.do(http.MethodPost, "/cgi-bin/message/send?access_token=x", "{}"); code != 401 {
		t.Fatal("token")
	}
}

func TestNoCallbackRouteOnCompatServer(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/wecom/callback", "/wecom/callback/", "/cgi-bin/other"} {
		if code, v := e.do(http.MethodPost, p, "<xml/>"); code != 404 || errcode(v) != ErrCodeParameter {
			t.Fatalf("%s served: %d", p, code)
		}
	}
}

func TestServerRequiresStoreAndFailsClosedWithoutAdapter(t *testing.T) {
	if _, err := NewServer(Config{}); err == nil {
		t.Fatal("server without store")
	}
	e := newEnv(t)
	if _, err := NewServer(Config{Store: e.st, Bindings: []Binding{{ID: "x"}}}); err == nil {
		t.Fatal("binding without corp accepted")
	}
	if _, err := NewServer(Config{Store: e.st, Bindings: []Binding{testBinding(), testBinding()}}); err == nil {
		t.Fatal("duplicate corp accepted")
	}
	n := newEnv(t, func(c *Config) { c.Adapter = nil })
	tok := n.token()
	if _, v := n.send(tok, n.cust.UID, "text", "x"); errcode(v) != ErrCodeUnavailable {
		t.Fatalf("nil adapter must fail closed: %v", v)
	}
	var u unavailableAdapter
	if _, err := u.GetUser(context.Background(), Binding{}, Customer{}); err == nil {
		t.Fatal("unavailable GetUser")
	}
	if _, err := u.SendText(context.Background(), SendRequest{}); sendKind(err) != SendUnavailable {
		t.Fatal("unavailable SendText")
	}
}

func TestBuildCallbackEncryptsEscapedXML(t *testing.T) {
	e := newEnv(t)
	cb, err := e.srv.BuildCallback("b1", InboundMessage{FromUserName: "u1", Content: "a<&]]>😀", MsgType: "text", MsgID: "1", CreateTime: 1700000000, AgentID: "1002"})
	if err != nil {
		t.Fatal(err)
	}
	b := testBinding()
	c := WeComCrypto{Token: b.CallbackToken, AESKey: b.CallbackAESKey, CorpID: b.CorpID}
	if !c.VerifySignature(cb.Signature, cb.Timestamp, cb.Nonce, cb.Encrypted) {
		t.Fatal("signature")
	}
	plain, err := c.Decrypt(cb.Encrypted)
	if err != nil {
		t.Fatal(err)
	}
	m, err := unmarshalInbound(plain)
	if err != nil || m.Content != "a<&]]>😀" || m.ToUserName != "corp-1" {
		t.Fatalf("%+v %v", m, err)
	}
	if env, err := unmarshalEnvelope(cb.Body); err != nil || env.Encrypt != cb.Encrypted {
		t.Fatal(err)
	}
	if _, err := e.srv.BuildCallback("none", InboundMessage{}); err == nil {
		t.Fatal("unknown binding")
	}
	d := newEnv(t, func(c *Config) { c.Bindings[0].Enabled = false })
	if _, err := d.srv.BuildCallback("b1", InboundMessage{}); err == nil {
		t.Fatal("disabled binding")
	}
}

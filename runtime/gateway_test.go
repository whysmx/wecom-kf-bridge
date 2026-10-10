package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/whysmx/wecom-kf-bridge/state"
	"github.com/whysmx/wecom-kf-bridge/wecom"
)

func key43(b byte) string {
	return strings.TrimRight(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32)), "=")
}

// fakeWeCom is a synthetic WeChat KF API (not an official sandbox).
type fakeWeCom struct {
	mu    sync.Mutex
	sends []map[string]any
	pages []string
}

func (f *fakeWeCom) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/cgi-bin/gettoken", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"errcode":0,"access_token":"REAL","expires_in":7200}`)
	})
	mux.HandleFunc("/cgi-bin/kf/sync_msg", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		if req["token"] != "PULL" || req["open_kfid"] != "kf1" {
			t.Errorf("sync request %v", req)
		}
		if len(f.pages) == 0 {
			fmt.Fprint(w, `{"errcode":0,"next_cursor":"c9","has_more":0,"msg_list":[]}`)
			return
		}
		p := f.pages[0]
		f.pages = f.pages[1:]
		fmt.Fprint(w, p)
	})
	mux.HandleFunc("/cgi-bin/kf/service_state/get", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"errcode":0,"service_state":1}`)
	})
	mux.HandleFunc("/cgi-bin/kf/customer/batchget", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"errcode":0,"customer_list":[{"external_userid":"ext-a","nickname":"小海"}]}`)
	})
	mux.HandleFunc("/cgi-bin/kf/send_msg", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.sends = append(f.sends, req)
		f.mu.Unlock()
		fmt.Fprint(w, `{"errcode":0,"msgid":"wx-out-1"}`)
	})
	return mux
}

type ccClient struct {
	mu   sync.Mutex
	msgs []string
}

func TestGatewayEndToEndWiring(t *testing.T) {
	fw := &fakeWeCom{pages: []string{`{"errcode":0,"next_cursor":"c1","has_more":0,"msg_list":[
		{"msgid":"wx-1","open_kfid":"kf1","external_userid":"ext-a","send_time":1800000000,"origin":3,"msgtype":"text","text":{"content":"你好 <客服>"}},
		{"msgid":"wx-2","open_kfid":"kf1","external_userid":"ext-a","send_time":1800000001,"origin":5,"msgtype":"text","text":{"content":"人工回复"}}]}`}}
	wx := httptest.NewServer(fw.handler(t))
	defer wx.Close()

	// fake unmodified-protocol cc-connect receiver
	cc := &ccClient{}
	ccKey, _ := wecom.DecodeAESKey(key43(2))
	ccSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var env struct {
			Encrypt string `xml:"Encrypt"`
		}
		_ = xml.Unmarshal(body, &env)
		q := r.URL.Query()
		if !wecom.VerifySignature("vtok", q.Get("timestamp"), q.Get("nonce"), env.Encrypt, q.Get("msg_signature")) {
			t.Errorf("bad signature to cc-connect")
		}
		plain, err := wecom.Decrypt(ccKey, env.Encrypt, "bridge_a")
		if err != nil {
			t.Errorf("cc decrypt %v", err)
		}
		cc.mu.Lock()
		cc.msgs = append(cc.msgs, plain)
		cc.mu.Unlock()
		w.WriteHeader(200)
	}))
	defer ccSrv.Close()
	ccURL, _ := url.Parse(ccSrv.URL)
	_, port, _ := net.SplitHostPort(ccURL.Host)

	t.Setenv("T_MASTER", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	t.Setenv("T_SECRET", "real-secret")
	t.Setenv("T_CBTOK", "realtok")
	t.Setenv("T_CBAES", key43(1))
	t.Setenv("T_VSECRET", "vsecret")
	t.Setenv("T_VTOK", "vtok")
	t.Setenv("T_VAES", key43(2))
	raw := fmt.Sprintf(`{
	 "server":{"public_listen":"127.0.0.1:0"},
	 "storage":{"database":%q},
	 "security":{"master_key_env":"T_MASTER","callback_targets":[{"host":"127.0.0.1","port":%s,"allowed_cidrs":["127.0.0.0/8"]}]},
	 "workers":{"sync_interval_seconds":1,"delivery_interval_millis":20},
	 "wecom":{"api_base_url":%q,"customer_origins":[3],"service_state_map":{"1":"AI_ELIGIBLE","3":"HUMAN"}},
	 "enterprises":[{"id":"e1","tenant_key":"acme","corp_id":"wwcorp","secret_env":"T_SECRET","callback_token_env":"T_CBTOK","callback_aes_key_env":"T_CBAES"}],
	 "bindings":[{"id":"b1","enterprise_id":"e1","open_kfid":"kf1","project_id":"p1","virtual_corp_id":"bridge_a","agent_id":"1000002","virtual_secret_env":"T_VSECRET","callback_token_env":"T_VTOK","callback_aes_key_env":"T_VAES","callback_url":%q}]}`,
		t.TempDir()+"/g.db", port, wx.URL, ccSrv.URL+"/wecom/callback")
	cfg, err := ParseConfig([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g, err := Build(ctx, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	pub := httptest.NewServer(g.Handler)
	defer pub.Close()
	var wg sync.WaitGroup
	for _, w := range g.Workers {
		wg.Add(1)
		go func(w Worker) { defer wg.Done(); w.Run(ctx) }(w)
	}

	// 1. official notification at the tenant path
	wh, _ := wecom.NewWebhook("realtok", key43(1), "wwcorp")
	body, _ := wh.BuildCallbackBody(`<xml><ToUserName>wwcorp</ToUserName><MsgType>event</MsgType><Event>kf_msg_or_event</Event><Token>PULL</Token><OpenKfId>kf1</OpenKfId></xml>`)
	var env struct {
		Encrypt string `xml:"Encrypt"`
	}
	_ = xml.Unmarshal(body, &env)
	q := url.Values{"msg_signature": {wh.Signature("1", "n", env.Encrypt)}, "timestamp": {"1"}, "nonce": {"n"}}
	resp, err := http.Post(pub.URL+"/webhooks/wechat-kf/acme?"+q.Encode(), "text/xml", bytes.NewReader(body))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("callback %v %v", resp, err)
	}
	if r, _ := http.Post(pub.URL+"/wecom/callback?"+q.Encode(), "text/xml", bytes.NewReader(body)); r.StatusCode == 200 {
		t.Fatal("legacy callback path served")
	}

	// 2. worker syncs and delivers only the customer message to cc-connect
	deadline := time.Now().Add(5 * time.Second)
	for {
		cc.mu.Lock()
		n := len(cc.msgs)
		cc.mu.Unlock()
		if n >= 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cc.mu.Lock()
	msgs := append([]string(nil), cc.msgs...)
	cc.mu.Unlock()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "你好 &lt;客服&gt;") || !strings.Contains(msgs[0], "<CreateTime>1800000000</CreateTime>") || !strings.Contains(msgs[0], "<MsgId>1</MsgId>") {
		t.Fatalf("cc-connect got %v", msgs)
	}
	uid := between(msgs[0], "<FromUserName>", "</FromUserName>")
	if !strings.HasPrefix(uid, "bcu_") || strings.Contains(msgs[0], "ext-a") {
		t.Fatalf("uid %q leaks external id", uid)
	}

	// 3. cc-connect answers via the compatible API
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	r, _ := http.Get(pub.URL + "/cgi-bin/gettoken?corpid=bridge_a&corpsecret=vsecret")
	_ = json.NewDecoder(r.Body).Decode(&tok)
	r, _ = http.Get(pub.URL + "/cgi-bin/user/get?access_token=" + tok.AccessToken + "&userid=" + uid)
	var user map[string]any
	_ = json.NewDecoder(r.Body).Decode(&user)
	if user["name"] != "小海" {
		t.Fatalf("user/get %v", user)
	}
	r, _ = http.Post(pub.URL+"/cgi-bin/message/send?access_token="+tok.AccessToken, "application/json", strings.NewReader(`{"touser":"`+uid+`","msgtype":"markdown","agentid":1000002,"markdown":{"content":"**答复**"}}`))
	var sendResp map[string]any
	_ = json.NewDecoder(r.Body).Decode(&sendResp)
	if sendResp["errcode"] != float64(0) {
		t.Fatalf("send %v", sendResp)
	}
	fw.mu.Lock()
	if len(fw.sends) != 1 || fw.sends[0]["touser"] != "ext-a" || fw.sends[0]["open_kfid"] != "kf1" || fw.sends[0]["text"].(map[string]any)["content"] != "答复" {
		t.Fatalf("wecom send %v", fw.sends)
	}
	fw.mu.Unlock()
	if r, _ := http.Get(pub.URL + "/healthz"); r == nil {
		t.Fatal("health not mounted")
	}
	cancel()
	wg.Wait()
	// human message was recorded but ignored
	m, err := g.Store.InboxByExternalID(context.Background(), "scope:e1:kf1", "wx-2")
	if err != nil || m.State != state.InboxIgnored {
		t.Fatalf("%+v %v", m, err)
	}
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	s = s[i+len(a):]
	if j := strings.Index(s, b); j >= 0 {
		return s[:j]
	}
	return ""
}

func TestConfigValidation(t *testing.T) {
	if _, err := ParseConfig([]byte(`{"nope":1}`)); err == nil {
		t.Fatal("unknown field accepted")
	}
	_, err := ParseConfig([]byte(`{"storage":{"database":"x"},"wecom":{"api_base_url":"https://qyapi.weixin.qq.com"}}`))
	if err == nil || !strings.Contains(err.Error(), "customer_origins") {
		t.Fatalf("origin policy not required: %v", err)
	}
	c := Config{}
	c.Security.CallbackTargets = []CallbackTarget{{Host: "cc.internal", Port: 8081}}
	for raw, ok := range map[string]bool{"http://cc.internal:8081/wecom": true, "http://u:p@cc.internal:8081/": false, "ftp://cc.internal:8081": false, "http://other:8081": false, "http://cc.internal:9999": false} {
		if (c.CheckCallbackURL(raw) == nil) != ok {
			t.Fatalf("%s", raw)
		}
	}
	if _, err := LoadConfig("/nonexistent.json"); err == nil {
		t.Fatal("missing file")
	}
	if _, err := Build(context.Background(), Config{Security: struct {
		MasterKeyEnv     string           `json:"master_key_env"`
		CallbackTargets  []CallbackTarget `json:"callback_targets"`
		TokenTTLSeconds  int              `json:"token_ttl_seconds"`
		MaxTokensPerBind int              `json:"max_tokens_per_binding"`
	}{MasterKeyEnv: "T_UNSET_MASTER"}}, nil); err == nil {
		t.Fatal("missing master key accepted")
	}
}

func TestCallbackDialerRejectsDisallowedAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	c := Config{}
	c.Security.CallbackTargets = []CallbackTarget{{Host: "127.0.0.1", Port: 1, AllowedCIDRs: []string{"10.0.0.0/8"}}}
	_, err := callbackHTTPClient(c).Get(srv.URL)
	if err == nil || !provablyNotDelivered(err) {
		t.Fatalf("disallowed address dialed: %v", err)
	}
}

func TestSendErrorClassification(t *testing.T) {
	if e := classifySendError(&wecom.APIError{Code: 95018, HTTPStatus: 200}); !strings.Contains(e.Error(), "95018") {
		t.Fatal(e)
	}
	if !tokenRejected(&wecom.APIError{Code: 42001}) || tokenRejected(fmt.Errorf("x")) {
		t.Fatal("token rejected")
	}
}

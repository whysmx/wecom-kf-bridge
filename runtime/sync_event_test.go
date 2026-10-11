package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
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

// Live bug (v0.1.5): one event-object / undecodable msg_list item failed the
// whole sync page. A page with an enter_session event, one bad item and three
// texts must deliver all three texts to cc-connect.
func TestSyncPageWithBadMessageStillDelivers(t *testing.T) {
	fw := &fakeWeCom{pages: []string{`{"errcode":0,"next_cursor":"c1","has_more":0,"msg_list":[
		{"msgid":"ev-1","send_time":1800000000,"origin":4,"msgtype":"event","event":{"event_type":"enter_session","open_kfid":"kf1","external_userid":"ext-a","scene":"1","welcome_code":"w"}},
		{"msgid":"bad-1","msgtype":"text","send_time":"oops","text":{"content":"x"}},
		{"msgid":"t1","open_kfid":"kf1","external_userid":"ext-a","send_time":1800000001,"origin":3,"msgtype":"text","text":{"content":"第一条"}},
		{"msgid":"t2","open_kfid":"kf1","external_userid":"ext-b","send_time":1800000002,"origin":3,"msgtype":"text","text":{"content":"第二条"}},
		{"msgid":"t3","open_kfid":"kf1","external_userid":"ext-c","send_time":1800000003,"origin":3,"msgtype":"text","text":{"content":"第三条"}}]}`},
		// Extra sync rounds (startup / periodic timer / webhook, whose count
		// depends on scheduling under -race) see no new messages and keep the
		// cursor at c1, like the real API. Determinism comes from the fake,
		// not from timing.
		idle: "c1"}
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
	 "wecom":{"allow_insecure_http":true,"api_base_url":%q,"customer_origins":[3],"service_state_map":{"1":"AI_ELIGIBLE","3":"HUMAN"}},
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

	deadline := time.Now().Add(5 * time.Second)
	for {
		cc.mu.Lock()
		n := len(cc.msgs)
		cc.mu.Unlock()
		if n >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cc.mu.Lock()
	all := strings.Join(cc.msgs, "\n")
	count := len(cc.msgs)
	cc.mu.Unlock()
	if count != 3 {
		t.Fatalf("cc-connect got %d messages: %s", count, all)
	}
	for _, want := range []string{"第一条", "第二条", "第三条"} {
		if !strings.Contains(all, want) {
			t.Fatalf("missing %s in %s", want, all)
		}
	}
	// Assert while workers still run: reading right after cancel() can land
	// on a pooled connection whose in-flight worker query was interrupted
	// (SQLITE_INTERRUPT); the state asserted here is already final.
	for id, cat := range map[string]string{"ev-1": wecom.CategoryEvent, "bad-1": wecom.CategoryUnparseable} {
		m, err := g.Store.InboxByExternalID(context.Background(), "scope:e1:kf1", id)
		if err != nil || m.State != state.InboxIgnored || m.ErrorCategory != cat {
			t.Fatalf("%s %+v %v", id, m, err)
		}
	}
	if sc, _ := g.Store.Scope(context.Background(), "scope:e1:kf1"); sc.Cursor != "c1" {
		t.Fatalf("cursor not advanced: %+v", sc)
	}
	// The page was requested from the start, and every later round resumed
	// from the committed c1 (never re-read the bad page from "").
	fw.mu.Lock()
	cursors := append([]string(nil), fw.cursors...)
	fw.mu.Unlock()
	if len(cursors) == 0 || cursors[0] != "" {
		t.Fatalf("cursors %q", cursors)
	}
	for _, c := range cursors[1:] {
		if c != "c1" {
			t.Fatalf("later round did not resume from c1: %q", cursors)
		}
	}
	cancel()
	wg.Wait()
}

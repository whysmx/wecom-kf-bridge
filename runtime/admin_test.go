package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	bridge "github.com/whysmx/wecom-kf-bridge"
	"golang.org/x/crypto/bcrypt"
)

const adminOrigin = "https://admin.test"
const adminPassword = "s3cret-admin-pw"

// fakeWeComAPI serves the official endpoints the console uses.
func fakeWeComAPI(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cgi-bin/gettoken":
			w.Write([]byte(`{"errcode":0,"access_token":"REAL-TOKEN","expires_in":7200}`))
		case "/cgi-bin/kf/account/list":
			w.Write([]byte(`{"errcode":0,"account_list":[{"open_kfid":"kf1","name":"售前"},{"open_kfid":"kf2","name":"售后"}]}`))
		case "/cgi-bin/kf/add_contact_way":
			w.Write([]byte(`{"errcode":0,"url":"https://work.weixin.qq.com/kf/abc"}`))
		case "/cgi-bin/kf/account/add":
			w.Write([]byte(`{"errcode":0,"open_kfid":"kf3"}`))
		case "/cgi-bin/kf/account/update", "/cgi-bin/kf/account/del":
			w.Write([]byte(`{"errcode":0}`))
		default:
			w.Write([]byte(`{"errcode":0}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeCC answers WeCom URL verification like cc-connect would, using the
// virtual callback token/AES key it was configured with.
func fakeCC(t *testing.T, token, aes, corp string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		c := bridge.WeComCrypto{Token: token, AESKey: aes, CorpID: corp}
		if !bridge.VerifySignature(token, q.Get("timestamp"), q.Get("nonce"), q.Get("echostr"), q.Get("msg_signature")) {
			http.Error(w, "bad sig", 403)
			return
		}
		plain, err := c.Decrypt(q.Get("echostr"))
		if err != nil {
			http.Error(w, "bad", 400)
			return
		}
		w.Write(plain)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func adminGatewayConfig(t *testing.T, ccPort int) Config {
	cfg := gatewayConfig(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte(adminPassword), bcrypt.MinCost)
	t.Setenv("G_ADMIN_HASH", string(hash))
	cfg.Admin = AdminConfig{Listen: "127.0.0.1:0", Origin: adminOrigin, PasswordHashEnv: "G_ADMIN_HASH", CompanyName: "测试企业"}
	cfg.Security.CallbackTargets = append(cfg.Security.CallbackTargets, CallbackTarget{Host: "127.0.0.1", Port: ccPort, AllowedCIDRs: []string{"127.0.0.0/8"}})
	return cfg
}

type adminClient struct {
	t      *testing.T
	h      http.Handler
	cookie string
	csrf   string
	n      int
}

func (c *adminClient) do(method, path string, form url.Values) *httptest.ResponseRecorder {
	var r *http.Request
	if form != nil {
		r = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", adminOrigin)
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if c.cookie != "" {
		r.AddCookie(&http.Cookie{Name: "wkb_admin", Value: c.cookie})
	}
	w := httptest.NewRecorder()
	c.h.ServeHTTP(w, r)
	return w
}

func (c *adminClient) post(path string, form url.Values) *httptest.ResponseRecorder {
	if form == nil {
		form = url.Values{}
	}
	c.n++
	form.Set("csrf_token", c.csrf)
	form.Set("idempotency_key", "rt-key-"+strconv.Itoa(c.n)+"-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	return c.do("POST", path, form)
}

func adminLogin(t *testing.T, h http.Handler) *adminClient {
	c := &adminClient{t: t, h: h}
	w := c.do("POST", "/admin/login", url.Values{"password": {adminPassword}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("login %d", w.Code)
	}
	c.cookie = w.Result().Cookies()[0].Value
	m := regexp.MustCompile(`name="csrf_token" value="([0-9a-f]+)"`).FindStringSubmatch(c.do("GET", "/admin/", nil).Body.String())
	if m == nil {
		t.Fatal("csrf")
	}
	c.csrf = m[1]
	return c
}

func gettoken(t *testing.T, h http.Handler, corp, secret string) int {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/cgi-bin/gettoken?corpid="+url.QueryEscape(corp)+"&corpsecret="+url.QueryEscape(secret), nil))
	var out struct {
		ErrCode int `json:"errcode"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 200 {
		return -1
	}
	return out.ErrCode
}

var exportRe = regexp.MustCompile(`corp_id = &#34;([^&]+)&#34;\s*corp_secret = &#34;([^&]+)&#34;`)

func TestAdminConsoleEndToEnd(t *testing.T) {
	cc := fakeCC(t, gatewayEnv["G_VTOK"], gatewayEnv["G_VAES"], "bridge_a")
	ccPort := cc.Listener.Addr().(*net.TCPAddr).Port
	cfg := adminGatewayConfig(t, ccPort)
	cfg.WeCom.APIBaseURL = fakeWeComAPI(t).URL
	g, err := Build(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if g.Admin == nil {
		t.Fatal("admin not built")
	}
	// the console is not on the public handler, and vice versa
	w := httptest.NewRecorder()
	g.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/admin/login", nil))
	if strings.Contains(w.Body.String(), "登录") {
		t.Fatal("admin reachable on public listener")
	}
	c := adminLogin(t, g.Admin)
	for _, p := range []string{"/admin/", "/admin/settings"} {
		body := c.do("GET", p, nil).Body.String()
		for _, secret := range []string{gatewayEnv["G_VAES"], gatewayEnv["G_CBAES"], "REAL-TOKEN", "wwcorp"} {
			if strings.Contains(body, secret) {
				t.Fatalf("%s leaks %q", p, secret)
			}
		}
	}
	if !strings.Contains(c.do("GET", "/admin/settings", nil).Body.String(), "G_SECRET") {
		t.Fatal("settings env names")
	}
	// official account sync through the server-side token
	c.post("/admin/accounts/sync", nil)
	if a, err := g.Store.Account(context.Background(), "kf2"); err != nil || a.Name != "售后" {
		t.Fatal("sync", a, err)
	}
	a, _ := g.Store.Account(context.Background(), "kf2")
	c.post("/admin/accounts/kf2/link", url.Values{"revision": {strconv.FormatInt(a.Revision, 10)}})
	if a, _ := g.Store.Account(context.Background(), "kf2"); a.URL == "" {
		t.Fatal("link")
	}
	c.post("/admin/accounts/create", url.Values{"name": {"新客服"}, "media_id": {"MEDIA"}})
	if _, err := g.Store.Account(context.Background(), "kf3"); err != nil {
		t.Fatal("create", err)
	}
	// rebind b1 to the fake cc-connect and verify the challenge
	if w := c.post("/admin/stepup", url.Values{"password": {adminPassword}}); w.Code != http.StatusSeeOther {
		t.Fatal("step-up")
	}
	cb := cc.URL + "/wecom"
	if w := c.post("/admin/bindings/b1/rebind", url.Values{"revision": {"1"}, "project_id": {"p1"}, "callback_url": {cb}}); w.Code != http.StatusSeeOther {
		t.Fatalf("rebind %d %s", w.Code, w.Body)
	}
	c.post("/admin/bindings/b1/verify", nil)
	if !strings.Contains(c.do("GET", "/admin/bindings", nil).Body.String(), "回调挑战验证通过") {
		t.Fatal("verify failed")
	}
	c.post("/admin/settings/test", url.Values{"url": {cc.URL + "/"}})
	if !strings.Contains(c.do("GET", "/admin/settings", nil).Body.String(), "连接测试通过") {
		t.Fatal("connection test")
	}
	// create b2 for kf2, export once, compatible API accepts the creds
	if w := c.post("/admin/bindings/create", url.Values{"id": {"b2"}, "open_kfid": {"kf2"}, "project_id": {"p2"}, "callback_url": {cb}, "agent_id": {"1000003"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("create binding %d %s", w.Code, w.Body)
	}
	c.post("/admin/bindings/b2/export", nil)
	m := exportRe.FindStringSubmatch(c.do("GET", "/admin/bindings", nil).Body.String())
	if m == nil {
		t.Fatal("export not shown")
	}
	corp, secret := m[1], m[2]
	if gettoken(t, g.Handler, corp, secret) != 0 {
		t.Fatal("new binding credentials refused")
	}
	// rotate: the exported credentials stop working immediately
	c.post("/admin/bindings/b2/rotate", url.Values{"revision": {"1"}})
	if gettoken(t, g.Handler, corp, secret) == 0 {
		t.Fatal("old credentials still valid after rotation")
	}
	c.post("/admin/bindings/b2/export", nil)
	m = exportRe.FindStringSubmatch(c.do("GET", "/admin/bindings", nil).Body.String())
	corp2, secret2 := m[1], m[2]
	// disable then restart: SQLite wins over the JSON bootstrap
	c.post("/admin/bindings/b2/disable", url.Values{"revision": {"2"}})
	if gettoken(t, g.Handler, corp2, secret2) == 0 {
		t.Fatal("disabled binding issued a token")
	}
	c.post("/admin/bindings/b2/enable", url.Values{"revision": {"3"}})
	g.Close()
	g, err = Build(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if b, _ := g.Store.Binding(context.Background(), "b1"); b.CallbackURL != cb || b.Revision != 2 {
		t.Fatal("config bootstrap overwrote console changes", b)
	}
	if gettoken(t, g.Handler, corp2, secret2) != 0 {
		t.Fatal("console-created binding not restored with its rotated credentials")
	}
	if gettoken(t, g.Handler, "bridge_a", gatewayEnv["G_VSECRET"]) != 0 {
		t.Fatal("config binding credentials")
	}
	// old sessions do not survive the restart
	c.h = g.Admin
	if w := c.do("GET", "/admin/", nil); w.Code != http.StatusSeeOther {
		t.Fatal("session survived restart")
	}
}

func TestAdminRuntimeEdges(t *testing.T) {
	cfg := adminGatewayConfig(t, 9)
	g, err := Build(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	rt := consoleRuntime{g: g, cfg: cfg}
	ctx := context.Background()
	if rt.TestURL(ctx, "http://127.0.0.1:9/") == nil {
		t.Fatal("closed port reported ok")
	}
	if rt.TestURL(ctx, "://bad") == nil {
		t.Fatal("bad url")
	}
	redirect := httptest.NewServer(http.RedirectHandler("http://169.254.169.254/", http.StatusFound))
	defer redirect.Close()
	port := redirect.Listener.Addr().(*net.TCPAddr).Port
	cfg2 := cfg
	cfg2.Security.CallbackTargets = append(cfg2.Security.CallbackTargets, CallbackTarget{Host: "127.0.0.1", Port: port, AllowedCIDRs: []string{"127.0.0.0/8"}})
	g.Delivery.HTTP = callbackHTTPClient(cfg2)
	rt.cfg = cfg2
	if err := rt.TestURL(ctx, redirect.URL); err == nil || !strings.Contains(err.Error(), "重定向") {
		t.Fatal("redirect followed", err)
	}
	// verify: unlisted target, closed port, wrong echo, unknown binding
	if rt.Verify(ctx, "b1") == nil {
		t.Fatal("verify against closed port")
	}
	g.Delivery.SetTarget("b1", redirect.URL, "1")
	if rt.Verify(ctx, "b1") == nil {
		t.Fatal("verify accepted redirect")
	}
	g.Delivery.SetTarget("b1", "http://10.0.0.1:1/", "1")
	if rt.Verify(ctx, "b1") == nil {
		t.Fatal("verify bypassed allowlist")
	}
	g.Delivery.SetTarget("nope", redirect.URL, "1")
	if rt.Verify(ctx, "nope") == nil {
		t.Fatal("unknown binding")
	}
	st := rt.Status(ctx)
	if st["last_sync"] != "—" || st["wecom_api"] != "未确认" {
		t.Fatal(st)
	}
	g.Store.DB().Exec(`UPDATE sync_scopes SET last_success_at=?`, time.Now().UnixNano())
	if st := rt.Status(ctx); st["wecom_api"] != "正常" {
		t.Fatal(st)
	}
	if maskID("abc") != "***" || maskID("wwcorp1234") != "wwc*****34" {
		t.Fatal(maskID("wwcorp1234"))
	}
	// a console binding whose credentials vanished is skipped at start-up
	g.Store.DB().Exec(`INSERT INTO bindings(id,enterprise_id,open_kfid,project_id,virtual_token_hash,callback_url,active,revision,created_at,updated_at) VALUES('b9','e1','kf9','p','','',1,1,0,0)`)
	g.Store.Close()
	if st := rt.Status(ctx); st["sqlite"] != "异常" {
		t.Fatal("closed store status", st)
	}
}

func TestAdminConfigValidation(t *testing.T) {
	cfg := adminGatewayConfig(t, 9)
	bad := []func(*Config){
		func(c *Config) { c.Admin.PasswordHashEnv = "" },
		func(c *Config) {
			c.Admin.Listen = c.Server.PublicListen
			c.Server.PublicListen = "x:1"
			c.Admin.Listen = "x:1"
		},
		func(c *Config) { c.Enterprises = append(c.Enterprises, c.Enterprises[0]) },
	}
	for i, m := range bad {
		c := cfg
		c.Enterprises = append([]EnterpriseConfig(nil), cfg.Enterprises...)
		m(&c)
		if c.Validate() == nil {
			t.Errorf("admin config %d accepted", i)
		}
	}
	t.Setenv("G_ADMIN_HASH", "")
	if _, err := Build(context.Background(), cfg, nil); err == nil {
		t.Fatal("missing admin hash accepted")
	}
	t.Setenv("G_ADMIN_HASH", "not-bcrypt")
	if _, err := Build(context.Background(), cfg, nil); err == nil {
		t.Fatal("non-bcrypt hash accepted")
	}
	cfg.Admin.Origin = ""
	cfg.Admin.InsecureCookie = true // loopback listen, plain http origin
	hash, _ := bcrypt.GenerateFromPassword([]byte("x"), bcrypt.MinCost)
	t.Setenv("G_ADMIN_HASH", string(hash))
	g, err := Build(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal("default origin", err)
	}
	g.Close()
}

func TestAppServesAdminSeparately(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("admin")) })
	app, _ := NewApp(AppConfig{Addr: "127.0.0.1:0", AdminAddr: "127.0.0.1:0", AdminHandler: h})
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	app.ServeAdmin(ln)
	resp, err := http.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	pub, _ := net.Listen("tcp", "127.0.0.1:0")
	done := make(chan error, 1)
	go func() { done <- app.Serve(pub) }()
	<-app.ready
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	<-done
	if _, err := http.Get("http://" + ln.Addr().String() + "/"); err == nil {
		t.Fatal("admin listener survived shutdown")
	}
	busy, _ := net.Listen("tcp", "127.0.0.1:0")
	defer busy.Close()
	bad, _ := NewApp(AppConfig{Addr: "127.0.0.1:0", AdminAddr: busy.Addr().String(), AdminHandler: h})
	if err := bad.ListenAndServe(); err == nil || !strings.Contains(err.Error(), "admin listener") {
		t.Fatal("admin listen error", err)
	}
	_ = errors.New
}

func TestReview3ConfigHardening(t *testing.T) {
	base := adminGatewayConfig(t, 9)
	cases := map[string]func(*Config){
		"http api without flag":        func(c *Config) { c.WeCom.AllowInsecureHTTP = false },
		"insecure cookie non-loopback": func(c *Config) { c.Admin.Listen = "0.0.0.0:8091"; c.Admin.InsecureCookie = true },
		"http origin secure cookie":    func(c *Config) { c.Admin.Origin = "http://127.0.0.1:8091" },
		"bad cidr": func(c *Config) {
			c.Security.CallbackTargets = []CallbackTarget{{Host: "h", Port: 1, AllowedCIDRs: []string{"10.0.0.0/33"}}}
		},
		"port zero": func(c *Config) {
			c.Security.CallbackTargets = []CallbackTarget{{Host: "h", Port: 0, AllowedCIDRs: []string{"10.0.0.0/8"}}}
		},
		"port too big": func(c *Config) {
			c.Security.CallbackTargets = []CallbackTarget{{Host: "h", Port: 70000, AllowedCIDRs: []string{"10.0.0.0/8"}}}
		},
		"no cidrs": func(c *Config) { c.Security.CallbackTargets = []CallbackTarget{{Host: "h", Port: 1}} },
		"empty host": func(c *Config) {
			c.Security.CallbackTargets = []CallbackTarget{{Port: 1, AllowedCIDRs: []string{"10.0.0.0/8"}}}
		},
		"admin without enterprise": func(c *Config) { c.Enterprises = nil; c.Bindings = nil },
	}
	for name, mut := range cases {
		c := base
		c.Enterprises = append([]EnterpriseConfig(nil), base.Enterprises...)
		c.Bindings = append([]BindingConfig(nil), base.Bindings...)
		mut(&c)
		if c.Validate() == nil {
			t.Errorf("%s: accepted by Validate", name)
		}
		// #37: Build validates on its own and never panics.
		if g, err := Build(context.Background(), c, nil); err == nil {
			g.Close()
			t.Errorf("%s: accepted by Build", name)
		}
	}
	for _, l := range []string{"127.0.0.1:1", "[::1]:1", "localhost:1"} {
		if !isLoopbackListen(l) {
			t.Error(l)
		}
	}
	if isLoopbackListen("10.0.0.1:1") || isLoopbackListen("bad") {
		t.Fatal("non-loopback accepted")
	}
	// Build applies defaults to a hand-built config.
	c := base
	c.Server.PublicListen, c.Workers.MaxConcurrency = "", 0
	c.Admin.Listen = "127.0.0.1:0"
	g, err := Build(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	g.Close()
}

type captureLogger struct{ events []string }

func (l *captureLogger) Log(ev string, _ map[string]any) { l.events = append(l.events, ev) }

func TestInsecureAPIWarns(t *testing.T) {
	l := &captureLogger{}
	g, err := Build(context.Background(), gatewayConfig(t), l)
	if err != nil {
		t.Fatal(err)
	}
	g.Close()
	if !strings.Contains(strings.Join(l.events, ","), "SECURITY_WARNING_insecure_wecom_api") {
		t.Fatal(l.events)
	}
}

func TestExampleConfigIsValid(t *testing.T) {
	raw, err := os.ReadFile("../config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Admin.InsecureCookie || !isLoopbackListen(c.Admin.Listen) || !strings.HasPrefix(c.WeCom.APIBaseURL, "https://") {
		t.Fatal("example must use loopback plain-http admin explicitly and https API")
	}
}

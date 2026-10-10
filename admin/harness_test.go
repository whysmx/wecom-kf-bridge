package admin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/whysmx/wecom-kf-bridge/state"
	"github.com/whysmx/wecom-kf-bridge/wecom"
	"golang.org/x/crypto/bcrypt"
)

const origin = "https://admin.example"
const password = "correct horse battery"

var errLost = errors.New("connection reset (response lost)")

type fakeKF struct {
	mu       sync.Mutex
	accounts []wecom.Account
	listErr  map[int]error
	err      error // returned by add/update/delete/link
	calls    []string
}

func (f *fakeKF) record(s string) { f.mu.Lock(); f.calls = append(f.calls, s); f.mu.Unlock() }
func (f *fakeKF) ListAccounts(_ context.Context, offset, limit int) ([]wecom.Account, error) {
	f.record(fmt.Sprint("list", offset))
	if e := f.listErr[offset]; e != nil {
		return nil, e
	}
	end := offset + limit
	if end > len(f.accounts) {
		end = len(f.accounts)
	}
	if offset > end {
		return nil, nil
	}
	return f.accounts[offset:end], nil
}
func (f *fakeKF) AddAccount(_ context.Context, name, media string) (string, error) {
	f.record("add:" + name)
	return "wk-new", f.err
}
func (f *fakeKF) UpdateAccount(_ context.Context, id, name, media string) error {
	f.record("update:" + id + ":" + name)
	return f.err
}
func (f *fakeKF) DeleteAccount(_ context.Context, id string) error {
	f.record("delete:" + id)
	return f.err
}
func (f *fakeKF) ContactURL(_ context.Context, id, scene string) (string, error) {
	f.record("link:" + id)
	return "https://work.weixin.qq.com/kfid/" + id, f.err
}
func (f *fakeKF) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

type fakeRuntime struct {
	mu        sync.Mutex
	applied   []state.Binding
	creds     []*Credentials
	applyErr  error
	verifyErr error
	testErr   error
}

func (f *fakeRuntime) ApplyBinding(_ context.Context, b state.Binding, c *Credentials) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied, f.creds = append(f.applied, b), append(f.creds, c)
	return f.applyErr
}
func (f *fakeRuntime) CheckURL(raw string) error {
	if !strings.HasPrefix(raw, "http://cc.internal:") {
		return errors.New("not allowlisted")
	}
	return nil
}
func (f *fakeRuntime) TestURL(context.Context, string) error { return f.testErr }
func (f *fakeRuntime) Verify(context.Context, string) error  { return f.verifyErr }
func (f *fakeRuntime) Status(context.Context) map[string]string {
	return map[string]string{"gateway": "ok", "wecom_api": "ok", "last_sync": "—"}
}

type env struct {
	t     *testing.T
	st    *state.Store
	kf    *fakeKF
	rt    *fakeRuntime
	c     *Console
	now   time.Time
	cust  state.Customer
	nowMu sync.Mutex
}

func (e *env) clock() time.Time { e.nowMu.Lock(); defer e.nowMu.Unlock(); return e.now }
func (e *env) advance(d time.Duration) {
	e.nowMu.Lock()
	e.now = e.now.Add(d)
	e.nowMu.Unlock()
}

func newEnv(t *testing.T, mut ...func(*Config)) *env {
	t.Helper()
	st, err := state.OpenWithOptions(t.TempDir()+"/a.db", state.Options{MasterKey: bytes.Repeat([]byte{3}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	must(t, st.PutEnterprise(ctx, state.Enterprise{ID: "e1", TenantID: "t1", CorpID: "ww1", CredentialRef: "env:S1", Status: "ACTIVE"}))
	must(t, st.PutEnterprise(ctx, state.Enterprise{ID: "e2", TenantID: "t2", CorpID: "ww2", CredentialRef: "env:S2", Status: "ACTIVE"}))
	must(t, st.PutBinding(ctx, state.Binding{ID: "b1", EnterpriseID: "e1", OpenKfID: "wk1", ProjectID: "p1", CallbackURL: "http://cc.internal:9000/wecom", Active: true, Revision: 1}))
	must(t, st.PutBinding(ctx, state.Binding{ID: "other", EnterpriseID: "e2", OpenKfID: "wk9", ProjectID: "p9", Active: true, Revision: 1}))
	cu, err := st.EnsureCustomer(ctx, "e1", "b1", "ext-secret-id")
	must(t, err)
	hash, _ := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	e := &env{t: t, st: st, kf: &fakeKF{listErr: map[int]error{}}, rt: &fakeRuntime{}, now: time.Unix(1_800_000_000, 0), cust: cu}
	cfg := Config{Store: st, KF: e.kf, Runtime: e.rt, PasswordHash: hash, Origin: origin, EnterpriseID: "e1", CompanyName: "测试企业", CorpID: "ww1234567890", Clock: e.clock,
		Settings: []Setting{{"企业", "Secret 环境变量", "WECOM_SECRET"}}}
	for _, m := range mut {
		m(&cfg)
	}
	e.c, err = New(cfg)
	must(t, err)
	return e
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type client struct {
	e      *env
	cookie string
	csrf   string
}

func (e *env) do(method, path string, form url.Values, hdr map[string]string, cookie string) *httptest.ResponseRecorder {
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	r := httptest.NewRequest(method, path, body)
	r.RemoteAddr = "10.0.0.5:4321"
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.c.Handler().ServeHTTP(w, r)
	return w
}

var csrfRe = regexp.MustCompile(`name="csrf_token" value="([0-9a-f]+)"`)

func (e *env) login() *client {
	e.t.Helper()
	w := e.do("POST", "/admin/login", url.Values{"password": {password}}, map[string]string{"Origin": origin}, "")
	if w.Code != http.StatusSeeOther {
		e.t.Fatalf("login %d %s", w.Code, w.Body)
	}
	var ck string
	for _, c := range w.Result().Cookies() {
		ck = c.Value
	}
	cl := &client{e: e, cookie: ck}
	page := cl.get("/admin/")
	m := csrfRe.FindStringSubmatch(page.Body.String())
	if m == nil {
		e.t.Fatal("no csrf token")
	}
	cl.csrf = m[1]
	return cl
}

func (c *client) get(path string) *httptest.ResponseRecorder {
	return c.e.do("GET", path, nil, nil, c.cookie)
}

var keyN int

// post sends a form with CSRF, Origin and a fresh idempotency key unless
// one is supplied.
func (c *client) post(path string, form url.Values) *httptest.ResponseRecorder {
	if form == nil {
		form = url.Values{}
	}
	if form.Get("csrf_token") == "" {
		form.Set("csrf_token", c.csrf)
	}
	if form.Get("idempotency_key") == "" {
		keyN++
		form.Set("idempotency_key", fmt.Sprintf("test-key-%08d", keyN))
	}
	return c.e.do("POST", path, form, map[string]string{"Origin": origin}, c.cookie)
}

func (c *client) stepUp() {
	c.e.t.Helper()
	if w := c.post("/admin/stepup", url.Values{"password": {password}, "next": {"/admin/bindings"}}); w.Code != http.StatusSeeOther {
		c.e.t.Fatalf("step-up %d", w.Code)
	}
}

func (c *client) flash() string {
	m := regexp.MustCompile(`class="flash">([^<]*)<`).FindStringSubmatch(c.get("/admin/settings").Body.String())
	if m == nil {
		return ""
	}
	return m[1]
}

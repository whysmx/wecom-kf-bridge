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
	// official service state per external user
	states   map[string]int
	transErr error
	// transErrFor fails only when Trans targets that official state (e.g. 4 after 2→3).
	transErrFor map[int]error
	getErr      error
	stuck       bool // trans "succeeds" but state does not change
	// stuckFor leaves state unchanged only when targeting that official state.
	stuckFor map[int]bool
}

func (f *fakeKF) TransServiceState(_ context.Context, kf, ext string, st int, servicer string) (string, error) {
	if servicer != "" {
		f.record(fmt.Sprintf("trans:%s:%d:%s", ext, st, servicer))
	} else {
		f.record(fmt.Sprintf("trans:%s:%d", ext, st))
	}
	if f.transErrFor != nil {
		if err := f.transErrFor[st]; err != nil {
			return "", err
		}
	}
	if f.transErr != nil {
		return "", f.transErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	stuck := f.stuck
	if f.stuckFor != nil && f.stuckFor[st] {
		stuck = true
	}
	if !stuck {
		f.states[ext] = st
	}
	return "msg-code-admin", nil
}
func (f *fakeKF) ServiceState(_ context.Context, kf, ext string) (int, error) {
	f.record("get:" + ext)
	if f.getErr != nil {
		return 0, f.getErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.states[ext], nil
}
func (f *fakeKF) SendMsgOnEvent(_ context.Context, code, text string) error {
	f.record("event:" + code + ":" + text)
	return nil
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
	mu             sync.Mutex
	applied        []state.Binding
	creds          []*Credentials
	applyErr       error
	verifyErr      error
	testErr        error
	editable       EditableSettings
	saveErr        error
	saved          []EditableSettings
	configBindings []EditableBindingConfig
	bindingSaveErr error
	savedBindings  []BindingConfigPatch
	savedBindingID []string
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
func (f *fakeRuntime) EditableSettings() EditableSettings {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.editable
}
func (f *fakeRuntime) SaveSettings(_ context.Context, s EditableSettings) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveErr != nil {
		return f.saveErr
	}
	f.editable = s
	f.saved = append(f.saved, s)
	return nil
}
func (f *fakeRuntime) ConfigBindings() []EditableBindingConfig {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]EditableBindingConfig, len(f.configBindings))
	copy(out, f.configBindings)
	return out
}
func (f *fakeRuntime) SaveBindingConfig(_ context.Context, id string, patch BindingConfigPatch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bindingSaveErr != nil {
		return f.bindingSaveErr
	}
	for i, b := range f.configBindings {
		if b.ID != id {
			continue
		}
		if patch.EnterpriseID != b.EnterpriseID || patch.OpenKfID != b.OpenKfID {
			return errors.New("enterprise_id/open_kfid 不可修改")
		}
		b.ProjectID = patch.ProjectID
		b.VirtualCorpID = patch.VirtualCorpID
		b.AgentID = patch.AgentID
		b.CallbackURL = patch.CallbackURL
		b.Disabled = patch.Disabled
		b.ServicerUserID = patch.ServicerUserID
		f.configBindings[i] = b
		f.savedBindings = append(f.savedBindings, patch)
		f.savedBindingID = append(f.savedBindingID, id)
		return nil
	}
	return errors.New("binding not in config.json")
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
	e := &env{t: t, st: st, kf: &fakeKF{listErr: map[int]error{}, states: map[string]int{}}, rt: &fakeRuntime{editable: EditableSettings{
		ServiceStateMap: map[string]string{"1": state.CustomerAIEligible, "2": state.CustomerWaitingHuman, "3": state.CustomerHuman, "4": state.CustomerClosed},
	}, configBindings: []EditableBindingConfig{{
		ID: "b1", EnterpriseID: "e1", OpenKfID: "wk1", ProjectID: "p1", VirtualCorpID: "bridge_b1",
		AgentID: "1000002", CallbackURL: "http://cc.internal:9000/wecom",
		VirtualSecretEnv: "B1_SECRET", CallbackTokenEnv: "B1_TOKEN", CallbackAESKeyEnv: "B1_AES",
		ServicerUserID: "WenHaiYan",
	}}}, now: time.Unix(1_800_000_000, 0), cust: cu}
	cfg := Config{Store: st, KF: e.kf, Runtime: e.rt, PasswordHash: hash, Origin: origin, EnterpriseID: "e1", CompanyName: "测试企业", CorpID: "ww1234567890", Clock: e.clock,
		ServiceStateMap: map[int]string{1: state.CustomerAIEligible, 2: state.CustomerWaitingHuman, 3: state.CustomerHuman, 4: state.CustomerClosed},
		Settings:        []Setting{{"企业", "Secret 环境变量", "WECOM_SECRET"}}}
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

func (e *env) doCustom(method, path string, form url.Values, hdr map[string]string, cookie, remote string) *httptest.ResponseRecorder {
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	r := httptest.NewRequest(method, path, body)
	r.RemoteAddr = remote
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

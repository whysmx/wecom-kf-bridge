package bridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/whysmx/wecom-kf-bridge/state"
)

// fakeAdapter is test-only; production code has no always-succeeding adapter.
type fakeAdapter struct {
	mu        sync.Mutex
	name      string
	userErr   error
	svcState  string
	svcErr    error
	sendErrAt int // 1-based chunk that fails; 0 = never
	sendErr   error
	sends     []SendRequest
}

func (a *fakeAdapter) GetUser(_ context.Context, _ Binding, c Customer) (Customer, error) {
	if a.userErr != nil {
		return c, a.userErr
	}
	c.Name = a.name
	return c, nil
}
func (a *fakeAdapter) ServiceState(context.Context, Binding, Customer) (string, error) {
	if a.svcErr != nil {
		return "", a.svcErr
	}
	if a.svcState == "" {
		return state.CustomerAIEligible, nil
	}
	return a.svcState, nil
}
func (a *fakeAdapter) SendText(_ context.Context, r SendRequest) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sendErrAt == r.Chunk {
		return "", a.sendErr
	}
	a.sends = append(a.sends, r)
	return "wx-" + string(rune('0'+r.Chunk)), nil
}
func (a *fakeAdapter) sent() []SendRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]SendRequest(nil), a.sends...)
}

type env struct {
	t     *testing.T
	st    *state.Store
	srv   *Server
	ad    *fakeAdapter
	cust  state.Customer
	now   time.Time
	clock func() time.Time
}

func testBinding() Binding {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	return Binding{ID: "b1", CorpID: "corp-1", CorpSecret: "secret-1", AgentID: "1002", CallbackToken: "callback-token", CallbackAESKey: strings.TrimSuffix(key, "="), Enabled: true, Revision: 1}
}

// newEnv uses a real SQLite store (docs/10: 真实 SQLite 事务).
func newEnv(t *testing.T, mut ...func(*Config)) *env {
	t.Helper()
	e := &env{t: t, ad: &fakeAdapter{name: "外部昵称"}, now: time.Unix(1800000000, 0)}
	e.clock = func() time.Time { return e.now }
	st, err := state.OpenWithOptions(t.TempDir()+"/b.db", state.Options{MasterKey: bytes.Repeat([]byte{8}, 32), Now: func() time.Time { return e.now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e.st = st
	ctx := context.Background()
	must(t, st.PutEnterprise(ctx, state.Enterprise{ID: "e1", TenantID: "t1", CorpID: "realcorp", CredentialRef: "env:X"}))
	must(t, st.PutBinding(ctx, state.Binding{ID: "b1", EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p", Active: true}))
	must(t, st.EnsureScope(ctx, "sc", "b1"))
	c, err := st.EnsureCustomer(ctx, "e1", "b1", "ext-1")
	must(t, err)
	must(t, st.SetNickname(ctx, c.ID, "缓存昵称", e.now))
	e.cust = c
	e.inbound("m-open")
	cfg := Config{Bindings: []Binding{testBinding()}, Store: st, Adapter: e.ad, Clock: e.clock}
	for _, m := range mut {
		m(&cfg)
	}
	e.srv, err = NewServer(cfg)
	must(t, err)
	return e
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// inbound stores a new customer message, which opens the send window.
func (e *env) inbound(id string) {
	_, err := e.st.CommitSyncPage(context.Background(), "sc", "cur-"+id, false, []state.InboxMessage{{BindingID: "b1", ExternalMsgID: id, CustomerID: e.cust.ID, CustomerInitiated: true, CreateTime: e.now}})
	must(e.t, err)
}

func (e *env) do(method, path, body string) (int, map[string]any) {
	rr := httptest.NewRecorder()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	e.srv.Handler().ServeHTTP(rr, req)
	var v map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &v)
	return rr.Code, v
}
func (e *env) token() string {
	code, v := e.do(http.MethodGet, "/cgi-bin/gettoken?corpid=corp-1&corpsecret=secret-1", "")
	if code != 200 {
		e.t.Fatalf("token %d %v", code, v)
	}
	return v["access_token"].(string)
}
func (e *env) send(tok, uid, msgtype, content string) (int, map[string]any) {
	b, _ := json.Marshal(map[string]any{"touser": uid, "msgtype": msgtype, "agentid": "1002", msgtype: map[string]string{"content": content}})
	return e.do(http.MethodPost, "/cgi-bin/message/send?access_token="+tok, string(b))
}
func errcode(v map[string]any) int {
	f, _ := v["errcode"].(float64)
	return int(f)
}
func (e *env) outboxes() []state.OutboxMessage {
	rows, err := e.st.DB().Query(`SELECT id FROM outbox ORDER BY created_at,rowid`)
	must(e.t, err)
	defer rows.Close()
	var out []state.OutboxMessage
	for rows.Next() {
		var id string
		must(e.t, rows.Scan(&id))
		o, err := e.st.Outbox(context.Background(), id)
		must(e.t, err)
		out = append(out, o)
	}
	return out
}

var errBoom = errors.New("boom")

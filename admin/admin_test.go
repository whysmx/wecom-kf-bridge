package admin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/whysmx/wecom-kf-bridge/state"
	"github.com/whysmx/wecom-kf-bridge/wecom"
)

var getRoutes = []string{"/admin/", "/admin/accounts", "/admin/bindings", "/admin/customers", "/admin/diagnostics", "/admin/settings", "/admin/audit"}

var postRoutes = []string{"/admin/logout", "/admin/stepup", "/admin/accounts/sync", "/admin/accounts/create", "/admin/accounts/wk1/edit", "/admin/accounts/wk1/delete",
	"/admin/bindings/create", "/admin/bindings/b1/enable", "/admin/bindings/b1/disable", "/admin/bindings/b1/rebind", "/admin/bindings/b1/rotate",
	"/admin/bindings/b1/verify", "/admin/bindings/b1/export", "/admin/customers/x/handover", "/admin/customers/x/recover", "/admin/diagnostics/outbox/x/view"}

func TestUnauthenticatedAccessIsRejected(t *testing.T) {
	e := newEnv(t)
	for _, p := range getRoutes {
		w := e.do("GET", p, nil, nil, "")
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/admin/login" {
			t.Errorf("GET %s -> %d", p, w.Code)
		}
	}
	for _, p := range postRoutes {
		if w := e.do("POST", p, url.Values{}, map[string]string{"Origin": origin}, "forged-session"); w.Code != http.StatusUnauthorized {
			t.Errorf("POST %s -> %d", p, w.Code)
		}
	}
	w := e.do("GET", "/admin/login", nil, nil, "")
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), "登录") {
		t.Fatal("login page")
	}
	if w := e.do("GET", "/cgi-bin/gettoken", nil, nil, ""); w.Code != http.StatusNotFound {
		t.Fatal("console must not serve compatible API")
	}
}

func TestLoginSessionCookieAndThrottle(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.MaxSessions = 2 })
	w := e.do("POST", "/admin/login", url.Values{"password": {password}}, map[string]string{"Origin": origin}, "")
	ck := w.Result().Cookies()[0]
	if !ck.HttpOnly || !ck.Secure || ck.SameSite != http.SameSiteStrictMode || ck.Path != "/admin" || ck.MaxAge <= 0 || len(ck.Value) != 64 {
		t.Fatalf("cookie %+v", ck)
	}
	if w := e.do("POST", "/admin/login", url.Values{"password": {password}}, map[string]string{"Origin": "https://evil.example"}, ""); w.Code != http.StatusForbidden {
		t.Fatal("cross-origin login accepted")
	}
	for i := 0; i < 5; i++ {
		if w := e.do("POST", "/admin/login", url.Values{"password": {"wrong"}}, map[string]string{"Origin": origin}, ""); w.Code != http.StatusUnauthorized {
			t.Fatal("wrong password")
		}
	}
	if w := e.do("POST", "/admin/login", url.Values{"password": {password}}, map[string]string{"Origin": origin}, ""); w.Code != http.StatusUnauthorized {
		t.Fatal("not throttled after 5 failures")
	}
	e.advance(16 * time.Minute)
	a, b := e.login(), e.login()
	_ = e.login() // evicts the oldest session (cap 2)
	if a.get("/admin/").Code != http.StatusSeeOther {
		t.Fatal("session cap not enforced")
	}
	if b.get("/admin/").Code != 200 {
		t.Fatal("newer session evicted")
	}
	e.advance(9 * time.Hour)
	if b.get("/admin/").Code != http.StatusSeeOther {
		t.Fatal("session did not expire")
	}
	audits, _, _ := e.st.Audits(context.Background(), 0, 50)
	var fails, locked int
	for _, x := range audits {
		if x.Operation == "login" && x.Result == "failed" {
			fails++
		}
		if x.Result == "locked" {
			locked++
		}
		if !strings.HasPrefix(x.Actor, "admin@10.0.0.5") {
			t.Fatal("actor", x.Actor)
		}
	}
	if fails != 5 || locked != 1 {
		t.Fatal(fails, locked)
	}
}

func TestRestartInvalidatesSessions(t *testing.T) {
	e := newEnv(t)
	cl := e.login()
	restarted, err := New(e.c.cfg)
	must(t, err)
	e.c = restarted
	if cl.get("/admin/").Code != http.StatusSeeOther {
		t.Fatal("session survived restart")
	}
}

func TestLogout(t *testing.T) {
	e := newEnv(t)
	cl := e.login()
	if w := cl.post("/admin/logout", nil); w.Code != http.StatusSeeOther {
		t.Fatal(w.Code)
	}
	if cl.get("/admin/").Code != http.StatusSeeOther {
		t.Fatal("session alive after logout")
	}
}

func TestCSRFAndOrigin(t *testing.T) {
	e := newEnv(t)
	cl := e.login()
	form := func() url.Values { return url.Values{"idempotency_key": {"csrf-test-key-0001"}} }
	if w := e.do("POST", "/admin/accounts/sync", form(), map[string]string{"Origin": origin}, cl.cookie); w.Code != http.StatusForbidden {
		t.Fatal("missing csrf accepted")
	}
	f := form()
	f.Set("csrf_token", strings.Repeat("0", 64))
	if w := e.do("POST", "/admin/accounts/sync", f, map[string]string{"Origin": origin}, cl.cookie); w.Code != http.StatusForbidden {
		t.Fatal("wrong csrf accepted")
	}
	f.Set("csrf_token", cl.csrf)
	if w := e.do("POST", "/admin/accounts/sync", f, map[string]string{"Origin": "https://evil.example"}, cl.cookie); w.Code != http.StatusForbidden {
		t.Fatal("cross origin accepted")
	}
	if w := e.do("POST", "/admin/accounts/sync", f, map[string]string{"Referer": "https://evil.example/admin/"}, cl.cookie); w.Code != http.StatusForbidden {
		t.Fatal("cross referer accepted")
	}
	if w := e.do("POST", "/admin/accounts/sync", f, nil, cl.cookie); w.Code != http.StatusForbidden {
		t.Fatal("no origin accepted")
	}
	if w := e.do("POST", "/admin/accounts/sync", f, map[string]string{"Referer": origin + "/admin/accounts"}, cl.cookie); w.Code != http.StatusSeeOther {
		t.Fatalf("same-origin referer refused %d", w.Code)
	}
	if e.kf.count("list") != 1 {
		t.Fatal("refused requests reached WeChat")
	}
	other := e.login()
	f.Set("csrf_token", other.csrf)
	f.Set("idempotency_key", "csrf-test-key-0002")
	if w := e.do("POST", "/admin/accounts/sync", f, map[string]string{"Origin": origin}, cl.cookie); w.Code != http.StatusForbidden {
		t.Fatal("another session's token accepted")
	}
	if w := e.do("POST", "/admin/accounts/sync", nil, map[string]string{"Origin": origin, "Content-Type": "application/x-www-form-urlencoded"}, cl.cookie); w.Code != http.StatusForbidden {
		t.Fatal(w.Code)
	}
}

func TestIdempotencyReplayAndMismatch(t *testing.T) {
	e := newEnv(t)
	cl := e.login()
	f := url.Values{"name": {"售后"}, "media_id": {"m1"}, "idempotency_key": {"same-key-00000001"}}
	w1 := cl.post("/admin/accounts/create", f)
	w2 := cl.post("/admin/accounts/create", f)
	if w1.Code != http.StatusSeeOther || w2.Code != http.StatusSeeOther || w1.Header().Get("Location") != w2.Header().Get("Location") {
		t.Fatalf("%d %d", w1.Code, w2.Code)
	}
	if e.kf.count("add:") != 1 {
		t.Fatal("replay called WeChat again")
	}
	f.Set("name", "改了")
	if w := cl.post("/admin/accounts/create", f); w.Code != http.StatusConflict {
		t.Fatal("same key different params", w.Code)
	}
	g := url.Values{"name": {"x"}, "media_id": {"m1"}, "idempotency_key": {"short"}}
	if w := cl.post("/admin/accounts/create", g); w.Code != http.StatusBadRequest {
		t.Fatal("short key")
	}
	// Header key wins; a refusal (409) replays as 409.
	b, _ := e.st.Binding(context.Background(), "b1")
	h := url.Values{"csrf_token": {cl.csrf}, "revision": {"99"}}
	for i := 0; i < 2; i++ {
		w := e.do("POST", "/admin/bindings/b1/disable", h, map[string]string{"Origin": origin, "Idempotency-Key": "header-key-000001"}, cl.cookie)
		if w.Code != http.StatusConflict {
			t.Fatal("revision conflict", w.Code)
		}
	}
	if nb, _ := e.st.Binding(context.Background(), "b1"); nb.Revision != b.Revision || !nb.Active {
		t.Fatal("conflicting write applied")
	}
	e.st.Close()
	if w := cl.post("/admin/accounts/sync", nil); w.Code != http.StatusServiceUnavailable {
		t.Fatal("store down", w.Code)
	}
}

func TestStepUpRequiredForDangerousActions(t *testing.T) {
	e := newEnv(t)
	cl := e.login()
	for _, p := range []string{"/admin/accounts/wk1/delete", "/admin/bindings/b1/rebind", "/admin/bindings/b1/rotate", "/admin/bindings/b1/export"} {
		if w := cl.post(p, url.Values{"revision": {"1"}}); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "二次认证") {
			t.Errorf("%s without step-up -> %d", p, w.Code)
		}
	}
	if w := cl.post("/admin/stepup", url.Values{"password": {"bad"}}); w.Code != http.StatusUnauthorized {
		t.Fatal("bad step-up")
	}
	w := cl.post("/admin/stepup", url.Values{"password": {password}, "next": {"//evil.example/"}})
	if w.Header().Get("Location") != "/admin/" {
		t.Fatal("open redirect", w.Header().Get("Location"))
	}
	if w := cl.post("/admin/bindings/b1/rotate", url.Values{"revision": {"1"}}); w.Code != http.StatusSeeOther {
		t.Fatal("rotate after step-up", w.Code)
	}
	e.advance(6 * time.Minute)
	if w := cl.post("/admin/bindings/b1/rotate", url.Values{"revision": {"2"}}); w.Code != http.StatusForbidden {
		t.Fatal("step-up did not expire")
	}
}

func TestAccountsSyncPagingAndFailureKeepsList(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 150; i++ {
		e.kf.accounts = append(e.kf.accounts, wecom.Account{OpenKfID: fmt.Sprintf("wk%03d", i), Name: "客服" + itoa(i)})
	}
	cl := e.login()
	cl.post("/admin/accounts/sync", nil)
	_, total, _ := e.st.Accounts(context.Background(), "", 0, 1)
	if total != 150 {
		t.Fatal(total)
	}
	page := cl.get("/admin/accounts?page=2&q=客服")
	if !strings.Contains(page.Body.String(), "上一页") || !strings.Contains(page.Body.String(), "下一页") {
		t.Fatal("pagination links")
	}
	e.kf.accounts = e.kf.accounts[:10]
	e.kf.listErr[100] = errors.New("page 2 failed")
	e.kf.accounts = append(e.kf.accounts, make([]wecom.Account, 95)...)
	cl.post("/admin/accounts/sync", nil)
	if !strings.Contains(cl.flash(), "保留现有列表") {
		t.Fatal("failure not reported")
	}
	if _, total, _ := e.st.Accounts(context.Background(), "", 0, 1); total != 150 {
		t.Fatal("partial sync changed the list", total)
	}
	if w := cl.get("/admin/accounts?id=missing"); w.Code != http.StatusNotFound {
		t.Fatal("missing account detail")
	}
	if w := cl.get("/admin/accounts?q=" + strings.Repeat("x", 100)); w.Code != 200 {
		t.Fatal("long query")
	}
}

func itoa(i int) string { return fmtInt(int64(i)) }

func seedAccount(t *testing.T, e *env, id string) state.KFAccount {
	must(t, e.st.UpsertAccount(context.Background(), state.KFAccount{OpenKfID: id, Name: "原名", Status: state.AccountActive}))
	a, _ := e.st.Account(context.Background(), id)
	return a
}

func TestAccountCreateOutcomes(t *testing.T) {
	e := newEnv(t)
	cl := e.login()
	if w := cl.post("/admin/accounts/create", url.Values{"name": {strings.Repeat("长", 17)}, "media_id": {"m"}}); w.Code != http.StatusBadRequest {
		t.Fatal("long name")
	}
	if w := cl.post("/admin/accounts/create", url.Values{"name": {"a"}, "media_id": {"bad media"}}); w.Code != http.StatusBadRequest {
		t.Fatal("bad media")
	}
	cl.post("/admin/accounts/create", url.Values{"name": {"售前"}, "media_id": {"m1"}})
	if a, err := e.st.Account(context.Background(), "wk-new"); err != nil || a.Status != state.AccountActive {
		t.Fatal(a, err)
	}
	e.kf.err = &wecom.APIError{Code: 95000, Message: "rejected"}
	cl.post("/admin/accounts/create", url.Values{"name": {"拒绝"}, "media_id": {"m1"}})
	if !strings.Contains(cl.flash(), "微信拒绝") {
		t.Fatal("api rejection")
	}
	e.kf.err = errLost
	cl.post("/admin/accounts/create", url.Values{"name": {"未知"}, "media_id": {"m1"}, "idempotency_key": {"lost-create-0001"}})
	a, err := e.st.Account(context.Background(), "unknown-lost-create-0001")
	if err != nil || a.Status != state.AccountUnknown {
		t.Fatal("unknown not recorded", err)
	}
	if !strings.Contains(cl.flash(), "UNKNOWN") {
		t.Fatal("unknown flash")
	}
}

func TestAccountEditLinkAndRevision(t *testing.T) {
	e := newEnv(t)
	a := seedAccount(t, e, "wk1")
	cl := e.login()
	rev := itoa64(a.Revision)
	if w := cl.post("/admin/accounts/wk1/edit", url.Values{"revision": {"999"}, "name": {"新名"}}); w.Code != http.StatusConflict {
		t.Fatal("stale revision accepted")
	}
	if e.kf.count("update") != 0 {
		t.Fatal("stale edit reached WeChat")
	}
	if w := cl.post("/admin/accounts/wk1/edit", url.Values{"revision": {rev}, "name": {""}}); w.Code != http.StatusBadRequest {
		t.Fatal("empty name")
	}
	cl.post("/admin/accounts/wk1/edit", url.Values{"revision": {rev}, "name": {"新名"}, "note": {"VIP 线"}})
	b, _ := e.st.Account(context.Background(), "wk1")
	if b.Name != "新名" || b.Note != "VIP 线" {
		t.Fatal(b)
	}
	// note-only edit does not call WeChat
	cl.post("/admin/accounts/wk1/edit", url.Values{"revision": {itoa64(b.Revision)}, "name": {"新名"}, "note": {"x"}})
	if e.kf.count("update") != 1 {
		t.Fatal("note-only edit called WeChat")
	}
	b, _ = e.st.Account(context.Background(), "wk1")
	cl.post("/admin/accounts/wk1/link", url.Values{"revision": {itoa64(b.Revision)}})
	b, _ = e.st.Account(context.Background(), "wk1")
	if !strings.HasPrefix(b.URL, "https://work.weixin.qq.com/") {
		t.Fatal("link", b.URL)
	}
	e.kf.err = &wecom.APIError{Code: 1, Message: "no"}
	cl.post("/admin/accounts/wk1/edit", url.Values{"revision": {itoa64(b.Revision)}, "name": {"再改"}})
	if !strings.Contains(cl.flash(), "微信拒绝修改") {
		t.Fatal("rejected edit")
	}
	cl.post("/admin/accounts/wk1/link", url.Values{"revision": {itoa64(b.Revision)}})
	if !strings.Contains(cl.flash(), "生成链接失败") {
		t.Fatal("link failure")
	}
	e.kf.err = errLost
	cl.post("/admin/accounts/wk1/edit", url.Values{"revision": {itoa64(b.Revision)}, "name": {"再改"}})
	if c, _ := e.st.Account(context.Background(), "wk1"); c.Status != state.AccountUnknown {
		t.Fatal("lost edit not UNKNOWN")
	}
	if w := cl.post("/admin/accounts/nope/edit", url.Values{"revision": {"1"}, "name": {"x"}}); w.Code != http.StatusNotFound {
		t.Fatal("missing")
	}
}

func itoa64(n int64) string { return fmtInt(n) }

func TestAccountDeleteTicket(t *testing.T) {
	e := newEnv(t)
	a := seedAccount(t, e, "wk1")
	cl := e.login()
	cl.stepUp()
	page := cl.get("/admin/accounts?id=wk1").Body.String()
	tk := between(page, `name="ticket" value="`, `"`)
	if tk == "" {
		t.Fatal("no ticket")
	}
	rev := itoa64(a.Revision)
	if w := cl.post("/admin/accounts/wk1/delete", url.Values{"revision": {rev}, "ticket": {tk}, "confirm": {"wrong"}}); w.Code != http.StatusForbidden {
		t.Fatal("wrong confirm")
	}
	// ticket is single use: consumed by the refused attempt
	if w := cl.post("/admin/accounts/wk1/delete", url.Values{"revision": {rev}, "ticket": {tk}, "confirm": {"wk1"}}); w.Code != http.StatusForbidden {
		t.Fatal("ticket reused")
	}
	tk = between(cl.get("/admin/accounts?id=wk1").Body.String(), `name="ticket" value="`, `"`)
	e.advance(3 * time.Minute)
	cl.stepUp()
	if w := cl.post("/admin/accounts/wk1/delete", url.Values{"revision": {rev}, "ticket": {tk}, "confirm": {"wk1"}}); w.Code != http.StatusForbidden {
		t.Fatal("expired ticket")
	}
	tk = between(cl.get("/admin/accounts?id=wk1").Body.String(), `name="ticket" value="`, `"`)
	e.kf.err = &wecom.APIError{Code: 2, Message: "in use"}
	cl.post("/admin/accounts/wk1/delete", url.Values{"revision": {rev}, "ticket": {tk}, "confirm": {"wk1"}})
	if !strings.Contains(cl.flash(), "微信拒绝删除") {
		t.Fatal("rejected delete")
	}
	e.kf.err = nil
	tk = between(cl.get("/admin/accounts?id=wk1").Body.String(), `name="ticket" value="`, `"`)
	cl.post("/admin/accounts/wk1/delete", url.Values{"revision": {rev}, "ticket": {tk}, "confirm": {"wk1"}})
	if b, _ := e.st.Account(context.Background(), "wk1"); b.Status != state.AccountDeleted {
		t.Fatal("not deleted", b.Status)
	}
	// #32: account deleted -> its binding (b1 uses wk1) is disabled, tokens
	// revoked via ApplyBinding, and old customer UIDs are stale.
	if b, _ := e.st.Binding(context.Background(), "b1"); b.Active || b.Revision != 2 {
		t.Fatal("binding of deleted account still active", b)
	}
	if last := e.rt.applied[len(e.rt.applied)-1]; last.ID != "b1" || last.Active {
		t.Fatal("runtime not told to revoke", last)
	}
	if _, err := e.st.CustomerByUID(context.Background(), "b1", e.cust.UID); !errors.Is(err, state.ErrStaleGeneration) {
		t.Fatal("old UID not frozen", err)
	}
	if b, _ := e.st.Binding(context.Background(), "other"); !b.Active {
		t.Fatal("other enterprise binding touched")
	}
	seedAccount(t, e, "wk2")
	a2, _ := e.st.Account(context.Background(), "wk2")
	tk = between(cl.get("/admin/accounts?id=wk2").Body.String(), `name="ticket" value="`, `"`)
	e.kf.err = errLost
	cl.post("/admin/accounts/wk2/delete", url.Values{"revision": {itoa64(a2.Revision)}, "ticket": {tk}, "confirm": {"wk2"}})
	if b, _ := e.st.Account(context.Background(), "wk2"); b.Status != state.AccountUnknown {
		t.Fatal("lost delete")
	}
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	s = s[i+len(a):]
	return s[:strings.Index(s, b)]
}

func TestBindingLifecycle(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	seedAccount(t, e, "wk5")
	cl := e.login()
	bad := []url.Values{
		{"id": {"bad id!"}, "open_kfid": {"wk5"}, "project_id": {"p"}, "callback_url": {"http://cc.internal:1/"}, "agent_id": {"1"}},
		{"id": {"b5"}, "open_kfid": {"nope"}, "project_id": {"p"}, "callback_url": {"http://cc.internal:1/"}, "agent_id": {"1"}},
		{"id": {"b5"}, "open_kfid": {"wk5"}, "project_id": {"p"}, "callback_url": {"http://169.254.169.254/"}, "agent_id": {"1"}},
	}
	for i, f := range bad {
		if w := cl.post("/admin/bindings/create", f); w.Code != http.StatusBadRequest {
			t.Errorf("bad create %d -> %d", i, w.Code)
		}
	}
	if w := cl.post("/admin/bindings/create", url.Values{"id": {"b1"}, "open_kfid": {"wk5"}, "project_id": {"p"}, "callback_url": {"http://cc.internal:1/"}, "agent_id": {"1"}}); w.Code != http.StatusConflict {
		t.Fatal("duplicate id")
	}
	if w := cl.post("/admin/bindings/create", url.Values{"id": {"b5"}, "open_kfid": {"wk5"}, "project_id": {"p5"}, "callback_url": {"http://cc.internal:1/cb"}, "agent_id": {"1000002"}}); w.Code != http.StatusSeeOther {
		t.Fatal("create", w.Code)
	}
	b, _ := e.st.Binding(ctx, "b5")
	if !b.Active || e.rt.creds[len(e.rt.creds)-1] == nil {
		t.Fatal("binding not applied with credentials")
	}
	created := *e.rt.creds[len(e.rt.creds)-1]
	if len(created.AESKey) != 43 {
		t.Fatal("aes key length", len(created.AESKey))
	}
	page := cl.get("/admin/bindings").Body.String()
	if strings.Contains(page, created.Secret) || strings.Contains(page, "other") {
		t.Fatal("secret shown or other enterprise listed")
	}
	cl.post("/admin/bindings/b5/disable", url.Values{"revision": {"1"}})
	if b, _ = e.st.Binding(ctx, "b5"); b.Active || b.Revision != 2 {
		t.Fatal("disable", b)
	}
	cl.post("/admin/bindings/b5/enable", url.Values{"revision": {"2"}})
	if b, _ = e.st.Binding(ctx, "b5"); !b.Active || b.Revision != 3 {
		t.Fatal("enable", b)
	}
	cl.post("/admin/bindings/b5/verify", nil)
	if !strings.Contains(cl.flash(), "通过") {
		t.Fatal("verify")
	}
	e.rt.verifyErr = errors.New("echo mismatch")
	cl.post("/admin/bindings/b5/verify", nil)
	if !strings.Contains(cl.flash(), "失败") {
		t.Fatal("verify failure")
	}
	cl.stepUp()
	// export once
	cl.post("/admin/bindings/b5/export", nil)
	shown := cl.get("/admin/bindings").Body.String()
	if !strings.Contains(shown, created.Secret) {
		t.Fatal("export not shown")
	}
	if strings.Contains(cl.get("/admin/bindings").Body.String(), created.Secret) {
		t.Fatal("export shown twice")
	}
	if w := cl.post("/admin/bindings/b5/export", nil); w.Code != http.StatusConflict {
		t.Fatal("second export allowed")
	}
	cl.post("/admin/bindings/b5/rotate", url.Values{"revision": {"3"}})
	rotated := *e.rt.creds[len(e.rt.creds)-1]
	if rotated.Secret == created.Secret {
		t.Fatal("rotation kept secret")
	}
	if b, _ = e.st.Binding(ctx, "b5"); b.Revision != 4 {
		t.Fatal("rotation revision")
	}
	cl.post("/admin/bindings/b5/export", nil)
	if !strings.Contains(cl.get("/admin/bindings").Body.String(), rotated.Secret) {
		t.Fatal("rotation did not re-arm export")
	}
	if w := cl.post("/admin/bindings/other/disable", url.Values{"revision": {"1"}}); w.Code != http.StatusNotFound {
		t.Fatal("other enterprise binding reachable by path id")
	}
	if w := cl.post("/admin/bindings/b5/rotate", url.Values{"revision": {"1"}}); w.Code != http.StatusConflict {
		t.Fatal("stale rotate")
	}
	audits, _, _ := e.st.Audits(ctx, 0, 100)
	for _, a := range audits {
		if strings.Contains(a.Summary, rotated.Secret) || strings.Contains(a.Summary, created.Secret) {
			t.Fatal("secret in audit")
		}
	}
}

func TestRebindRotatesCustomerGenerations(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cl := e.login()
	cl.stepUp()
	if w := cl.post("/admin/bindings/b1/rebind", url.Values{"revision": {"1"}, "project_id": {"p2"}, "callback_url": {"http://evil/"}}); w.Code != http.StatusBadRequest {
		t.Fatal("ssrf rebind")
	}
	if w := cl.post("/admin/bindings/b1/rebind", url.Values{"revision": {"1"}, "project_id": {"bad project"}, "callback_url": {"http://cc.internal:9/"}}); w.Code != http.StatusBadRequest {
		t.Fatal("bad project")
	}
	cl.post("/admin/bindings/b1/rebind", url.Values{"revision": {"1"}, "project_id": {"p2"}, "callback_url": {"http://cc.internal:9100/x"}})
	b, _ := e.st.Binding(ctx, "b1")
	if b.ProjectID != "p2" || b.Revision != 2 {
		t.Fatal(b)
	}
	if _, err := e.st.CustomerByUID(ctx, "b1", e.cust.UID); !errors.Is(err, state.ErrStaleGeneration) {
		t.Fatal("old UID still valid", err)
	}
	if last := e.rt.applied[len(e.rt.applied)-1]; last.Revision != 2 || last.CallbackURL != "http://cc.internal:9100/x" {
		t.Fatal("runtime not updated", last)
	}
}

func TestBindingRuntimeAndStoreFailures(t *testing.T) {
	e := newEnv(t)
	seedAccount(t, e, "wk5")
	cl := e.login()
	cl.stepUp()
	e.rt.applyErr = errors.New("duplicate corp")
	if w := cl.post("/admin/bindings/b1/disable", url.Values{"revision": {"1"}}); w.Code != http.StatusInternalServerError {
		t.Fatal("apply failure", w.Code)
	}
	if w := cl.post("/admin/bindings/create", url.Values{"id": {"b6"}, "open_kfid": {"wk5"}, "project_id": {"p"}, "callback_url": {"http://cc.internal:1/"}, "agent_id": {"1"}}); w.Code != http.StatusInternalServerError {
		t.Fatal("apply failure on create")
	}
	if w := cl.post("/admin/bindings/b1/rotate", url.Values{"revision": {"2"}}); w.Code != http.StatusInternalServerError {
		t.Fatal("apply failure on rotate", w.Code)
	}
	if w := cl.post("/admin/bindings/b1/rebind", url.Values{"revision": {"3"}, "project_id": {"p"}, "callback_url": {"http://cc.internal:1/"}}); w.Code != http.StatusInternalServerError {
		t.Fatal("apply failure on rebind", w.Code)
	}
	e.rt.applyErr = nil
	e.st.DB().Exec(`CREATE TRIGGER f1 BEFORE INSERT ON customer_uids BEGIN SELECT RAISE(ABORT,'x'); END`)
	if w := cl.post("/admin/bindings/b1/rebind", url.Values{"revision": {"4"}, "project_id": {"p"}, "callback_url": {"http://cc.internal:1/"}}); w.Code != http.StatusServiceUnavailable {
		t.Fatal("rotation failure", w.Code)
	}
	e.st.DB().Exec(`CREATE TRIGGER f2 BEFORE INSERT ON binding_secrets BEGIN SELECT RAISE(ABORT,'x'); END`)
	if w := cl.post("/admin/bindings/b1/rotate", url.Values{"revision": {"5"}}); w.Code != http.StatusServiceUnavailable {
		t.Fatal("secret save failure", w.Code)
	}
	if w := cl.post("/admin/bindings/create", url.Values{"id": {"b7"}, "open_kfid": {"wk5"}, "project_id": {"p"}, "callback_url": {"http://cc.internal:1/"}, "agent_id": {"1"}}); w.Code != http.StatusServiceUnavailable {
		t.Fatal("secret save failure on create", w.Code)
	}
	e.st.DB().Exec(`CREATE TRIGGER f3 BEFORE INSERT ON bindings BEGIN SELECT RAISE(ABORT,'x'); END`)
	if w := cl.post("/admin/bindings/create", url.Values{"id": {"b8"}, "open_kfid": {"wk5"}, "project_id": {"p"}, "callback_url": {"http://cc.internal:1/"}, "agent_id": {"1"}}); w.Code != http.StatusServiceUnavailable {
		t.Fatal("binding save failure", w.Code)
	}
	e.st.DB().Exec(`CREATE TRIGGER f4 BEFORE UPDATE ON bindings BEGIN SELECT RAISE(ABORT,'x'); END`)
	if w := cl.post("/admin/bindings/b1/enable", url.Values{"revision": {"6"}}); w.Code != http.StatusServiceUnavailable {
		t.Fatal("update failure", w.Code)
	}
	e.st.Close()
	if w := cl.post("/admin/bindings/b1/verify", nil); w.Code != http.StatusServiceUnavailable {
		t.Fatal("store closed", w.Code)
	}
}

func TestCustomersHandoverRecover(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cl := e.login()
	page := cl.get("/admin/customers").Body.String()
	if strings.Contains(page, "ext-secret-id") || !strings.Contains(page, e.cust.UID) {
		t.Fatal("external_userid not masked or customer missing")
	}
	other, _ := e.st.EnsureCustomer(ctx, "e2", "other", "ext-other")
	if w := cl.post("/admin/customers/"+other.ID+"/handover", url.Values{"revision": {itoa64(other.Revision)}}); w.Code != http.StatusNotFound {
		t.Fatal("cross-enterprise customer reachable")
	}
	if strings.Contains(cl.get("/admin/customers?q="+other.ID).Body.String(), other.UID) {
		t.Fatal("cross-enterprise customer listed")
	}
	if w := cl.post("/admin/customers/"+e.cust.ID+"/recover", url.Values{"revision": {itoa64(e.cust.Revision)}}); w.Code != http.StatusConflict {
		t.Fatal("recover without handover")
	}
	if w := cl.post("/admin/customers/"+e.cust.ID+"/handover", url.Values{"revision": {"999"}}); w.Code != http.StatusConflict {
		t.Fatal("stale customer revision")
	}
	cl.post("/admin/customers/"+e.cust.ID+"/handover", url.Values{"revision": {itoa64(e.cust.Revision)}})
	cu, _ := e.st.Customer(ctx, e.cust.ID)
	if cu.State != state.CustomerHuman {
		t.Fatal(cu.State)
	}
	cu, _ = e.st.Customer(ctx, e.cust.ID)
	cl.post("/admin/customers/"+e.cust.ID+"/recover", url.Values{"revision": {itoa64(cu.Revision)}})
	nc, _ := e.st.Customer(ctx, e.cust.ID)
	if nc.Generation != e.cust.Generation+1 || nc.UID == e.cust.UID {
		t.Fatal("recover did not create a new generation")
	}
	detail := cl.get("/admin/customers?q=" + nc.ID + "&detail=1")
	if !strings.Contains(detail.Body.String(), "最近 outbox") {
		t.Fatal("detail")
	}
	if w := cl.post("/admin/customers/nope/handover", url.Values{"revision": {"1"}}); w.Code != http.StatusNotFound {
		t.Fatal("missing customer")
	}
	e.st.DB().Exec(`CREATE TRIGGER f5 BEFORE UPDATE OF state ON customers WHEN NEW.state='HUMAN' BEGIN SELECT RAISE(ABORT,'x'); END`)
	nc, _ = e.st.Customer(ctx, e.cust.ID)
	if w := cl.post("/admin/customers/"+nc.ID+"/handover", url.Values{"revision": {itoa64(nc.Revision)}}); w.Code != http.StatusServiceUnavailable && w.Code != http.StatusConflict {
		t.Fatal("handover status failure", w.Code)
	}
}

func seedMessages(t *testing.T, e *env) (int64, string) {
	ctx := context.Background()
	must(t, e.st.EnsureScope(ctx, "s1", "b1"))
	_, err := e.st.CommitSyncPage(ctx, "s1", "n1", false, []state.InboxMessage{{BindingID: "b1", ExternalMsgID: "m1", CustomerID: e.cust.ID, Generation: e.cust.Generation, Type: "text", PayloadRef: "客户机密正文"}})
	must(t, err)
	in, _ := e.st.InboxByStates(ctx, "e1", "", nil, 1)
	for _, to := range []string{state.InboxClassified, state.InboxReady, state.InboxPosting, state.InboxDeliveryUnknown} {
		_, err = e.st.TransitionInbox(ctx, in[0].ID, to, "timeout")
		must(t, err)
	}
	must(t, e.st.SetCustomerAuthorized(ctx, e.cust.ID, true))
	_, err = e.st.DB().Exec(`UPDATE customers SET last_inbound_at=?,window_used=0 WHERE id=?`, time.Now().UnixNano(), e.cust.ID)
	must(t, err)
	o, err := e.st.CreateOutbox(ctx, state.OutboxMessage{CustomerID: e.cust.ID, Generation: e.cust.Generation, UID: e.cust.UID, Body: "AI 机密回复", BudgetUnits: 1})
	must(t, err)
	_, err = e.st.MarkOutboxSending(ctx, o.ID)
	must(t, err)
	_, err = e.st.MarkOutboxUnknown(ctx, o.ID, "lost")
	must(t, err)
	return in[0].ID, o.ID
}

func TestDiagnosticsHideBodiesAndMark(t *testing.T) {
	e := newEnv(t)
	inID, outID := seedMessages(t, e)
	cl := e.login()
	for _, v := range []string{"", "?view=inbox", "?view=outbox", "?view=pending"} {
		body := cl.get("/admin/diagnostics" + v).Body.String()
		if strings.Contains(body, "机密") {
			t.Fatalf("body leaked in %q", v)
		}
	}
	if !strings.Contains(cl.get("/admin/diagnostics?view=pending").Body.String(), outID) {
		t.Fatal("pending view")
	}
	if w := cl.post("/admin/diagnostics/outbox/"+outID+"/view", nil); w.Code != http.StatusForbidden {
		t.Fatal("body without step-up")
	}
	cl.stepUp()
	w := cl.post("/admin/diagnostics/outbox/"+outID+"/view", nil)
	if !strings.Contains(w.Body.String(), "AI 机密回复") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("body view")
	}
	if !strings.Contains(cl.post("/admin/diagnostics/inbox/"+itoa64(inID)+"/view", nil).Body.String(), "客户机密正文") {
		t.Fatal("inbox body view")
	}
	for _, p := range []string{"/admin/diagnostics/inbox/abc/view", "/admin/diagnostics/inbox/999/view", "/admin/diagnostics/other/1/view"} {
		if cl.post(p, nil).Code != http.StatusNotFound {
			t.Fatal(p)
		}
	}
	if w := e.do("POST", "/admin/diagnostics/outbox/"+outID+"/view", url.Values{}, map[string]string{"Origin": origin}, cl.cookie); w.Code != http.StatusForbidden {
		t.Fatal("view without csrf")
	}
	audits, _, _ := e.st.Audits(context.Background(), 0, 100)
	views := 0
	for _, a := range audits {
		if a.Operation == "view_body" {
			views++
		}
	}
	if views != 2 {
		t.Fatal("body views not audited", views)
	}
	if w := cl.post("/admin/diagnostics/outbox/"+outID+"/mark", url.Values{"note": {"已人工核实"}}); w.Code != http.StatusSeeOther {
		t.Fatal(w.Code)
	}
	if o, _ := e.st.Outbox(context.Background(), outID); o.State != state.OutboxUnknown {
		t.Fatal("mark resent/changed state")
	}
	if !strings.Contains(cl.get("/admin/diagnostics").Body.String(), "已人工核实") {
		t.Fatal("mark not shown")
	}
	if w := cl.post("/admin/diagnostics/outbox/"+outID+"/mark", url.Values{"note": {"确认成功"}}); w.Code != http.StatusBadRequest {
		t.Fatal("arbitrary mark accepted")
	}
}

func TestPagesRenderAndSettingsTest(t *testing.T) {
	e := newEnv(t)
	seedMessages(t, e)
	cl := e.login()
	for _, p := range getRoutes {
		w := cl.get(p)
		if w.Code != 200 || w.Header().Get("X-Frame-Options") != "DENY" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'none'") {
			t.Fatalf("%s %d", p, w.Code)
		}
		if strings.Contains(w.Body.String(), "ww1234567890") || strings.Contains(w.Body.String(), "<script") {
			t.Fatalf("%s leaks corp id or has scripts", p)
		}
	}
	if !strings.Contains(cl.get("/admin/settings").Body.String(), "WECOM_SECRET") {
		t.Fatal("settings env name")
	}
	cl.post("/admin/settings/test", url.Values{"url": {"http://169.254.169.254/"}})
	if !strings.Contains(cl.flash(), "失败") {
		t.Fatal("ssrf test url")
	}
	cl.post("/admin/settings/test", url.Values{"url": {"http://cc.internal:9000/"}})
	if !strings.Contains(cl.flash(), "通过") {
		t.Fatal("test ok")
	}
	for i := 0; i < 25; i++ {
		cl.post("/admin/settings/test", url.Values{"url": {"http://cc.internal:9000/"}})
	}
	if !strings.Contains(cl.get("/admin/audit").Body.String(), "下一页") || cl.get("/admin/audit?page=2").Code != 200 {
		t.Fatal("audit paging")
	}
}

func TestStoreFailuresOnPages(t *testing.T) {
	e := newEnv(t)
	cl := e.login()
	e.st.Close()
	for _, p := range getRoutes {
		if p == "/admin/settings" {
			continue
		}
		if w := cl.get(p); w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s -> %d", p, w.Code)
		}
	}
}

func TestNewValidatesConfig(t *testing.T) {
	e := newEnv(t)
	base := e.c.cfg
	for i, m := range []func(*Config){
		func(c *Config) { c.Store = nil },
		func(c *Config) { c.PasswordHash = []byte("plain") },
		func(c *Config) { c.Origin = "admin.example" },
		func(c *Config) { c.EnterpriseID = "" },
	} {
		c := base
		m(&c)
		if _, err := New(c); err == nil {
			t.Errorf("config %d accepted", i)
		}
	}
	if mask("abc") != "***" || mask("ww1234567890") != "ww1*******90" {
		t.Fatal(mask("ww1234567890"))
	}
	if !definitive(&wecom.APIError{}) || definitive(errLost) {
		t.Fatal("definitive")
	}
	_ = bytes.MinRead
}

func trig(t *testing.T, e *env, name, when string) {
	t.Helper()
	if _, err := e.st.DB().Exec(`CREATE TRIGGER ` + name + ` ` + when + ` BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
}

func TestWriteFailuresAreReported(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	inID, outID := seedMessages(t, e)
	cl := e.login()
	cl.stepUp()
	// pending view hides non-UNKNOWN focus items
	e.st.DB().Exec(`UPDATE inbox SET state='RETRY_WAIT' WHERE id=?`, inID)
	e.st.DB().Exec(`UPDATE outbox SET state='REJECTED' WHERE id=?`, outID)
	if body := cl.get("/admin/diagnostics?view=pending").Body.String(); strings.Contains(body, outID) || strings.Contains(body, ">"+itoa64(inID)+"<") {
		t.Fatal("pending view shows resolved states")
	}
	// out-of-scope and customer-less messages are not viewable
	must(t, e.st.EnsureScope(ctx, "s9", "other"))
	other, _ := e.st.EnsureCustomer(ctx, "e2", "other", "ext-o")
	e.st.CommitSyncPage(ctx, "s9", "n", false, []state.InboxMessage{{BindingID: "other", ExternalMsgID: "x1", CustomerID: other.ID, Type: "text", PayloadRef: "别家正文"}, {BindingID: "other", ExternalMsgID: "x2", Type: "event"}})
	in, _ := e.st.InboxByStates(ctx, "e2", "", nil, 10)
	if len(in) != 2 {
		t.Fatal("foreign rows not seeded")
	}
	for _, m := range in {
		if m.BindingID == "other" {
			if w := cl.post("/admin/diagnostics/inbox/"+itoa64(m.ID)+"/view", nil); w.Code != http.StatusNotFound {
				t.Fatal("foreign/customer-less body viewable", w.Code)
			}
		}
	}
	trig(t, e, "t1", "BEFORE INSERT ON diag_marks")
	if w := cl.post("/admin/diagnostics/outbox/"+outID+"/mark", url.Values{"note": {"关闭诊断项"}}); w.Code != http.StatusServiceUnavailable {
		t.Fatal("mark failure", w.Code)
	}
	trig(t, e, "t2", "BEFORE INSERT ON handover_events")
	if w := cl.post("/admin/customers/"+e.cust.ID+"/handover", url.Values{"revision": {itoa64(mustCustomer(t, e).Revision)}}); w.Code != http.StatusConflict {
		t.Fatal("handover failure", w.Code)
	}
	a := seedAccount(t, e, "wk7")
	trig(t, e, "t3", "BEFORE UPDATE ON kf_accounts")
	if w := cl.post("/admin/accounts/wk7/edit", url.Values{"revision": {itoa64(a.Revision)}, "name": {"原名"}, "note": {"n"}}); w.Code != http.StatusConflict {
		t.Fatal("local edit failure", w.Code)
	}
	e.kf.accounts = []wecom.Account{{OpenKfID: "wk8", Name: "n"}}
	trig(t, e, "t4", "BEFORE INSERT ON kf_accounts")
	if w := cl.post("/admin/accounts/sync", nil); w.Code != http.StatusServiceUnavailable {
		t.Fatal("sync store failure", w.Code)
	}
	trig(t, e, "t5", "BEFORE UPDATE ON bindings")
	if w := cl.post("/admin/bindings/b1/rotate", url.Values{"revision": {"1"}}); w.Code != http.StatusConflict {
		t.Fatal("rotate store failure", w.Code)
	}
}

func mustCustomer(t *testing.T, e *env) state.Customer {
	c, err := e.st.Customer(context.Background(), e.cust.ID)
	must(t, err)
	return c
}

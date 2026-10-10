package admin

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/whysmx/wecom-kf-bridge/state"
	"github.com/whysmx/wecom-kf-bridge/wecom"
)

// #39: diagnostics only list and mark this enterprise's rows.
func TestDiagnosticsScopedToEnterprise(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	must(t, e.st.EnsureScope(ctx, "s9", "other"))
	other, _ := e.st.EnsureCustomer(ctx, "e2", "other", "ext-o")
	_, err0 := e.st.CommitSyncPage(ctx, "s9", "n", false, []state.InboxMessage{{BindingID: "other", ExternalMsgID: "foreign-msg", CustomerID: other.ID, Type: "text"}})
	must(t, err0)
	o, err := e.st.CreateOutbox(ctx, state.OutboxMessage{CustomerID: other.ID, Generation: other.Generation, UID: other.UID, Body: "x"})
	if o.ID == "" {
		t.Fatal(err)
	}
	if in, _ := e.st.InboxByStates(ctx, "e1", "", nil, 10); len(in) != 0 {
		t.Fatal("foreign inbox leaked", in)
	}
	if out, _ := e.st.OutboxByStates(ctx, "e1", "", nil, 10); len(out) != 0 {
		t.Fatal("foreign outbox leaked")
	}
	fin, _ := e.st.InboxByStates(ctx, "e2", "", nil, 10)
	cl := e.login()
	for _, view := range []string{"", "?view=pending", "?view=all"} {
		if body := cl.get("/admin/diagnostics" + view).Body.String(); strings.Contains(body, o.ID) {
			t.Fatal("foreign outbox shown", view)
		}
	}
	for _, p := range []string{"/admin/diagnostics/outbox/" + o.ID + "/mark", "/admin/diagnostics/inbox/" + itoa64(fin[0].ID) + "/mark", "/admin/diagnostics/outbox/nope/mark"} {
		if w := cl.post(p, url.Values{"note": {"关闭诊断项"}}); w.Code != http.StatusNotFound {
			t.Fatal("foreign/missing mark", p, w.Code)
		}
	}
	if m, _ := e.st.DiagnosticMarks(ctx, "outbox"); len(m) != 0 {
		t.Fatal("foreign row marked")
	}
	if _, err := e.st.DiagnosticEnterprise(ctx, "bad", "1"); err == nil {
		t.Fatal("bad kind")
	}
}

// #40: rotation is atomic; runtime failure restores the old credentials.
func TestRotationRollsBack(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cl := e.login()
	cl.stepUp()
	old := `{"corp_id":"old","secret":"oldsecret","agent_id":"7"}`
	must(t, e.st.SaveBindingSecret(ctx, "b1", old, 1))
	e.st.ExportBindingSecret(ctx, "b1")
	rev := func() int64 { b, _ := e.st.Binding(ctx, "b1"); return b.Revision }

	// step 1: secret write fails inside the tx -> revision unchanged
	trig(t, e, "r1", "BEFORE UPDATE ON binding_secrets")
	if w := cl.post("/admin/bindings/b1/rotate", url.Values{"revision": {"1"}}); w.Code != http.StatusServiceUnavailable {
		t.Fatal(w.Code)
	}
	if rev() != 1 {
		t.Fatal("revision bumped without secret")
	}
	if raw, exp, _ := e.st.BindingSecret(ctx, "b1"); raw != old || !exp {
		t.Fatal("old secret changed")
	}
	e.st.DB().Exec(`DROP TRIGGER r1`)

	// step 2: runtime apply fails -> old secret and export state restored
	e.rt.applyErr = errors.New("apply")
	if w := cl.post("/admin/bindings/b1/rotate", url.Values{"revision": {"1"}}); w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "已恢复旧凭证") {
		t.Fatal(w.Code)
	}
	if raw, exp, _ := e.st.BindingSecret(ctx, "b1"); raw != old || !exp {
		t.Fatal("old secret not restored", raw)
	}
	// restart recovery: what a new process would load is the old secret,
	// matching what the running runtime still uses.
	var sealedRev int64
	e.st.DB().QueryRow(`SELECT revision FROM binding_secrets WHERE binding_id='b1'`).Scan(&sealedRev)
	if sealedRev != rev() || rev() != 2 {
		t.Fatal("secret revision out of sync", sealedRev, rev())
	}

	// step 3: apply fails and restore fails -> warned
	trig(t, e, "r2", "BEFORE UPDATE ON binding_secrets WHEN NEW.exported_at != 0")
	if w := cl.post("/admin/bindings/b1/rotate", url.Values{"revision": {"2"}}); w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "恢复旧凭证失败") {
		t.Fatal(w.Code)
	}
	e.st.DB().Exec(`DROP TRIGGER r2`)

	// no previous secret: apply failure is reported, new one persists
	e.st.DB().Exec(`DELETE FROM binding_secrets`)
	if w := cl.post("/admin/bindings/b1/rotate", url.Values{"revision": {"3"}}); w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "重启后生效") {
		t.Fatal(w.Code)
	}

	// success keeps the agent id and syncs revision
	e.rt.applyErr = nil
	must(t, e.st.SaveBindingSecret(ctx, "b1", old, rev()))
	cl.post("/admin/bindings/b1/rotate", url.Values{"revision": {itoa64(rev())}})
	raw, exp, _ := e.st.BindingSecret(ctx, "b1")
	if raw == old || exp || !strings.Contains(raw, `"7"`) {
		t.Fatal("rotation result", raw, exp)
	}
}

// #43: accounts absent from a complete official list become UNKNOWN;
// new bindings and sends for them are refused; a partial sync changes nothing.
func TestSyncMarksMissingAccounts(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	seedAccount(t, e, "wk1")
	seedAccount(t, e, "wk2")
	cl := e.login()
	// partial failure: nothing marked
	e.kf.accounts = make([]wecom.Account, 150)
	for i := range e.kf.accounts {
		e.kf.accounts[i] = wecom.Account{OpenKfID: "x" + itoa64(int64(i)), Name: "n"}
	}
	e.kf.listErr[100] = errLost
	cl.post("/admin/accounts/sync", nil)
	if a, _ := e.st.Account(ctx, "wk1"); a.Status != state.AccountActive {
		t.Fatal("partial sync marked accounts")
	}
	delete(e.kf.listErr, 100)
	e.kf.accounts = []wecom.Account{{OpenKfID: "wk2", Name: "n"}}
	cl.post("/admin/accounts/sync", nil)
	if !strings.Contains(cl.flash(), "1 个本地账号不在官方列表中") {
		t.Fatal(cl.flash())
	}
	if a, _ := e.st.Account(ctx, "wk1"); a.Status != state.AccountUnknown {
		t.Fatal("missing account still ACTIVE", a.Status)
	}
	if a, _ := e.st.Account(ctx, "wk2"); a.Status != state.AccountActive {
		t.Fatal("present account changed")
	}
	if w := cl.post("/admin/bindings/create", url.Values{"id": {"b5"}, "open_kfid": {"wk1"}, "project_id": {"p"}, "callback_url": {"http://cc.internal:1/"}, "agent_id": {"1"}}); w.Code != http.StatusBadRequest {
		t.Fatal("binding created for missing account", w.Code)
	}
	// reappearing restores it
	e.kf.accounts = []wecom.Account{{OpenKfID: "wk1", Name: "n"}, {OpenKfID: "wk2", Name: "n"}}
	cl.post("/admin/accounts/sync", nil)
	if a, _ := e.st.Account(ctx, "wk1"); a.Status != state.AccountActive {
		t.Fatal("reappeared account not restored")
	}
	// empty official list marks all
	e.kf.accounts = nil
	cl.post("/admin/accounts/sync", nil)
	if a, _ := e.st.Account(ctx, "wk2"); a.Status != state.AccountUnknown {
		t.Fatal("empty list")
	}
	e.st.DB().Exec(`UPDATE kf_accounts SET status='ACTIVE'`)
	e.kf.accounts = []wecom.Account{{OpenKfID: "wk2", Name: "n"}}
	trig(t, e, "m1", "BEFORE UPDATE ON kf_accounts WHEN NEW.status='UNKNOWN'")
	if w := cl.post("/admin/accounts/sync", nil); w.Code != http.StatusServiceUnavailable {
		t.Fatal(w.Code)
	}
}

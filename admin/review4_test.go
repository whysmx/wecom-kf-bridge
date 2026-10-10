package admin

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/whysmx/wecom-kf-bridge/state"
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

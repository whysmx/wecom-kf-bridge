package bridge

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/whysmx/wecom-kf-bridge/state"
)

func (e *env) inject(event, table string) func() {
	e.t.Helper()
	name := "fault_" + event + "_" + table
	if _, err := e.st.DB().Exec(`CREATE TRIGGER ` + name + ` BEFORE ` + event + ` ON ` + table + ` BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		e.t.Fatal(err)
	}
	return func() { e.st.DB().Exec(`DROP TRIGGER ` + name) }
}

func authorize(e *env) { must(e.t, e.st.SetCustomerAuthorized(context.Background(), e.cust.ID, true)) }

// The state store failing at any step must never be reported as success,
// and a chunk that left upstream but could not be recorded is UNKNOWN.
func TestSendStoreFailuresAreNeverSuccess(t *testing.T) {
	e := newEnv(t)
	authorize(e)
	tok := e.token()

	undo := e.inject("UPDATE", "outbox")
	code, v := e.send(tok, e.cust.UID, "text", "a")
	undo()
	if code == 200 || errcode(v) == 0 || len(e.ad.sent()) != 0 {
		t.Fatalf("mark-sending failure: %d %v sent=%d", code, v, len(e.ad.sent()))
	}

	undo = e.inject("INSERT", "outbox_chunks")
	code, v = e.send(tok, e.cust.UID, "text", "b")
	undo()
	if errcode(v) != ErrCodeUnknown || len(e.ad.sent()) != 1 {
		t.Fatalf("unrecorded chunk must be unknown: %d %v", code, v)
	}
	if o := e.outboxes(); o[len(o)-1].State != state.OutboxUnknown {
		t.Fatalf("outbox %s", o[len(o)-1].State)
	}

	e.st.Close()
	if _, v := e.send(tok, e.cust.UID, "text", "c"); errcode(v) != ErrCodeUnavailable {
		t.Fatalf("closed store send: %v", v)
	}
	if _, v := e.do(http.MethodGet, "/cgi-bin/user/get?access_token="+tok+"&userid="+e.cust.UID, ""); errcode(v) != ErrCodeUnavailable {
		t.Fatalf("closed store user/get: %v", v)
	}
}

func TestSendPolicyRejectionsMapToCodes(t *testing.T) {
	e := newEnv(t)
	tok := e.token()
	must(t, e.st.SetCustomerAuthorized(context.Background(), e.cust.ID, false))
	if _, v := e.send(tok, e.cust.UID, "text", "x"); errcode(v) != ErrCodeForbidden {
		t.Fatalf("unauthorized: %v", v)
	}
	authorize(e)
	must(t, e.st.PutBinding(context.Background(), state.Binding{ID: "b1", EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p", Active: false}))
	if _, v := e.send(tok, e.cust.UID, "text", "x"); errcode(v) != ErrCodeDisabled {
		t.Fatalf("inactive binding: %v", v)
	}
	must(t, e.st.PutBinding(context.Background(), state.Binding{ID: "b1", EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p", Active: true}))
	e.ad.svcState = " "
	if _, v := e.send(tok, e.cust.UID, "text", "x"); errcode(v) == 0 {
		t.Fatalf("unmapped official state sent: %v", v)
	}
	if (errAPI{Message: "m"}).Error() != "m" {
		t.Fatal("error text")
	}
}

type failReader struct{}

func (failReader) Read([]byte) (int, error) { return 0, errors.New("entropy exhausted") }

func TestBuildCallbackFailures(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Random = failReader{} })
	if _, err := e.srv.BuildCallback("b1", InboundMessage{FromUserName: "u", MsgType: "text", Content: "x", CreateTime: 1}); err == nil {
		t.Fatal("callback built without randomness")
	}
	if _, err := e.srv.BuildCallback("missing", InboundMessage{}); err == nil {
		t.Fatal("unknown binding")
	}
}

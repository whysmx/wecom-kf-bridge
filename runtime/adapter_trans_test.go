package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	bridge "github.com/whysmx/wecom-kf-bridge"
	"github.com/whysmx/wecom-kf-bridge/state"
	"github.com/whysmx/wecom-kf-bridge/wecom"
)

func TestAdapterTransServiceStateAndSendMsgOnEvent(t *testing.T) {
	var body string
	var hitTrans, hitEvent int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "gettoken"):
			fmt.Fprint(w, `{"errcode":0,"access_token":"TOK","expires_in":7200}`)
		case strings.Contains(r.URL.Path, "service_state/trans"):
			hitTrans++
			fmt.Fprint(w, body)
		case strings.Contains(r.URL.Path, "send_msg_on_event"):
			hitEvent++
			fmt.Fprint(w, body)
		default:
			fmt.Fprint(w, body)
		}
	}))
	t.Cleanup(srv.Close)
	cl := wecom.NewClient(srv.URL, "c", "s")
	cl.MaxRetries = 0
	l := &fieldLogger{}
	a := &WeComAdapter{
		clients: map[string]*wecom.Client{"e1": cl},
		tokens:  map[string]*tokenCache{"e1": {client: cl, now: time.Now}},
		routes:  map[string]bindingRoute{"b1": {"e1", "kf1"}},
		states:  map[int]string{1: state.CustomerAIEligible, 3: state.CustomerHuman},
		now:     time.Now, logger: l,
	}
	ctx := context.Background()
	cust := bridge.Customer{ExternalUserID: "ext-1"}
	b := bridge.Binding{ID: "b1"}

	// route error
	if _, err := a.TransServiceState(ctx, bridge.Binding{ID: "nope"}, cust, bridge.OfficialStateAI); err == nil {
		t.Fatal("route")
	}

	// API errcode → SendRejected
	body = `{"errcode":48002,"errmsg":"forbidden"}`
	_, err := a.TransServiceState(ctx, b, cust, bridge.OfficialStateAI)
	if err == nil {
		t.Fatal("errcode")
	}
	var se *bridge.SendError
	if !errors.As(err, &se) || se.Kind != bridge.SendRejected {
		t.Fatalf("want SendRejected got %T %v", err, err)
	}

	// call error (bad json)
	body = `not-json`
	if _, err := a.TransServiceState(ctx, b, cust, bridge.OfficialStateWaiting); err == nil {
		t.Fatal("bad json")
	}

	// success with msg_code
	body = `{"errcode":0,"msg_code":"mc-1"}`
	code, err := a.TransServiceState(ctx, b, cust, bridge.OfficialStateAI)
	if err != nil || code != "mc-1" || hitTrans == 0 {
		t.Fatalf("ok %q %v hit=%d", code, err, hitTrans)
	}

	// SendMsgOnEvent empty → no-op
	if err := a.SendMsgOnEvent(ctx, b, "  ", "hi"); err != nil {
		t.Fatal(err)
	}
	if err := a.SendMsgOnEvent(ctx, b, "c", "  "); err != nil {
		t.Fatal(err)
	}
	if hitEvent != 0 {
		t.Fatal("empty should skip")
	}

	// route error
	if err := a.SendMsgOnEvent(ctx, bridge.Binding{ID: "nope"}, "c", "hi"); err == nil {
		t.Fatal("route")
	}

	// upstream fail
	body = `{"errcode":40013,"errmsg":"bad"}`
	if err := a.SendMsgOnEvent(ctx, b, "c", "welcome"); err == nil {
		t.Fatal("want err")
	}

	// success
	body = `{"errcode":0,"msgid":"m1"}`
	if err := a.SendMsgOnEvent(ctx, b, "c", "welcome"); err != nil || hitEvent == 0 {
		t.Fatalf("%v hit=%d", err, hitEvent)
	}
}

package runtime

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/whysmx/wecom-kf-bridge/wecom"
)

func TestConsoleKFTransAndSendMsgOnEvent(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "gettoken") {
			fmt.Fprint(w, `{"errcode":0,"access_token":"T","expires_in":7200}`)
			return
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	cl := wecom.NewClient(srv.URL, "c", "s")
	cl.MaxRetries = 0
	k := consoleKF{client: cl, tokens: &tokenCache{client: cl, now: time.Now}}
	ctx := context.Background()

	// token fail
	bad := consoleKF{client: cl, tokens: &tokenCache{client: wecom.NewClient("http://127.0.0.1:1", "c", "s"), now: time.Now}}
	if _, err := bad.TransServiceState(ctx, "kf", "ext", 1); err == nil {
		t.Fatal("token")
	}
	if err := bad.SendMsgOnEvent(ctx, "c", "hi"); err == nil {
		t.Fatal("token event")
	}

	// trans API error
	body = `{"errcode":48002,"errmsg":"no"}`
	if _, err := k.TransServiceState(ctx, "kf", "ext", 1); err == nil {
		t.Fatal("api")
	}

	// trans OK
	body = `{"errcode":0,"msg_code":"mc"}`
	code, err := k.TransServiceState(ctx, "kf", "ext", 1)
	if err != nil || code != "mc" {
		t.Fatalf("%q %v", code, err)
	}

	// SendMsgOnEvent empty no-op
	if err := k.SendMsgOnEvent(ctx, "", "x"); err != nil {
		t.Fatal(err)
	}
	if err := k.SendMsgOnEvent(ctx, "c", ""); err != nil {
		t.Fatal(err)
	}

	// SendMsgOnEvent fail
	body = `{"errcode":40013,"errmsg":"bad"}`
	if err := k.SendMsgOnEvent(ctx, "c", "hi"); err == nil {
		t.Fatal("want err")
	}

	// SendMsgOnEvent OK
	body = `{"errcode":0,"msgid":"m"}`
	if err := k.SendMsgOnEvent(ctx, "c", "hi"); err != nil {
		t.Fatal(err)
	}
}

func TestConsoleKFUpdateAccount(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "gettoken") {
			fmt.Fprint(w, `{"errcode":0,"access_token":"T","expires_in":7200}`)
			return
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	cl := wecom.NewClient(srv.URL, "c", "s")
	cl.MaxRetries = 0
	k := consoleKF{client: cl, tokens: &tokenCache{client: cl, now: time.Now}}
	ctx := context.Background()
	bad := consoleKF{client: cl, tokens: &tokenCache{client: wecom.NewClient("http://127.0.0.1:1", "c", "s"), now: time.Now}}
	if err := bad.UpdateAccount(ctx, "kf", "n", "m"); err == nil {
		t.Fatal("token")
	}
	body = `{"errcode":0}`
	if err := k.UpdateAccount(ctx, "kf", "n", "m"); err != nil {
		t.Fatal(err)
	}
}

func TestSyncWorkerFinish(t *testing.T) {
	w := &SyncWorker{running: map[string]bool{"sc": true}}
	w.finish("sc")
	if w.running["sc"] {
		t.Fatal("not cleared")
	}
}

package wecom

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestWebhook(t *testing.T, receiver string) *Webhook {
	t.Helper()
	w, err := NewWebhook("tok", validKeyString(), receiver)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestEnvelopeEscapesCDATAInjection(t *testing.T) {
	evil := "corp]]></ToUserName><Injected>1</Injected><x><![CDATA["
	w := newTestWebhook(t, evil)
	body, err := w.BuildCallbackBody("<xml/>")
	if err != nil {
		t.Fatal(err)
	}
	cb, err := w.BuildClientCallback(ClientMessage{ToUserName: "a", FromUserName: "b", CreateTime: 1, MsgID: 1, AgentID: "1]]><Evil/>"}, "1", "n")
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range [][]byte{body, cb.Body} {
		if bytes.Contains(b, []byte("<Injected>")) || bytes.Contains(b, []byte("<Evil/>")) {
			t.Fatalf("markup injected: %s", b)
		}
		var env outerEnvelope
		if err := xml.Unmarshal(b, &env); err != nil || env.ToUserName != evil || env.Encrypt == "" {
			t.Fatalf("round trip %v %+v", err, env)
		}
	}
}

func TestNotificationTokenRedacted(t *testing.T) {
	w := newTestWebhook(t, "corp")
	logged := &strings.Builder{}
	w.Logger = loggerFunc(func(e string, f map[string]any) { fmt.Fprintf(logged, "%s %v\n", e, f) })
	var got Notification
	w.OnNotification = func(_ context.Context, n Notification) error {
		got = n
		return fmt.Errorf("fail: %v", n) // even a careless handler error must not leak
	}
	inner := `<xml><ToUserName>corp</ToUserName><MsgType>event</MsgType><Event>kf_msg_or_event</Event><Token>SECRET-PULL</Token><OpenKfId>kf</OpenKfId></xml>`
	body, _ := w.BuildCallbackBody(inner)
	var env outerEnvelope
	_ = xml.Unmarshal(body, &env)
	ts := fmt.Sprint(time.Now().Unix())
	req := httptest.NewRequest(http.MethodPost, "/?msg_signature="+w.Signature(ts, "n", env.Encrypt)+"&timestamp="+ts+"&nonce=n", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	w.ServeHTTP(rr, req)
	if got.Token != "SECRET-PULL" {
		t.Fatalf("token not delivered to handler: %q", got.Token)
	}
	for _, s := range []string{fmt.Sprint(got), fmt.Sprintf("%+v", got), fmt.Sprintf("%#v", got), fmt.Sprint(got.LogFields()), logged.String(), rr.Body.String()} {
		if strings.Contains(s, "SECRET-PULL") {
			t.Fatalf("token leaked: %s", s)
		}
	}
}

type loggerFunc func(string, map[string]any)

func (f loggerFunc) Log(e string, m map[string]any) { f(e, m) }

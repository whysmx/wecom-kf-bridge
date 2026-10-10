package wecom

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRouterRoutesByTenantKeyOnly(t *testing.T) {
	keyA := strings.TrimRight(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)), "=")
	keyB := strings.TrimRight(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32)), "=")
	a, _ := NewWebhook("tokA", keyA, "corpA")
	b, _ := NewWebhook("tokB", keyB, "corpB")
	var hitA, hitB int
	a.OnNotification = func(context.Context, Notification) error { hitA++; return nil }
	b.OnNotification = func(context.Context, Notification) error { hitB++; return nil }
	r := NewRouter()
	if err := r.Handle("tenant-a", a); err != nil {
		t.Fatal(err)
	}
	if err := r.Handle("tenant-b", b); err != nil {
		t.Fatal(err)
	}
	if r.Handle("tenant-a", a) == nil || r.Handle("bad/key", a) == nil || r.Handle("x", nil) == nil {
		t.Fatal("invalid registration accepted")
	}
	post := func(path string, w *Webhook) int {
		body, _ := w.BuildCallbackBody(`<xml><MsgType>event</MsgType><Event>kf_msg_or_event</Event></xml>`)
		var env outerEnvelope
		_ = xml.Unmarshal(body, &env)
		q := fmt.Sprintf("?msg_signature=%s&timestamp=1&nonce=n", w.Signature("1", "n", env.Encrypt))
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, path+q, bytes.NewReader(body)))
		return rr.Code
	}
	if c := post(RouterPrefix+"tenant-a", a); c != 200 || hitA != 1 {
		t.Fatalf("tenant a: %d", c)
	}
	// tenant B's callback sent to tenant A's path is rejected; no key fallback
	if c := post(RouterPrefix+"tenant-a", b); c != http.StatusForbidden || hitB != 0 {
		t.Fatalf("cross-tenant accepted: %d", c)
	}
	for _, p := range []string{RouterPrefix + "unknown", RouterPrefix, "/wecom/callback", RouterPrefix + "a/b"} {
		if c := post(p, a); c != http.StatusNotFound {
			t.Fatalf("%s -> %d", p, c)
		}
	}
}

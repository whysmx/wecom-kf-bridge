package runtime

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	bridge "github.com/whysmx/wecom-kf-bridge"
	"github.com/whysmx/wecom-kf-bridge/state"
	"github.com/whysmx/wecom-kf-bridge/wecom"
)

func TestDeliverImageAndFileCallback(t *testing.T) {
	st, c := unitStore(t)
	ctx := context.Background()
	var gotBodies [][]byte
	cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBodies = append(gotBodies, b)
		w.WriteHeader(200)
	}))
	defer cc.Close()
	srv, err := bridge.NewServer(bridge.Config{Store: st, Bindings: []bridge.Binding{{ID: "b1", CorpID: "v", CallbackToken: "t", CallbackAESKey: key43(4), Enabled: true}}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	imgPayload := bridge.EncodeMediaPayload("IMGMEDIA", "")
	filePayload := bridge.EncodeMediaPayload("FILEMEDIA", "../dir/doc.pdf")
	_, err = st.CommitSyncPage(ctx, "sc", "c1", false, []state.InboxMessage{
		{BindingID: "b1", ExternalMsgID: "img1", CustomerID: c.ID, Generation: 1, Type: "image", PayloadRef: imgPayload, CreateTime: now},
		{BindingID: "b1", ExternalMsgID: "file1", CustomerID: c.ID, Generation: 1, Type: "file", PayloadRef: filePayload, CreateTime: now},
		{BindingID: "b1", ExternalMsgID: "badimg", CustomerID: c.ID, Generation: 1, Type: "image", PayloadRef: "", CreateTime: now},
	})
	if err != nil {
		t.Fatal(err)
	}
	d := &DeliveryWorker{Store: st, Server: srv, CallbackURL: map[string]string{"b1": cc.URL}, AgentID: map[string]string{"b1": "1002"}, HTTP: cc.Client(), MaxAttempts: 3, MaxConc: 1, Logger: nopRuntimeLogger{}, Interval: time.Hour}
	d.DeliverOnce(ctx)
	get := func(id string) state.InboxMessage { m, _ := st.InboxByExternalID(ctx, "sc", id); return m }
	if get("img1").State != state.InboxHTTPAccepted || get("file1").State != state.InboxHTTPAccepted {
		t.Fatalf("img=%s file=%s", get("img1").State, get("file1").State)
	}
	if get("badimg").State != state.InboxUnsupported {
		t.Fatalf("badimg=%s", get("badimg").State)
	}
	if len(gotBodies) != 2 {
		t.Fatalf("callbacks=%d", len(gotBodies))
	}
	in, err := buildInboundCallback("uid", "1002", state.InboxMessage{Type: "file", PayloadRef: filePayload, CreateTime: now, CompatMsgID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if in.MsgType != "file" || in.MediaID != "FILEMEDIA" || in.FileName != "doc.pdf" {
		t.Fatalf("%+v", in)
	}
	plain, err := marshalProbe(in)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(plain, []byte("<FileName>doc.pdf</FileName>")) || !bytes.Contains(plain, []byte("<MediaId>FILEMEDIA</MediaId>")) {
		t.Fatalf("%s", plain)
	}
}

func marshalProbe(m bridge.InboundMessage) ([]byte, error) {
	type cb struct {
		XMLName  xml.Name `xml:"xml"`
		MsgType  string   `xml:"MsgType"`
		MediaID  string   `xml:"MediaId,omitempty"`
		FileName string   `xml:"FileName,omitempty"`
	}
	return xml.Marshal(cb{MsgType: m.MsgType, MediaID: m.MediaID, FileName: m.FileName})
}

func TestDeliverRetryOnCallbackFailureKeepsMessage(t *testing.T) {
	st, c := unitStore(t)
	ctx := context.Background()
	srv, _ := bridge.NewServer(bridge.Config{Store: st, Bindings: []bridge.Binding{{ID: "b1", CorpID: "v", CallbackToken: "t", CallbackAESKey: key43(4), Enabled: true}}})
	now := time.Now()
	payload := bridge.EncodeMediaPayload("MID", "a.pdf")
	_, err := st.CommitSyncPage(ctx, "sc", "c1", false, []state.InboxMessage{
		{BindingID: "b1", ExternalMsgID: "f1", CustomerID: c.ID, Generation: 1, Type: "file", PayloadRef: payload, CreateTime: now},
	})
	if err != nil {
		t.Fatal(err)
	}
	d := &DeliveryWorker{Store: st, Server: srv, CallbackURL: map[string]string{"b1": "http://127.0.0.1:1/cb"}, AgentID: map[string]string{}, HTTP: &http.Client{Timeout: time.Second}, MaxAttempts: 3, MaxConc: 1, Logger: nopRuntimeLogger{}, Interval: time.Hour}
	d.DeliverOnce(ctx)
	m, _ := st.InboxByExternalID(ctx, "sc", "f1")
	if m.State != state.InboxRetryWait {
		t.Fatalf("state=%s want RETRY_WAIT", m.State)
	}
}

func TestAdapterGetMedia(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/cgi-bin/gettoken", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"errcode":0,"access_token":"TOK","expires_in":7200}`)
	})
	mux.HandleFunc("/cgi-bin/media/get", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("access_token") != "TOK" || r.URL.Query().Get("media_id") != "MID1" {
			t.Fatalf("query %v", r.URL.Query())
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Disposition", `attachment; filename="a.png"`)
		_, _ = w.Write([]byte("PNGDATA"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	cl := wecom.NewClient(srv.URL, "c", "s")
	cl.HTTPClient = srv.Client()
	a := &WeComAdapter{
		clients: map[string]*wecom.Client{"e1": cl},
		tokens:  map[string]*tokenCache{"e1": {client: cl, now: time.Now}},
		routes:  map[string]bindingRoute{"b1": {"e1", "kf1"}},
		now:     time.Now,
	}
	ctx := context.Background()
	b := bridge.Binding{ID: "b1"}
	if _, err := a.GetMedia(ctx, b, "  "); err == nil {
		t.Fatal("empty media_id")
	}
	if _, err := a.GetMedia(ctx, bridge.Binding{ID: "nope"}, "MID1"); err == nil {
		t.Fatal("unknown binding")
	}
	md, err := a.GetMedia(ctx, b, "MID1")
	if err != nil {
		t.Fatal(err)
	}
	if string(md.Data) != "PNGDATA" || md.ContentType != "image/png" || md.ContentDisposition == "" {
		t.Fatalf("%+v", md)
	}
}

func TestBuildInboundCallbackVoiceAndUnknown(t *testing.T) {
	now := time.Now()
	voicePayload := bridge.EncodeMediaPayload("VOICEMEDIA", "")
	in, err := buildInboundCallback("uid", "1002", state.InboxMessage{Type: "voice", PayloadRef: voicePayload, CreateTime: now, CompatMsgID: 9})
	if err != nil || in.MsgType != "voice" || in.MediaID != "VOICEMEDIA" || in.FileName != "" {
		t.Fatalf("%+v %v", in, err)
	}
	if _, err := buildInboundCallback("uid", "1002", state.InboxMessage{Type: "location", PayloadRef: "x", CreateTime: now}); err == nil {
		t.Fatal("unknown type")
	}
	if _, err := buildInboundCallback("uid", "1002", state.InboxMessage{Type: "image", PayloadRef: `{"media_id":""}`, CreateTime: now}); err == nil {
		t.Fatal("bad payload")
	}
	if _, err := buildInboundCallback("uid", "1002", state.InboxMessage{Type: "image", PayloadRef: "", CreateTime: now}); err == nil {
		t.Fatal("empty payload")
	}
}

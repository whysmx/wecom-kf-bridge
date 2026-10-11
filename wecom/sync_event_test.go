package wecom

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/whysmx/wecom-kf-bridge/state"
)

func rawServer(t *testing.T, body string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
	t.Cleanup(srv.Close)
	return srv
}

// Real sync_msg returns msg_list[].event as an object for msgtype=event.
func TestSyncAcceptsEventObject(t *testing.T) {
	fixture, err := os.ReadFile("testdata/sync_msg_enter_session.json")
	if err != nil {
		t.Fatal(err)
	}
	var page SyncResponse
	if err := json.Unmarshal(fixture, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 2 || len(page.Bad) != 0 || page.Messages[0].EventType() != "enter_session" || page.Messages[1].EventType() != "" {
		t.Fatalf("%+v", page)
	}
	if (SyncMessage{Event: json.RawMessage(`"legacy"`)}).EventType() != "" {
		t.Fatal("non-object event")
	}
	st := syncStore(t)
	c := NewClient(rawServer(t, string(fixture)).URL, "c", "s")
	res, err := c.SyncAll(context.Background(), SyncOptions{Scope: "sc", EnterpriseID: "e1", BindingID: "b1", AccessToken: "t", Store: st, Origins: OriginPolicy{CustomerOrigins: []int{3}}})
	if err != nil || res.Inserted != 2 || res.Cursor != "4gw7MepFLfgF2VC5npN" {
		t.Fatalf("%+v %v", res, err)
	}
	ev, _ := st.InboxByExternalID(context.Background(), "sc", "from_msgid_4622416642169452483")
	if ev.State != state.InboxIgnored || ev.ErrorCategory != CategoryEvent {
		t.Fatalf("event %+v", ev)
	}
	tx, _ := st.InboxByExternalID(context.Background(), "sc", "from_msgid_REDACTED_TEXT_1")
	if tx.State != state.InboxReceived {
		t.Fatalf("text %+v", tx)
	}
}

const badPage = `{"errcode":0,"next_cursor":"c1","has_more":0,"msg_list":[
 {"msgid":"bad-1","msgtype":"text","send_time":"not-a-number","text":{"content":"SECRET-BODY"}},
 {"msgid":"t1","open_kfid":"kf1","external_userid":"ext-a","send_time":1800000001,"origin":3,"msgtype":"text","text":{"content":"一"}},
 {"msgid":"t2","open_kfid":"kf1","external_userid":"ext-a","send_time":1800000002,"origin":3,"msgtype":"text","text":{"content":"二"}},
 {"msgid":12345,"msgtype":"text","origin":"x"},
 {"msgid":"t3","open_kfid":"kf1","external_userid":"ext-b","send_time":1800000003,"origin":3,"msgtype":"text","text":{"content":"三"}}]}`

// One undecodable item must not fail the page: the good ones are ingested,
// the bad one is quarantined under its msgid, and the cursor advances.
func TestSyncQuarantinesBadMessage(t *testing.T) {
	st := syncStore(t)
	log := &syncLog{}
	c := NewClient(rawServer(t, badPage).URL, "c", "s")
	opts := SyncOptions{Scope: "sc", EnterpriseID: "e1", BindingID: "b1", OpenKfID: "kf1", AccessToken: "t", Store: st, Origins: OriginPolicy{CustomerOrigins: []int{3}}, Logger: log}
	res, err := c.SyncAll(context.Background(), opts)
	if err != nil || res.Cursor != "c1" || res.Messages != 5 || res.Inserted != 4 {
		t.Fatalf("%+v %v", res, err)
	}
	for _, id := range []string{"t1", "t2", "t3"} {
		m, err := st.InboxByExternalID(context.Background(), "sc", id)
		if err != nil || m.State != state.InboxReceived || m.CustomerID == "" {
			t.Fatalf("%s %+v %v", id, m, err)
		}
	}
	bad, err := st.InboxByExternalID(context.Background(), "sc", "bad-1")
	if err != nil || bad.State != state.InboxIgnored || bad.ErrorCategory != CategoryUnparseable || bad.PayloadRef != "" {
		t.Fatalf("bad %+v %v", bad, err)
	}
	if !log.has("wecom_sync_message_quarantined") {
		t.Fatal("not alerted")
	}
	for _, f := range log.fields {
		if strings.Contains(fmt.Sprint(f), "SECRET-BODY") {
			t.Fatal("raw body logged")
		}
	}
	// re-sync of the same page is idempotent (no loop, no duplicates)
	res, err = c.SyncAll(context.Background(), opts)
	if err != nil || res.Inserted != 0 {
		t.Fatalf("resync %+v %v", res, err)
	}
	// a broken envelope still fails the page (cursor not committed)
	c2 := NewClient(rawServer(t, `{"errcode":0,"msg_list":{}}`).URL, "c", "s")
	if _, err := c2.SyncMsg(context.Background(), "t", SyncRequest{}); err == nil {
		t.Fatal("bad envelope accepted")
	}
}

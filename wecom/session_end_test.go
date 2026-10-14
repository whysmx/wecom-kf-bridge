package wecom

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/whysmx/wecom-kf-bridge/state"
)

func TestSessionEndChangeHelpers(t *testing.T) {
	m := SyncMessage{
		MsgType: "event",
		Event: json.RawMessage(`{
			"event_type":"session_status_change",
			"open_kfid":"kf-e",
			"external_userid":"ext-e",
			"change_type":3,
			"msg_code":"CODE3"
		}`),
	}
	if code, ok := m.SessionEndChange(); !ok || code != "CODE3" {
		t.Fatalf("end: %q %v", code, ok)
	}
	if m.EffectiveExternalUserID() != "ext-e" || m.EffectiveOpenKfID() != "kf-e" {
		t.Fatal(m.EffectiveExternalUserID(), m.EffectiveOpenKfID())
	}
	m2 := SyncMessage{MsgType: "event", Event: json.RawMessage(`{"event_type":"session_status_change","change_type":1}`)}
	if _, ok := m2.SessionEndChange(); ok {
		t.Fatal("change_type=1 must not be session end")
	}
	m3 := SyncMessage{MsgType: "event", Event: json.RawMessage(`{"event_type":"enter_session"}`)}
	if _, ok := m3.SessionEndChange(); ok {
		t.Fatal("enter_session")
	}
}

func TestClassifySessionStatusChangeEnd(t *testing.T) {
	st := syncStore(t)
	body := `{"errcode":0,"next_cursor":"c-end","has_more":0,"msg_list":[
		{"msgid":"se-1","send_time":1800000000,"origin":4,"msgtype":"event",
		 "event":{"event_type":"session_status_change","open_kfid":"kf1","external_userid":"ext-end","change_type":3,"msg_code":"ENDCODE"}},
		{"msgid":"se-2","send_time":1800000001,"origin":4,"msgtype":"event",
		 "event":{"event_type":"session_status_change","open_kfid":"kf1","external_userid":"ext-end","change_type":1}},
		{"msgid":"se-3","open_kfid":"kf1","external_userid":"ext-end","send_time":1800000002,"origin":3,"msgtype":"text","text":{"content":"hi"}}
	]}`
	var ends []SessionEndEvent
	c := NewClient(rawServer(t, body).URL, "c", "s")
	res, err := c.SyncAll(context.Background(), SyncOptions{
		Scope: "sc", EnterpriseID: "e1", BindingID: "b1", OpenKfID: "kf1", AccessToken: "t",
		Store: st, Origins: OriginPolicy{CustomerOrigins: []int{3}},
		OnSessionEnd: func(_ context.Context, ev SessionEndEvent) { ends = append(ends, ev) },
	})
	if err != nil || res.Inserted != 3 {
		t.Fatalf("%+v %v", res, err)
	}
	ev, _ := st.InboxByExternalID(context.Background(), "sc", "se-1")
	if ev.State != state.InboxIgnored || ev.ErrorCategory != CategorySessionEnd || ev.CustomerID == "" || ev.PayloadRef != "ENDCODE" {
		t.Fatalf("session end %+v", ev)
	}
	other, _ := st.InboxByExternalID(context.Background(), "sc", "se-2")
	if other.ErrorCategory != CategoryEvent {
		t.Fatalf("change_type=1 %+v", other)
	}
	if len(ends) != 1 || ends[0].CustomerID != ev.CustomerID || ends[0].MsgCode != "ENDCODE" || ends[0].BindingID != "b1" {
		t.Fatalf("hook %+v", ends)
	}
}

func TestClassifySessionEndForeignKf(t *testing.T) {
	st := syncStore(t)
	body := `{"errcode":0,"next_cursor":"c","has_more":0,"msg_list":[
		{"msgid":"fx","send_time":1,"origin":4,"msgtype":"event",
		 "event":{"event_type":"session_status_change","open_kfid":"other","external_userid":"ext","change_type":3}}
	]}`
	c := NewClient(rawServer(t, body).URL, "c", "s")
	_, err := c.SyncAll(context.Background(), SyncOptions{
		Scope: "sc", EnterpriseID: "e1", BindingID: "b1", OpenKfID: "kf1", AccessToken: "t",
		Store: st, Origins: OriginPolicy{CustomerOrigins: []int{3}},
	})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := st.InboxByExternalID(context.Background(), "sc", "fx")
	if m.ErrorCategory != CategoryForeignKf {
		t.Fatalf("%+v", m)
	}
}

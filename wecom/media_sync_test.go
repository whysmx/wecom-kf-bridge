package wecom

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/whysmx/wecom-kf-bridge/state"
)

func TestNestedMediaParsing(t *testing.T) {
	t.Parallel()
	raw := `{
  "errcode":0,"next_cursor":"c1","has_more":0,
  "msg_list":[
    {"msgid":"i1","open_kfid":"kf1","external_userid":"ext","send_time":1800000001,"origin":3,"msgtype":"image","image":{"media_id":"IMG1"}},
    {"msgid":"f1","open_kfid":"kf1","external_userid":"ext","send_time":1800000002,"origin":3,"msgtype":"file","file":{"media_id":"FILE1","filename":"../x/a.pdf"}},
    {"msgid":"v1","open_kfid":"kf1","external_userid":"ext","send_time":1800000003,"origin":3,"msgtype":"voice","voice":{"media_id":"VOC1"}},
    {"msgid":"bad","open_kfid":"kf1","external_userid":"ext","send_time":1800000004,"origin":3,"msgtype":"image"}
  ]
}`
	var page SyncResponse
	if err := json.Unmarshal([]byte(raw), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 4 {
		t.Fatalf("msgs=%d", len(page.Messages))
	}
	mid, fn := page.Messages[0].NestedMedia()
	if mid != "IMG1" || fn != "" {
		t.Fatalf("image: %s %s", mid, fn)
	}
	mid, fn = page.Messages[1].NestedMedia()
	if mid != "FILE1" || fn != "../x/a.pdf" {
		t.Fatalf("file: %s %s", mid, fn)
	}
	mid, _ = page.Messages[2].NestedMedia()
	if mid != "VOC1" {
		t.Fatalf("voice: %s", mid)
	}
	mid, _ = page.Messages[3].NestedMedia()
	if mid != "" {
		t.Fatalf("missing nested want empty, got %s", mid)
	}
}

type memSyncStore struct {
	msgs []state.InboxMessage
}

func (m *memSyncStore) Scope(context.Context, string) (state.SyncScope, error) {
	return state.SyncScope{}, nil
}
func (m *memSyncStore) CommitSyncPage(_ context.Context, _ string, _ string, _ bool, msgs []state.InboxMessage) (int, error) {
	m.msgs = append(m.msgs, msgs...)
	return len(msgs), nil
}
func (m *memSyncStore) EnsureCustomer(context.Context, string, string, string) (state.Customer, error) {
	return state.Customer{ID: "c1", Generation: 1}, nil
}
func (m *memSyncStore) SetSyncState(context.Context, string, string) error { return nil }

func TestClassifyMediaPayload(t *testing.T) {
	t.Parallel()
	st := &memSyncStore{}
	opts := SyncOptions{
		Scope: "sc", EnterpriseID: "e", BindingID: "b1", OpenKfID: "kf1",
		Store: st, Origins: OriginPolicy{CustomerOrigins: []int{3}},
		Now: func() time.Time { return time.Unix(1800000000, 0) },
	}
	page := SyncResponse{Messages: []SyncMessage{
		{MsgID: "i1", OpenKfID: "kf1", ExternalUserID: "ext", SendTime: 1800000001, Origin: 3, MsgType: "image", Image: &SyncMedia{MediaID: "IMG1"}},
		{MsgID: "f1", OpenKfID: "kf1", ExternalUserID: "ext", SendTime: 1800000002, Origin: 3, MsgType: "file", File: &SyncMedia{MediaID: "FILE1", Filename: "../x/a.pdf"}},
		{MsgID: "t1", OpenKfID: "kf1", ExternalUserID: "ext", SendTime: 1800000003, Origin: 3, MsgType: "text", Text: &SyncText{Content: "hi"}},
	}}
	msgs, err := opts.classify(context.Background(), page, time.Unix(1800000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("len=%d", len(msgs))
	}
	if msgs[0].Type != "image" || msgs[0].PayloadRef == "" || msgs[0].State != state.InboxReceived {
		t.Fatalf("image row: %+v", msgs[0])
	}
	if msgs[1].Type != "file" || msgs[1].PayloadRef == "" {
		t.Fatalf("file row: %+v", msgs[1])
	}
	if msgs[2].PayloadRef != "hi" {
		t.Fatalf("text payload %q", msgs[2].PayloadRef)
	}
}

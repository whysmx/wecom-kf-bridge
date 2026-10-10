package wecom

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/whysmx/wecom-kf-bridge/state"
)

type syncLog struct {
	mu     sync.Mutex
	events []string
}

func (l *syncLog) Log(e string, _ map[string]any) {
	l.mu.Lock()
	l.events = append(l.events, e)
	l.mu.Unlock()
}
func (l *syncLog) has(e string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, x := range l.events {
		if x == e {
			return true
		}
	}
	return false
}

func syncStore(t *testing.T) *state.Store {
	t.Helper()
	st, err := state.OpenWithOptions(t.TempDir()+"/s.db", state.Options{MasterKey: bytes.Repeat([]byte{5}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err := st.PutEnterprise(ctx, state.Enterprise{ID: "e1", TenantID: "t1", CorpID: "c1", CredentialRef: "env:S"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutBinding(ctx, state.Binding{ID: "b1", EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p", Active: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureScope(ctx, "sc", "b1"); err != nil {
		t.Fatal(err)
	}
	return st
}

// pagedServer serves the given pages in order and records request cursors.
func pagedServer(t *testing.T, pages ...SyncResponse) (*httptest.Server, *[]string) {
	var mu sync.Mutex
	var cursors []string
	i := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req SyncRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		cursors = append(cursors, req.Cursor)
		p := pages[len(pages)-1]
		if i < len(pages) {
			p = pages[i]
		}
		i++
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(p)
	}))
	t.Cleanup(srv.Close)
	return srv, &cursors
}

func opts(st *state.Store, l Logger) SyncOptions {
	return SyncOptions{Scope: "sc", EnterpriseID: "e1", BindingID: "b1", OpenKfID: "kf1", AccessToken: "tok", Store: st, Origins: OriginPolicy{CustomerOrigins: []int{3}}, Logger: l}
}

func TestSyncAllNoFabricatedCompatIDAndOriginFilter(t *testing.T) {
	st := syncStore(t)
	srv, _ := pagedServer(t,
		SyncResponse{NextCursor: "c1", HasMore: 1, Messages: []SyncMessage{
			{MsgID: "wx-1", OpenKfID: "kf1", ExternalUserID: "ext-a", SendTime: 1700000000, Origin: 3, MsgType: "text", Text: &SyncText{Content: "问"}},
			{MsgID: "wx-2", OpenKfID: "kf1", ExternalUserID: "ext-a", Origin: 5, MsgType: "text", Text: &SyncText{Content: "人工回复"}},
			{MsgID: "wx-3", OpenKfID: "kf1", Origin: 4, MsgType: "event", Event: "enter_session"},
		}},
		SyncResponse{NextCursor: "c2", HasMore: 0, Messages: []SyncMessage{
			{MsgID: "wx-4", OpenKfID: "kf-other", ExternalUserID: "ext-b", Origin: 3, MsgType: "text"},
			{MsgID: "wx-5", OpenKfID: "kf1", Origin: 3, MsgType: "text"},
			{MsgID: "wx-6", OpenKfID: "kf1", ExternalUserID: "ext-a", Origin: 9, MsgType: "text"},
		}},
	)
	ctx := context.Background()
	res, err := NewClient(srv.URL, "c", "s").SyncAll(ctx, opts(st, nil))
	if err != nil || res.Pages != 2 || res.Inserted != 6 || res.HasMore || res.Truncated {
		t.Fatalf("%+v %v", res, err)
	}
	m1, _ := st.InboxByExternalID(ctx, "sc", "wx-1")
	if m1.State != state.InboxReceived || m1.CustomerID == "" || m1.Generation != 1 || m1.PayloadRef != "问" || m1.CompatMsgID != 1 {
		t.Fatalf("customer message %+v", m1)
	}
	cust, _ := st.Customer(ctx, m1.CustomerID)
	if cust.ExternalUserID != "ext-a" || cust.LastInboundAt.Unix() != 1700000000 {
		t.Fatalf("customer association/window %+v", cust)
	}
	want := map[string]string{"wx-2": "origin:5", "wx-3": CategoryEvent, "wx-4": CategoryForeignKf, "wx-5": CategoryNoExternalUID, "wx-6": "origin:9"}
	for id, cat := range want {
		m, err := st.InboxByExternalID(ctx, "sc", id)
		if err != nil || m.State != state.InboxIgnored || m.ErrorCategory != cat || m.CustomerID != "" || m.PayloadRef != "" {
			t.Fatalf("%s not ignored as %s: %+v %v", id, cat, m, err)
		}
	}
	ids := map[int64]bool{}
	for _, id := range []string{"wx-1", "wx-2", "wx-3", "wx-4", "wx-5", "wx-6"} {
		m, _ := st.InboxByExternalID(ctx, "sc", id)
		if m.CompatMsgID <= 0 || ids[m.CompatMsgID] {
			t.Fatalf("compat id not unique/positive: %d", m.CompatMsgID)
		}
		ids[m.CompatMsgID] = true
	}
}

func TestSyncAllMissingMsgIDPausesWithoutCommit(t *testing.T) {
	st := syncStore(t)
	l := &syncLog{}
	srv, _ := pagedServer(t, SyncResponse{NextCursor: "c1", HasMore: 0, Messages: []SyncMessage{{MsgID: "", ExternalUserID: "x", Origin: 3, MsgType: "text"}}})
	_, err := NewClient(srv.URL, "c", "s").SyncAll(context.Background(), opts(st, l))
	if !errors.Is(err, ErrMissingMsgID) {
		t.Fatalf("err=%v", err)
	}
	sc, _ := st.Scope(context.Background(), "sc")
	if sc.Cursor != "" || sc.State != ScopePausedNoMsgID || !l.has("wecom_sync_missing_msgid") {
		t.Fatalf("scope advanced or not paused: %+v", sc)
	}
	var n int
	_ = st.DB().QueryRow(`SELECT COUNT(1) FROM inbox`).Scan(&n)
	if n != 0 {
		t.Fatalf("fabricated inbox rows: %d", n)
	}
}

func TestSyncAllCursorChecksAndPageLimitResume(t *testing.T) {
	ctx := context.Background()
	for name, page := range map[string]SyncResponse{
		"empty-next": {NextCursor: "", HasMore: 1},
		"same-next":  {NextCursor: "start", HasMore: 1},
	} {
		st := syncStore(t)
		l := &syncLog{}
		srv, _ := pagedServer(t, page)
		_, err := NewClient(srv.URL, "c", "s").SyncAll(ctx, SyncOptions{Scope: "sc", EnterpriseID: "e1", BindingID: "b1", AccessToken: "t", Store: st, Origins: OriginPolicy{CustomerOrigins: []int{3}}, Logger: l, Request: SyncRequest{Cursor: "start"}})
		sc, _ := st.Scope(ctx, "sc")
		if !errors.Is(err, ErrCursorNoProgress) || sc.State != ScopeStalled || !l.has("wecom_sync_cursor_stalled") {
			t.Fatalf("%s: %v %+v", name, err, sc)
		}
	}
	st := syncStore(t)
	l := &syncLog{}
	srv, cursors := pagedServer(t,
		SyncResponse{NextCursor: "p1", HasMore: 1},
		SyncResponse{NextCursor: "p2", HasMore: 1},
		SyncResponse{NextCursor: "p3", HasMore: 0})
	c := NewClient(srv.URL, "c", "s")
	o := opts(st, l)
	o.MaxPages = 2
	res, err := c.SyncAll(ctx, o)
	sc, _ := st.Scope(ctx, "sc")
	if err != nil || !res.Truncated || !res.HasMore || sc.Cursor != "p2" || !sc.Pending || sc.State != ScopePageLimit || !l.has("wecom_sync_page_limit") {
		t.Fatalf("page limit: %+v %v %+v", res, err, sc)
	}
	res, err = c.SyncAll(ctx, o) // resumes from durable cursor
	sc, _ = st.Scope(ctx, "sc")
	if err != nil || res.Truncated || sc.Cursor != "p3" || sc.Pending || sc.State != ScopeReady || (*cursors)[2] != "p2" {
		t.Fatalf("resume: %+v %v %+v %v", res, err, sc, *cursors)
	}
}

func TestSyncAllArgumentValidation(t *testing.T) {
	st := syncStore(t)
	c := NewClient("http://127.0.0.1:1", "c", "s")
	ctx := context.Background()
	if _, err := c.SyncAll(ctx, SyncOptions{}); err == nil {
		t.Fatal("nil store")
	}
	if _, err := c.SyncAll(ctx, SyncOptions{Store: st, Scope: "sc"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("missing ids: %v", err)
	}
	if _, err := c.SyncAll(ctx, SyncOptions{Store: st, Scope: "sc", EnterpriseID: "e1", BindingID: "b1"}); !errors.Is(err, ErrOriginPolicy) {
		t.Fatalf("origin policy: %v", err)
	}
	o := opts(st, nil)
	o.Scope = "missing"
	if _, err := c.SyncAll(ctx, o); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("missing scope: %v", err)
	}
	o = opts(st, nil)
	if _, err := c.SyncAll(ctx, o); err == nil {
		t.Fatal("network error expected")
	}
}

package bridge

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/whysmx/wecom-kf-bridge/state"
)

type stubChatStore struct {
	Store
	mu                                  sync.Mutex
	err                                 error
	calls                               int
	lastCustomer, lastRole, lastContent string
}

func (s *stubChatStore) AppendChatTurn(_ context.Context, customerID, role, msgType, content string, _ int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.lastCustomer, s.lastRole, s.lastContent = customerID, role, content
	_ = msgType
	return s.err
}

func TestRecordAIChatTurnBranches(t *testing.T) {
	e := newEnv(t)
	stub := &stubChatStore{Store: e.st}
	e.srv.cfg.Store = stub
	e.srv.recordAIChatTurn(context.Background(), "cust-1", "AI 回复")
	stub.mu.Lock()
	if stub.calls != 1 || stub.lastCustomer != "cust-1" || stub.lastRole != state.ChatRoleAI || stub.lastContent != "AI 回复" {
		stub.mu.Unlock()
		t.Fatalf("stub calls=%d cust=%q role=%q content=%q", stub.calls, stub.lastCustomer, stub.lastRole, stub.lastContent)
	}
	stub.mu.Unlock()

	var logs []string
	e.srv.cfg.Logger = LoggerFunc(func(ev string, _ map[string]any) { logs = append(logs, ev) })
	stub.err = errors.New("db down")
	e.srv.recordAIChatTurn(context.Background(), "cust-1", "x")
	if len(logs) != 1 || logs[0] != "chat_context_append_failed" {
		t.Fatalf("logs %v", logs)
	}
	// err with nil logger must not panic
	e.srv.cfg.Logger = nil
	e.srv.recordAIChatTurn(context.Background(), "cust-1", "y")
}

func TestSendRecordsAIChatTurn(t *testing.T) {
	e := newEnv(t)
	text := "你好，这是助手回复"
	tok := e.token()
	code, v := e.send(tok, e.cust.UID, "text", text)
	if code != 200 || errcode(v) != 0 {
		t.Fatalf("%d %v", code, v)
	}
	turns, err := e.st.ListChatTurnsByCustomer(context.Background(), e.cust.ID, 20, 30)
	if err != nil {
		t.Fatal(err)
	}
	var ai []state.ChatTurn
	for _, tr := range turns {
		if tr.Role == state.ChatRoleAI {
			ai = append(ai, tr)
		}
	}
	if len(ai) != 1 || ai[0].Content != text {
		t.Fatalf("ai turns %#v (all %#v)", ai, turns)
	}
}

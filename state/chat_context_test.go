package state

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func seedChatCustomer(t *testing.T, s *Store) Customer {
	t.Helper()
	ctx := context.Background()
	if err := s.PutEnterprise(ctx, Enterprise{ID: "e1", TenantID: "t1", CorpID: "c1", CredentialRef: "env:X"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutBinding(ctx, Binding{ID: "b1", EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p1", Active: true}); err != nil {
		t.Fatal(err)
	}
	c, err := s.EnsureCustomer(ctx, "e1", "b1", "ext-chat")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAppendAndListChatTurnsWindow(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c := seedChatCustomer(t, s)

	if err := s.AppendChatTurn(ctx, c.ID, ChatRoleUser, "text", "你好", 0); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendChatTurn(ctx, c.ID, ChatRoleAI, "text", "您好，有什么可以帮您", 0); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendChatTurn(ctx, c.ID, ChatRoleUser, "image", "", 0); err != nil {
		t.Fatal(err)
	}

	turns, err := s.ListChatTurns(ctx, "kf1", "ext-chat", 20, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 3 {
		t.Fatalf("turns %d", len(turns))
	}
	if turns[0].Role != ChatRoleUser || turns[0].Content != "你好" {
		t.Fatalf("first %#v", turns[0])
	}
	if turns[1].Role != ChatRoleAI || !strings.Contains(turns[1].Content, "您好") {
		t.Fatalf("ai %#v", turns[1])
	}
	if turns[2].Content != "[图片]" {
		t.Fatalf("image placeholder %q", turns[2].Content)
	}

	byCust, err := s.ListChatTurnsByCustomer(ctx, c.ID, 2, 30)
	if err != nil || len(byCust) != 2 {
		t.Fatalf("by customer %d %v", len(byCust), err)
	}
	// max 2 → newest two in oldest-first: AI then image
	if byCust[0].Role != ChatRoleAI || byCust[1].Content != "[图片]" {
		t.Fatalf("window %#v", byCust)
	}
}

func TestChatTurnsTTLAndClear(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c := seedChatCustomer(t, s)
	if err := s.AppendChatTurn(ctx, c.ID, ChatRoleUser, "text", "old", 0); err != nil {
		t.Fatal(err)
	}
	// Force created_at into the past.
	if _, err := s.db.ExecContext(ctx, `UPDATE chat_context SET created_at=?`, unix(s.now().Add(-10*24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	n, err := s.PurgeChatTurnsOlderThan(ctx, 7*24*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("purge %d %v", n, err)
	}
	turns, _ := s.ListChatTurnsByCustomer(ctx, c.ID, 20, 30)
	if len(turns) != 0 {
		t.Fatalf("expected empty after purge, got %d", len(turns))
	}

	_ = s.AppendChatTurn(ctx, c.ID, ChatRoleAI, "text", "fresh", 0)
	if err := s.ClearChatTurnsByCustomer(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	turns, _ = s.ListChatTurnsByCustomer(ctx, c.ID, 20, 30)
	if len(turns) != 0 {
		t.Fatal("clear failed")
	}
}

func TestClearManualFenceClearsChatContext(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c := seedChatCustomer(t, s)
	_ = s.AppendChatTurn(ctx, c.ID, ChatRoleUser, "text", "hi", 0)
	c, err := s.BeginHandover(ctx, c.ID, "t")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.SetHandoverStatus(ctx, c.ID, CustomerHuman, "t")
	if _, err := s.ClearManualFenceAfterOfficialEnd(ctx, c.ID, "session_end"); err != nil {
		t.Fatal(err)
	}
	turns, _ := s.ListChatTurnsByCustomer(ctx, c.ID, 20, 30)
	if len(turns) != 0 {
		t.Fatalf("fence end should clear draft, got %d", len(turns))
	}
}

func TestCommitSyncPageRecordsUserChatTurn(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c := seedChatCustomer(t, s)
	if err := s.EnsureScope(ctx, "sc1", "b1"); err != nil {
		t.Fatal(err)
	}
	now := s.now()
	n, err := s.CommitSyncPage(ctx, "sc1", "cur1", false, []InboxMessage{{
		BindingID: "b1", ExternalMsgID: "m1", CustomerID: c.ID, Generation: 1,
		Type: "text", PayloadRef: "客户问价", CreateTime: now, CustomerInitiated: true,
	}})
	if err != nil || n != 1 {
		t.Fatalf("commit %d %v", n, err)
	}
	turns, err := s.ListChatTurnsByCustomer(ctx, c.ID, 20, 30)
	if err != nil || len(turns) != 1 || turns[0].Content != "客户问价" || turns[0].Role != ChatRoleUser {
		t.Fatalf("draft %#v %v", turns, err)
	}
	// Duplicate page must not double-write draft.
	n, err = s.CommitSyncPage(ctx, "sc1", "cur2", false, []InboxMessage{{
		BindingID: "b1", ExternalMsgID: "m1", CustomerID: c.ID, Generation: 1,
		Type: "text", PayloadRef: "客户问价", CreateTime: now, CustomerInitiated: true,
	}})
	if err != nil || n != 0 {
		t.Fatalf("dup %d %v", n, err)
	}
	turns, _ = s.ListChatTurnsByCustomer(ctx, c.ID, 20, 30)
	if len(turns) != 1 {
		t.Fatalf("dup draft %d", len(turns))
	}
}

func TestFormatChatSummaryTruncate(t *testing.T) {
	turns := []ChatTurn{
		{Role: ChatRoleUser, Content: "短问"},
		{Role: ChatRoleAI, Content: strings.Repeat("答", 200)},
	}
	sum := FormatChatSummary(turns, 80)
	if sum == "" || !strings.Contains(sum, "【近期对话】") || !strings.Contains(sum, "用户") {
		t.Fatalf("sum %q", sum)
	}
	if utf8.RuneCountInString(sum) > 80 {
		t.Fatalf("too long %d", utf8.RuneCountInString(sum))
	}
}

func TestMergeHandoverTipWithContext(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c := seedChatCustomer(t, s)
	_ = s.AppendChatTurn(ctx, c.ID, ChatRoleUser, "text", "要退货", 0)
	_ = s.AppendChatTurn(ctx, c.ID, ChatRoleAI, "text", "请提供订单号", 0)
	got := s.MergeHandoverTipWithContext(ctx, c.ID, "正在转接…", 20, 30, 600)
	if !strings.Contains(got, "正在转接") || !strings.Contains(got, "要退货") || !strings.Contains(got, "请提供订单号") {
		t.Fatalf("merged %q", got)
	}
	// Missing customer → tip unchanged
	if s.MergeHandoverTipWithContext(ctx, "missing", "tip", 0, 0, 0) != "tip" {
		t.Fatal("missing should keep tip")
	}
}

func TestChatContentForInbound(t *testing.T) {
	if ChatContentForInbound("text", "hi") != "hi" {
		t.Fatal("text")
	}
	if ChatContentForInbound("file", "x") != "[文件]" {
		t.Fatal("file")
	}
}

func TestChatContextEdgeCases(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c := seedChatCustomer(t, s)

	if err := s.AppendChatTurn(ctx, "", ChatRoleUser, "text", "x", 0); err != ErrInvalidID {
		t.Fatalf("empty customer: %v", err)
	}
	if err := s.AppendChatTurn(ctx, c.ID, "bot", "text", "x", 0); err != ErrInvalidID {
		t.Fatalf("bad role: %v", err)
	}
	if err := s.AppendChatTurn(ctx, "missing", ChatRoleUser, "text", "x", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing customer: %v", err)
	}
	// empty content + empty type → placeholder
	if err := s.AppendChatTurn(ctx, c.ID, ChatRoleAI, "", "", 7); err != nil {
		t.Fatal(err)
	}
	turns, err := s.ListChatTurns(ctx, "kf1", "ext-chat", 5, 10)
	if err != nil || len(turns) != 1 || turns[0].Content != "[空文本]" || turns[0].MsgType != "text" {
		t.Fatalf("placeholder %#v %v", turns, err)
	}

	if _, err := s.ListChatTurns(ctx, "", "ext", 0, 0); err != ErrInvalidID {
		t.Fatal(err)
	}
	if _, err := s.ListChatTurnsByCustomer(ctx, "", 0, 0); err != ErrInvalidID {
		t.Fatal(err)
	}
	if _, err := s.ListChatTurnsByCustomer(ctx, "nope", 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.ClearChatTurns(ctx, "", "x"); err != ErrInvalidID {
		t.Fatal(err)
	}
	if err := s.ClearChatTurnsByCustomer(ctx, ""); err != ErrInvalidID {
		t.Fatal(err)
	}
	if err := s.ClearChatTurnsByCustomer(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}

	// ttl<=0 uses default
	if _, err := s.PurgeChatTurnsOlderThan(ctx, 0); err != nil {
		t.Fatal(err)
	}

	if FormatChatSummary(nil, 0) != "" {
		t.Fatal("empty summary")
	}
	if FormatChatSummary([]ChatTurn{{Role: ChatRoleUser, Content: "a"}}, 0) == "" {
		t.Fatal("default max chars")
	}
	// truncateRunes short path
	if truncateRunes("ab", 5) != "ab" || truncateRunes("abcd", 0) != "" {
		t.Fatal("truncate")
	}

	cases := map[string]string{
		"voice": "[语音]", "video": "[视频]", "xyz": "[xyz]", "": "[消息]",
	}
	for typ, want := range cases {
		if got := ChatContentForInbound(typ, ""); got != want {
			t.Fatalf("%s → %q want %q", typ, got, want)
		}
	}
	if ChatContentForInbound("text", "  ") != "[空文本]" {
		t.Fatal("blank text")
	}

	// Hit normalizeChatWindow defaults (0 → 20/30).
	if _, err := s.ListChatTurns(ctx, "kf1", "ext-chat", 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PurgeChatTurnsOlderThan(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	// Merge with empty draft after clear
	_ = s.ClearChatTurns(ctx, "kf1", "ext-chat")
	if s.MergeHandoverTipWithContext(ctx, c.ID, "only", 0, 0, 0) != "only" {
		t.Fatal("empty draft keeps tip")
	}

	// Merge: empty tip keeps summary only; empty turns keep tip
	_ = s.AppendChatTurn(ctx, c.ID, ChatRoleUser, "text", "问", 0)
	got := s.MergeHandoverTipWithContext(ctx, c.ID, "", 20, 30, 100)
	if !strings.Contains(got, "【近期对话】") || !strings.Contains(got, "问") {
		t.Fatalf("empty tip: %q", got)
	}
	got = s.MergeHandoverTipWithContext(ctx, c.ID, "  tip  ", 20, 30, 10)
	if !strings.HasPrefix(got, "tip") {
		t.Fatalf("truncated merge %q", got)
	}
}

func TestRecoverCustomerClearsChatContext(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	c := seedChatCustomer(t, s)
	_ = s.AppendChatTurn(ctx, c.ID, ChatRoleUser, "text", "x", 0)
	c, err := s.BeginHandover(ctx, c.ID, "t")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.SetHandoverStatus(ctx, c.ID, CustomerHuman, "t")
	if _, err := s.RecoverCustomer(ctx, c.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	turns, _ := s.ListChatTurnsByCustomer(ctx, c.ID, 20, 30)
	if len(turns) != 0 {
		t.Fatal("recover should clear")
	}
}

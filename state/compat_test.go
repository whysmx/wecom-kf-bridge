package state

import (
	"context"
	"testing"
)

func TestCompatMsgIDSequenceStableAndUnique(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedScope(t, s)
	page := []InboxMessage{{BindingID: "b1", ExternalMsgID: "wx-a"}, {BindingID: "b1", ExternalMsgID: "wx-b"}}
	if n, err := s.CommitSyncPage(ctx, "s1", "c1", true, page); err != nil || n != 2 {
		t.Fatalf("commit %d %v", n, err)
	}
	a, _ := s.InboxByExternalID(ctx, "s1", "wx-a")
	b, _ := s.InboxByExternalID(ctx, "s1", "wx-b")
	if a.CompatMsgID != 1 || b.CompatMsgID != 2 {
		t.Fatalf("ids %d %d", a.CompatMsgID, b.CompatMsgID)
	}
	// retried page re-uses ids, new message gets the next value
	page = append(page, InboxMessage{BindingID: "b1", ExternalMsgID: "wx-c"})
	if n, err := s.CommitSyncPage(ctx, "s1", "c2", false, page); err != nil || n != 1 {
		t.Fatalf("retry %d %v", n, err)
	}
	a2, _ := s.InboxByExternalID(ctx, "s1", "wx-a")
	c, _ := s.InboxByExternalID(ctx, "s1", "wx-c")
	if a2.CompatMsgID != 1 || c.CompatMsgID != 3 {
		t.Fatalf("retry ids %d %d", a2.CompatMsgID, c.CompatMsgID)
	}
	if _, err := s.CommitSyncPage(ctx, "s1", "c3", false, []InboxMessage{{BindingID: "b1", ExternalMsgID: "x", CompatMsgID: -1}}); err != ErrInvalidID {
		t.Fatalf("negative id: %v", err)
	}
}

package state

import (
	"context"
	"testing"
)

func TestWorkerQueries(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedScope(t, s)
	c, _ := s.EnsureCustomer(ctx, "e1", "b1", "ext")
	_, err := s.CommitSyncPage(ctx, "s1", "c1", true, []InboxMessage{
		{BindingID: "b1", ExternalMsgID: "a", CustomerID: c.ID, PayloadRef: "问1"},
		{BindingID: "b1", ExternalMsgID: "b", State: InboxIgnored},
		{BindingID: "b1", ExternalMsgID: "c", CustomerID: c.ID, PayloadRef: "问2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sc, err := s.PendingScopes(ctx, 0)
	if err != nil || len(sc) != 1 || sc[0].ID != "s1" {
		t.Fatalf("%v %v", sc, err)
	}
	ms, err := s.InboxForDelivery(ctx, 3, 0)
	if err != nil || len(ms) != 2 || ms[0].ExternalMsgID != "a" || ms[0].PayloadRef != "问1" {
		t.Fatalf("%+v %v", ms, err)
	}
	for _, to := range []string{InboxClassified, InboxReady, InboxPosting} {
		if _, err := s.TransitionInbox(ctx, ms[0].ID, to, ""); err != nil {
			t.Fatal(err)
		}
	}
	o := func() OutboxMessage {
		openWindow(t, s, c.ID)
		o, err := s.CreateOutbox(ctx, OutboxMessage{CustomerID: c.ID, Generation: 1, UID: c.UID, Body: "x"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.MarkOutboxSending(ctx, o.ID); err != nil {
			t.Fatal(err)
		}
		return o
	}()
	in, out, err := s.RecoverInFlight(ctx)
	if err != nil || in != 1 || out != 1 {
		t.Fatalf("%d %d %v", in, out, err)
	}
	m, _ := s.Inbox(ctx, ms[0].ID)
	got, _ := s.Outbox(ctx, o.ID)
	if m.State != InboxDeliveryUnknown || got.State != OutboxUnknown {
		t.Fatalf("%s %s", m.State, got.State)
	}
	ms, _ = s.InboxForDelivery(ctx, 3, 10)
	if len(ms) != 1 || ms[0].ExternalMsgID != "c" {
		t.Fatalf("unknown row redelivered: %+v", ms)
	}
}

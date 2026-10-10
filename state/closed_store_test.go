package state

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// Every operation on a closed database must return an error, never panic
// or report success.
func TestClosedStoreReturnsErrors(t *testing.T) {
	s, err := OpenWithOptions(t.TempDir()+"/c.db", Options{MasterKey: bytes.Repeat([]byte{5}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	ctx := context.Background()
	now := time.Now()
	errs := []error{
		s.SaveSyncToken(ctx, "s", "t", now.Add(time.Hour)),
		s.PutEnterprise(ctx, Enterprise{ID: "e"}),
		s.PutBinding(ctx, Binding{ID: "b"}),
		s.EnsureScope(ctx, "s", "b"),
		s.SetSyncState(ctx, "s", "READY"),
		s.MarkSyncPending(ctx, "s", true),
		s.SetCustomerAuthorized(ctx, "c", true),
		s.SetNickname(ctx, "c", "n", now),
		s.SetOfficialStatus(ctx, "c", "x"),
		s.SetCustomerState(ctx, "c", CustomerHuman),
		s.SetHandoverStatus(ctx, "c", CustomerHuman, "r"),
		s.RecordOutboxChunk(ctx, "o", 0, "m"),
		s.ReleaseBudget(ctx, "c", 1),
	}
	add := func(_ any, e error) { errs = append(errs, e) }
	add(s.SyncToken(ctx, "s"))
	add(s.Enterprise(ctx, "e"))
	add(s.Binding(ctx, "b"))
	add(s.BindingByOpenKfID(ctx, "e", "k"))
	add(s.Scope(ctx, "s"))
	add(s.EnsureCustomer(ctx, "e", "b", "x"))
	add(s.Customer(ctx, "c"))
	add(s.CustomerByUID(ctx, "b", "u"))
	add(s.BeginHandover(ctx, "c", "r"))
	add(s.RecoverCustomer(ctx, "c", "r"))
	add(s.RotateGeneration(ctx, "c", "r"))
	add(s.CommitSyncPage(ctx, "s", "n", false, []InboxMessage{{ExternalMsgID: "m"}}))
	add(s.Inbox(ctx, 1))
	add(s.InboxByExternalID(ctx, "s", "m"))
	add(s.TransitionInbox(ctx, 1, InboxClassified, ""))
	add(s.CreateOutbox(ctx, OutboxMessage{CustomerID: "c"}))
	add(s.BlockOutbox(ctx, "o", "x"))
	add(s.Outbox(ctx, "o"))
	add(s.MarkOutboxSending(ctx, "o"))
	add(s.TransitionOutbox(ctx, "o", OutboxRejected, ""))
	add(s.MarkOutboxUnknown(ctx, "o", "x"))
	add(s.OutboxChunkIDs(ctx, "o"))
	add(s.PendingOutbox(ctx, "b"))
	add(s.AddAudit(ctx, "k", "t", "i", "o", "r", "s", 1))
	add(s.PendingScopes(ctx, 1))
	add(s.InboxForDelivery(ctx, 1, 1))
	_, _, e := s.RecoverInFlight(ctx)
	errs = append(errs, e)
	for i, e := range errs {
		if e == nil {
			t.Errorf("op %d succeeded on closed store", i)
		}
	}
}

package state

import (
	"context"
	"database/sql"
	"errors"
	_ "modernc.org/sqlite"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	now := time.Unix(1700000000, 123)
	uids := []string{"alias", "next"}
	s, err := New(db, Options{MasterKey: testMasterKey, Now: func() time.Time { return now }, UIDGenerator: func() (string, error) {
		if len(uids) == 0 {
			return "last", nil
		}
		v := uids[0]
		uids = uids[1:]
		return v, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func TestStoreLifecycleAndHandover(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if _, err := New(nil, Options{}); err == nil {
		t.Fatal("nil db")
	}
	if err := s.PutEnterprise(ctx, Enterprise{ID: "e", TenantID: "t", CorpID: "corp", CredentialRef: "ref"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutBinding(ctx, Binding{ID: "b", EnterpriseID: "e", OpenKfID: "kf", ProjectID: "p", Active: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindingByOpenKfID(ctx, "e", "kf"); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureScope(ctx, "scope", "b"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkSyncPending(ctx, "missing", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pending %v", err)
	}
	if err := s.MarkSyncPending(ctx, "scope", true); err != nil {
		t.Fatal(err)
	}
	c, err := s.EnsureCustomer(ctx, "e", "b", "u")
	if err != nil {
		t.Fatal(err)
	}
	if c.Generation != 1 || c.UID != "alias_g1" {
		t.Fatalf("customer %#v", c)
	}
	if _, err := s.CustomerByUID(ctx, "b", c.UID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNickname(ctx, c.ID, "小海😀", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOfficialStatus(ctx, c.ID, "SERVING"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCustomerState(ctx, c.ID, "bad"); !errors.Is(err, ErrInvalidState) {
		t.Fatal(err)
	}
	c, err = s.BeginHandover(ctx, c.ID, "request")
	if err != nil || c.State != CustomerWaitingHuman {
		t.Fatalf("handover %#v %v", c, err)
	}
	if err := s.SetHandoverStatus(ctx, c.ID, CustomerAIEligible, "bad"); !errors.Is(err, ErrInvalidState) {
		t.Fatal(err)
	}
	if err := s.SetHandoverStatus(ctx, c.ID, CustomerHuman, "accepted"); err != nil {
		t.Fatal(err)
	}
	c, err = s.RecoverCustomer(ctx, c.ID, "resume")
	if err != nil || c.Generation != 2 || c.UID != "next_g2" {
		t.Fatalf("recover %#v %v", c, err)
	}
	if _, err := s.RecoverCustomer(ctx, c.ID, "again"); err == nil {
		t.Fatal(err)
	}
}
func TestSyncInboxOutboxAndAudit(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	_ = s.PutEnterprise(ctx, Enterprise{ID: "e", TenantID: "t", CorpID: "corp", CredentialRef: "ref"})
	_ = s.PutBinding(ctx, Binding{ID: "b", EnterpriseID: "e", OpenKfID: "kf", ProjectID: "p", Active: true})
	_ = s.EnsureScope(ctx, "scope", "b")
	c, _ := s.EnsureCustomer(ctx, "e", "b", "u")
	msgs := []InboxMessage{{BindingID: "b", CustomerID: c.ID, Generation: 1, ExternalMsgID: "m", CompatMsgID: 1, CreateTime: time.Now(), Type: "text"}}
	if _, err := s.CommitSyncPage(ctx, "scope", "next", true, msgs); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitSyncPage(ctx, "scope", "next", true, nil); !errors.Is(err, ErrNoProgress) {
		t.Fatal(err)
	}
	if n, err := s.CommitSyncPage(ctx, "scope", "done", false, msgs); err != nil || n != 0 {
		t.Fatalf("duplicate %d %v", n, err)
	}
	in, err := s.InboxByExternalID(ctx, "scope", "m")
	if err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{InboxClassified, InboxReady, InboxPosting, InboxHTTPAccepted, InboxDeliveryUnknown} {
		in, err = s.TransitionInbox(ctx, in.ID, to, "")
		if err != nil {
			t.Fatalf("%s %v", to, err)
		}
	}
	if _, err := s.TransitionInbox(ctx, in.ID, InboxReceived, ""); !errors.Is(err, ErrInvalidState) {
		t.Fatal(err)
	}
	o, err := s.CreateOutbox(ctx, OutboxMessage{CustomerID: c.ID, Generation: c.Generation, UID: c.UID, Body: "answer", BudgetUnits: 1})
	if err != nil {
		t.Fatal(err)
	}
	o, err = s.MarkOutboxSending(ctx, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.MarkOutboxUnknown(ctx, o.ID, "timeout"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.MarkOutboxUnknown(ctx, o.ID, "again"); !errors.Is(err, ErrUnknownResult) {
		t.Fatal(err)
	}
	if ok, err := s.AddAudit(ctx, "k", "outbox", o.ID, "send", "UNKNOWN", "", 1); err != nil || !ok {
		t.Fatalf("audit %v %v", ok, err)
	}
	if ok, err := s.AddAudit(ctx, "k", "outbox", o.ID, "send", "UNKNOWN", "", 1); err != nil || ok {
		t.Fatalf("audit duplicate %v %v", ok, err)
	}
	if err := s.WithCustomerLock(c.ID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.WithLock("", func() error { return nil }); !errors.Is(err, ErrInvalidID) {
		t.Fatal(err)
	}
}

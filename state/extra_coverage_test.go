package state

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHelpersAndBasicQueries(t *testing.T) {
	if unix(time.Time{}) != 0 || unix(time.Unix(2, 3)) == 0 || !timeFrom(0).IsZero() || timeFrom(2).IsZero() {
		t.Fatal("time helpers")
	}
	if nullString("x") != "x" || !validUID("a") || validUID("") || validUID(strings.Repeat("x", 201)) || validUID("a@all") || validUID("a!") {
		t.Fatal("uid helpers")
	}
	if generationUID("alias_g1", 2) != "alias_g2" || !strings.HasPrefix(generationUID("alias", 1), "alias_g1") {
		t.Fatal("generation")
	}
	if a, err := randomAlias(); err != nil || a == "" {
		t.Fatal(err)
	}
	if id, err := randomID("x_"); err != nil || !strings.HasPrefix(id, "x_") {
		t.Fatal(err)
	}
	if !errors.Is(mapNotFound(sql.ErrNoRows), ErrNotFound) || mapNotFound(errors.New("x")) == nil || translateNoRows(nil) != nil {
		t.Fatal("errors")
	}
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Enterprise(ctx, "none"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.Binding(ctx, "none"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.Scope(ctx, "none"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.Customer(ctx, "none"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.CustomerByUID(ctx, "b", "none"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.WithScopeLock("scope", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if db := s.DB(); db == nil {
		t.Fatal("db")
	}
}

func TestStoreValidationAndReads(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, e := range []Enterprise{{}, {ID: "e"}, {ID: "e", TenantID: "t"}, {ID: "e", TenantID: "t", CorpID: "c"}} {
		if err := s.PutEnterprise(ctx, e); !errors.Is(err, ErrInvalidID) {
			t.Fatalf("enterprise %v", err)
		}
	}
	if err := s.PutEnterprise(ctx, Enterprise{ID: "e", TenantID: "t", CorpID: "c", CredentialRef: "r"}); err != nil {
		t.Fatal(err)
	}
	e, err := s.Enterprise(ctx, "e")
	if err != nil || e.ID != "e" {
		t.Fatal(err)
	}
	if err := s.PutEnterprise(ctx, Enterprise{ID: "e", TenantID: "t", CorpID: "c", CredentialRef: "r2", Status: "DISABLED", Revision: 2}); err != nil {
		t.Fatal(err)
	}
	for _, b := range []Binding{{}, {ID: "b"}, {ID: "b", EnterpriseID: "e"}, {ID: "b", EnterpriseID: "e", OpenKfID: "k"}} {
		if err := s.PutBinding(ctx, b); !errors.Is(err, ErrInvalidID) {
			t.Fatalf("binding %v", err)
		}
	}
	if err := s.PutBinding(ctx, Binding{ID: "b", EnterpriseID: "e", OpenKfID: "k", ProjectID: "p"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Binding(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureScope(ctx, "scope", "b"); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureScope(ctx, "", "b"); !errors.Is(err, ErrInvalidID) {
		t.Fatal(err)
	}
	if _, err := s.Scope(ctx, "scope"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkSyncPending(ctx, "", true); !errors.Is(err, ErrInvalidID) {
		t.Fatal(err)
	}
	if err := s.MarkSyncPending(ctx, "missing", false); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	for _, x := range [][3]string{{"", "b", "u"}, {"e", "", "u"}, {"e", "b", ""}} {
		if _, err := s.EnsureCustomer(ctx, x[0], x[1], x[2]); !errors.Is(err, ErrInvalidID) {
			t.Fatal(err)
		}
	}
	c, err := s.EnsureCustomer(ctx, "e", "b", "u")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureCustomer(ctx, "e", "b", "u"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNickname(ctx, "missing", "n", time.Time{}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.SetOfficialStatus(ctx, "missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.SetCustomerState(ctx, "missing", CustomerHuman); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.SetCustomerState(ctx, c.ID, CustomerUnknown); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginHandover(ctx, "missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.SetHandoverStatus(ctx, "missing", CustomerHuman, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestStoreTransitionBranches(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_ = s.PutEnterprise(ctx, Enterprise{ID: "e", TenantID: "t", CorpID: "c", CredentialRef: "r"})
	_ = s.PutBinding(ctx, Binding{ID: "b", EnterpriseID: "e", OpenKfID: "k", ProjectID: "p"})
	_ = s.EnsureScope(ctx, "scope", "b")
	c, _ := s.EnsureCustomer(ctx, "e", "b", "u")
	if _, err := s.CommitSyncPage(ctx, "missing", "x", false, nil); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.CommitSyncPage(ctx, "scope", "x", false, []InboxMessage{{BindingID: "", ExternalMsgID: "m", CompatMsgID: 1}}); !errors.Is(err, ErrInvalidID) {
		t.Fatal(err)
	}
	if _, err := s.CommitSyncPage(ctx, "scope", "x", false, []InboxMessage{{BindingID: "b", ExternalMsgID: "", CompatMsgID: 1}}); !errors.Is(err, ErrInvalidID) {
		t.Fatal(err)
	}
	if _, err := s.CommitSyncPage(ctx, "scope", "x", false, []InboxMessage{{BindingID: "b", ExternalMsgID: "m", CompatMsgID: -1}}); !errors.Is(err, ErrInvalidID) {
		t.Fatal(err)
	}
	n, err := s.CommitSyncPage(ctx, "scope", "x", false, []InboxMessage{{BindingID: "b", ExternalMsgID: "m", CompatMsgID: 1}})
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
	in, _ := s.InboxByExternalID(ctx, "scope", "m")
	if _, err := s.Inbox(ctx, in.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Inbox(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.TransitionInbox(ctx, in.ID, "BAD", ""); !errors.Is(err, ErrInvalidState) {
		t.Fatal(err)
	}
	if _, err := s.TransitionInbox(ctx, 999, InboxClassified, ""); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.TransitionInbox(ctx, in.ID, InboxIgnored, "why"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionInbox(ctx, in.ID, InboxReady, ""); !errors.Is(err, ErrInvalidState) {
		t.Fatal(err)
	}
	o, err := s.CreateOutbox(ctx, OutboxMessage{CustomerID: c.ID, Generation: c.Generation, UID: c.UID, Body: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Outbox(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Outbox(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.CreateOutbox(ctx, OutboxMessage{CustomerID: c.ID, Generation: 9, UID: c.UID}); !errors.Is(err, ErrStaleGeneration) {
		t.Fatal(err)
	}
	if _, err := s.MarkOutboxSending(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.MarkOutboxUnknown(ctx, "missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.TransitionOutbox(ctx, o.ID, "BAD", ""); !errors.Is(err, ErrInvalidState) {
		t.Fatal(err)
	}
	if _, err := s.TransitionOutbox(ctx, "missing", OutboxValidated, ""); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.MarkOutboxSending(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionOutbox(ctx, o.ID, OutboxUpstreamAccepted, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionOutbox(ctx, o.ID, OutboxDeliveryFailed, "net"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionOutbox(ctx, o.ID, OutboxSending, ""); !errors.Is(err, ErrInvalidState) {
		t.Fatal(err)
	}
	if _, err := s.MarkOutboxUnknown(ctx, o.ID, "again"); !errors.Is(err, ErrUnknownResult) {
		t.Fatal(err)
	}
	if err := s.SetCustomerState(ctx, c.ID, CustomerHuman); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateOutbox(ctx, OutboxMessage{CustomerID: c.ID, Generation: c.Generation, UID: c.UID}); !errors.Is(err, ErrHeld) {
		t.Fatal(err)
	}
	s2 := testStore(t)
	_ = s2.PutEnterprise(ctx, Enterprise{ID: "e", TenantID: "t", CorpID: "c", CredentialRef: "r"})
	_ = s2.PutBinding(ctx, Binding{ID: "b", EnterpriseID: "e", OpenKfID: "k", ProjectID: "p"})
	cc, _ := s2.EnsureCustomer(ctx, "e", "b", "u")
	oo, _ := s2.CreateOutbox(ctx, OutboxMessage{CustomerID: cc.ID, Generation: cc.Generation, UID: cc.UID})
	_ = s2.SetCustomerState(ctx, cc.ID, CustomerHuman)
	if _, err := s2.MarkOutboxSending(ctx, oo.ID); !errors.Is(err, ErrHeld) {
		t.Fatal(err)
	}
	s3 := testStore(t)
	_ = s3.PutEnterprise(ctx, Enterprise{ID: "e", TenantID: "t", CorpID: "c", CredentialRef: "r"})
	_ = s3.PutBinding(ctx, Binding{ID: "b", EnterpriseID: "e", OpenKfID: "k", ProjectID: "p"})
	cc, _ = s3.EnsureCustomer(ctx, "e", "b", "u")
	oo, _ = s3.CreateOutbox(ctx, OutboxMessage{CustomerID: cc.ID, Generation: cc.Generation, UID: cc.UID})
	_, _ = s3.BeginHandover(ctx, cc.ID, "h")
	_, _ = s3.RecoverCustomer(ctx, cc.ID, "r")
	if _, err := s3.MarkOutboxSending(ctx, oo.ID); !errors.Is(err, ErrStaleGeneration) {
		t.Fatal(err)
	}
	if _, err := s.PendingOutbox(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PendingOutbox(ctx, "none"); err != nil {
		t.Fatal(err)
	}
}

func TestStoreOpenAndUIDFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, _ := sql.Open("sqlite", ":memory:")
	db.SetMaxOpenConns(1)
	badUID := func() (string, error) { return "", errors.New("uid") }
	s, err = New(db, Options{UIDGenerator: badUID})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	_ = s.PutEnterprise(ctx, Enterprise{ID: "e", TenantID: "t", CorpID: "c", CredentialRef: "r"})
	_ = s.PutBinding(ctx, Binding{ID: "b", EnterpriseID: "e", OpenKfID: "k", ProjectID: "p"})
	if _, err := s.EnsureCustomer(ctx, "e", "b", "u"); err == nil {
		t.Fatal("uid")
	}
	invalidUID := func() (string, error) { return "bad!", nil }
	s2, err := New(db, Options{UIDGenerator: invalidUID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.EnsureCustomer(ctx, "e", "b", "v"); err == nil {
		t.Fatal("invalid uid")
	}
	s2.Close()
}

func TestExhaustiveTransitionsAndErrors(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_ = s.PutEnterprise(ctx, Enterprise{ID: "e", TenantID: "t", CorpID: "c", CredentialRef: "r"})
	_ = s.PutBinding(ctx, Binding{ID: "b", EnterpriseID: "e", OpenKfID: "k", ProjectID: "p"})
	_ = s.EnsureScope(ctx, "scope", "b")
	if _, err := s.BindingByOpenKfID(ctx, "e", "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindingByOpenKfID(ctx, "e", "none"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	c, _ := s.EnsureCustomer(ctx, "e", "b", "u")
	if _, err := s.RecoverCustomer(ctx, c.ID, "bad"); err == nil {
		t.Fatal("recover ai")
	}
	if _, err := s.RotateGeneration(ctx, c.ID, "bad"); err == nil {
		t.Fatal("rotate ai")
	}
	_, _ = s.BeginHandover(ctx, c.ID, "human")
	if err := s.SetHandoverStatus(ctx, c.ID, CustomerClosed, "closed"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecoverCustomer(ctx, c.ID, "closed"); err == nil {
		t.Fatal("recover closed")
	}
	if err := s.SetHandoverStatus(ctx, c.ID, CustomerAIEligible, "bad"); !errors.Is(err, ErrInvalidState) {
		t.Fatal(err)
	}
	if err := s.SetHandoverStatus(ctx, c.ID, CustomerHuman, "resume"); err != nil {
		t.Fatal(err)
	}
	// Every transition helper has explicit allow/deny edges.
	for _, a := range []string{InboxReceived, InboxClassified, InboxReady, InboxPosting, InboxRetryWait, InboxHTTPAccepted, InboxDeliveryUnknown, InboxHeld, InboxExpired, InboxIgnored, InboxUnsupported} {
		for _, b := range []string{InboxReceived, InboxClassified, InboxReady, InboxPosting, InboxHTTPAccepted, InboxRetryWait, InboxDeliveryUnknown, InboxHeld, InboxExpired, InboxIgnored, InboxUnsupported} {
			_ = allowedInbox(a, b)
		}
	}
	for _, a := range []string{OutboxCreated, OutboxValidated, OutboxSending, OutboxUpstreamAccepted, OutboxDeliveryFailed, OutboxUnknown, OutboxRejected, OutboxBlocked} {
		for _, b := range []string{OutboxCreated, OutboxValidated, OutboxSending, OutboxUpstreamAccepted, OutboxDeliveryFailed, OutboxUnknown, OutboxRejected, OutboxBlocked} {
			_ = allowedOutbox(a, b)
		}
	}
	if ok, err := s.AddAudit(ctx, "", "", "", "", "", "", 0); !errors.Is(err, ErrInvalidID) || ok {
		t.Fatal(ok, err)
	}
	// Keep one pending row so rows.Next and scanOutbox are exercised.
	c, _ = s.RecoverCustomer(ctx, c.ID, "resume")
	o, err := s.CreateOutbox(ctx, OutboxMessage{CustomerID: c.ID, Generation: c.Generation, UID: c.UID, Body: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	if rows, err := s.PendingOutbox(ctx, "b"); err != nil || len(rows) != 1 || rows[0].ID != o.ID {
		t.Fatalf("%#v %v", rows, err)
	}
	if _, err := s.MarkOutboxSending(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkOutboxSending(ctx, o.ID); err == nil {
		t.Fatal("sending twice")
	}
	if _, err := s.MarkOutboxUnknown(ctx, o.ID, "timeout"); err != nil {
		t.Fatal(err)
	}
}

func TestClosedDBErrorBranches(t *testing.T) {
	s := testStore(t)
	_ = s.db.Close()
	ctx := context.Background()
	now := time.Now()
	_ = s.PutEnterprise(ctx, Enterprise{ID: "e", TenantID: "t", CorpID: "c", CredentialRef: "r"})
	_ = s.PutBinding(ctx, Binding{ID: "b", EnterpriseID: "e", OpenKfID: "k", ProjectID: "p"})
	_ = s.EnsureScope(ctx, "scope", "b")
	_ = s.MarkSyncPending(ctx, "scope", true)
	_, _ = s.Enterprise(ctx, "e")
	_, _ = s.Binding(ctx, "b")
	_, _ = s.BindingByOpenKfID(ctx, "e", "k")
	_, _ = s.Scope(ctx, "scope")
	_, _ = s.EnsureCustomer(ctx, "e", "b", "u")
	_, _ = s.Customer(ctx, "c")
	_, _ = s.CustomerByUID(ctx, "b", "u")
	_ = s.SetNickname(ctx, "c", "n", now)
	_ = s.SetOfficialStatus(ctx, "c", "x")
	_ = s.SetCustomerState(ctx, "c", CustomerHuman)
	_, _ = s.BeginHandover(ctx, "c", "x")
	_ = s.SetHandoverStatus(ctx, "c", CustomerHuman, "x")
	_, _ = s.RecoverCustomer(ctx, "c", "x")
	_, _ = s.CommitSyncPage(ctx, "scope", "x", false, nil)
	_, _ = s.Inbox(ctx, 1)
	_, _ = s.InboxByExternalID(ctx, "scope", "m")
	_, _ = s.TransitionInbox(ctx, 1, InboxReady, "")
	_, _ = s.CreateOutbox(ctx, OutboxMessage{ID: "o", CustomerID: "c", Generation: 1, UID: "u"})
	_, _ = s.Outbox(ctx, "o")
	_, _ = s.MarkOutboxSending(ctx, "o")
	_, _ = s.TransitionOutbox(ctx, "o", OutboxValidated, "")
	_, _ = s.MarkOutboxUnknown(ctx, "o", "x")
	_, _ = s.PendingOutbox(ctx, "b")
	_, _ = s.AddAudit(ctx, "k", "o", "i", "op", "ok", "", 1)
}

type resultErr struct{}

func (resultErr) LastInsertId() (int64, error) { return 0, errors.New("last id") }
func (resultErr) RowsAffected() (int64, error) { return 0, errors.New("rows") }

func TestNilCloseAndResultErrors(t *testing.T) {
	var s *Store
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rowsOrNotFound(resultErr{}); err == nil {
		t.Fatal("rows error")
	}
	db, _ := sql.Open("sqlite", ":memory:")
	_ = db.Close()
	if _, err := New(db, Options{}); err == nil {
		t.Fatal("closed db")
	}
}

func TestRecoverInvalidGeneratedUID(t *testing.T) {
	db, _ := sql.Open("sqlite", ":memory:")
	db.SetMaxOpenConns(1)
	defer db.Close()
	vals := []string{"base", "bad!"}
	s, err := New(db, Options{UIDGenerator: func() (string, error) { v := vals[0]; vals = vals[1:]; return v, nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_ = s.PutEnterprise(ctx, Enterprise{ID: "e", TenantID: "t", CorpID: "c", CredentialRef: "r"})
	_ = s.PutBinding(ctx, Binding{ID: "b", EnterpriseID: "e", OpenKfID: "k", ProjectID: "p"})
	c, err := s.EnsureCustomer(ctx, "e", "b", "u")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.SetCustomerState(ctx, c.ID, CustomerHuman)
	if _, err := s.RecoverCustomer(ctx, c.ID, "x"); !errors.Is(err, ErrInvalidID) {
		t.Fatal(err)
	}
}

func TestMigrationError(t *testing.T) {
	db, _ := sql.Open("sqlite", ":memory:")
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE customers(id TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := New(db, Options{}); err == nil {
		t.Fatal("expected migration error")
	}
	_ = db.Close()
}

func TestCanceledContextTransactions(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = s.BeginHandover(ctx, "x", "r")
	_ = s.SetHandoverStatus(ctx, "x", CustomerHuman, "r")
	_, _ = s.RecoverCustomer(ctx, "x", "r")
	_, _ = s.CommitSyncPage(ctx, "x", "n", false, nil)
	_, _ = s.CreateOutbox(ctx, OutboxMessage{CustomerID: "x", Generation: 1, UID: "u"})
}

func TestSQLTriggerErrorBranches(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) *Store {
		t.Helper()
		s := testStore(t)
		_ = s.PutEnterprise(ctx, Enterprise{ID: "e", TenantID: "t", CorpID: "c", CredentialRef: "r"})
		_ = s.PutBinding(ctx, Binding{ID: "b", EnterpriseID: "e", OpenKfID: "k", ProjectID: "p"})
		_ = s.EnsureScope(ctx, "scope", "b")
		return s
	}
	s := setup(t)
	c, _ := s.EnsureCustomer(ctx, "e", "b", "u")
	_, _ = s.Customer(ctx, c.ID)
	_, _ = s.DB().Exec(`CREATE TRIGGER bu BEFORE UPDATE ON customers BEGIN SELECT RAISE(ABORT,'blocked'); END`)
	if _, err := s.BeginHandover(ctx, c.ID, "x"); err == nil {
		t.Fatal("handover trigger")
	}
	s = setup(t)
	c, _ = s.EnsureCustomer(ctx, "e", "b", "u")
	_, _ = s.DB().Exec(`CREATE TRIGGER su BEFORE UPDATE ON customers BEGIN SELECT RAISE(ABORT,'blocked'); END`)
	if err := s.SetHandoverStatus(ctx, c.ID, CustomerHuman, "x"); err == nil {
		t.Fatal("status trigger")
	}
	s = setup(t)
	c, _ = s.EnsureCustomer(ctx, "e", "b", "u")
	_ = s.SetCustomerState(ctx, c.ID, CustomerHuman)
	_, _ = s.DB().Exec(`CREATE TRIGGER ru BEFORE UPDATE ON customers BEGIN SELECT RAISE(ABORT,'blocked'); END`)
	if _, err := s.RecoverCustomer(ctx, c.ID, "x"); err == nil {
		t.Fatal("recover trigger")
	}
	s = setup(t)
	c, _ = s.EnsureCustomer(ctx, "e", "b", "u")
	_, _ = s.DB().Exec(`CREATE TRIGGER ss BEFORE UPDATE ON sync_scopes BEGIN SELECT RAISE(ABORT,'blocked'); END`)
	if _, err := s.CommitSyncPage(ctx, "scope", "x", false, []InboxMessage{{BindingID: "b", ExternalMsgID: "m", CompatMsgID: 1}}); err == nil {
		t.Fatal("scope trigger")
	}
	s = setup(t)
	_, _ = s.DB().Exec(`CREATE TRIGGER ii BEFORE INSERT ON inbox BEGIN SELECT RAISE(ABORT,'blocked'); END`)
	if _, err := s.CommitSyncPage(ctx, "scope", "x", false, []InboxMessage{{BindingID: "b", ExternalMsgID: "m", CompatMsgID: 1}}); err == nil {
		t.Fatal("inbox insert trigger")
	}
	s = setup(t)
	_, _ = s.DB().Exec(`CREATE TRIGGER ci BEFORE INSERT ON customers BEGIN SELECT RAISE(ABORT,'blocked'); END`)
	if _, err := s.EnsureCustomer(ctx, "e", "b", "new"); err == nil {
		t.Fatal("customer insert trigger")
	}
	s = setup(t)
	c, _ = s.EnsureCustomer(ctx, "e", "b", "u")
	_, _ = s.DB().Exec(`CREATE TRIGGER oi BEFORE INSERT ON outbox BEGIN SELECT RAISE(ABORT,'blocked'); END`)
	if _, err := s.CreateOutbox(ctx, OutboxMessage{CustomerID: c.ID, Generation: c.Generation, UID: c.UID}); err == nil {
		t.Fatal("outbox trigger")
	}
	s = setup(t)
	c, _ = s.EnsureCustomer(ctx, "e", "b", "u")
	_, _ = s.DB().Exec(`CREATE TRIGGER he BEFORE INSERT ON handover_events BEGIN SELECT RAISE(ABORT,'blocked'); END`)
	if _, err := s.BeginHandover(ctx, c.ID, "x"); err == nil {
		t.Fatal("handover event trigger")
	}
	s = setup(t)
	c, _ = s.EnsureCustomer(ctx, "e", "b", "u")
	_, _ = s.DB().Exec(`CREATE TRIGGER he2 BEFORE INSERT ON handover_events BEGIN SELECT RAISE(ABORT,'blocked'); END`)
	if err := s.SetHandoverStatus(ctx, c.ID, CustomerHuman, "x"); err == nil {
		t.Fatal("status event trigger")
	}
	s = setup(t)
	c, _ = s.EnsureCustomer(ctx, "e", "b", "u")
	_ = s.SetCustomerState(ctx, c.ID, CustomerHuman)
	_, _ = s.DB().Exec(`CREATE TRIGGER he3 BEFORE INSERT ON handover_events BEGIN SELECT RAISE(ABORT,'blocked'); END`)
	if _, err := s.RecoverCustomer(ctx, c.ID, "x"); err == nil {
		t.Fatal("recover event trigger")
	}
	s = setup(t)
	c, _ = s.EnsureCustomer(ctx, "e", "b", "u")
	_, _ = s.DB().Exec(`CREATE TRIGGER cu BEFORE UPDATE ON customers BEGIN SELECT RAISE(ABORT,'blocked'); END`)
	if err := s.SetNickname(ctx, c.ID, "n", time.Time{}); err == nil {
		t.Fatal("nickname trigger")
	}
	if err := s.SetOfficialStatus(ctx, c.ID, "x"); err == nil {
		t.Fatal("official trigger")
	}
	if err := s.SetCustomerState(ctx, c.ID, CustomerHuman); err == nil {
		t.Fatal("state trigger")
	}
	s = setup(t)
	c, _ = s.EnsureCustomer(ctx, "e", "b", "u")
	_, _ = s.CommitSyncPage(ctx, "scope", "x", false, []InboxMessage{{BindingID: "b", ExternalMsgID: "m", CompatMsgID: 1}})
	in, _ := s.InboxByExternalID(ctx, "scope", "m")
	_, _ = s.DB().Exec(`CREATE TRIGGER iu BEFORE UPDATE ON inbox BEGIN SELECT RAISE(ABORT,'blocked'); END`)
	if _, err := s.TransitionInbox(ctx, in.ID, InboxClassified, ""); err == nil {
		t.Fatal("inbox trigger")
	}
	s = setup(t)
	c, _ = s.EnsureCustomer(ctx, "e", "b", "u")
	o, _ := s.CreateOutbox(ctx, OutboxMessage{CustomerID: c.ID, Generation: c.Generation, UID: c.UID})
	_, _ = s.DB().Exec(`CREATE TRIGGER ou BEFORE UPDATE ON outbox BEGIN SELECT RAISE(ABORT,'blocked'); END`)
	if _, err := s.MarkOutboxSending(ctx, o.ID); err == nil {
		t.Fatal("sending trigger")
	}
	if _, err := s.TransitionOutbox(ctx, o.ID, OutboxValidated, ""); err == nil {
		t.Fatal("transition trigger")
	}
	if _, err := s.MarkOutboxUnknown(ctx, o.ID, "x"); err == nil {
		t.Fatal("unknown trigger")
	}
	s = setup(t)
	_, _ = s.DB().Exec(`CREATE TRIGGER ai BEFORE INSERT ON audit BEGIN SELECT RAISE(ABORT,'blocked'); END`)
	if _, err := s.AddAudit(ctx, "k", "o", "i", "op", "r", "", 1); err == nil {
		t.Fatal("audit trigger")
	}
	_ = c
}

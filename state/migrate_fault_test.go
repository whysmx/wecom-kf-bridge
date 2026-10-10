package state

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"
)

// A database created by an older build gains the new columns on open; a
// database that cannot be migrated (read-only) refuses to open instead of
// running against a half-upgraded schema.
func TestMigrationUpgradesOldSchemaAndFailsOnReadOnly(t *testing.T) {
	path := t.TempDir() + "/old.db"
	fresh, err := OpenWithOptions(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	fresh.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// simulate a database written before chunk tracking existed
	if _, err := db.Exec(`ALTER TABLE outbox DROP COLUMN chunks_total`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := OpenWithOptions("file:"+path+"?mode=ro", Options{}); err == nil {
		t.Fatal("read-only old schema opened")
	}
	s, err := OpenWithOptions(path, Options{MasterKey: bytes.Repeat([]byte{1}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(1) FROM pragma_table_info('outbox') WHERE name='chunks_total'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("column not added: %d %v", n, err)
	}
}

func TestCorruptRowsAreReportedNotSkipped(t *testing.T) {
	s, c := faultStore(t)
	ctx := context.Background()
	if _, err := s.CommitSyncPage(ctx, "s1", "n", false, []InboxMessage{{BindingID: "b1", ExternalMsgID: "m", CustomerID: c.ID, Generation: 1, Type: "text", PayloadRef: "a"}}); err != nil {
		t.Fatal(err)
	}
	s.DB().Exec(`UPDATE inbox SET create_time='not-a-number'`)
	if _, err := s.InboxForDelivery(ctx, 3, 10); err == nil {
		t.Fatal("corrupt inbox row ignored")
	}
	s.DB().Exec(`UPDATE sync_scopes SET pending=1, updated_at='bad'`)
	if _, err := s.PendingScopes(ctx, 10); err == nil {
		t.Fatal("corrupt scope row ignored")
	}
}

func TestMissingRowsAndUIDFailure(t *testing.T) {
	s, c := faultStore(t)
	ctx := context.Background()
	if _, err := s.TransitionInbox(ctx, 12345, InboxClassified, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("inbox: %v", err)
	}
	if _, err := s.TransitionOutbox(ctx, "missing", OutboxRejected, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outbox: %v", err)
	}
	if _, err := s.MarkOutboxUnknown(ctx, "missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	s.uid = func() (string, error) { return "", errors.New("no entropy") }
	if _, err := s.RotateGeneration(ctx, c.ID, "r"); err == nil {
		t.Fatal("rotated without a new uid")
	}
	if got, _ := s.Customer(ctx, c.ID); got.UID != c.UID || got.Generation != c.Generation {
		t.Fatal("customer changed")
	}
}

func TestRecoverWithoutUIDAndCustomerInitiatedWindow(t *testing.T) {
	s, c := faultStore(t)
	ctx := context.Background()
	if _, err := s.BeginHandover(ctx, c.ID, "agent"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetHandoverStatus(ctx, c.ID, CustomerHuman, "agent"); err != nil {
		t.Fatal(err)
	}
	orig := s.uid
	s.uid = func() (string, error) { return "", errors.New("no entropy") }
	if _, err := s.RecoverCustomer(ctx, c.ID, "done"); err == nil {
		t.Fatal("recovered without a new uid")
	}
	s.uid = orig
	// customer message without CreateTime still opens the window (at now)
	s.DB().Exec(`UPDATE customers SET last_inbound_at=0 WHERE id=?`, c.ID)
	msg := []InboxMessage{{BindingID: "b1", ExternalMsgID: "w1", CustomerID: c.ID, Generation: 1, Type: "text", PayloadRef: "a", CustomerInitiated: true}}
	undo := inject(t, s, "UPDATE", "customers")
	if _, err := s.CommitSyncPage(ctx, "s1", "n", false, msg); err == nil {
		t.Fatal("window update failure ignored")
	}
	undo()
	if _, err := s.CommitSyncPage(ctx, "s1", "n", false, msg); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Customer(ctx, c.ID)
	if got.LastInboundAt.IsZero() {
		t.Fatal("window not opened")
	}
}

// Without a master key, customer content is never written in plaintext.
func TestNoMasterKeyRefusesToStoreContent(t *testing.T) {
	s, err := OpenWithOptions(t.TempDir()+"/nokey.db", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedScope(t, s)
	ctx := context.Background()
	c, _ := s.EnsureCustomer(ctx, "e1", "b1", "x")
	_ = s.SetCustomerAuthorized(ctx, c.ID, true)
	openWindow(t, s, c.ID)
	if _, err := s.CreateOutbox(ctx, OutboxMessage{CustomerID: c.ID, Generation: c.Generation, UID: c.UID, Body: "b", BudgetUnits: 1}); !errors.Is(err, ErrNoSealer) {
		t.Fatalf("outbox: %v", err)
	}
	if _, err := s.CommitSyncPage(ctx, "s1", "n", false, []InboxMessage{{BindingID: "b1", ExternalMsgID: "m", CustomerID: c.ID, PayloadRef: "p"}}); !errors.Is(err, ErrNoSealer) {
		t.Fatalf("inbox: %v", err)
	}
	var n int
	s.DB().QueryRow(`SELECT (SELECT COUNT(1) FROM outbox)+(SELECT COUNT(1) FROM inbox)`).Scan(&n)
	if n != 0 {
		t.Fatal("content stored without encryption")
	}
}

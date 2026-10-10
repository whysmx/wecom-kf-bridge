package state

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

var testMasterKey = bytes.Repeat([]byte{9}, 32)

func TestBodiesEncryptedAtRest(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedScope(t, s)
	c, err := s.EnsureCustomer(ctx, "e1", "b1", "ext-secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitSyncPage(ctx, "s1", "n1", false, []InboxMessage{{BindingID: "b1", ExternalMsgID: "m1", CompatMsgID: 1, PayloadRef: "客户机密正文"}}); err != nil {
		t.Fatal(err)
	}
	openWindow(t, s, c.ID)
	o, err := s.CreateOutbox(ctx, OutboxMessage{CustomerID: c.ID, Generation: c.Generation, UID: c.UID, Body: "AI 回答正文"})
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := s.DB().QueryRow(`SELECT payload_ref FROM inbox`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "机密") || !strings.HasPrefix(raw, sealPrefix) {
		t.Fatalf("inbox payload stored in clear: %q", raw)
	}
	m, err := s.InboxByExternalID(ctx, "s1", "m1")
	if err != nil || m.PayloadRef != "客户机密正文" {
		t.Fatalf("decrypt inbox %v %q", err, m.PayloadRef)
	}
	if o.ID != "" {
		if err := s.DB().QueryRow(`SELECT body FROM outbox WHERE id=?`, o.ID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(raw, "回答") {
			t.Fatalf("outbox body in clear: %q", raw)
		}
		got, err := s.Outbox(ctx, o.ID)
		if err != nil || got.Body != "AI 回答正文" {
			t.Fatalf("decrypt outbox %v %q", err, got.Body)
		}
	}
	// tampering / wrong key is detected
	if _, err := s.DB().Exec(`UPDATE inbox SET payload_ref=?`, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InboxByExternalID(ctx, "s1", "m1"); !errors.Is(err, ErrSealed) {
		t.Fatalf("swapped ciphertext accepted: %v", err)
	}
}

func TestNoSealerRejectsSensitiveWrites(t *testing.T) {
	ctx := context.Background()
	s, err := OpenWithOptions(t.TempDir()+"/x.db", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedScope(t, s)
	if _, err := s.CommitSyncPage(ctx, "s1", "n", false, []InboxMessage{{BindingID: "b1", ExternalMsgID: "m", CompatMsgID: 1, PayloadRef: "x"}}); !errors.Is(err, ErrNoSealer) {
		t.Fatalf("expected ErrNoSealer, got %v", err)
	}
	if _, err := NewSealer([]byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
	if _, err := ParseMasterKey("bad"); err == nil {
		t.Fatal("bad master key accepted")
	}
	if k, err := ParseMasterKey("CQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQk"); err != nil || len(k) != 32 {
		t.Fatalf("parse %v", err)
	}
	if _, err := OpenWithOptions(t.TempDir()+"/y.db", Options{MasterKey: []byte{1}}); err == nil {
		t.Fatal("invalid master key accepted by Open")
	}
}

// openWindow simulates a fresh customer-initiated message for c.
func openWindow(t *testing.T, s *Store, customerID string) {
	t.Helper()
	if _, err := s.DB().Exec(`UPDATE customers SET last_inbound_at=?,window_used=0 WHERE id=?`, unix(s.now()), customerID); err != nil {
		t.Fatal(err)
	}
}

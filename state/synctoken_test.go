package state

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestSyncTokenSealedAndExpires(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedScope(t, s)
	now := s.now()
	if err := s.SaveSyncToken(ctx, "s1", "pull-token-secret", now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var raw string
	_ = s.DB().QueryRow(`SELECT sync_token FROM sync_scopes WHERE id='s1'`).Scan(&raw)
	if strings.Contains(raw, "pull-token") {
		t.Fatalf("token stored in clear: %q", raw)
	}
	if tok, err := s.SyncToken(ctx, "s1"); err != nil || tok != "pull-token-secret" {
		t.Fatalf("token %q %v", tok, err)
	}
	sc, _ := s.Scope(ctx, "s1")
	if !sc.Pending {
		t.Fatal("pending not set")
	}
	if err := s.SaveSyncToken(ctx, "s1", "old", now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if tok, _ := s.SyncToken(ctx, "s1"); tok != "" {
		t.Fatal("expired token returned")
	}
	if err := s.SaveSyncToken(ctx, "nope", "x", now); err != ErrNotFound {
		t.Fatalf("missing scope %v", err)
	}
	if err := s.SaveSyncToken(ctx, "", "x", now); err != ErrInvalidID {
		t.Fatal(err)
	}
	if _, err := s.SyncToken(ctx, "nope"); err != ErrNotFound {
		t.Fatal(err)
	}
}

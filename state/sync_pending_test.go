package state

import (
	"context"
	"testing"
)

func seedScope(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	if err := s.PutEnterprise(ctx, Enterprise{ID: "e1", TenantID: "t1", CorpID: "c1", CredentialRef: "env:X"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutBinding(ctx, Binding{ID: "b1", EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p1", Active: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureScope(ctx, "s1", "b1"); err != nil {
		t.Fatal(err)
	}
}

func TestCommitSyncPageKeepsPendingWhileHasMore(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	seedScope(t, s)
	if err := s.MarkSyncPending(ctx, "s1", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitSyncPage(ctx, "s1", "c1", true, nil); err != nil {
		t.Fatal(err)
	}
	sc, _ := s.Scope(ctx, "s1")
	if !sc.Pending || sc.Cursor != "c1" {
		t.Fatalf("pending lost mid-pagination: %+v", sc)
	}
	if _, err := s.CommitSyncPage(ctx, "s1", "c2", false, nil); err != nil {
		t.Fatal(err)
	}
	sc, _ = s.Scope(ctx, "s1")
	if sc.Pending || sc.Cursor != "c2" {
		t.Fatalf("pending not cleared at end: %+v", sc)
	}
}

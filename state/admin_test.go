package state

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestKFAccountsMirrorPagingAndCAS(t *testing.T) {
	s, _ := faultStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := s.UpsertAccount(ctx, KFAccount{OpenKfID: fmt.Sprintf("kf%d", i), Name: fmt.Sprintf("客服%d", i), Status: AccountActive}); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.UpsertAccount(ctx, KFAccount{OpenKfID: "odd_%", Name: "x", Status: AccountActive})
	page, total, err := s.Accounts(ctx, "", 2, 2)
	if err != nil || total != 6 || len(page) != 2 || page[0].OpenKfID != "kf2" {
		t.Fatalf("%v %d %v", page, total, err)
	}
	if got, n, _ := s.Accounts(ctx, "客服3", 0, 10); n != 1 || got[0].OpenKfID != "kf3" {
		t.Fatalf("search %v", got)
	}
	// LIKE wildcards in the query are literal
	if _, n, _ := s.Accounts(ctx, "_%", 0, 10); n != 1 {
		t.Fatalf("wildcard not escaped: %d", n)
	}
	a, _ := s.Account(ctx, "kf1")
	if _, err := s.UpdateAccountLocal(ctx, "kf1", a.Revision+5, func(*KFAccount) {}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale revision: %v", err)
	}
	b, err := s.UpdateAccountLocal(ctx, "kf1", a.Revision, func(x *KFAccount) { x.Note = "备注" })
	if err != nil || b.Note != "备注" || b.Revision != a.Revision+1 {
		t.Fatalf("%+v %v", b, err)
	}
	// a re-sync keeps the local note and bumps the revision
	_ = s.UpsertAccount(ctx, KFAccount{OpenKfID: "kf1", Name: "改名", Status: AccountActive})
	c, _ := s.Account(ctx, "kf1")
	if c.Note != "备注" || c.Name != "改名" || c.Revision != b.Revision+1 {
		t.Fatalf("%+v", c)
	}
	if _, err := s.Account(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.UpdateAccountLocal(ctx, "missing", 1, nil); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if s.UpsertAccount(ctx, KFAccount{}) == nil {
		t.Fatal("empty account")
	}
}

func TestAdminOperationIdempotency(t *testing.T) {
	s, _ := faultStore(t)
	ctx := context.Background()
	if op, err := s.BeginOperation(ctx, "k1", "h1"); op != nil || err != nil {
		t.Fatalf("%v %v", op, err)
	}
	if _, err := s.BeginOperation(ctx, "k1", "h1"); !errors.Is(err, ErrOperationInProgress) {
		t.Fatalf("in progress: %v", err)
	}
	if _, err := s.BeginOperation(ctx, "k1", "h2"); !errors.Is(err, ErrIdempotencyMismatch) {
		t.Fatalf("mismatch: %v", err)
	}
	if err := s.FinishOperation(ctx, "k1", "/admin/x"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishOperation(ctx, "k1", "/admin/y"); !errors.Is(err, ErrConflict) {
		t.Fatal("finished twice")
	}
	op, err := s.BeginOperation(ctx, "k1", "h1")
	if err != nil || op == nil || op.Location != "/admin/x" {
		t.Fatalf("replay %v %v", op, err)
	}
	if _, err := s.BeginOperation(ctx, "", "h"); !errors.Is(err, ErrInvalidID) {
		t.Fatal("empty key")
	}
}

func TestActorAuditAndPaging(t *testing.T) {
	s, _ := faultStore(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := s.AddActorAudit(ctx, AuditEntry{Actor: "admin", Key: fmt.Sprint("a", i), ObjectType: "binding", ObjectID: "b1", Operation: "disable", Result: "ok", Revision: int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.AddActorAudit(ctx, AuditEntry{Actor: "admin", Key: "a0", Operation: "dup"})
	got, total, err := s.Audits(ctx, 0, 2)
	if err != nil || total != 3 || len(got) != 2 || got[0].Key != "a2" || got[0].Actor != "admin" {
		t.Fatalf("%+v %d %v", got, total, err)
	}
	if s.AddActorAudit(ctx, AuditEntry{}) == nil {
		t.Fatal("empty key")
	}
}

func TestUpdateBindingAndRotateCustomers(t *testing.T) {
	s, c := faultStore(t)
	ctx := context.Background()
	o, _ := s.CreateOutbox(ctx, OutboxMessage{CustomerID: c.ID, Generation: c.Generation, UID: c.UID, Body: "x", BudgetUnits: 1})
	b, _ := s.Binding(ctx, "b1")
	if _, err := s.UpdateBinding(ctx, "b1", b.Revision+1, func(*Binding) {}); !errors.Is(err, ErrConflict) {
		t.Fatal("stale binding revision")
	}
	nb, err := s.UpdateBinding(ctx, "b1", b.Revision, func(x *Binding) { x.ProjectID = "p9"; x.CallbackURL = "http://cc:1/x" })
	if err != nil || nb.Revision != b.Revision+1 || nb.ProjectID != "p9" || !nb.Active {
		t.Fatalf("%+v %v", nb, err)
	}
	if _, err := s.MarkOutboxSending(ctx, o.ID); !errors.Is(err, ErrBindingInactive) {
		t.Fatalf("old revision outbox: %v", err)
	}
	n, err := s.RotateBindingCustomers(ctx, "b1")
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if _, err := s.CustomerByUID(ctx, "b1", c.UID); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("old uid: %v", err)
	}
	if _, err := s.UpdateBinding(ctx, "missing", 1, nil); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	undo := inject(t, s, "INSERT", "customer_uids")
	if _, err := s.RotateBindingCustomers(ctx, "b1"); err == nil {
		t.Fatal("partial rotation")
	}
	undo()
	cur, _ := s.Customer(ctx, c.ID)
	if cur.Generation != c.Generation+1 {
		t.Fatalf("rotation not atomic: %d", cur.Generation)
	}
	s.uid = func() (string, error) { return "", errors.New("no entropy") }
	if _, err := s.RotateBindingCustomers(ctx, "b1"); err == nil {
		t.Fatal("rotation without uid")
	}
	list, _ := s.Bindings(ctx)
	if len(list) != 1 || list[0].ID != "b1" {
		t.Fatal(list)
	}
}

func TestBindingSecretExportOnce(t *testing.T) {
	s, _ := faultStore(t)
	ctx := context.Background()
	if _, _, err := s.BindingSecret(ctx, "b1"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.SaveBindingSecret(ctx, "b1", `{"secret":"s1"}`, 2); err != nil {
		t.Fatal(err)
	}
	var raw string
	s.DB().QueryRow(`SELECT sealed FROM binding_secrets`).Scan(&raw)
	if raw == "" || raw == `{"secret":"s1"}` {
		t.Fatal("secret stored in plaintext")
	}
	if v, err := s.ExportBindingSecret(ctx, "b1"); err != nil || v != `{"secret":"s1"}` {
		t.Fatal(v, err)
	}
	if _, err := s.ExportBindingSecret(ctx, "b1"); !errors.Is(err, ErrConflict) {
		t.Fatal("exported twice")
	}
	if _, exported, _ := s.BindingSecret(ctx, "b1"); !exported {
		t.Fatal("export flag")
	}
	_ = s.SaveBindingSecret(ctx, "b1", `{"secret":"s2"}`, 3)
	if v, err := s.ExportBindingSecret(ctx, "b1"); err != nil || v != `{"secret":"s2"}` {
		t.Fatal("rotation re-arms export", v, err)
	}
	plain, _ := OpenWithOptions(t.TempDir()+"/k.db", Options{})
	defer plain.Close()
	if plain.SaveBindingSecret(ctx, "b1", "x", 1) == nil {
		t.Fatal("secret without master key")
	}
}

func TestDiagnosticsListsAndOverview(t *testing.T) {
	s, c := faultStore(t)
	ctx := context.Background()
	_, _ = s.CommitSyncPage(ctx, "s1", "n", false, []InboxMessage{{BindingID: "b1", ExternalMsgID: "m1", CustomerID: c.ID, Generation: 1, Type: "text", PayloadRef: "正文"}})
	o, _ := s.CreateOutbox(ctx, OutboxMessage{CustomerID: c.ID, Generation: c.Generation, UID: c.UID, Body: "答", BudgetUnits: 1})
	s.MarkOutboxSending(ctx, o.ID)
	s.MarkOutboxUnknown(ctx, o.ID, "x")
	in, err := s.InboxByStates(ctx, "", []string{InboxReceived}, 10)
	if err != nil || len(in) != 1 {
		t.Fatal(in, err)
	}
	if in, _ := s.InboxByStates(ctx, c.ID, nil, 10); len(in) != 1 {
		t.Fatal("by customer")
	}
	out, err := s.OutboxByStates(ctx, c.ID, []string{OutboxUnknown}, 10)
	if err != nil || len(out) != 1 {
		t.Fatal(out, err)
	}
	if out, _ := s.OutboxByStates(ctx, "", nil, 10); len(out) != 1 {
		t.Fatal("all")
	}
	if err := s.MarkDiagnostic(ctx, "outbox", o.ID, "已人工核实"); err != nil {
		t.Fatal(err)
	}
	m, _ := s.DiagnosticMarks(ctx, "outbox")
	if m[o.ID] != "已人工核实" {
		t.Fatal(m)
	}
	if got, _ := s.Outbox(ctx, o.ID); got.State != OutboxUnknown {
		t.Fatal("mark changed message state")
	}
	if s.MarkDiagnostic(ctx, "", "", "") == nil {
		t.Fatal("empty")
	}
	cs, err := s.Customers(ctx, c.UID, 10)
	if err != nil || len(cs) != 1 {
		t.Fatal(cs, err)
	}
	if cs, _ := s.Customers(ctx, "", 10); len(cs) != 1 {
		t.Fatal("all customers")
	}
	ov, err := s.Overview(ctx)
	if err != nil || ov["outbox_unknown"] != 1 || ov["bindings_active"] != 1 {
		t.Fatal(ov, err)
	}
	s.DB().Exec(`UPDATE inbox SET payload_ref='tampered'`)
	if _, err := s.InboxByStates(ctx, "", nil, 10); err == nil {
		t.Fatal("tampered inbox")
	}
	s.DB().Exec(`UPDATE outbox SET body='tampered'`)
	if _, err := s.OutboxByStates(ctx, "", nil, 10); err == nil {
		t.Fatal("tampered outbox")
	}
}

func TestAdminQueriesOnClosedStore(t *testing.T) {
	s, _ := faultStore(t)
	s.Close()
	ctx := context.Background()
	errs := []error{s.UpsertAccount(ctx, KFAccount{OpenKfID: "k", Status: AccountActive}), s.FinishOperation(ctx, "k", "l"), s.AddActorAudit(ctx, AuditEntry{Key: "k"}), s.SaveBindingSecret(ctx, "b1", "x", 1), s.MarkDiagnostic(ctx, "a", "b", "")}
	add := func(_ any, e error) { errs = append(errs, e) }
	add(s.BeginOperation(ctx, "k", "h"))
	add(s.ExportBindingSecret(ctx, "b1"))
	add(s.DiagnosticMarks(ctx, "a"))
	add(s.InboxByStates(ctx, "", nil, 1))
	add(s.OutboxByStates(ctx, "", nil, 1))
	add(s.Customers(ctx, "", 1))
	add(s.Bindings(ctx))
	add(s.Overview(ctx))
	add(s.RotateBindingCustomers(ctx, "b1"))
	_, _, e1 := s.Accounts(ctx, "", 0, 1)
	_, _, e2 := s.Audits(ctx, 0, 1)
	errs = append(errs, e1, e2)
	for i, e := range errs {
		if e == nil {
			t.Errorf("op %d succeeded on closed store", i)
		}
	}
}

func TestCollectReportsCorruptRows(t *testing.T) {
	s, _ := faultStore(t)
	ctx := context.Background()
	if _, err := s.DB().Exec(`INSERT INTO audit(idempotency_key,object_type,object_id,operation,result,created_at) VALUES('k','o','i','op','r','not-a-time')`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Audits(ctx, 0, 10); err == nil {
		t.Fatal("corrupt row ignored")
	}
}

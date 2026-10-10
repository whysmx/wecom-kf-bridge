package bridge

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/whysmx/wecom-kf-bridge/state"
)

// racingStore runs a concurrent change just before the n-th guarded chunk
// (outside the store lock, as another request/goroutine would).
type racingStore struct {
	*state.Store
	before func(chunk int)
	calls  atomic.Int32
}

func (r *racingStore) SendGuarded(ctx context.Context, id string, fn func() error) error {
	r.before(int(r.calls.Add(1)))
	return r.Store.SendGuarded(ctx, id, fn)
}

func withRacing(e *env, before func(chunk int)) {
	e.t.Helper()
	srv, err := NewServer(Config{Bindings: []Binding{testBinding()}, Store: &racingStore{Store: e.st, before: before}, Adapter: e.ad, Clock: e.clock, MaxChunkBytes: 8})
	must(e.t, err)
	e.srv = srv
}

// #25/#27: a binding rotation, disable or takeover that lands between two
// chunks stops the remaining chunks; the outbox records the progress and
// the unsent budget is released.
func TestSendStopsWhenPreconditionChangesMidMessage(t *testing.T) {
	ctx := context.Background()
	for name, change := range map[string]func(e *env){
		"rotate": func(e *env) {
			e.st.PutBinding(ctx, state.Binding{ID: "b1", EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p", Active: true, Revision: 2})
		},
		"disable": func(e *env) {
			e.st.PutBinding(ctx, state.Binding{ID: "b1", EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p", Active: false})
		},
		"takeover": func(e *env) { e.st.BeginHandover(ctx, e.cust.ID, "agent") },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			authorize(e)
			withRacing(e, func(chunk int) {
				if chunk == 2 {
					change(e)
				}
			})
			_, v := e.send(e.token(), e.cust.UID, "text", strings.Repeat("a", 24))
			if errcode(v) != ErrCodeDisabled {
				t.Fatalf("errcode %v", v)
			}
			if n := len(e.ad.sent()); n != 1 {
				t.Fatalf("%d chunks went out after the change", n)
			}
			o := e.outboxes()[0]
			if o.State != state.OutboxRejected || o.ErrorCategory != "precondition_lost:1/3" || o.ChunksSent != 1 || o.BindingRevision != 1 {
				t.Fatalf("outbox %+v", o)
			}
			c, _ := e.st.Customer(ctx, e.cust.ID)
			if c.WindowUsed != 1 {
				t.Fatalf("budget not released: used=%d", c.WindowUsed)
			}
		})
	}
}

func TestSendAbortRecordFailureIsUnknown(t *testing.T) {
	e := newEnv(t)
	authorize(e)
	undo := func() {}
	withRacing(e, func(chunk int) {
		if chunk == 2 {
			e.st.PutBinding(context.Background(), state.Binding{ID: "b1", EnterpriseID: "e1", OpenKfID: "kf1", ProjectID: "p", Active: false})
			undo = e.inject("UPDATE", "outbox")
		}
	})
	_, v := e.send(e.token(), e.cust.UID, "text", strings.Repeat("a", 24))
	undo()
	if errcode(v) != ErrCodeUnknown {
		t.Fatalf("errcode %v", v)
	}
}

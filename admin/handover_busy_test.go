package admin

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/whysmx/wecom-kf-bridge/state"
)

func TestRetrySQLiteBusy(t *testing.T) {
	ctx := context.Background()
	var n atomic.Int32
	err := retrySQLiteBusy(ctx, 5, func() error {
		if n.Add(1) < 3 {
			return errors.New("database is locked (517)")
		}
		return nil
	})
	if err != nil || n.Load() != 3 {
		t.Fatalf("err=%v n=%d", err, n.Load())
	}
	n.Store(0)
	perm := errors.New("constraint failed")
	if err := retrySQLiteBusy(ctx, 5, func() error {
		n.Add(1)
		return perm
	}); !errors.Is(err, perm) || n.Load() != 1 {
		t.Fatalf("non-busy should not retry: err=%v n=%d", err, n.Load())
	}
	if !isSQLiteBusy(errors.New("database is locked (517)")) || isSQLiteBusy(perm) {
		t.Fatal("isSQLiteBusy")
	}
}

// #10: official handover already applied; local BeginHandover fails → 503,
// audit official_ok_local_failed, customer must not silently look like a
// clean "转人工失败" 409, and flash must warn AI may still run.
func TestHandoverLocalWriteFailureAfterOfficial(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cl := e.login()
	cu, err := e.st.Customer(ctx, e.cust.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cu.State != state.CustomerAIEligible {
		t.Fatalf("precondition %s", cu.State)
	}
	trig(t, e, "busy_ho", "BEFORE INSERT ON handover_events")
	w := cl.post("/admin/customers/"+cu.ID+"/handover", url.Values{"revision": {itoa64(cu.Revision)}})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "官方已转人工") || !strings.Contains(body, "本地状态保存失败") {
		t.Fatalf("flash: %s", body)
	}
	got, _ := e.st.Customer(ctx, cu.ID)
	if got.State != state.CustomerAIEligible {
		// Transaction rolled back; still AI — flash must have warned.
		t.Fatalf("unexpected state after rolled-back BeginHandover: %s", got.State)
	}
	audits, _, _ := e.st.Audits(ctx, 0, 20)
	found := false
	for _, a := range audits {
		if a.ObjectID == cu.ID && a.Operation == "handover" && a.Result == "official_ok_local_failed" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("missing audit official_ok_local_failed: %+v", audits)
	}
}

// #10: BeginHandover ok but SetHandoverStatus fails — still 503 + audit;
// local fence from BeginHandover must leave customer out of AI_ELIGIBLE.
func TestHandoverSetStatusFailureAfterBegin(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cl := e.login()
	cu, _ := e.st.Customer(ctx, e.cust.ID)
	// Abort only the HUMAN write inside SetHandoverStatus; BeginHandover
	// writes WAITING_HUMAN first.
	if _, err := e.st.DB().Exec(`CREATE TRIGGER block_human BEFORE UPDATE OF state ON customers WHEN NEW.state='HUMAN' BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	w := cl.post("/admin/customers/"+cu.ID+"/handover", url.Values{"revision": {itoa64(cu.Revision)}})
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "官方已转人工") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	got, _ := e.st.Customer(ctx, cu.ID)
	if got.State == state.CustomerAIEligible {
		t.Fatalf("BeginHandover fence missing: %+v", got)
	}
	audits, _, _ := e.st.Audits(ctx, 0, 20)
	found := false
	for _, a := range audits {
		if a.ObjectID == cu.ID && a.Result == "official_ok_local_failed" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("missing audit")
	}
}

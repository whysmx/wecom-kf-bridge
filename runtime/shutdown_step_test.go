package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	bridge "github.com/whysmx/wecom-kf-bridge"
	"github.com/whysmx/wecom-kf-bridge/state"
)

type cancelAfterRT struct {
	rt     http.RoundTripper
	cancel context.CancelFunc
}

func (c cancelAfterRT) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := c.rt.RoundTrip(r)
	c.cancel() // shutdown arrives right after cc-connect accepted the callback
	return resp, err
}

// Issue #5: shutdown cancelling the worker context after cc-connect returned
// 200 must still record HTTP_ACCEPTED, not leave the message POSTING.
func TestDeliveryRecordsAcceptanceDespiteShutdown(t *testing.T) {
	st, c := unitStore(t)
	bg := context.Background()
	_ = st.SetCustomerAuthorized(bg, c.ID, true)
	srv, _ := bridge.NewServer(bridge.Config{Store: st, Bindings: []bridge.Binding{{ID: "b1", CorpID: "v", CallbackToken: "t", CallbackAESKey: key43(4), Enabled: true}}})
	cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer cc.Close()
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	d := &DeliveryWorker{Store: st, Server: srv, CallbackURL: map[string]string{"b1": cc.URL}, AgentID: map[string]string{}, HTTP: &http.Client{Transport: cancelAfterRT{cc.Client().Transport, cancel}}, MaxAttempts: 3, MaxConc: 1, Logger: nopRuntimeLogger{}}
	if _, err := st.CommitSyncPage(bg, "sc", "x", false, []state.InboxMessage{{BindingID: "b1", ExternalMsgID: "x", CustomerID: c.ID, Generation: 1, Type: "text", PayloadRef: "q", CreateTime: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	d.DeliverOnce(ctx)
	if m, _ := st.InboxByExternalID(bg, "sc", "x"); m.State != state.InboxHTTPAccepted {
		t.Fatalf("accepted callback not recorded after shutdown: %s", m.State)
	}
}

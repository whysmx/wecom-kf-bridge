package wecom

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func fastClient(url string) *Client {
	c := NewClient(url, "c", "s")
	c.Backoff = time.Millisecond
	c.MaxBackoff = 2 * time.Millisecond
	return c
}

func TestClientReusesHTTPClient(t *testing.T) {
	c := NewClient("http://x", "c", "s")
	if c.httpClient() != c.httpClient() || c.httpClient().Timeout != 30*time.Second {
		t.Fatal("http.Client not reused")
	}
}

func TestReadRetriesOn5xxThenSucceeds(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte(`{"errcode":0}`))
			return
		}
		w.Write([]byte(`{"errcode":0,"has_more":0}`))
	}))
	defer srv.Close()
	if _, err := fastClient(srv.URL).SyncMsg(context.Background(), "t", SyncRequest{}); err != nil || n.Load() != 3 {
		t.Fatalf("err=%v attempts=%d", err, n.Load())
	}
}

func TestSendNotRetriedAfterResponseLoss(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"errcode":0}`))
	}))
	defer srv.Close()
	_, err := fastClient(srv.URL).SendMsg(context.Background(), "t", SendRequest{ToUser: "u", OpenKfID: "k", MsgType: "text", Text: &SendText{Content: "x"}})
	if err == nil || n.Load() != 1 {
		t.Fatalf("send retried after upstream saw it: err=%v attempts=%d", err, n.Load())
	}
	// connection refused proves non-delivery: bounded retries are allowed
	c := fastClient("http://127.0.0.1:1")
	start := time.Now()
	if _, err := c.SendMsg(context.Background(), "t", SendRequest{ToUser: "u", OpenKfID: "k", MsgType: "text"}); err == nil || !notConnected(err) {
		t.Fatalf("dial error expected: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("retry not bounded")
	}
}

func TestRateLimiterSpacesRequests(t *testing.T) {
	b := newTokenBucket(10, 1)
	now := time.Unix(0, 0)
	b.now = func() time.Time { return now }
	var slept time.Duration
	sleep := func(_ context.Context, d time.Duration) error { slept += d; now = now.Add(d); return nil }
	for i := 0; i < 3; i++ {
		if err := b.wait(context.Background(), sleep); err != nil {
			t.Fatal(err)
		}
	}
	if slept < 190*time.Millisecond || slept > 210*time.Millisecond {
		t.Fatalf("slept %v, want ~200ms for 3 calls at 10/s burst 1", slept)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.wait(ctx, sleepCtx); err == nil {
		t.Fatal("cancelled wait succeeded")
	}
}

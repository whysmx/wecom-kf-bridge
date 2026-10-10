package runtime

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

type countWorker struct{ started, stopped atomic.Int32 }

func (w *countWorker) Run(ctx context.Context) { w.started.Add(1); <-ctx.Done(); w.stopped.Add(1) }

func TestAppStartsAndStopsWorkers(t *testing.T) {
	w := &countWorker{}
	a, _ := NewApp(AppConfig{Addr: "127.0.0.1:0", Workers: []Worker{w}})
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go a.Serve(ln)
	deadline := time.Now().Add(2 * time.Second)
	for w.started.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.Shutdown(ctx); err != nil || w.stopped.Load() != 1 {
		t.Fatalf("err=%v stopped=%d", err, w.stopped.Load())
	}
}

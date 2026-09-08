package messaging

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"clawsynapse/internal/discovery"
	"clawsynapse/internal/identity"
	"clawsynapse/internal/protocol"
)

// ── T2.1 acceptance: per-sessionKey serial dispatch ──────────────────

func newTestDispatcher(t *testing.T) *sessionDispatcher {
	t.Helper()
	d := newSessionDispatcher(slog.Default())
	d.enqueueTimeout = 200 * time.Millisecond
	d.idleTTL = 100 * time.Millisecond
	return d
}

// TestSessionDispatcher_SameKeySerial: 20 deliveries on one key with
// deliberately inverted execution costs must run strictly in dispatch
// order — without serialization the later (faster) items would finish
// first and scramble the order.
func TestSessionDispatcher_SameKeySerial(t *testing.T) {
	d := newTestDispatcher(t)
	const n = 20
	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		d.Dispatch("sk", func() {
			defer wg.Done()
			time.Sleep(time.Duration(n-i) * time.Millisecond)
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
		})
	}
	wg.Wait()
	for i, got := range order {
		if got != i {
			t.Fatalf("order[%d] = %d (serialization broken)", i, got)
		}
	}
}

// TestSessionDispatcher_DifferentKeysConcurrent: a blocked key A must
// not prevent key B's delivery from running.
func TestSessionDispatcher_DifferentKeysConcurrent(t *testing.T) {
	d := newTestDispatcher(t)
	release := make(chan struct{})
	done := make(chan struct{})
	d.Dispatch("a", func() { <-release }) // blocks key A's worker
	d.Dispatch("b", func() { close(done) })
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("key B did not run concurrently with blocked key A")
	}
	close(release)
}

// TestSessionDispatcher_QueueFullDrops: when the per-key queue is full
// the extra delivery is dropped (bounded wait, then warn) instead of
// growing the queue unboundedly.
func TestSessionDispatcher_QueueFullDrops(t *testing.T) {
	d := newTestDispatcher(t)
	d.enqueueTimeout = 100 * time.Millisecond
	release := make(chan struct{})
	entered := make(chan struct{})
	var ran atomic.Int32

	d.Dispatch("k", func() { close(entered); <-release }) // occupies the worker
	<-entered
	for i := 0; i < sessionQueueDepth; i++ {
		d.Dispatch("k", func() { ran.Add(1) })
	}
	// One beyond capacity: dropped after the enqueue timeout.
	d.Dispatch("k", func() { ran.Add(1) })
	close(release)

	deadline := time.After(2 * time.Second)
	for ran.Load() < sessionQueueDepth {
		select {
		case <-deadline:
			t.Fatalf("ran = %d, want %d", ran.Load(), sessionQueueDepth)
		case <-time.After(5 * time.Millisecond):
		}
	}
	time.Sleep(50 * time.Millisecond) // the dropped one must never run
	if got := ran.Load(); got != sessionQueueDepth {
		t.Fatalf("ran = %d, want exactly %d (drop failed)", got, sessionQueueDepth)
	}
}

// TestSessionDispatcher_WorkerRebuildAfterIdle: after the idle TTL the
// worker deregisters and exits; the next dispatch rebuilds it.
func TestSessionDispatcher_WorkerRebuildAfterIdle(t *testing.T) {
	d := newTestDispatcher(t)
	d.idleTTL = 50 * time.Millisecond

	done1 := make(chan struct{})
	d.Dispatch("k", func() { close(done1) })
	<-done1
	time.Sleep(120 * time.Millisecond) // worker idles out and exits

	done2 := make(chan struct{})
	d.Dispatch("k", func() { close(done2) })
	select {
	case <-done2:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch after worker idle exit was not rebuilt")
	}
}

// TestMaybeDeliver_SameSessionKeySerial: 20 out-of-order-cost messages on
// the same session key reach the handler strictly in publish order.
func TestMaybeDeliver_SameSessionKeySerial(t *testing.T) {
	peers := discovery.NewRegistry()
	base := t.TempDir()
	id, err := identity.LoadOrCreate(base+"/identity.key", base+"/identity.pub")
	if err != nil {
		t.Fatalf("identity init failed: %v", err)
	}

	const n = 20
	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	wg.Add(n)

	svc := NewService(slog.Default(), peers, nil, "node-alpha", id, "open", nil)
	svc.SetMessageHandler(MessageHandlerFunc(func(msg IncomingMessage) (HandlerResult, error) {
		defer wg.Done()
		seq, _ := msg.Metadata["seq"].(int)
		time.Sleep(time.Duration(n-seq) * time.Millisecond)
		mu.Lock()
		order = append(order, seq)
		mu.Unlock()
		return HandlerResult{}, nil // empty reply: no publish path
	}))

	for i := 0; i < n; i++ {
		svc.maybeDeliver(protocol.MessageEnvelope{
			Type:       "chat.message",
			From:       "node-beta",
			SessionKey: "task-1",
			Content:    "m",
			Metadata:   map[string]any{"seq": i},
		})
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for deliveries")
	}
	for i, got := range order {
		if got != i {
			t.Fatalf("delivery order[%d] = %d, want %d", i, got, i)
		}
	}
}

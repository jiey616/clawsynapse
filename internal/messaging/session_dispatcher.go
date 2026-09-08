package messaging

import (
	"log/slog"
	"strings"
	"sync"
	"time"

	"clawsynapse/internal/protocol"
)

// ── Per-sessionKey serial dispatch (dev-spec T2.1) ───────────────────

const (
	// sessionQueueDepth bounds how many deliveries may wait in line for
	// one session key. Beyond it, Dispatch waits synchronously (bounded
	// by sessionEnqueueTimeout) before dropping with a warning.
	sessionQueueDepth = 32
	// sessionEnqueueTimeout bounds the synchronous wait when a queue is
	// full.
	sessionEnqueueTimeout = 10 * time.Second
	// sessionWorkerIdleTTL is how long an idle per-key worker stays
	// registered before exiting; the next dispatch for the key rebuilds
	// it.
	sessionWorkerIdleTTL = 5 * time.Minute
	// enqueueRetryInterval is the poll step while waiting for queue space.
	enqueueRetryInterval = 50 * time.Millisecond
)

// sessionDispatcher serializes deliveries per session key: one FIFO
// queue and one worker goroutine per key. Different keys run
// concurrently; a global concurrency bound is intentionally out of
// scope here (the runs domain is capped by the TaskCoordinator, and the
// responses domain stays parallel across keys — same-key serialization
// alone eliminates session races).
type sessionDispatcher struct {
	log    *slog.Logger
	mu     sync.Mutex
	queues map[string]*sessionQueue

	// tunables (tests only)
	enqueueTimeout time.Duration
	idleTTL        time.Duration
}

type sessionQueue struct {
	pending []func()
	wake    chan struct{} // cap-1 "work available" signal
}

func newSessionDispatcher(log *slog.Logger) *sessionDispatcher {
	if log == nil {
		log = slog.Default()
	}
	return &sessionDispatcher{
		log:            log,
		queues:         make(map[string]*sessionQueue),
		enqueueTimeout: sessionEnqueueTimeout,
		idleTTL:        sessionWorkerIdleTTL,
	}
}

// sessionDispatchKey returns the dispatch key for an envelope: the
// session key when present, otherwise a per-sender fallback so
// unrelated senders never share a queue.
func sessionDispatchKey(env protocol.MessageEnvelope) string {
	if k := strings.TrimSpace(env.SessionKey); k != "" {
		return k
	}
	return "from:" + env.From
}

// Dispatch schedules fn on the key's FIFO queue, creating the queue and
// its worker on first use. When the queue is full it waits synchronously
// for space up to enqueueTimeout, then drops the delivery with a warning
// (backpressure surface, not silent growth). Returns whether the delivery
// was enqueued; false lets the durable inbox (T2.5) Nak and redeliver.
func (d *sessionDispatcher) Dispatch(key string, fn func()) bool {
	deadline := time.Now().Add(d.enqueueTimeout)
	for {
		d.mu.Lock()
		q, ok := d.queues[key]
		if !ok {
			q = &sessionQueue{wake: make(chan struct{}, 1)}
			d.queues[key] = q
			go d.worker(key, q)
		}
		if len(q.pending) < sessionQueueDepth {
			q.pending = append(q.pending, fn)
			select {
			case q.wake <- struct{}{}:
			default: // worker already signalled
			}
			d.mu.Unlock()
			return true
		}
		d.mu.Unlock()
		if time.Now().After(deadline) {
			d.log.Warn("session queue full; dropping delivery", "sessionKey", key)
			return false
		}
		time.Sleep(enqueueRetryInterval)
	}
}

// worker serves the queue until it has been idle for the TTL, then
// deregisters and exits. Deregistration and enqueue are mutually
// exclusive under d.mu, so a delivery can never be appended to a queue
// whose worker is gone.
func (d *sessionDispatcher) worker(key string, q *sessionQueue) {
	idle := time.NewTimer(d.idleTTL)
	defer idle.Stop()
	for {
		select {
		case <-q.wake:
			stopAndResetTimer(idle, d.idleTTL)
			d.drain(key, q, idle)
		case <-idle.C:
			d.mu.Lock()
			if cur, ok := d.queues[key]; ok && cur == q && len(q.pending) == 0 {
				delete(d.queues, key)
				d.mu.Unlock()
				return
			}
			d.mu.Unlock()
			// Work arrived before expiry (or the entry changed): keep
			// serving until the queue is empty.
			d.drain(key, q, idle)
		}
	}
}

// drain runs every pending fn in FIFO order. fn runs outside every lock,
// so handlers may publish/dispatch freely.
func (d *sessionDispatcher) drain(key string, q *sessionQueue, idle *time.Timer) {
	for {
		d.mu.Lock()
		if len(q.pending) == 0 {
			d.mu.Unlock()
			return
		}
		fn := q.pending[0]
		q.pending = q.pending[1:]
		d.mu.Unlock()
		fn()
		stopAndResetTimer(idle, d.idleTTL)
	}
}

// stopAndResetTimer stops t, drains a possibly-pending tick (a long fn
// execution must not let the idle tick survive and kill a fresh worker)
// and rearms it.
func stopAndResetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

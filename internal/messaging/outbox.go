package messaging

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"clawsynapse/internal/logging"
	"clawsynapse/internal/store"
)

// ── Outbox: reliable return path for agent replies (dev-spec T2.7) ──

const (
	outboxFlushInterval = 30 * time.Second
	outboxBaseDelay     = 30 * time.Second
	outboxMaxDelay      = 10 * time.Minute
	outboxMaxRetries    = 10
	outboxMaxAge        = 24 * time.Hour
)

// OutboxEntry is one pending agent result/error receipt waiting for
// delivery.
type OutboxEntry struct {
	ID          string `json:"id"`          // uuid
	TargetNode  string `json:"targetNode"`  // original sender node
	Type        string `json:"type"`        // reply type (e.g. todo.response)
	SessionKey  string `json:"sessionKey"`  // original session key
	Payload     string `json:"payload"`     // reply content
	RetryCount  int    `json:"retryCount"`  // failed delivery attempts so far
	NextRetryAt int64  `json:"nextRetryAt"` // unix ms — due time for the next attempt
	CreatedAtMs int64  `json:"createdAtMs"` // unix ms — enqueue time
}

// Outbox persists agent replies as <dir>/<id>.json and redelivers them
// with exponential backoff until the bus accepts them.
//
// Durability contract: the entry is on disk BEFORE the first publish
// attempt and removed only after a confirmed success — a crash anywhere
// in between can duplicate a delivery, never lose one. The persisted
// reply is regenerated on each attempt (fresh envelope id/signature), so
// only the logical content is durable.
type Outbox struct {
	mu      sync.Mutex
	dir     string
	log     *slog.Logger
	publish func(PublishRequest) error

	flushInterval time.Duration
	baseDelay     time.Duration
	maxDelay      time.Duration
	maxRetries    int
	maxAge        time.Duration

	stopCh   chan struct{}
	stopOnce sync.Once
}

// NewOutbox builds an outbox over dir. publish is the delivery function
// invoked for every attempt (the Service binds it to Publish).
func NewOutbox(dir string, log *slog.Logger, publish func(PublishRequest) error) *Outbox {
	if log == nil {
		log = slog.Default()
	}
	return &Outbox{
		dir:           dir,
		log:           log,
		publish:       publish,
		flushInterval: outboxFlushInterval,
		baseDelay:     outboxBaseDelay,
		maxDelay:      outboxMaxDelay,
		maxRetries:    outboxMaxRetries,
		maxAge:        outboxMaxAge,
		stopCh:        make(chan struct{}),
	}
}

func (ob *Outbox) path(id string) string {
	return filepath.Join(ob.dir, id+".json")
}

// Enqueue persists the reply before any publish attempt.
func (ob *Outbox) Enqueue(req PublishRequest) (*OutboxEntry, error) {
	now := time.Now().UnixMilli()
	entry := &OutboxEntry{
		ID:          randID(),
		TargetNode:  req.TargetNode,
		Type:        req.Type,
		SessionKey:  req.SessionKey,
		Payload:     req.Message,
		NextRetryAt: now, // due immediately
		CreatedAtMs: now,
	}
	ob.mu.Lock()
	defer ob.mu.Unlock()
	if err := ob.writeLocked(entry); err != nil {
		return nil, err
	}
	return entry, nil
}

// Remove deletes the entry file (ENOENT tolerated).
func (ob *Outbox) Remove(id string) {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	_ = os.Remove(ob.path(id))
}

// Snapshot returns all pending entries.
func (ob *Outbox) Snapshot() []OutboxEntry {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	return ob.scanLocked()
}

func (ob *Outbox) scanLocked() []OutboxEntry {
	files, err := filepath.Glob(filepath.Join(ob.dir, "*.json"))
	if err != nil {
		return nil
	}
	out := make([]OutboxEntry, 0, len(files))
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var e OutboxEntry
		if json.Unmarshal(b, &e) != nil || e.ID == "" {
			// A corrupt entry can never be delivered; drop it loudly.
			ob.log.Warn("outbox entry unreadable, dropping", logging.MessageID(filepath.Base(p)))
			_ = os.Remove(p)
			continue
		}
		out = append(out, e)
	}
	return out
}

func (ob *Outbox) writeLocked(e *OutboxEntry) error {
	if err := os.MkdirAll(ob.dir, 0o755); err != nil {
		return err
	}
	return store.WriteJSONAtomic(ob.path(e.ID), e, 0o600)
}

// attempt publishes one entry: on success it is removed; on failure the
// retry counter/backoff advance, or it dead-letters past the budget.
func (ob *Outbox) attempt(e OutboxEntry) {
	err := ob.publish(PublishRequest{
		TargetNode: e.TargetNode,
		Type:       e.Type,
		SessionKey: e.SessionKey,
		Message:    e.Payload,
	})

	ob.mu.Lock()
	defer ob.mu.Unlock()
	if err == nil {
		_ = os.Remove(ob.path(e.ID))
		ob.log.Info("outbox reply delivered",
			logging.Event("message.reply.ok"),
			logging.To(e.TargetNode),
			logging.MessageID(e.ID),
			"retries", e.RetryCount,
		)
		return
	}

	e.RetryCount++
	age := time.Since(time.UnixMilli(e.CreatedAtMs))
	if e.RetryCount >= ob.maxRetries || age > ob.maxAge {
		_ = os.Remove(ob.path(e.ID))
		ob.log.Error("outbox entry dead-lettered",
			logging.To(e.TargetNode),
			logging.MessageID(e.ID),
			"retries", e.RetryCount,
			"age", age.String(),
			logging.Error(err),
		)
		return
	}
	e.NextRetryAt = time.Now().Add(ob.delayFor(e.RetryCount)).UnixMilli()
	if werr := ob.writeLocked(&e); werr != nil {
		ob.log.Error("outbox retry persist failed", logging.Error(werr))
	}
}

// delayFor returns the wait before retry number retryCount (1-based):
// base × 2^(n-1), capped at maxDelay.
func (ob *Outbox) delayFor(retryCount int) time.Duration {
	if retryCount < 1 {
		retryCount = 1
	}
	d := ob.baseDelay << uint(retryCount-1)
	if d <= 0 || d > ob.maxDelay {
		return ob.maxDelay
	}
	return d
}

// flushOnce delivers every due entry. Called by the background flusher
// and directly by tests.
func (ob *Outbox) flushOnce() {
	now := time.Now().UnixMilli()
	for _, e := range ob.Snapshot() {
		if e.NextRetryAt > now {
			continue
		}
		ob.attempt(e)
	}
}

// Start flushes due entries immediately (startup recovery: entries
// persisted by a previous process redeliver right away) then keeps a
// background flusher running until Close.
func (ob *Outbox) Start() {
	ob.flushOnce()
	go func() {
		t := time.NewTicker(ob.flushInterval)
		defer t.Stop()
		for {
			select {
			case <-ob.stopCh:
				return
			case <-t.C:
				ob.flushOnce()
			}
		}
	}()
}

// Close stops the background flusher.
func (ob *Outbox) Close() {
	ob.stopOnce.Do(func() { close(ob.stopCh) })
}

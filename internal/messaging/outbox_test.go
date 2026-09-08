package messaging

import (
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"clawsynapse/internal/protocol"
)

// ── T2.7 acceptance: Outbox reliable return path ─────────────────────

func newTestOutbox(t *testing.T, dir string, publish func(PublishRequest) error) *Outbox {
	t.Helper()
	ob := NewOutbox(dir, slog.Default(), publish)
	// Fast tunables for tests; production defaults live in the constants.
	ob.flushInterval = 10 * time.Millisecond
	ob.baseDelay = 5 * time.Millisecond
	ob.maxDelay = 20 * time.Millisecond
	ob.maxRetries = 10
	ob.maxAge = time.Hour
	return ob
}

func testOutboxReq() PublishRequest {
	return PublishRequest{TargetNode: "n2", Type: "todo.response", SessionKey: "sk-1", Message: "done"}
}

// TestOutbox_RedeliversUntilSuccess: publish fails twice then succeeds →
// the reply is eventually delivered and the outbox is empty.
func TestOutbox_RedeliversUntilSuccess(t *testing.T) {
	var attempts atomic.Int32
	ob := newTestOutbox(t, t.TempDir(), func(PublishRequest) error {
		if attempts.Add(1) <= 2 {
			return errors.New("bus down")
		}
		return nil
	})

	if _, err := ob.Enqueue(testOutboxReq()); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	ob.flushOnce() // fail #1
	if got := len(ob.Snapshot()); got != 1 {
		t.Fatalf("entries after fail #1 = %d, want 1", got)
	}
	time.Sleep(10 * time.Millisecond) // let the 5ms backoff come due
	ob.flushOnce()                    // fail #2
	if got := len(ob.Snapshot()); got != 1 {
		t.Fatalf("entries after fail #2 = %d, want 1", got)
	}
	if e := ob.Snapshot()[0]; e.RetryCount != 2 {
		t.Fatalf("retryCount = %d, want 2", e.RetryCount)
	}

	time.Sleep(10 * time.Millisecond)
	ob.flushOnce() // success
	if got := len(ob.Snapshot()); got != 0 {
		t.Fatalf("entries after success = %d, want 0 (outbox should be empty)", got)
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d, want 3", attempts.Load())
	}
}

// TestOutbox_DeadLettersAfterMaxRetries: permanently failing publishes
// dead-letter (entry removed) once the retry budget is exhausted.
func TestOutbox_DeadLettersAfterMaxRetries(t *testing.T) {
	var attempts atomic.Int32
	ob := newTestOutbox(t, t.TempDir(), func(PublishRequest) error {
		attempts.Add(1)
		return errors.New("forever down")
	})
	ob.maxRetries = 3

	if _, err := ob.Enqueue(testOutboxReq()); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	for i := 0; i < 3; i++ {
		if i > 0 {
			time.Sleep(10 * time.Millisecond) // let the backoff come due
		}
		ob.flushOnce()
	}

	if got := len(ob.Snapshot()); got != 0 {
		t.Fatalf("entries after dead letter = %d, want 0", got)
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d, want 3", attempts.Load())
	}
}

// TestOutbox_BackoffGrowsAndCaps: consecutive failures double the wait
// (30s → 1m → 2m …) up to the 10m cap. Checked against delayFor directly
// — deterministic, no real waiting.
func TestOutbox_BackoffGrowsAndCaps(t *testing.T) {
	ob := newTestOutbox(t, t.TempDir(), func(PublishRequest) error { return errors.New("down") })
	ob.baseDelay = 30 * time.Second
	ob.maxDelay = 10 * time.Minute

	wantDelays := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute}
	for retry, want := range wantDelays {
		if got := ob.delayFor(retry + 1); got != want {
			t.Fatalf("retry %d: backoff = %v, want %v", retry+1, got, want)
		}
	}
}

// TestOutbox_RestartRecoversPendingEntries: an entry persisted by a
// previous process is redelivered by a fresh Outbox over the same dir.
func TestOutbox_RestartRecoversPendingEntries(t *testing.T) {
	dir := t.TempDir()

	ob1 := newTestOutbox(t, dir, func(PublishRequest) error { return errors.New("down") })
	if _, err := ob1.Enqueue(testOutboxReq()); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	ob1.flushOnce() // fail → stays on disk
	if got := len(ob1.Snapshot()); got != 1 {
		t.Fatalf("entries before restart = %d, want 1", got)
	}

	var attempts atomic.Int32
	ob2 := newTestOutbox(t, dir, func(PublishRequest) error {
		attempts.Add(1)
		return nil
	})
	time.Sleep(10 * time.Millisecond) // let NextRetryAt come due
	ob2.flushOnce()                   // startup recovery
	if got := len(ob2.Snapshot()); got != 0 {
		t.Fatalf("entries after recovery = %d, want 0", got)
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts after recovery = %d, want 1", attempts.Load())
	}
}

// TestReplyToSender_OutboxPath: replyToSender persists before attempting;
// a failed immediate attempt leaves the entry for the flusher, a
// successful one removes it.
func TestReplyToSender_OutboxPath(t *testing.T) {
	var attempts atomic.Int32
	s := NewService(slog.Default(), nil, nil, "n1", nil, "open", nil)
	s.outbox = newTestOutbox(t, t.TempDir(), func(PublishRequest) error {
		attempts.Add(1)
		if attempts.Load() == 1 {
			return errors.New("bus down")
		}
		return nil
	})

	env := protocol.MessageEnvelope{ID: "m1", Type: "todo.assigned", From: "n2", SessionKey: "sk-1"}

	// First reply: immediate publish fails → entry persisted.
	s.replyToSender(env, "work done", false)
	if got := len(s.outbox.Snapshot()); got != 1 {
		t.Fatalf("entries after failed reply = %d, want 1", got)
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts = %d, want 1", attempts.Load())
	}

	// Flusher redelivers (after the 5ms backoff comes due).
	time.Sleep(10 * time.Millisecond)
	s.outbox.flushOnce()
	if got := len(s.outbox.Snapshot()); got != 0 {
		t.Fatalf("entries after flush = %d, want 0", got)
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2", attempts.Load())
	}

	// Second reply: immediate publish succeeds → no residue.
	s.replyToSender(env, "another done", false)
	if got := len(s.outbox.Snapshot()); got != 0 {
		t.Fatalf("entries after successful reply = %d, want 0", got)
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d, want 3", attempts.Load())
	}
}

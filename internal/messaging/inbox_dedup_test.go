package messaging

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"clawsynapse/internal/discovery"
	"clawsynapse/internal/identity"
	"clawsynapse/internal/protocol"
	"clawsynapse/internal/replay"
	"clawsynapse/internal/store"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func marshalEnvelope(env protocol.MessageEnvelope) ([]byte, error) {
	return json.Marshal(env)
}

// newDedupTestService builds an open-trust service whose inbox deliveries
// are observable through a channel, with the shared replay guard enabled.
func newDedupTestService(t *testing.T) (*Service, chan string) {
	t.Helper()
	peers := discovery.NewRegistry()
	base := t.TempDir()
	id, err := identity.LoadOrCreate(base+"/identity.key", base+"/identity.pub")
	if err != nil {
		t.Fatalf("identity init failed: %v", err)
	}

	delivered := make(chan string, 10)
	svc := NewService(discardLogger(), peers, nil, "node-alpha", id, "open", nil)
	svc.SetMessageHandler(MessageHandlerFunc(func(msg IncomingMessage) (HandlerResult, error) {
		delivered <- msg.Message
		return HandlerResult{Reply: "ok"}, nil
	}))
	svc.EnableReplayGuard(newTestReplayGuard(t))
	return svc, delivered
}

func newTestReplayGuard(t *testing.T) *replay.ReplayGuard {
	t.Helper()
	fs := store.NewFSStore(t.TempDir())
	if err := fs.EnsureLayout(); err != nil {
		t.Fatalf("EnsureLayout: %v", err)
	}
	g, err := replay.NewReplayGuard(fs, 1000, 10*time.Minute)
	if err != nil {
		t.Fatalf("NewReplayGuard: %v", err)
	}
	return g
}

// TestInboxDedupDropsDuplicateEnvelope (T2.7 acceptance, T2.2): the same
// env.ID delivered twice reaches the handler once; a different id is
// unaffected. TTL-expiry re-entry is covered by the replay package tests
// (TestReplayGuardTTLExpiryAllowsReentry) since the inbox TTL is fixed.
func TestInboxDedupDropsDuplicateEnvelope(t *testing.T) {
	svc, delivered := newDedupTestService(t)

	first := protocol.MessageEnvelope{ID: "env-1", Type: "chat.message", Content: "hello"}
	again := protocol.MessageEnvelope{ID: "env-1", Type: "chat.message", Content: "hello"}
	other := protocol.MessageEnvelope{ID: "env-2", Type: "chat.message", Content: "world"}

	for _, env := range []protocol.MessageEnvelope{first, again, other} {
		data, err := marshalEnvelope(env)
		if err != nil {
			t.Fatalf("marshal envelope: %v", err)
		}
		svc.handleInbox("clawsynapse.msg.node-alpha.inbox", data)
	}

	deadline := time.After(1 * time.Second)
	got := map[string]int{}
	for i := 0; i < 2; i++ {
		select {
		case msg := <-delivered:
			got[msg]++
		case <-deadline:
			t.Fatalf("expected 2 deliveries (env-1 once + env-2), got %v", got)
		}
	}
	if got["hello"] != 1 {
		t.Fatalf("duplicate env-1 delivered %d times, want exactly 1", got["hello"])
	}
	if got["world"] != 1 {
		t.Fatalf("env-2 delivered %d times, want 1", got["world"])
	}
	select {
	case msg := <-delivered:
		t.Fatalf("unexpected extra delivery: %q", msg)
	default:
	}
}

// TestInboxDedupDisabledWithoutGuard: legacy behavior — a nil guard must
// not block duplicate deliveries (existing tests and deployments that have
// not opted in keep working).
func TestInboxDedupDisabledWithoutGuard(t *testing.T) {
	peers := discovery.NewRegistry()
	base := t.TempDir()
	id, err := identity.LoadOrCreate(base+"/identity.key", base+"/identity.pub")
	if err != nil {
		t.Fatalf("identity init failed: %v", err)
	}

	delivered := make(chan string, 10)
	svc := NewService(discardLogger(), peers, nil, "node-alpha", id, "open", nil)
	svc.SetMessageHandler(MessageHandlerFunc(func(msg IncomingMessage) (HandlerResult, error) {
		delivered <- msg.Message
		return HandlerResult{Reply: "ok"}, nil
	}))

	env := protocol.MessageEnvelope{ID: "env-x", Type: "chat.message", Content: "same"}
	for i := 0; i < 2; i++ {
		data, err := marshalEnvelope(env)
		if err != nil {
			t.Fatalf("marshal envelope: %v", err)
		}
		svc.handleInbox("clawsynapse.msg.node-alpha.inbox", data)
	}

	deadline := time.After(1 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-delivered:
		case <-deadline:
			t.Fatalf("delivery %d missing without guard (legacy behavior broken)", i+1)
		}
	}
}

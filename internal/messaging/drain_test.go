package messaging

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"clawsynapse/internal/protocol"
)

// protocolEnv marshals a chat envelope (unique id per call) as raw inbox
// payload, mirroring what a peer publishes onto the wire.
func protocolEnv(t *testing.T, id, content, sessionKey string) []byte {
	t.Helper()
	data, err := json.Marshal(protocol.MessageEnvelope{
		ID:         id,
		Type:       "chat.message",
		From:       "node-beta",
		To:         "node-alpha",
		Content:    content,
		SessionKey: sessionKey,
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return data
}

// newDrainTestService: open-trust service with outbox wired (bus nil so
// every reply attempt fails and stays observable in the outbox snapshot).
func newDrainTestService(t *testing.T) (*Service, chan struct{}, chan struct{}) {
	t.Helper()
	svc, _ := newDedupTestService(t)
	svc.EnableOutbox(t.TempDir())
	entered := make(chan struct{}, 1) // handler signals it started
	gate := make(chan struct{})      // test controls handler completion
	svc.SetMessageHandler(MessageHandlerFunc(func(msg IncomingMessage) (HandlerResult, error) {
		entered <- struct{}{}
		<-gate
		return HandlerResult{}, nil
	}))
	return svc, entered, gate
}

// TestDrainWaitsForInFlight (T2.6): Drain returns only after the in-flight
// handler finished, and no interruption reply is sent.
func TestDrainWaitsForInFlight(t *testing.T) {
	svc, entered, gate := newDrainTestService(t)

	go svc.handleInbox("clawsynapse.msg.node-alpha.inbox", protocolEnv(t, "drain-wait-1", "hello", "sess-1"))
	<-entered

	drained := make(chan struct{})
	go func() {
		svc.Drain(context.Background()) // generous budget: must not fire .error
		close(drained)
	}()

	// Give Drain a beat; it must still be waiting while the handler runs.
	select {
	case <-drained:
		t.Fatal("Drain returned while the handler was still running")
	case <-time.After(200 * time.Millisecond):
	}

	close(gate) // let the handler finish
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("Drain did not return after the in-flight handler finished")
	}
	for _, e := range svc.outbox.Snapshot() {
		if strings.HasSuffix(e.Type, ".error") {
			t.Fatalf("unexpected interruption reply: %+v", e)
		}
	}
}

// TestDrainTimeoutInterruptsPending (T2.6): a handler still running when
// the grace expires gets an interruption .error reply through the outbox.
func TestDrainTimeoutInterruptsPending(t *testing.T) {
	svc, entered, gate := newDrainTestService(t)
	defer close(gate) // unblock the handler so the goroutine exits

	go svc.handleInbox("clawsynapse.msg.node-alpha.inbox", protocolEnv(t, "drain-t/o-1", "hello", "sess-1"))
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	svc.Drain(ctx)

	entries := svc.outbox.Snapshot()
	found := false
	for _, e := range entries {
		if strings.HasSuffix(e.Type, ".error") && strings.Contains(e.Payload, "node shutting down, task interrupted") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an interruption .error in the outbox, got %+v", entries)
	}
}

// TestDrainingRejectsNewDeliveries (T2.6): after Drain, new inbox payloads
// are Nak'd (handleInbox errors) so the durable inbox redelivers them
// after restart, and dispatch is refused.
func TestDrainingRejectsNewDeliveries(t *testing.T) {
	svc, _, gate := newDrainTestService(t)
	defer close(gate)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	svc.Drain(ctx)

	err := svc.handleInbox("clawsynapse.msg.node-alpha.inbox", protocolEnv(t, "drain-late-1", "late", "sess-1"))
	if err == nil {
		t.Fatal("handleInbox accepted a delivery while draining, want Nak error")
	}
	if svc.dispatchSession(protocol.MessageEnvelope{Type: "chat.message", SessionKey: "s"}, func() {}) {
		t.Fatal("dispatchSession accepted work while draining")
	}
}

package natsbus

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// TestSubscribeDurableRedelivery runs only against a JetStream-enabled
// NATS server: CLAWSYNAPSE_NATS_TEST_URL=nats://127.0.0.1:4222 go test ./internal/natsbus/ -run TestSubscribeDurable -v
//
// Covers the T2.5 acceptance points:
//  1. Nak → immediate redelivery (handler error path)
//  2. messages published while the consumer is offline are held by the
//     durable consumer and delivered on re-attach
func TestSubscribeDurableRedelivery(t *testing.T) {
	url := testURL(t)

	durable := fmt.Sprintf("inbox-test-%d", time.Now().UnixNano())
	subject := "clawsynapse.msg.durabletest-" + durable + ".inbox"
	defer func() { cleanupConsumer(t, url, durable) }()

	var mu sync.Mutex
	seen := map[string]int{}
	consumerA, err := Connect(testCtx(t), []string{url}, "durable-test-consumer")
	if err != nil {
		t.Fatalf("connect consumer: %v", err)
	}
	defer consumerA.Close()

	sub, err := consumerA.SubscribeDurable(subject, durable, func(_ string, data []byte) error {
		var marker string
		if err := json.Unmarshal(data, &marker); err != nil {
			return nil // not ours; ack
		}
		mu.Lock()
		seen[marker]++
		attempts := seen[marker]
		mu.Unlock()
		if marker == "m2" && attempts == 1 {
			return fmt.Errorf("nak m2 once") // T2.5: handler error → Nak
		}
		return nil
	})
	if err != nil {
		t.Fatalf("SubscribeDurable: %v", err)
	}
	if !sub.Durable() {
		t.Fatal("expected JetStream durable subscription, got core fallback")
	}

	publisher, err := Connect(testCtx(t), []string{url}, "durable-test-publisher")
	if err != nil {
		t.Fatalf("connect publisher: %v", err)
	}
	defer publisher.Close()

	for _, m := range []string{"m1", "m2"} {
		if err := publisher.PublishJSON(subject, m); err != nil {
			t.Fatalf("publish %s: %v", m, err)
		}
	}
	waitFor(t, &mu, seen, "m1", 1)
	waitFor(t, &mu, seen, "m2", 1)
	waitFor(t, &mu, seen, "m2", 2) // Nak → immediate redelivery → acked

	// Offline retention: consumer down, message published, consumer
	// re-attaches with the same durable name and must receive it.
	consumerA.Close()
	time.Sleep(500 * time.Millisecond)
	if err := publisher.PublishJSON(subject, "m3"); err != nil {
		t.Fatalf("publish m3 while offline: %v", err)
	}

	consumerA2, err := Connect(testCtx(t), []string{url}, "durable-test-consumer-2")
	if err != nil {
		t.Fatalf("reconnect consumer: %v", err)
	}
	defer consumerA2.Close()
	if _, err := consumerA2.SubscribeDurable(subject, durable, func(_ string, data []byte) error {
		var marker string
		if err := json.Unmarshal(data, &marker); err != nil {
			return nil
		}
		mu.Lock()
		seen[marker]++
		mu.Unlock()
		return nil
	}); err != nil {
		t.Fatalf("re-attach durable: %v", err)
	}
	waitFor(t, &mu, seen, "m3", 1)
}

// TestSubscribeDurableFallbackNoJS: against a NATS server without
// JetStream the durable subscribe must degrade to a plain core
// subscription (Durable()==false) and still deliver messages — legacy
// behavior preserved (T2.5 backward compat).
func TestSubscribeDurableFallbackNoJS(t *testing.T) {
	url := os.Getenv("CLAWSYNAPSE_NATS_TEST_NOJS_URL")
	if url == "" {
		t.Skip("CLAWSYNAPSE_NATS_TEST_NOJS_URL not set; skipping no-JS fallback test")
	}

	c, err := Connect(testCtx(t), []string{url}, "fallback-test")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()

	got := make(chan string, 4)
	sub, err := c.SubscribeDurable("clawsynapse.msg.fallbacktest.inbox", "inbox-fallback-test",
		func(_ string, data []byte) error {
			got <- string(data)
			return nil
		})
	if err != nil {
		t.Fatalf("SubscribeDurable: %v", err)
	}
	if sub.Durable() {
		t.Fatal("expected core fallback on a no-JS server, got durable")
	}
	if err := c.PublishJSON("clawsynapse.msg.fallbacktest.inbox", "ping"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case msg := <-got:
		if msg != `"ping"` {
			t.Fatalf("fallback received %q, want %q", msg, `"ping"`)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fallback subscription did not deliver the message")
	}
}

func waitFor(t *testing.T, mu *sync.Mutex, seen map[string]int, key string, want int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := seen[key]
		mu.Unlock()
		if got >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q seen=%d want=%d (seen=%v)", key, seen[key], want, seen)
}

func cleanupConsumer(t *testing.T, url, durable string) {
	t.Helper()
	c, err := Connect(testCtx(t), []string{url}, "durable-test-cleanup")
	if err != nil {
		return
	}
	defer c.Close()
	if js := c.JetStream(); js != nil {
		_ = js.DeleteConsumer("CLAWSYNAPSE", durable)
	}
}

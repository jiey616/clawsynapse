package natsbus

import (
	"context"
	"os"
	"testing"
)

// testURL returns the JetStream NATS server URL for integration tests and
// skips the test when CLAWSYNAPSE_NATS_TEST_URL is not set.
func testURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("CLAWSYNAPSE_NATS_TEST_URL")
	if url == "" {
		t.Skip("CLAWSYNAPSE_NATS_TEST_URL not set; skipping JetStream integration test")
	}
	return url
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

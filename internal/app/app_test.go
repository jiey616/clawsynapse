package app

import (
	"testing"
	"time"

	"clawsynapse/internal/config"
)

func TestResolveAgentAdapterTimeout(t *testing.T) {
	got, err := resolveAgentAdapterTimeout(config.Config{})
	if err != nil {
		t.Fatalf("resolveAgentAdapterTimeout failed: %v", err)
	}
	if got != 10*time.Minute {
		t.Fatalf("default timeout = %s, want 10m", got)
	}

	got, err = resolveAgentAdapterTimeout(config.Config{AgentAdapterTimeout: "45s"})
	if err != nil {
		t.Fatalf("resolveAgentAdapterTimeout failed: %v", err)
	}
	if got != 45*time.Second {
		t.Fatalf("configured timeout = %s, want 45s", got)
	}
}

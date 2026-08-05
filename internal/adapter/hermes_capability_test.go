package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// capabilityFakeGateway mocks the read endpoints used by Capabilities():
// /v1/skills, /api/jobs, /health. It counts hits so cache behavior is testable.
type capabilityFakeGateway struct {
	mu        sync.Mutex
	skillsHit int
	jobsHit   int
}

func (fg *capabilityFakeGateway) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})

	mux.HandleFunc("/v1/skills", func(w http.ResponseWriter, r *http.Request) {
		fg.mu.Lock()
		fg.skillsHit++
		fg.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"name": "tm-task-plan", "description": "plan tasks", "category": "task"},
				{"name": "tm-task-exec", "description": "exec tasks", "category": nil},
			},
		})
	})

	mux.HandleFunc("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
		fg.mu.Lock()
		fg.jobsHit++
		fg.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jobs": []map[string]any{
				{"id": "job-1", "name": "daily", "schedule": "0 9 * * *", "enabled": true, "prompt": "summarize"},
			},
		})
	})

	return mux
}

// newCapabilityTestAdapter builds a HermesAdapter pointing at the fake gateway
// with a temp hermes config.yaml containing one custom provider.
func newCapabilityTestAdapter(t *testing.T, fg *capabilityFakeGateway) (*HermesAdapter, string) {
	t.Helper()
	srv := httptest.NewServer(fg.handler())
	t.Cleanup(srv.Close)

	home := t.TempDir()
	cfgPath := filepath.Join(home, "config.yaml")
	cfgYAML := `model:
  default: agnes-2.0-flash
  provider: agnes
custom_providers:
  - name: agnes
    base_url: https://apihub.agnes-ai.com/v1
    api_key: sk-secret
    model: agnes-2.0-flash
`
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	a, err := NewHermesAdapter(HermesConfig{
		NodeID:     "n1",
		BaseURL:    srv.URL + "/v1",
		Model:      "hermes-agent",
		ConfigPath: cfgPath,
	})
	if err != nil {
		t.Fatalf("NewHermesAdapter failed: %v", err)
	}
	return a, cfgPath
}

func TestHermesCapabilitiesReadsSkillsModelsJobs(t *testing.T) {
	fg := &capabilityFakeGateway{}
	a, _ := newCapabilityTestAdapter(t, fg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := a.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if !res.Available {
		t.Fatalf("Available = false, want true (reason: %s)", res.Reason)
	}
	if res.Product != "hermes" {
		t.Errorf("Product = %q, want hermes", res.Product)
	}
	if len(res.Skills) != 2 || res.Skills[0].Name != "tm-task-plan" {
		t.Errorf("Skills = %+v, want 2 entries starting with tm-task-plan", res.Skills)
	}
	if len(res.Jobs) != 1 || res.Jobs[0].ID != "job-1" {
		t.Errorf("Jobs = %+v, want 1 entry job-1", res.Jobs)
	}
	// Models come from config.yaml custom_providers.
	if len(res.Models) != 1 {
		t.Fatalf("Models = %+v, want 1 entry", res.Models)
	}
	m := res.Models[0]
	if m.Provider != "agnes" || m.Model != "agnes-2.0-flash" || !m.IsDefault {
		t.Errorf("Model = %+v, want agnes/agnes-2.0-flash default", m)
	}
}

func TestHermesCapabilitiesCachedWithinTTL(t *testing.T) {
	fg := &capabilityFakeGateway{}
	a, _ := newCapabilityTestAdapter(t, fg)

	ctx := context.Background()
	if _, err := a.Capabilities(ctx); err != nil {
		t.Fatalf("first Capabilities: %v", err)
	}
	if _, err := a.Capabilities(ctx); err != nil {
		t.Fatalf("second Capabilities: %v", err)
	}

	fg.mu.Lock()
	skillsHit := fg.skillsHit
	jobsHit := fg.jobsHit
	fg.mu.Unlock()

	if skillsHit != 1 {
		t.Errorf("skills fetched %d times, want 1 (cache not effective)", skillsHit)
	}
	if jobsHit != 1 {
		t.Errorf("jobs fetched %d times, want 1 (cache not effective)", jobsHit)
	}
}

func TestHermesCapabilitiesGatewayDownDegrades(t *testing.T) {
	// Point the adapter at a closed port: gateway unreachable → available:false.
	home := t.TempDir()
	cfgPath := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("model:\n  default: x\n  provider: y\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	a, err := NewHermesAdapter(HermesConfig{
		NodeID:     "n1",
		BaseURL:    "http://127.0.0.1:1/v1", // nothing listens on port 1
		Model:      "hermes-agent",
		ConfigPath: cfgPath,
	})
	if err != nil {
		t.Fatalf("NewHermesAdapter failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	res, err := a.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if res.Available {
		t.Error("Available = true, want false when gateway is down")
	}
	if res.Reason == "" {
		t.Error("Reason is empty, want an explanation")
	}
}

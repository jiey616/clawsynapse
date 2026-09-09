package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"strings"

	"clawsynapse/internal/adapter"
	"clawsynapse/internal/discovery"
	"clawsynapse/internal/obs"
	"clawsynapse/pkg/types"
)

// statsStub wraps the existing stub adapter with TaskStats support.
type statsStub struct {
	stubAgentAdapter
	stats adapter.TaskStats
}

func (s statsStub) TaskStats() adapter.TaskStats { return s.stats }

func TestHandleHealthDetailedIncludesTaskStats(t *testing.T) {
	peers := discovery.NewRegistry()
	srv := &Server{
		peers:       peers,
		adapter:     statsStub{stats: adapter.TaskStats{Available: true, InFlight: 2, QueueDepth: 3, MaxConcurrent: 4, AdmittedTotal: 7}},
		adapterName: "hermes",
		version:     "vtest",
		self:        SelfInfo{NodeID: "n1-self", DID: "did:key:z6Mkself", TrustMode: "tofu"},
		startedAt:   time.Now().Add(-90 * time.Second),
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/health/detailed", nil)
	rec := httptest.NewRecorder()
	srv.handleHealthDetailed(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var result types.APIResult
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	task, ok := result.Data["task"].(map[string]any)
	if !ok {
		t.Fatalf("task data missing: %#v", result.Data["task"])
	}
	if task["inFlight"] != float64(2) || task["queueDepth"] != float64(3) || task["maxConcurrentRuns"] != float64(4) {
		t.Errorf("task stats = %#v, want inFlight 2 queueDepth 3 max 4", task)
	}
	uptime, ok := result.Data["uptimeSeconds"].(float64)
	if !ok || uptime < 89 {
		t.Errorf("uptimeSeconds = %#v, want >= 89", result.Data["uptimeSeconds"])
	}
}

func TestHandleMetricsEmitsGaugesAndCounters(t *testing.T) {
	obs.CounterInc("clawsynapse_api_test_probe_total")
	srv := &Server{
		adapter:     statsStub{stats: adapter.TaskStats{Available: true, InFlight: 1, QueueDepth: 2, MaxConcurrent: 8}},
		adapterName: "hermes",
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	srv.handleMetrics(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct == "" {
		t.Error("Content-Type not set")
	}
	body := rec.Body.String()
	for _, want := range []string{
		"clawsynapse_task_inflight 1",
		"clawsynapse_task_queue_depth 2",
		"clawsynapse_task_max_concurrent_runs 8",
		"# TYPE clawsynapse_api_test_probe_total counter",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics body missing %q", want)
		}
	}
}

func TestHandleMetricsWithoutTaskProvider(t *testing.T) {
	srv := &Server{adapter: stubAgentAdapter{status: &adapter.AgentStatus{Healthy: true}}}
	rec := httptest.NewRecorder()
	srv.handleMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "clawsynapse_task_inflight") {
		t.Error("task gauge emitted without a TaskStatsProvider adapter")
	}
}

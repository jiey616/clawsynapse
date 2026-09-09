package adapter

import (
	"context"
	"errors"
)

type AgentAdapter interface {
	DeliverMessage(ctx context.Context, req DeliverMessageRequest) (*DeliverMessageResult, error)
	GetStatus(ctx context.Context) (*AgentStatus, error)
}

// ErrNoActiveRun is returned by RunCanceller.SteerActive when no run is
// currently in flight for the given task key.
var ErrNoActiveRun = errors.New("no active run for task key")

// RunCanceller is implemented by adapters that support cancelling or
// steering an in-flight task run (runtime type assertion, same pattern as
// CapabilityProvider).
type RunCanceller interface {
	// CancelActive cancels the in-flight execution for taskKey: runs-mode
	// adapters POST /v1/runs/{id}/stop (404/409 tolerated) and then cancel
	// the local execution context. No in-flight run → nil (idempotent).
	CancelActive(ctx context.Context, taskKey string) error
	// SteerActive injects text into the in-flight run; no in-flight run →
	// ErrNoActiveRun.
	SteerActive(ctx context.Context, taskKey string, input string) error
	// ActiveRunID returns the in-flight runID ("" if none).
	ActiveRunID(taskKey string) string
}

type DeliverMessageRequest struct {
	Type       string
	AgentID    string
	SessionKey string
	Message    string
	From       string
	Metadata   map[string]any
	// MessageID is the upstream protocol message id (MessageEnvelope.ID).
	// It seeds the gateway Idempotency-Key so a gateway-restart-window
	// redelivery cannot create a duplicate run/response.
	MessageID string
}

type DeliverMessageResult struct {
	Success   bool
	Accepted  bool
	RunID     string
	SessionID string
	Reply     string
	Error     string
}

type AgentStatus struct {
	Healthy bool
}

// TaskStats is a point-in-time snapshot of task-run coordinator occupancy.
type TaskStats struct {
	Available     bool  `json:"available"`
	InFlight      int   `json:"inFlight"`
	QueueDepth    int   `json:"queueDepth"`
	MaxConcurrent int   `json:"maxConcurrentRuns"`
	AdmittedTotal int64 `json:"admittedTotal"`
}

// TaskStatsProvider is an optional capability interface implemented by
// adapters that expose task-run coordinator statistics (Phase 3.3). The API
// layer type-asserts AgentAdapter against it for /v1/health/detailed and
// /metrics.
type TaskStatsProvider interface {
	TaskStats() TaskStats
}

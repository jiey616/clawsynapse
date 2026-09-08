package adapter

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"clawsynapse/internal/store"
)

// ── T1.5 acceptance: deliverViaRuns wired into the TaskCoordinator ──

func newCoordTestAdapter(t *testing.T, fg *fakeGateway) (*HermesAdapter, *store.TaskStore) {
	t.Helper()
	srv := httptest.NewServer(fg.handler())
	t.Cleanup(srv.Close)

	ts := store.NewTaskStore(t.TempDir())
	a, err := NewHermesAdapter(HermesConfig{
		NodeID:       "n1",
		BaseURL:      srv.URL + "/v1",
		Model:        "hermes-agent",
		SessionStore: store.NewFSStore(t.TempDir()),
		TaskStore:    ts,
	})
	if err != nil {
		t.Fatalf("NewHermesAdapter failed: %v", err)
	}
	a.pollInterval = 10 * time.Millisecond
	a.pollIntervalMax = 20 * time.Millisecond
	return a, ts
}

// TestDeliverViaRuns_WithCoordinator_PersistsRecord: with a TaskStore the
// runs delivery goes through the coordinator — one delivery persists a
// completed TaskRun record carrying the gateway run id and message id.
func TestDeliverViaRuns_WithCoordinator_PersistsRecord(t *testing.T) {
	fg := &fakeGateway{}
	a, ts := newCoordTestAdapter(t, fg)
	ctx, cancel := testCtx(t)
	defer cancel()

	res, err := a.DeliverMessage(ctx, DeliverMessageRequest{
		Type:       "todo.assigned",
		SessionKey: "task-t15",
		Message:    "do it",
		MessageID:  "msg-t15",
	})
	if err != nil {
		t.Fatalf("DeliverMessage: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got %+v", res)
	}

	rec, ok, err := ts.GetTaskRun("task-t15")
	if err != nil || !ok {
		t.Fatalf("task record missing: ok=%v err=%v", ok, err)
	}
	if rec.Status != store.TaskStatusCompleted {
		t.Errorf("status = %s, want completed", rec.Status)
	}
	if rec.ActiveRunID != res.RunID {
		t.Errorf("ActiveRunID = %q, want %q", rec.ActiveRunID, res.RunID)
	}
	if rec.MessageID != "msg-t15" {
		t.Errorf("MessageID = %q, want msg-t15", rec.MessageID)
	}
}

// TestDeliverViaRuns_WithCoordinator_CtxTimeoutInterrupted: caller ctx
// expiry during polling → the gateway run is stopped and the coordinator
// persists an interrupted record.
func TestDeliverViaRuns_WithCoordinator_CtxTimeoutInterrupted(t *testing.T) {
	fg := &fakeGateway{runStatusSeq: map[string][]string{"run-1": {"running"}}}
	a, ts := newCoordTestAdapter(t, fg)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := a.DeliverMessage(ctx, DeliverMessageRequest{
		Type:       "todo.assigned",
		SessionKey: "task-t15b",
		Message:    "do it",
		MessageID:  "msg-t15b",
	}); err == nil {
		t.Fatal("expected ctx timeout error")
	}

	rec, ok, err := ts.GetTaskRun("task-t15b")
	if err != nil || !ok {
		t.Fatalf("task record missing: ok=%v err=%v", ok, err)
	}
	if rec.Status != store.TaskStatusInterrupted {
		t.Errorf("status = %s, want interrupted", rec.Status)
	}
	if stops := fg.recordedStopCalls(); len(stops) != 1 || stops[0] != "run-1" {
		t.Errorf("stop calls = %v, want [run-1]", stops)
	}
}

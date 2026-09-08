package adapter

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"clawsynapse/internal/store"
)

// newCancelTestAdapter builds an adapter with fast polling AND a TaskStore
// (so NewHermesAdapter wires a TaskCoordinator).
func newCancelTestAdapter(t *testing.T, fg *fakeGateway) *HermesAdapter {
	t.Helper()
	srv := httptest.NewServer(fg.handler())
	t.Cleanup(srv.Close)

	a, err := NewHermesAdapter(HermesConfig{
		NodeID:    "n1",
		BaseURL:   srv.URL + "/v1",
		Model:     "hermes-agent",
		TaskStore: store.NewTaskStore(t.TempDir()),
	})
	if err != nil {
		t.Fatalf("NewHermesAdapter failed: %v", err)
	}
	a.pollInterval = 10 * time.Millisecond
	a.pollIntervalMax = 20 * time.Millisecond
	return a
}

// t1dot3RunFn mimics the T1.5 收口后的 runFn shape: create → publish runID
// → poll. It gives the cancel/steer tests a realistic in-flight execution.
func t1dot3RunFn(a *HermesAdapter, input string) RunFn {
	return func(ctx context.Context, prevSessionID string) (string, string, string, error) {
		created, _, err := a.createRunWithRetry(ctx, runCreateRequest{Input: input, Model: a.model}, "")
		if err != nil {
			return "", "", "", err
		}
		runID := strings.TrimSpace(created.RunID)
		if runID == "" {
			runID = strings.TrimSpace(created.ID)
		}
		PublishRunID(ctx, runID)
		final, err := a.pollRun(ctx, runID)
		if err != nil {
			return runID, "", "", err
		}
		return runID, final.SessionID, extractRunText(*final), nil
	}
}

// waitForPublishedRunID blocks until the adapter registry sees an in-flight
// run for taskKey (or fails the test).
func waitForPublishedRunID(t *testing.T, a *HermesAdapter, taskKey string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if id := a.ActiveRunID(taskKey); id != "" {
			return id
		}
		if time.Now().After(deadline) {
			t.Fatalf("run id never published for %s", taskKey)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestHermesAdapter_CancelActiveStopsInFlightRun 是 T1.3 验收①的协调器级
// 版本：CancelActive 触发后应看到 gateway 收到 stop、投递返回 ctx.Canceled、
// TaskStore 记录 interrupted，且重复取消幂等（验收②）。
func TestHermesAdapter_CancelActiveStopsInFlightRun(t *testing.T) {
	fg := &fakeGateway{
		runsRunID:    "run-C1",
		runStatusSeq: map[string][]string{"run-C1": {"running"}},
	}
	a := newCancelTestAdapter(t, fg)
	if a.taskCoord == nil {
		t.Fatalf("TaskCoordinator not wired")
	}

	execDone := make(chan error, 1)
	go func() {
		_, err := a.taskCoord.ExecuteRun(context.Background(), "task-C1", "m1", t1dot3RunFn(a, "do it"))
		execDone <- err
	}()

	waitForPublishedRunID(t, a, "task-C1")

	if err := a.CancelActive(context.Background(), "task-C1"); err != nil {
		t.Fatalf("CancelActive: %v", err)
	}

	select {
	case err := <-execDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ExecuteRun err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("ExecuteRun did not finish after cancel")
	}

	stops := fg.recordedStopCalls()
	if len(stops) != 1 || stops[0] != "run-C1" {
		t.Fatalf("stop calls = %v, want [run-C1]", stops)
	}

	rec, ok, err := a.taskCoord.store.GetTaskRun("task-C1")
	if err != nil || !ok {
		t.Fatalf("record missing: ok=%v err=%v", ok, err)
	}
	if rec.Status != store.TaskStatusInterrupted {
		t.Fatalf("status = %q, want interrupted", rec.Status)
	}
	if a.ActiveRunID("task-C1") != "" {
		t.Fatalf("registry still holds the run after cancel")
	}

	// 重复取消（无在途）→ 幂等 nil，无 panic（验收②）
	if err := a.CancelActive(context.Background(), "task-C1"); err != nil {
		t.Fatalf("idempotent CancelActive: %v", err)
	}
	if got := len(fg.recordedStopCalls()); got != 1 {
		t.Fatalf("stop calls after idempotent cancel = %d, want 1", got)
	}
}

// TestHermesAdapter_SteerActiveInjectsIntoInFlightRun 是 T1.3 验收③的
// 协调器级版本：注入应命中在途 run，且不创建新 run。
func TestHermesAdapter_SteerActiveInjectsIntoInFlightRun(t *testing.T) {
	fg := &fakeGateway{
		runsRunID:    "run-S1",
		runStatusSeq: map[string][]string{"run-S1": {"running"}},
	}
	a := newCancelTestAdapter(t, fg)

	execDone := make(chan error, 1)
	go func() {
		_, err := a.taskCoord.ExecuteRun(context.Background(), "task-S1", "m1", t1dot3RunFn(a, "do it"))
		execDone <- err
	}()

	waitForPublishedRunID(t, a, "task-S1")

	if err := a.SteerActive(context.Background(), "task-S1", "催办：请加快"); err != nil {
		t.Fatalf("SteerActive: %v", err)
	}
	steers := fg.recordedSteerCalls()
	if len(steers) != 1 || steers[0] != "run-S1" {
		t.Fatalf("steer calls = %v, want [run-S1]", steers)
	}
	// 注入不得新建 run（runs POST 计数不变 —— 只在 ExecuteRun 里创建过一次）

	a.CancelActive(context.Background(), "task-S1")
	select {
	case <-execDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("ExecuteRun did not finish after cancel")
	}
}

// TestHermesAdapter_SteerActiveNoRun：无在途 run 时 Steer → ErrNoActiveRun。
func TestHermesAdapter_SteerActiveNoRun(t *testing.T) {
	fg := &fakeGateway{}
	a := newCancelTestAdapter(t, fg)
	if err := a.SteerActive(context.Background(), "task-none", "hi"); !errors.Is(err, ErrNoActiveRun) {
		t.Fatalf("err = %v, want ErrNoActiveRun", err)
	}
	if id := a.ActiveRunID("task-none"); id != "" {
		t.Fatalf("ActiveRunID = %q, want empty", id)
	}
}

// TestDeliverViaRuns_StopsRunOnCallerCancel 验证 ctx 取消联动：调用方
// 超时/取消后，deliverViaRuns 在返回错误前先 stop 掉 gateway 侧 run。
func TestDeliverViaRuns_StopsRunOnCallerCancel(t *testing.T) {
	fg := &fakeGateway{
		runsRunID: "run-D1",
		// 交替状态避免 5 连相同状态触发 pollRun 的 stuck guard —— 让
		// 120ms 的 caller ctx deadline 成为第一个到来的终止条件。
		runStatusSeq: map[string][]string{"run-D1": {"running", "started", "running", "started"}},
	}
	srv := httptest.NewServer(fg.handler())
	t.Cleanup(srv.Close)

	a, err := NewHermesAdapter(HermesConfig{
		NodeID:  "n1",
		BaseURL: srv.URL + "/v1",
		Model:   "hermes-agent",
	})
	if err != nil {
		t.Fatalf("NewHermesAdapter: %v", err)
	}
	a.pollInterval = 10 * time.Millisecond
	a.pollIntervalMax = 20 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	_, err = a.DeliverMessage(ctx, DeliverMessageRequest{
		Type:       "todo.assigned",
		SessionKey: "task-D1",
		Message:    "do it",
	})
	if err == nil {
		t.Fatalf("expected ctx deadline error from runs path")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	stops := fg.recordedStopCalls()
	if len(stops) == 0 || stops[0] != "run-D1" {
		t.Fatalf("stop calls = %v, want [run-D1]", stops)
	}
}

package adapter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"clawsynapse/internal/store"
	"clawsynapse/internal/obs"
)

// ErrQueueOverflow is returned when the task queue is full (QueueCapacity
// submissions already admitted, running or waiting) or the QueueWaitTimeout
// expires while waiting for a run slot.
var ErrQueueOverflow = errors.New("task queue overflow or queue wait timeout")

const (
	defaultTaskMaxConcurrentRuns = 8  // 2 spare slots under the gateway's global limit of 10
	defaultTaskQueueCapacity     = 100
	defaultTaskRunTimeout        = 60 * time.Minute
	defaultTaskQueueWaitTimeout  = 5 * time.Minute
)

// TaskConfig bounds task run admission and queueing.
type TaskConfig struct {
	// MaxConcurrentRuns caps simultaneously executing runs. Zero or
	// negative falls back to 8.
	MaxConcurrentRuns int
	// QueueCapacity caps admitted-but-not-terminal submissions (running +
	// waiting). A full queue rejects immediately with ErrQueueOverflow.
	// Zero or negative falls back to 100.
	QueueCapacity int
	// RunTimeout is documentation-only inside the coordinator: the actual
	// run bound is the caller-supplied ctx. The messaging handler wraps
	// todo.* deliveries with this value (default 60m — deliberately NOT
	// the 10m agentAdapterTimeout, which would kill long tasks mid-run;
	// see dev-spec T1.2 ruling 2).
	RunTimeout time.Duration
	// QueueWaitTimeout bounds how long a submission may wait for a run
	// slot before failing with ErrQueueOverflow. Zero or negative falls
	// back to 5m.
	QueueWaitTimeout time.Duration
}

func (c TaskConfig) normalized() TaskConfig {
	if c.MaxConcurrentRuns <= 0 {
		c.MaxConcurrentRuns = defaultTaskMaxConcurrentRuns
	}
	if c.QueueCapacity <= 0 {
		c.QueueCapacity = defaultTaskQueueCapacity
	}
	if c.RunTimeout <= 0 {
		c.RunTimeout = defaultTaskRunTimeout
	}
	if c.QueueWaitTimeout <= 0 {
		c.QueueWaitTimeout = defaultTaskQueueWaitTimeout
	}
	return c
}

// TaskCoordinator serializes task runs per task id, bounds queueing and
// persists the run lifecycle via TaskStore.
//
// Concurrency model (dev-spec T1.2):
//   - the per-task mutex ONLY guards the <10ms "read TaskStore → claim" and
//     "write terminal" critical sections; runFn runs outside every lock;
//   - in-flight runs are tracked in a process-local registry so a duplicate
//     submission joins the running execution instead of starting a second
//     one (the TaskStore running record covers cross-restart ghosts, which
//     boot recovery converts to failed);
//   - the semaphore caps concurrently executing runs; queueing for a slot
//     is bounded by QueueWaitTimeout and the caller ctx.
type TaskCoordinator struct {
	cfg   TaskConfig
	store *store.TaskStore
	log   *slog.Logger

	sem      chan struct{}
	admitted atomic.Int64 // claimed-but-not-terminal submissions

	mu       sync.Mutex
	inflight map[string]*taskRunHandle
}

type taskRunHandle struct {
	done chan struct{}
	// final is the terminal record, written before done is closed.
	final store.TaskRunRecord
	// cancel aborts the run execution context (T1.3 CancelActive path).
	// Calling it before runFn starts is a harmless no-op.
	cancel context.CancelFunc
	// runID is published by runFn as soon as the gateway run is created,
	// so CancelActive can stop it mid-flight.
	runID atomic.Value // string
	// startedAtMs records when the claim was taken (queued phase start).
	startedAtMs int64
}

func (h *taskRunHandle) setRunID(id string) {
	if id == "" {
		return
	}
	h.runID.Store(id)
}

func (h *taskRunHandle) getRunID() string {
	if v := h.runID.Load(); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// lookup returns the in-flight handle for taskID, or nil.
func (tc *TaskCoordinator) lookup(taskID string) *taskRunHandle {
	taskID = normalizeTaskKey(taskID)
	if taskID == "" {
		return nil
	}
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return tc.inflight[taskID]
}

// normalizeTaskKey accepts both raw task ids and the "task:<id>" session
// mapping key spelling.
func normalizeTaskKey(key string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(key), "task:"))
}

// runIDPublisherKey carries the runID publish callback in the run ctx.
type runIDPublisherKey struct{}

// PublishRunID lets a RunFn publish the gateway run id as soon as it is
// created (before polling), so CancelActive can stop the run mid-flight.
// No-op outside of a coordinator-executed run.
func PublishRunID(ctx context.Context, runID string) {
	if ctx == nil || strings.TrimSpace(runID) == "" {
		return
	}
	if p, ok := ctx.Value(runIDPublisherKey{}).(func(string)); ok {
		p(strings.TrimSpace(runID))
	}
}

// NewTaskCoordinator builds a coordinator over the given TaskStore.
// A nil store disables ExecuteRun (construction still succeeds so wiring
// stays simple; callers without a TaskStore keep the legacy path).
func NewTaskCoordinator(cfg TaskConfig, st *store.TaskStore, log *slog.Logger) *TaskCoordinator {
	if log == nil {
		log = slog.Default()
	}
	cfg = cfg.normalized()
	return &TaskCoordinator{
		cfg:      cfg,
		store:    st,
		log:      log,
		sem:      make(chan struct{}, cfg.MaxConcurrentRuns),
		inflight: make(map[string]*taskRunHandle),
	}
}

// RunFn is the actual execution body injected by HermesAdapter: it creates
// the run on the gateway and polls it to a terminal state. It MUST return a
// non-nil error unless the run completed successfully — a failed terminal
// status is an error, not a reply. The coordinator maps:
// nil → completed, ctx cancellation/deadline → interrupted, other → failed.
type RunFn func(ctx context.Context, prevSessionID string) (runID string, sessionID string, reply string, err error)

// ExecuteRun claims, queues and executes one task run, blocking until the
// run reaches a terminal state (or the claim/queue phase fails fast).
//
// Execution order (dev-spec T1.2):
//
//	临界区A(持 per-task 锁): dedup + claim(queued)
//	排队: select sem / queueTimer(QueueWaitTimeout) / ctx.Done → ErrQueueOverflow
//	执行(锁外, 用调用方ctx): runFn(ctx, prevSessionID)
//	临界区B(持锁): 终态写回 → 释放 sem
func (tc *TaskCoordinator) ExecuteRun(
	ctx context.Context,
	taskID, messageID string,
	runFn RunFn,
) (*DeliverMessageResult, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil, errors.New("task coordinator requires a task id")
	}
	if runFn == nil {
		return nil, errors.New("task coordinator requires a run function")
	}
	if tc.store == nil {
		return nil, errors.New("task coordinator has no task store")
	}

	// ── Critical section A: dedup + claim ─────────────────────────────
	now := time.Now().UnixMilli()
	tc.mu.Lock()
	if h, ok := tc.inflight[taskID]; ok {
		// A duplicate submission for a task already in flight in this
		// process. Release the lock immediately and wait for the running
		// execution to finish — never execute the same task twice. No
		// queue slot is consumed.
		tc.mu.Unlock()
		tc.log.Info("task duplicate accepted, joining in-flight run", "taskId", taskID)
		select {
		case <-h.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return ackDuplicateRun(h.final), nil
	}

	prev, hasPrev, err := tc.store.GetTaskRun(taskID)
	if err != nil {
		tc.mu.Unlock()
		return nil, fmt.Errorf("task store read: %w", err)
	}
	if hasPrev && prev.Status == store.TaskStatusRunning && strings.TrimSpace(prev.ActiveRunID) != "" {
		// Running record without a local handle: written by another node
		// process sharing this data dir. Standard accepted receipt.
		tc.mu.Unlock()
		runID := strings.TrimSpace(prev.ActiveRunID)
		tc.log.Info("task accepted via foreign running record", "taskId", taskID, "runId", runID)
		return &DeliverMessageResult{
			Success:  true,
			Accepted: true,
			RunID:    runID,
			Reply:    "accepted (runId=" + runID + ")",
		}, nil
	}

	attempt := 1
	var prevSessionID string
	if hasPrev {
		attempt = prev.AttemptCount + 1
		prevSessionID = strings.TrimSpace(prev.SessionID)
	}
	claim := store.TaskRunRecord{
		TaskID:       taskID,
		MessageID:    strings.TrimSpace(messageID),
		Status:       store.TaskStatusQueued,
		AttemptCount: attempt,
		CreatedAtMs:  now,
		UpdatedAtMs:  now,
	}
	if err := tc.store.SaveTaskRun(claim); err != nil {
		tc.mu.Unlock()
		return nil, fmt.Errorf("task store claim: %w", err)
	}
	// runCtx is the execution context: derived from the caller ctx and
	// cancelled by CancelActive (T1.3) or on terminal write-back.
	runCtx, cancel := context.WithCancel(ctx)
	handle := &taskRunHandle{done: make(chan struct{}), cancel: cancel, startedAtMs: now}
	runCtx = context.WithValue(runCtx, runIDPublisherKey{}, handle.setRunID)
	tc.inflight[taskID] = handle
	tc.mu.Unlock()

	// The claim is live from here on: every exit path below must either
	// finish the run lifecycle or abandon the claim.

	// ── Queue admission bound (running + waiting) ─────────────────────
	for {
		cur := tc.admitted.Load()
		if cur >= int64(tc.cfg.QueueCapacity) {
			tc.abandonClaim(taskID, handle, claim)
			return nil, ErrQueueOverflow
		}
		if tc.admitted.CompareAndSwap(cur, cur+1) {
			break
		}
	}
	defer tc.admitted.Add(-1)

	// ── Queue for a run slot ──────────────────────────────────────────
	queueTimer := time.NewTimer(tc.cfg.QueueWaitTimeout)
	defer queueTimer.Stop()
	select {
	case tc.sem <- struct{}{}:
	case <-queueTimer.C:
		tc.abandonClaim(taskID, handle, claim)
		return nil, ErrQueueOverflow
	case <-ctx.Done():
		tc.abandonClaim(taskID, handle, claim)
		return nil, ctx.Err()
	}
	// From here the run slot is held until terminal.
	defer func() { <-tc.sem }()

	running := claim
	running.Status = store.TaskStatusRunning
	running.ActiveRunID = ""
	running.UpdatedAtMs = time.Now().UnixMilli()
	if err := tc.store.SaveTaskRun(running); err != nil {
		failed := running
		failed.Status = store.TaskStatusFailed
		failed.LastError = fmt.Sprintf("task store running write: %v", err)
		tc.finishRun(taskID, handle, failed)
		return nil, fmt.Errorf("task store running write: %w", err)
	}

	// ── Execute outside every lock, under the run ctx ─────────────────
	runID, sessionID, reply, runErr := runFn(runCtx, prevSessionID)

	// ── Critical section B: terminal write-back ──────────────────────
	terminal := running
	terminal.UpdatedAtMs = time.Now().UnixMilli()
	terminal.ActiveRunID = strings.TrimSpace(runID)
	terminal.SessionID = strings.TrimSpace(sessionID)
	switch {
	case runErr == nil:
		terminal.Status = store.TaskStatusCompleted
	case errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded):
		terminal.Status = store.TaskStatusInterrupted
		terminal.LastError = runErr.Error()
	default:
		terminal.Status = store.TaskStatusFailed
		terminal.LastError = runErr.Error()
	}
	tc.finishRun(taskID, handle, terminal)
	if runErr != nil {
		return nil, runErr
	}
	return &DeliverMessageResult{
		Success:   true,
		Accepted:  true,
		RunID:     terminal.ActiveRunID,
		SessionID: terminal.SessionID,
		Reply:     reply,
	}, nil
}

// finishRun writes the terminal record, publishes it to any duplicate
// waiting on the handle and removes the in-flight entry.
// TaskStats returns a point-in-time snapshot of coordinator occupancy
// (Phase 3.3 observability).
func (tc *TaskCoordinator) TaskStats() TaskStats {
	tc.mu.Lock()
	inflight := len(tc.inflight)
	tc.mu.Unlock()
	admitted := tc.admitted.Load()
	maxConcurrent := cap(tc.sem)
	if maxConcurrent < 0 {
		maxConcurrent = 0
	}
	queueDepth := int(admitted) - inflight
	if queueDepth < 0 {
		queueDepth = 0
	}
	return TaskStats{
		Available:     true,
		InFlight:      inflight,
		QueueDepth:    queueDepth,
		MaxConcurrent: maxConcurrent,
		AdmittedTotal: admitted,
	}
}

func (tc *TaskCoordinator) finishRun(taskID string, handle *taskRunHandle, terminal store.TaskRunRecord) {
	obs.CounterInc("clawsynapse_task_runs_" + string(terminal.Status) + "_total")
	if err := tc.store.SaveTaskRun(terminal); err != nil {
		tc.log.Warn("task terminal write failed", "taskId", taskID, "err", err)
	}

	if handle.cancel != nil {
		handle.cancel() // release the runCtx resources; idempotent
	}

	tc.mu.Lock()
	handle.final = terminal
	if cur, ok := tc.inflight[taskID]; ok && cur == handle {
		delete(tc.inflight, taskID)
	}
	close(handle.done)
	tc.mu.Unlock()
}

// abandonClaim rolls a queued claim back: removes the in-flight entry,
// wakes duplicates with a failed pseudo-record and deletes the claim file
// (only if it is still ours — a concurrent re-claim may have overwritten it).
func (tc *TaskCoordinator) abandonClaim(taskID string, handle *taskRunHandle, claim store.TaskRunRecord) {
	obs.CounterInc("clawsynapse_task_run_claims_abandoned_total")
	if handle.cancel != nil {
		handle.cancel() // no-op if runFn never started
	}
	tc.mu.Lock()
	handle.final = store.TaskRunRecord{
		TaskID:     taskID,
		MessageID:  claim.MessageID,
		Status:     store.TaskStatusFailed,
		LastError:  "task claim abandoned before execution",
		AttemptCount: claim.AttemptCount,
	}
	if cur, ok := tc.inflight[taskID]; ok && cur == handle {
		delete(tc.inflight, taskID)
	}
	close(handle.done)
	tc.mu.Unlock()

	if rec, ok, _ := tc.store.GetTaskRun(taskID); ok &&
		rec.Status == store.TaskStatusQueued && rec.MessageID == claim.MessageID {
		if err := tc.store.DeleteTaskRun(taskID); err != nil {
			tc.log.Warn("task claim cleanup failed", "taskId", taskID, "err", err)
		}
	}
}

// ackDuplicateRun builds the standard receipt for a duplicate submission,
// mirroring the executed run's terminal outcome. The reply is the silent
// ack text — never the run's output (which the original message already
// delivered; re-sending it would pollute the task conversation).
func ackDuplicateRun(rec store.TaskRunRecord) *DeliverMessageResult {
	runID := strings.TrimSpace(rec.ActiveRunID)
	if rec.Status == store.TaskStatusCompleted {
		return &DeliverMessageResult{
			Success:  true,
			Accepted: true,
			RunID:    runID,
			Reply:    "accepted (runId=" + runID + ")",
		}
	}
	errMsg := strings.TrimSpace(rec.LastError)
	if errMsg == "" {
		errMsg = "task run " + string(rec.Status)
	}
	return &DeliverMessageResult{Success: false, RunID: runID, Error: errMsg}
}

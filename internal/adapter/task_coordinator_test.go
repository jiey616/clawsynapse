package adapter

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"clawsynapse/internal/store"
)

func newCoordTestStore(t *testing.T) *store.TaskStore {
	t.Helper()
	st := store.NewTaskStore(t.TempDir())
	if err := st.EnsureLayout(); err != nil {
		t.Fatalf("EnsureLayout failed: %v", err)
	}
	return st
}

// waitForClaimedRecords polls the store until every listed taskID has a
// queued-or-running record, proving those submissions were admitted.
func waitForClaimedRecords(t *testing.T, st *store.TaskStore, taskIDs []string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		pending := 0
		for _, id := range taskIDs {
			rec, ok, err := st.GetTaskRun(id)
			if err != nil {
				t.Fatalf("GetTaskRun(%s): %v", id, err)
			}
			if ok && (rec.Status == store.TaskStatusQueued || rec.Status == store.TaskStatusRunning) {
				continue
			}
			pending++
		}
		if pending == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("submissions not all claimed in time (%d pending)", pending)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestTaskCoordinator_SingleTaskMutex 按验收：10 goroutine 同 taskId 并发提交，
// runFn 执行计数 == 1，其余拿到相同 RunID 的 accepted。
func TestTaskCoordinator_SingleTaskMutex(t *testing.T) {
	st := newCoordTestStore(t)
	tc := NewTaskCoordinator(TaskConfig{MaxConcurrentRuns: 1}, st, nil)

	var execCount atomic.Int64
	const goroutines = 10
	start := make(chan struct{})
	results := make([]*DeliverMessageResult, goroutines)
	errs := make([]error, goroutines)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			<-start
			res, err := tc.ExecuteRun(context.Background(), "task-x", fmt.Sprintf("msg-%d", g),
				func(ctx context.Context, prevSessionID string) (string, string, string, error) {
					execCount.Add(1)
					time.Sleep(30 * time.Millisecond)
					return "run-1", "ses-1", "done", nil
				})
			results[g], errs[g] = res, err
		}(g)
	}
	close(start)
	wg.Wait()

	if got := execCount.Load(); got != 1 {
		t.Fatalf("runFn executed %d times, want exactly 1", got)
	}
	executorReplies, duplicates := 0, 0
	for g, res := range results {
		if errs[g] != nil {
			t.Fatalf("g%d: unexpected error: %v", g, errs[g])
		}
		if res == nil {
			t.Fatalf("g%d: nil result", g)
		}
		if res.RunID != "run-1" {
			t.Fatalf("g%d: RunID = %q, want run-1", g, res.RunID)
		}
		if res.Reply == "done" {
			executorReplies++
		} else if res.Accepted && res.Reply == "accepted (runId=run-1)" {
			duplicates++
		} else {
			t.Fatalf("g%d: unexpected result %+v", g, res)
		}
	}
	if executorReplies != 1 || duplicates != goroutines-1 {
		t.Fatalf("executor replies = %d, duplicates = %d, want 1/%d", executorReplies, duplicates, goroutines-1)
	}

	rec, ok, err := st.GetTaskRun("task-x")
	if err != nil || !ok {
		t.Fatalf("terminal record missing: ok=%v err=%v", ok, err)
	}
	if rec.Status != store.TaskStatusCompleted || rec.AttemptCount != 1 ||
		rec.ActiveRunID != "run-1" || rec.SessionID != "ses-1" {
		t.Fatalf("terminal record mismatch: %+v", rec)
	}
}

// TestTaskCoordinator_ConcurrencyLimit 按验收：20 并发，峰值在途 ≤ 8。
func TestTaskCoordinator_ConcurrencyLimit(t *testing.T) {
	st := newCoordTestStore(t)
	tc := NewTaskCoordinator(TaskConfig{MaxConcurrentRuns: 8, QueueCapacity: 100}, st, nil)

	var inFlight, peak atomic.Int64
	const n = 20
	errCh := make(chan error, n)
	var wg sync.WaitGroup
	for g := 0; g < n; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			_, err := tc.ExecuteRun(context.Background(), fmt.Sprintf("task-%d", g), "m",
				func(ctx context.Context, prevSessionID string) (string, string, string, error) {
					cur := inFlight.Add(1)
					for {
						p := peak.Load()
						if cur <= p || peak.CompareAndSwap(p, cur) {
							break
						}
					}
					time.Sleep(50 * time.Millisecond)
					inFlight.Add(-1)
					return "run-" + fmt.Sprint(g), "ses", "ok", nil
				})
			if err != nil {
				errCh <- fmt.Errorf("g%d: %w", g, err)
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent execution error: %v", err)
	}

	if p := peak.Load(); p > 8 {
		t.Fatalf("peak in-flight = %d, want <= 8", p)
	}
	if p := peak.Load(); p < 2 {
		t.Fatalf("peak in-flight = %d, gate seems to serialize runs (want >= 2)", p)
	}
}

// TestTaskCoordinator_QueueOverflow 按验收：mock runFn 全部阻塞，灌满队列后
// 第 101 条（此处 2 并发 + 5 容量中的第 6 条）→ ErrQueueOverflow。
func TestTaskCoordinator_QueueOverflow(t *testing.T) {
	st := newCoordTestStore(t)
	tc := NewTaskCoordinator(TaskConfig{MaxConcurrentRuns: 2, QueueCapacity: 5, QueueWaitTimeout: time.Hour}, st, nil)

	release := make(chan struct{})
	claimed := []string{"task-0", "task-1", "task-2", "task-3", "task-4"}
	var wg sync.WaitGroup
	for _, id := range claimed {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, _ = tc.ExecuteRun(context.Background(), id, "m",
				func(ctx context.Context, prevSessionID string) (string, string, string, error) {
					<-release
					return "run", "ses", "ok", nil
				})
		}(id)
	}
	waitForClaimedRecords(t, st, claimed)

	_, err := tc.ExecuteRun(context.Background(), "task-overflow", "m",
		func(ctx context.Context, prevSessionID string) (string, string, string, error) {
			return "run", "ses", "ok", nil
		})
	if !errors.Is(err, ErrQueueOverflow) {
		t.Fatalf("err = %v, want ErrQueueOverflow", err)
	}

	// 被拒绝的提交不得留下占位记录
	if _, ok, _ := st.GetTaskRun("task-overflow"); ok {
		t.Fatalf("rejected submission left a claim record")
	}

	close(release)
	wg.Wait()
}

// TestTaskCoordinator_QueueWaitTimeout 按验收：信号量占满，排队超过
// QueueWaitTimeout（测试设 50ms）→ ErrQueueOverflow。
func TestTaskCoordinator_QueueWaitTimeout(t *testing.T) {
	st := newCoordTestStore(t)
	tc := NewTaskCoordinator(TaskConfig{MaxConcurrentRuns: 1, QueueCapacity: 10, QueueWaitTimeout: 50 * time.Millisecond}, st, nil)

	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := tc.ExecuteRun(context.Background(), "task-holder", "m",
			func(ctx context.Context, prevSessionID string) (string, string, string, error) {
				<-release
				return "run", "ses", "ok", nil
			})
		done <- err
	}()
	waitForClaimedRecords(t, st, []string{"task-holder"})

	start := time.Now()
	_, err := tc.ExecuteRun(context.Background(), "task-waiter", "m",
		func(ctx context.Context, prevSessionID string) (string, string, string, error) {
			return "run", "ses", "ok", nil
		})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrQueueOverflow) {
		t.Fatalf("err = %v, want ErrQueueOverflow", err)
	}
	if elapsed < 50*time.Millisecond {
		t.Fatalf("returned after %v, queue wait timeout did not apply", elapsed)
	}
	if _, ok, _ := st.GetTaskRun("task-waiter"); ok {
		t.Fatalf("timed-out submission left a claim record")
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("holder failed: %v", err)
	}
}

// TestTaskCoordinator_CtxCancelWhileQueued：排队期间调用方 ctx 取消 →
// 返回 ctx.Err() 且占位记录被清理。
func TestTaskCoordinator_CtxCancelWhileQueued(t *testing.T) {
	st := newCoordTestStore(t)
	tc := NewTaskCoordinator(TaskConfig{MaxConcurrentRuns: 1, QueueCapacity: 10, QueueWaitTimeout: time.Hour}, st, nil)

	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := tc.ExecuteRun(context.Background(), "task-holder", "m",
			func(ctx context.Context, prevSessionID string) (string, string, string, error) {
				<-release
				return "run", "ses", "ok", nil
			})
		done <- err
	}()
	waitForClaimedRecords(t, st, []string{"task-holder"})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := tc.ExecuteRun(ctx, "task-cancelled", "m",
		func(ctx context.Context, prevSessionID string) (string, string, string, error) {
			return "run", "ses", "ok", nil
		})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if _, ok, _ := st.GetTaskRun("task-cancelled"); ok {
		t.Fatalf("cancelled submission left a claim record")
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("holder failed: %v", err)
	}
}

// TestTaskCoordinator_TerminalStatusMapping：runFn 结果到六态终态的映射
// （nil→completed；ctx 取消/超时→interrupted；其他→failed），
// 以及 AttemptCount 累加与 prevSessionID 续接参数传递。
func TestTaskCoordinator_TerminalStatusMapping(t *testing.T) {
	successFn := func(ctx context.Context, prevSessionID string) (string, string, string, error) {
		return "run-1", "ses-1", "ok", nil
	}

	t.Run("success", func(t *testing.T) {
		st := newCoordTestStore(t)
		tc := NewTaskCoordinator(TaskConfig{}, st, nil)

		var gotPrev string
		fn := func(ctx context.Context, prevSessionID string) (string, string, string, error) {
			gotPrev = prevSessionID
			return successFn(ctx, prevSessionID)
		}
		if _, err := tc.ExecuteRun(context.Background(), "task-a", "m1", fn); err != nil {
			t.Fatalf("first run: %v", err)
		}
		if _, err := tc.ExecuteRun(context.Background(), "task-a", "m2", fn); err != nil {
			t.Fatalf("second run: %v", err)
		}
		if gotPrev != "ses-1" {
			t.Fatalf("prevSessionID = %q, want ses-1", gotPrev)
		}
		rec, ok, _ := st.GetTaskRun("task-a")
		if !ok || rec.Status != store.TaskStatusCompleted || rec.AttemptCount != 2 {
			t.Fatalf("record = %+v ok=%v, want completed attempt=2", rec, ok)
		}
	})

	t.Run("generic error maps to failed", func(t *testing.T) {
		st := newCoordTestStore(t)
		tc := NewTaskCoordinator(TaskConfig{}, st, nil)
		_, err := tc.ExecuteRun(context.Background(), "task-b", "m",
			func(ctx context.Context, prevSessionID string) (string, string, string, error) {
				return "run-1", "", "", errors.New("boom")
			})
		if err == nil {
			t.Fatalf("expected error")
		}
		rec, ok, _ := st.GetTaskRun("task-b")
		if !ok || rec.Status != store.TaskStatusFailed || rec.LastError != "boom" {
			t.Fatalf("record = %+v ok=%v, want failed with LastError=boom", rec, ok)
		}
	})

	t.Run("ctx canceled maps to interrupted", func(t *testing.T) {
		st := newCoordTestStore(t)
		tc := NewTaskCoordinator(TaskConfig{}, st, nil)
		_, err := tc.ExecuteRun(context.Background(), "task-c", "m",
			func(ctx context.Context, prevSessionID string) (string, string, string, error) {
				return "", "", "", context.Canceled
			})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		rec, ok, _ := st.GetTaskRun("task-c")
		if !ok || rec.Status != store.TaskStatusInterrupted {
			t.Fatalf("record = %+v ok=%v, want interrupted", rec, ok)
		}
	})

	t.Run("ctx deadline maps to interrupted", func(t *testing.T) {
		st := newCoordTestStore(t)
		tc := NewTaskCoordinator(TaskConfig{}, st, nil)
		_, err := tc.ExecuteRun(context.Background(), "task-d", "m",
			func(ctx context.Context, prevSessionID string) (string, string, string, error) {
				return "", "", "", context.DeadlineExceeded
			})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
		rec, ok, _ := st.GetTaskRun("task-d")
		if !ok || rec.Status != store.TaskStatusInterrupted {
			t.Fatalf("record = %+v ok=%v, want interrupted", rec, ok)
		}
	})
}

// TestTaskCoordinator_DuplicateSeesFailure：在途任务失败后，等待中的重复
// 提交拿到失败镜像而非 accepted。
func TestTaskCoordinator_DuplicateSeesFailure(t *testing.T) {
	st := newCoordTestStore(t)
	tc := NewTaskCoordinator(TaskConfig{MaxConcurrentRuns: 1}, st, nil)

	release := make(chan struct{})
	holderErr := make(chan error, 1)
	go func() {
		_, err := tc.ExecuteRun(context.Background(), "task-f", "m1",
			func(ctx context.Context, prevSessionID string) (string, string, string, error) {
				<-release
				return "", "", "", errors.New("gateway exploded")
			})
		holderErr <- err
	}()
	waitForClaimedRecords(t, st, []string{"task-f"})

	// 重复提交放后台 goroutine —— 它会阻塞到 holder 终态；
	// 主 goroutine 先放行 holder，避免三方循环等待。
	type dupOutcome struct {
		res *DeliverMessageResult
		err error
	}
	dupCh := make(chan dupOutcome, 1)
	go func() {
		res, err := tc.ExecuteRun(context.Background(), "task-f", "m2",
			func(ctx context.Context, prevSessionID string) (string, string, string, error) {
				return "run", "ses", "ok", nil
			})
		dupCh <- dupOutcome{res: res, err: err}
	}()
	time.Sleep(50 * time.Millisecond)

	close(release)
	if err := <-holderErr; err == nil {
		t.Fatalf("holder should have failed")
	}

	out := <-dupCh
	if out.err != nil {
		t.Fatalf("duplicate: %v", out.err)
	}
	if out.res == nil || out.res.Success || out.res.Error != "gateway exploded" {
		t.Fatalf("duplicate result = %+v, want failure mirror", out.res)
	}
}

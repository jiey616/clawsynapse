package store

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newTestTaskStore(t *testing.T) *TaskStore {
	t.Helper()
	dir := t.TempDir()
	ts := NewTaskStore(dir)
	if err := ts.EnsureLayout(); err != nil {
		t.Fatalf("EnsureLayout failed: %v", err)
	}
	return ts
}

func TestTaskRunPathDeterministic(t *testing.T) {
	ts := NewTaskStore(t.TempDir())

	path1, err := ts.TaskRunPath("task-abc")
	if err != nil {
		t.Fatalf("TaskRunPath failed: %v", err)
	}
	path2, err := ts.TaskRunPath("task-abc")
	if err != nil {
		t.Fatalf("TaskRunPath failed: %v", err)
	}
	if path1 != path2 {
		t.Fatalf("path not deterministic: %q vs %q", path1, path2)
	}

	sum := sha1.Sum([]byte("task-abc"))
	want := filepath.Join(ts.BaseDir(), hex.EncodeToString(sum[:])[:16]+".json")
	if path1 != want {
		t.Fatalf("path = %q, want %q", path1, want)
	}

	if _, err := ts.TaskRunPath("  "); err == nil {
		t.Fatalf("expected error for empty task id")
	}
}

func TestTaskStoreSaveGetDeleteRoundTrip(t *testing.T) {
	ts := newTestTaskStore(t)

	// 未写入时不存在
	rec, ok, err := ts.GetTaskRun("task-1")
	if err != nil || ok || rec != nil {
		t.Fatalf("expected missing record, got rec=%v ok=%v err=%v", rec, ok, err)
	}

	in := TaskRunRecord{
		TaskID:       "task-1",
		MessageID:    "msg-42",
		ActiveRunID:  "run_abc",
		SessionID:    "ses_1",
		Status:       TaskStatusRunning,
		AttemptCount: 2,
		CreatedAtMs:  1000,
		UpdatedAtMs:  2000,
	}
	if err := ts.SaveTaskRun(in); err != nil {
		t.Fatalf("SaveTaskRun failed: %v", err)
	}

	path, _ := ts.TaskRunPath("task-1")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("file perm = %v, want 0600", got)
	}

	out, ok, err := ts.GetTaskRun("task-1")
	if err != nil || !ok {
		t.Fatalf("GetTaskRun failed: ok=%v err=%v", ok, err)
	}
	if out.TaskID != in.TaskID || out.MessageID != in.MessageID ||
		out.ActiveRunID != in.ActiveRunID || out.SessionID != in.SessionID ||
		out.Status != TaskStatusRunning || out.AttemptCount != in.AttemptCount ||
		out.CreatedAtMs != in.CreatedAtMs || out.UpdatedAtMs != in.UpdatedAtMs {
		t.Fatalf("record mismatch: %+v vs %+v", out, in)
	}

	// 终态覆盖写
	out.Status = TaskStatusCompleted
	out.UpdatedAtMs = 3000
	if err := ts.SaveTaskRun(*out); err != nil {
		t.Fatalf("SaveTaskRun(terminal) failed: %v", err)
	}
	final, _, _ := ts.GetTaskRun("task-1")
	if final.Status != TaskStatusCompleted || final.UpdatedAtMs != 3000 {
		t.Fatalf("terminal overwrite failed: %+v", final)
	}

	// 删除 + 幂等
	if err := ts.DeleteTaskRun("task-1"); err != nil {
		t.Fatalf("DeleteTaskRun failed: %v", err)
	}
	if err := ts.DeleteTaskRun("task-1"); err != nil {
		t.Fatalf("idempotent DeleteTaskRun failed: %v", err)
	}
	if _, ok, _ := ts.GetTaskRun("task-1"); ok {
		t.Fatalf("record still present after delete")
	}
}

func TestTaskStoreValidatesInput(t *testing.T) {
	ts := newTestTaskStore(t)

	if err := ts.SaveTaskRun(TaskRunRecord{Status: TaskStatusRunning}); err == nil {
		t.Fatalf("expected error for empty task id on save")
	}
	if err := ts.SaveTaskRun(TaskRunRecord{TaskID: "task-x", Status: "bogus"}); err == nil {
		t.Fatalf("expected error for invalid status on save")
	}
	if err := ts.DeleteTaskRun(""); err == nil {
		t.Fatalf("expected error for empty task id on delete")
	}
}

func TestListRunningTasksFiltersStatus(t *testing.T) {
	ts := newTestTaskStore(t)

	records := []TaskRunRecord{
		{TaskID: "t-queued", Status: TaskStatusQueued, CreatedAtMs: 1, UpdatedAtMs: 1},
		{TaskID: "t-running-1", Status: TaskStatusRunning, ActiveRunID: "run_1", CreatedAtMs: 2, UpdatedAtMs: 2},
		{TaskID: "t-running-2", Status: TaskStatusRunning, ActiveRunID: "run_2", CreatedAtMs: 3, UpdatedAtMs: 3},
		{TaskID: "t-completed", Status: TaskStatusCompleted, CreatedAtMs: 4, UpdatedAtMs: 4},
		{TaskID: "t-failed", Status: TaskStatusFailed, CreatedAtMs: 5, UpdatedAtMs: 5},
		{TaskID: "t-interrupted", Status: TaskStatusInterrupted, CreatedAtMs: 6, UpdatedAtMs: 6},
	}
	for _, rec := range records {
		if err := ts.SaveTaskRun(rec); err != nil {
			t.Fatalf("SaveTaskRun(%s) failed: %v", rec.TaskID, err)
		}
	}

	running, err := ts.ListRunningTasks()
	if err != nil {
		t.Fatalf("ListRunningTasks failed: %v", err)
	}
	if len(running) != 2 {
		t.Fatalf("got %d running records, want 2: %+v", len(running), running)
	}
	ids := map[string]bool{}
	for _, rec := range running {
		ids[rec.TaskID] = true
	}
	if !ids["t-running-1"] || !ids["t-running-2"] {
		t.Fatalf("unexpected running set: %v", ids)
	}
}

func TestRecoverStaleRunningTasks(t *testing.T) {
	dir := t.TempDir()

	// 第一段：写入 running + completed + interrupted 记录
	ts1 := NewTaskStore(dir)
	if err := ts1.EnsureLayout(); err != nil {
		t.Fatalf("EnsureLayout failed: %v", err)
	}
	if err := ts1.SaveTaskRun(TaskRunRecord{TaskID: "t-live", Status: TaskStatusRunning, ActiveRunID: "run_a", CreatedAtMs: 1, UpdatedAtMs: 1}); err != nil {
		t.Fatalf("save running failed: %v", err)
	}
	if err := ts1.SaveTaskRun(TaskRunRecord{TaskID: "t-done", Status: TaskStatusCompleted, CreatedAtMs: 2, UpdatedAtMs: 2}); err != nil {
		t.Fatalf("save completed failed: %v", err)
	}
	if err := ts1.SaveTaskRun(TaskRunRecord{TaskID: "t-int", Status: TaskStatusInterrupted, CreatedAtMs: 3, UpdatedAtMs: 3}); err != nil {
		t.Fatalf("save interrupted failed: %v", err)
	}

	// 第二段：模拟重启 —— 重建 store 并调恢复函数
	ts2 := NewTaskStore(dir)
	n, err := ts2.RecoverStaleRunningTasks()
	if err != nil {
		t.Fatalf("RecoverStaleRunningTasks failed: %v", err)
	}
	if n != 1 {
		t.Fatalf("recovered %d records, want 1", n)
	}

	rec, ok, err := ts2.GetTaskRun("t-live")
	if err != nil || !ok {
		t.Fatalf("GetTaskRun(t-live) failed: ok=%v err=%v", ok, err)
	}
	if rec.Status != TaskStatusFailed {
		t.Fatalf("status = %q, want failed", rec.Status)
	}
	if rec.LastError != "node restarted during run" {
		t.Fatalf("LastError = %q, want node restarted during run", rec.LastError)
	}

	// 终态记录不受影响
	if done, _, _ := ts2.GetTaskRun("t-done"); done.Status != TaskStatusCompleted {
		t.Fatalf("completed record corrupted: %+v", done)
	}
	if intRec, _, _ := ts2.GetTaskRun("t-int"); intRec.Status != TaskStatusInterrupted {
		t.Fatalf("interrupted record corrupted: %+v", intRec)
	}

	// 二次恢复应为 0
	if n2, _ := ts2.RecoverStaleRunningTasks(); n2 != 0 {
		t.Fatalf("second recovery recovered %d, want 0", n2)
	}
}

func TestEnsureLayoutCreatesTaskRunsDir(t *testing.T) {
	dir := t.TempDir()
	fs := NewFSStore(dir)
	if err := fs.EnsureLayout(); err != nil {
		t.Fatalf("FSStore.EnsureLayout failed: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "task_runs"))
	if err != nil || !info.IsDir() {
		t.Fatalf("task_runs dir missing: err=%v isDir=%v", err, err == nil && info.IsDir())
	}
}

// TestTaskStoreConcurrentAccess 按验收要求：100 goroutine 对 10 个不同
// taskID 并发写读删，-race 下无数据竞争、无文件损坏、无临时文件残留。
//
// 语义说明：TaskStore 本身是纯持久化层（原子单文件写，last-write-win），
// 不提供 per-key 线性一致性 —— 那是 T1.2 TaskCoordinator 的 per-taskId
// 锁的职责。因此并发测试分两阶段：
//   - Phase A（独占）：每 goroutine 独占 taskID，严格断言写后读；
//   - Phase B（共享）：100 goroutine 共享 10 个 taskID 混合写读删，
//     只断言「无错误」—— 共享 key 上读到 ok=false 属合法交错
//     （并发 goroutine 恰好删掉了该 key）。
// 两个阶段中任何 JSON 解析错误都意味着文件损坏（GetTaskRun 会报错）。
func TestTaskStoreConcurrentAccess(t *testing.T) {
	ts := newTestTaskStore(t)

	const goroutines = 100
	const sharedTaskIDs = 10

	var wg sync.WaitGroup
	errCh := make(chan error, 2*goroutines)

	// Phase A：独占 taskID，严格写后读
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			taskID := fmt.Sprintf("solo-%d", g)
			for round := 0; round < 5; round++ {
				rec := TaskRunRecord{
					TaskID:      taskID,
					ActiveRunID: fmt.Sprintf("run-g%d-r%d", g, round),
					Status:      TaskStatusRunning,
					CreatedAtMs: int64(g),
					UpdatedAtMs: int64(round),
				}
				if err := ts.SaveTaskRun(rec); err != nil {
					errCh <- fmt.Errorf("save running: %w", err)
					return
				}
				got, ok, err := ts.GetTaskRun(taskID)
				if err != nil {
					errCh <- fmt.Errorf("get: %w", err)
					return
				}
				if !ok || got.Status != TaskStatusRunning || got.ActiveRunID != rec.ActiveRunID {
					errCh <- fmt.Errorf("unexpected read: ok=%v rec=%+v", ok, got)
					return
				}
				got.Status = TaskStatusCompleted
				if err := ts.SaveTaskRun(*got); err != nil {
					errCh <- fmt.Errorf("save terminal: %w", err)
					return
				}
				if err := ts.DeleteTaskRun(taskID); err != nil {
					errCh <- fmt.Errorf("delete: %w", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	// Phase B：共享 10 个 taskID，混合写读删（容忍合法交错）
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			taskID := fmt.Sprintf("shared-%d", g%sharedTaskIDs)
			for round := 0; round < 5; round++ {
				rec := TaskRunRecord{
					TaskID:      taskID,
					ActiveRunID: fmt.Sprintf("run-g%d-r%d", g, round),
					Status:      TaskStatusRunning,
					CreatedAtMs: int64(g),
					UpdatedAtMs: int64(round),
				}
				if err := ts.SaveTaskRun(rec); err != nil {
					errCh <- fmt.Errorf("save running: %w", err)
					return
				}
				got, ok, err := ts.GetTaskRun(taskID)
				if err != nil {
					errCh <- fmt.Errorf("get: %w", err)
					return
				}
				// ok=false 合法：其他 goroutine 可能已删除该 key；
				// ok=true 时必须是可解析的合法记录（JSON 未损坏）。
				if ok && got.Status != TaskStatusRunning && got.Status != TaskStatusCompleted {
					errCh <- fmt.Errorf("corrupt status: %+v", got)
					return
				}
				if ok {
					got.Status = TaskStatusCompleted
					if err := ts.SaveTaskRun(*got); err != nil {
						errCh <- fmt.Errorf("save terminal: %w", err)
						return
					}
				}
				if err := ts.DeleteTaskRun(taskID); err != nil {
					errCh <- fmt.Errorf("delete: %w", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent access error: %v", err)
	}

	// 无文件损坏：目录中残留的每个 .json 都必须可解析
	entries, err := os.ReadDir(ts.BaseDir())
	if err != nil {
		t.Fatalf("read dir failed: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("temp file leaked: %s", e.Name())
		}
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(ts.BaseDir(), e.Name()))
		if err != nil {
			t.Fatalf("read %s failed: %v", e.Name(), err)
		}
		var probe TaskRunRecord
		if err := json.Unmarshal(b, &probe); err != nil {
			t.Fatalf("corrupt file %s: %v", e.Name(), err)
		}
	}
}

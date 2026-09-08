package store

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TaskRunStatus 描述一次任务 run 在节点侧的生命周期状态。
type TaskRunStatus string

const (
	TaskStatusQueued      TaskRunStatus = "queued"
	TaskStatusRunning     TaskRunStatus = "running"
	TaskStatusCompleted   TaskRunStatus = "completed"
	TaskStatusFailed      TaskRunStatus = "failed"
	TaskStatusCancelled   TaskRunStatus = "cancelled"
	TaskStatusInterrupted TaskRunStatus = "interrupted" // /stop 或 ctx 取消后的 gateway 终态
)

// TaskRunRecord 是单个任务 run 的持久化状态记录，
// 落盘为 <DataDir>/task_runs/<sha1(taskID) 前 16 hex>.json。
type TaskRunRecord struct {
	TaskID       string        `json:"taskId"`
	MessageID    string        `json:"messageId,omitempty"`
	ActiveRunID  string        `json:"activeRunId,omitempty"`
	SessionID    string        `json:"sessionId,omitempty"`
	Status       TaskRunStatus `json:"status"`
	AttemptCount int           `json:"attemptCount"`
	LastError    string        `json:"lastError,omitempty"`
	CreatedAtMs  int64         `json:"createdAtMs"`
	UpdatedAtMs  int64         `json:"updatedAtMs"`
}

// TaskStore 以纯文件方式持久化任务 run 状态（禁止 SQLite）。
// 并发模型：单文件写入走 WriteJSONAtomic（temp+rename，last-write-win 原子），
// 「查 TaskStore → 占位落盘」临界区的互斥由上层 TaskCoordinator 的
// per-taskId 锁保证；本结构只做纯持久化。
type TaskStore struct {
	baseDir string
}

// NewTaskStore 构造 TaskStore；dataDir 为节点数据目录（与 FSStore.BaseDir 同源）。
func NewTaskStore(dataDir string) *TaskStore {
	return &TaskStore{baseDir: filepath.Join(dataDir, "task_runs")}
}

// BaseDir 返回落盘目录（<DataDir>/task_runs）。
func (s *TaskStore) BaseDir() string {
	return s.baseDir
}

func taskRunFileName(taskID string) string {
	sum := sha1.Sum([]byte(taskID))
	return hex.EncodeToString(sum[:])[:16] + ".json"
}

// TaskRunPath 返回 taskID 对应的落盘路径；空 taskID 视为编程错误。
func (s *TaskStore) TaskRunPath(taskID string) (string, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return "", errors.New("task id is required")
	}
	return filepath.Join(s.baseDir, taskRunFileName(taskID)), nil
}

// EnsureLayout 创建 task_runs 目录（0700）。
func (s *TaskStore) EnsureLayout() error {
	if s.baseDir == "" {
		return errors.New("task store base dir is empty")
	}
	return os.MkdirAll(s.baseDir, 0o700)
}

// GetTaskRun 读取任务记录；不存在时返回 (nil, false, nil)。
func (s *TaskStore) GetTaskRun(taskID string) (*TaskRunRecord, bool, error) {
	path, err := s.TaskRunPath(taskID)
	if err != nil {
		return nil, false, err
	}

	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}

	var rec TaskRunRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, false, fmt.Errorf("corrupt task run record %s: %w", path, err)
	}
	if strings.TrimSpace(rec.TaskID) == "" {
		rec.TaskID = strings.TrimSpace(taskID)
	}
	return &rec, true, nil
}

// SaveTaskRun 原子写入任务记录。状态必须是六态枚举之一，
// 时间戳由调用方（TaskCoordinator）负责维护。
func (s *TaskStore) SaveTaskRun(rec TaskRunRecord) error {
	rec.TaskID = strings.TrimSpace(rec.TaskID)
	if rec.TaskID == "" {
		return errors.New("task id is required")
	}
	if !isValidTaskRunStatus(rec.Status) {
		return fmt.Errorf("invalid task run status %q", rec.Status)
	}

	path, err := s.TaskRunPath(rec.TaskID)
	if err != nil {
		return err
	}
	return WriteJSONAtomic(path, rec, 0o600)
}

// DeleteTaskRun 删除任务记录；文件不存在视为已删除（幂等）。
func (s *TaskStore) DeleteTaskRun(taskID string) error {
	path, err := s.TaskRunPath(taskID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ListRunningTasks 扫描目录并返回 running 状态的记录。
// 单个损坏/不可解析文件跳过不报错 —— 恢复流程不能被脏文件阻塞。
func (s *TaskStore) ListRunningTasks() ([]TaskRunRecord, error) {
	entries, err := os.ReadDir(s.baseDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []TaskRunRecord{}, nil
		}
		return nil, err
	}

	running := []TaskRunRecord{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".tmp-") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.baseDir, e.Name()))
		if err != nil {
			continue
		}
		var rec TaskRunRecord
		if json.Unmarshal(b, &rec) != nil {
			continue
		}
		if rec.Status == TaskStatusRunning {
			running = append(running, rec)
		}
	}
	return running, nil
}

// RecoverStaleRunningTasks 是启动恢复语义：进程重启后，本进程已失去对
// 所有 running 记录的轮询权（gateway 侧的 run 可能仍在跑）。宁可报失败
// 让平台重派，不可留幽灵状态 —— 全部改写为 failed 并标注原因。
// 返回恢复的记录数。
func (s *TaskStore) RecoverStaleRunningTasks() (int, error) {
	records, err := s.ListRunningTasks()
	if err != nil {
		return 0, err
	}

	recovered := 0
	for _, rec := range records {
		rec.Status = TaskStatusFailed
		rec.LastError = "node restarted during run"
		rec.UpdatedAtMs = time.Now().UnixMilli()
		if err := s.SaveTaskRun(rec); err != nil {
			return recovered, err
		}
		recovered++
	}
	return recovered, nil
}

func isValidTaskRunStatus(st TaskRunStatus) bool {
	switch st {
	case TaskStatusQueued, TaskStatusRunning, TaskStatusCompleted,
		TaskStatusFailed, TaskStatusCancelled, TaskStatusInterrupted:
		return true
	}
	return false
}

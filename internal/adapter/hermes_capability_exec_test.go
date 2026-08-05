package adapter

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"clawsynapse/internal/protocol"
)

// seedExecutionsDB creates a minimal executions.db + output files matching the
// real hermes cron layout, all under the adapter's temp hermes home.
func seedExecutionsDB(t *testing.T, home string) {
	t.Helper()
	cronDir := filepath.Join(home, "cron")
	if err := os.MkdirAll(cronDir, 0o755); err != nil {
		t.Fatalf("mkdir cron: %v", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(cronDir, "executions.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	schema := `CREATE TABLE executions (
		id TEXT PRIMARY KEY, job_id TEXT, source TEXT, process_id TEXT,
		status TEXT, claimed_at TEXT, started_at TEXT, finished_at TEXT, error TEXT);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("create table: %v", err)
	}
	// One completed execution with an output file, one failed execution.
	_, _ = db.Exec(`INSERT INTO executions (id, job_id, source, process_id, status, started_at, finished_at) VALUES (?,?,?,?,?,?,?)`,
		"exec-1", "job-1", "builtin", "p1", "completed",
		"2026-08-04T07:28:40.991745+00:00", "2026-08-04T07:28:44.286442+00:00")
	_, _ = db.Exec(`INSERT INTO executions (id, job_id, source, process_id, status, started_at, finished_at, error) VALUES (?,?,?,?,?,?,?,?)`,
		"exec-2", "job-1", "builtin", "p1", "failed",
		"2026-08-04T08:00:00.000000+00:00", "2026-08-04T08:00:03.000000+00:00", "boom")
	_, _ = db.Exec(`INSERT INTO executions (id, job_id, source, process_id, status, started_at) VALUES (?,?,?,?,?,?)`,
		"exec-3", "job-2", "builtin", "p1", "running", "2026-08-04T09:00:00.000000+00:00")

	// Output markdown matching exec-1's finished time.
	jobOut := filepath.Join(home, "cron", "output", "job-1")
	if err := os.MkdirAll(jobOut, 0o755); err != nil {
		t.Fatalf("mkdir output: %v", err)
	}
	md := "# Cron Job: 测试\n\n## Response\n\n测试任务\n"
	if err := os.WriteFile(filepath.Join(jobOut, "2026-08-04_07-28-44.md"), []byte(md), 0o644); err != nil {
		t.Fatalf("write output: %v", err)
	}
}

func newExecTestAdapter(t *testing.T, home string) *HermesAdapter {
	t.Helper()
	cfgPath := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("model:\n  default: x\n  provider: y\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	a, err := NewHermesAdapter(HermesConfig{
		NodeID:     "n1",
		BaseURL:    "http://127.0.0.1:1/v1",
		Model:      "hermes-agent",
		ConfigPath: cfgPath,
	})
	if err != nil {
		t.Fatalf("NewHermesAdapter: %v", err)
	}
	return a
}

func TestListExecutionsReadsDBAndPreview(t *testing.T) {
	home := t.TempDir()
	seedExecutionsDB(t, home)
	a := newExecTestAdapter(t, home)

	ctx := context.Background()
	execs, err := a.ListExecutions(ctx, "job-1", 10)
	if err != nil {
		t.Fatalf("ListExecutions: %v", err)
	}
	if len(execs) != 2 {
		t.Fatalf("executions = %d, want 2", len(execs))
	}

	first := execs[0] // newest first
	if first.Status != "failed" || first.ExecutionID != "exec-2" {
		t.Fatalf("first = %+v, want exec-2 failed (newest)", first)
	}
	// exec-2 is failed; find exec-1 which has the output preview.
	var withPreview protocol.ExecutionInfo
	for _, e := range execs {
		if e.ExecutionID == "exec-1" {
			withPreview = e
		}
	}
	if withPreview.OutputPreview == "" {
		t.Fatalf("exec-1 has no output preview: %+v", withPreview)
	}
	if !strings.Contains(withPreview.OutputPreview, "测试任务") {
		t.Errorf("preview = %q, want to contain 测试任务", withPreview.OutputPreview)
	}
	if withPreview.Output == "" {
		t.Errorf("exec-1 has no full output, want full markdown")
	}
	if !strings.Contains(withPreview.Output, "测试任务") {
		t.Errorf("output = %q, want to contain 测试任务", withPreview.Output)
	}
	if withPreview.DurationMs <= 0 {
		t.Errorf("durationMs = %d, want > 0", withPreview.DurationMs)
	}
	if withPreview.FinishedAt <= withPreview.StartedAt {
		t.Errorf("timestamps invalid: started=%d finished=%d", withPreview.StartedAt, withPreview.FinishedAt)
	}
}

// filePreview 应跳过 hermes cron 的英文模板块，从 "## Response" 处开始预览，
// 保证模型真实输出（可能含中文）出现在 preview 中。
func TestFilePreviewSkipsTemplateBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.md")
	content := `# Cron Job: 测试任务

**Job ID:** 73f3e3475cb2
**Run Time:** 2026-08-04 09:02:03
**Schedule:** 0 9 * * *

## Prompt

[IMPORTANT: You are running as a scheduled cron job. DELIVERY: ...]

回复"测试任务"

## Response

收到，这是定时任务的测试响应。✅

- 执行时间：2026-08-04
- 状态：正常运行，无异常
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	p := filePreview(path)
	if !strings.Contains(p, "## Response") {
		t.Errorf("preview should start from ## Response, got: %q", p[:min(60, len(p))])
	}
	if strings.Contains(p, "[IMPORTANT:") {
		t.Errorf("preview should not contain the English template block, got: %q", p[:min(60, len(p))])
	}
	if !strings.Contains(p, "测试响应") {
		t.Errorf("preview should contain the model's real output, got: %q", p[:min(120, len(p))])
	}
	if len(p) > 4000+8 {
		t.Errorf("preview length %d exceeds 4000 chars", len(p))
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestListExecutionsFiltersByJob(t *testing.T) {
	home := t.TempDir()
	seedExecutionsDB(t, home)
	a := newExecTestAdapter(t, home)

	execs, err := a.ListExecutions(context.Background(), "job-2", 10)
	if err != nil {
		t.Fatalf("ListExecutions: %v", err)
	}
	if len(execs) != 1 || execs[0].Status != "running" {
		t.Fatalf("execs = %+v, want single running exec for job-2", execs)
	}
}

func TestListExecutionsNoDBReturnsEmpty(t *testing.T) {
	home := t.TempDir()
	a := newExecTestAdapter(t, home) // no cron/ dir at all

	execs, err := a.ListExecutions(context.Background(), "", 10)
	if err != nil {
		t.Fatalf("ListExecutions: %v", err)
	}
	if len(execs) != 0 {
		t.Fatalf("executions = %d, want 0", len(execs))
	}
}

func TestListExecutionsClampsLimit(t *testing.T) {
	home := t.TempDir()
	seedExecutionsDB(t, home)
	a := newExecTestAdapter(t, home)

	execs, err := a.ListExecutions(context.Background(), "job-1", 1)
	if err != nil {
		t.Fatalf("ListExecutions: %v", err)
	}
	if len(execs) != 1 {
		t.Fatalf("executions = %d, want 1 (limit)", len(execs))
	}
	// Default limit when <= 0 must not explode.
	if _, err := a.ListExecutions(context.Background(), "job-1", 0); err != nil {
		t.Fatalf("limit 0: %v", err)
	}
}

func TestLoadJobExecutionsAttachesToList(t *testing.T) {
	home := t.TempDir()
	seedExecutionsDB(t, home)
	a := newExecTestAdapter(t, home)

	jobs := []protocol.CronJobInfo{
		{ID: "job-1", Name: "a"},
		{ID: "job-2", Name: "b"},
		{ID: "job-3", Name: "c"}, // no executions
	}
	a.loadJobExecutions(context.Background(), jobs, 3)
	if len(jobs[0].Executions) != 2 {
		t.Errorf("job-1 executions = %d, want 2", len(jobs[0].Executions))
	}
	if len(jobs[1].Executions) != 1 {
		t.Errorf("job-2 executions = %d, want 1", len(jobs[1].Executions))
	}
	if len(jobs[2].Executions) != 0 {
		t.Errorf("job-3 executions = %d, want 0", len(jobs[2].Executions))
	}
	// 列表场景（第 1 层）不应带完整 output，仅 preview
	for _, e := range jobs[0].Executions {
		if e.Output != "" {
			t.Errorf("list executions should not carry full output, got %q", e.Output)
		}
	}
}

func TestParseExecTime(t *testing.T) {
	ms := parseExecTime("2026-08-04T07:28:44.286442+00:00")
	if ms <= 0 {
		t.Fatalf("parse RFC3339Nano failed: %d", ms)
	}
	ms2 := parseExecTime("2026-08-04 07:28:44")
	if ms2 <= 0 {
		t.Fatalf("parse space format failed: %d", ms2)
	}
	if parseExecTime("") != 0 || parseExecTime("garbage") != 0 {
		t.Fatal("invalid inputs should return 0")
	}
}

var _ = time.Second

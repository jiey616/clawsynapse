package adapter

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"clawsynapse/internal/protocol"
)

// executionsDBPath locates the hermes cron executions SQLite database.
func (a *HermesAdapter) executionsDBPath() string {
	return filepath.Join(a.hermesHomeDir(), "cron", "executions.db")
}

// cronOutputDir locates the hermes cron output directory
// ($HERMES_HOME/cron/output/<jobId>/<ts>.md).
func (a *HermesAdapter) cronOutputDir() string {
	return filepath.Join(a.hermesHomeDir(), "cron", "output")
}

// ListExecutions implements CapabilityProvider. It reads execution records
// from the hermes cron executions.db (SQLite) and attaches a result preview
// from the matching markdown output file. Only read queries are issued — the
// db is the gateway's live write target and SQLite read locks are safe.
func (a *HermesAdapter) ListExecutions(ctx context.Context, jobID string, limit int) ([]protocol.ExecutionInfo, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	dbPath := a.executionsDBPath()
	if _, err := os.Stat(dbPath); err != nil {
		return []protocol.ExecutionInfo{}, nil
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open executions db: %w", err)
	}
	defer db.Close()

	query := `SELECT id, job_id, status, started_at, finished_at, error
	          FROM executions ORDER BY rowid DESC LIMIT ?`
	args := []any{limit}
	if jobID != "" {
		query = `SELECT id, job_id, status, started_at, finished_at, error
		         FROM executions WHERE job_id = ? ORDER BY rowid DESC LIMIT ?`
		args = []any{jobID, limit}
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query executions: %w", err)
	}
	defer rows.Close()

	out := make([]protocol.ExecutionInfo, 0, limit)
	for rows.Next() {
		var (
			id, jid, status   string
			started, finished sql.NullString
			errText           sql.NullString
		)
		if err := rows.Scan(&id, &jid, &status, &started, &finished, &errText); err != nil {
			return nil, fmt.Errorf("scan execution: %w", err)
		}
		info := protocol.ExecutionInfo{
			ExecutionID: id,
			JobID:       jid,
			Status:      status,
			Error:       errText.String,
		}
		if st := parseExecTime(started.String); st > 0 {
			info.StartedAt = st
		}
		if ft := parseExecTime(finished.String); ft > 0 {
			info.FinishedAt = ft
			info.DurationMs = ft - info.StartedAt
		}
		out = append(out, info)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Attach result previews (and full output) from the output directory.
	previews := a.executionOutputPreviews(out)
	for i := range out {
		if p, ok := previews[out[i].ExecutionID]; ok {
			out[i].OutputFile = p.file
			out[i].OutputPreview = p.preview
			out[i].Output = p.output
		}
	}
	return out, nil
}

func parseExecTime(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().UnixMilli()
		}
	}
	return 0
}

type outputRef struct {
	file    string
	preview string
	output  string
}

// executionOutputPreviews matches each execution to its output markdown file
// (named <jobId>/<finished-ts>.md) and reads a short preview.
func (a *HermesAdapter) executionOutputPreviews(execs []protocol.ExecutionInfo) map[string]outputRef {
	root := a.cronOutputDir()
	byKey := map[string]string{} // "<jobId>/<yyyy-MM-dd_HH-mm-ss>" -> file
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	for _, jobDir := range entries {
		if !jobDir.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, jobDir.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			name := f.Name()
			if !strings.HasSuffix(name, ".md") {
				continue
			}
			key := jobDir.Name() + "/" + strings.TrimSuffix(name, ".md")
			byKey[key] = filepath.Join(root, jobDir.Name(), name)
		}
	}

	out := map[string]outputRef{}
	for _, e := range execs {
		if e.FinishedAt <= 0 {
			continue
		}
		stamp := time.UnixMilli(e.FinishedAt).UTC().Format("2006-01-02_15-04-05")
		f, ok := byKey[e.JobID+"/"+stamp]
		if !ok {
			// Fall back to the nearest output file for this job (tolerance
			// for second-level clock skew between start and file naming).
			f = nearestOutputFile(root, e.JobID, e.FinishedAt, byKey)
			if f == "" {
				continue
			}
		}
		out[e.ExecutionID] = outputRef{
			file:    f,
			preview: filePreview(f),
			output:  readFullOutput(f),
		}
	}
	return out
}

// nearestOutputFile picks the output file whose timestamp is closest to the
// execution finish time within a small window.
func nearestOutputFile(root, jobID string, finishedMs int64, byKey map[string]string) string {
	type cand struct {
		diff time.Duration
		file string
	}
	var best *cand
	prefix := jobID + "/"
	for key, f := range byKey {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		ts, err := time.Parse("2006-01-02_15-04-05", strings.TrimPrefix(key, prefix))
		if err != nil {
			continue
		}
		diff := time.UnixMilli(finishedMs).UTC().Sub(ts)
		if diff < 0 {
			diff = -diff
		}
		if diff > 5*time.Minute {
			continue
		}
		if best == nil || diff < best.diff {
			best = &cand{diff: diff, file: f}
		}
	}
	if best == nil {
		return ""
	}
	return best.file
}

// filePreview reads a preview of a markdown output file. Hermes cron reports
// start with an English template block (Job ID / Run Time / Schedule / Prompt
// + delivery instructions) before the model's actual response under
// "## Response". Truncating from the top would show only the template, so we
// skip the template section and preview from the first response marker.
func filePreview(path string) string {
	const previewChars = 4000
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	content := string(data)

	// Skip the leading template block: find the first "## Response" heading
	// (or a standalone "Response" marker) and preview from there.
	if idx := strings.Index(content, "## Response"); idx >= 0 {
		content = content[idx:]
	} else if idx := strings.Index(content, "\n## Response\n"); idx >= 0 {
		content = content[idx:]
	} else if idx := strings.Index(content, "\n# Response\n"); idx >= 0 {
		content = content[idx:]
	}

	trimmed := strings.TrimLeft(content, "\r\n")
	if len(trimmed) <= previewChars {
		return trimmed
	}
	return trimmed[:previewChars] + "\n..."
}

// readFullOutput reads the complete markdown output file. Large outputs are
// clamped to 256 KiB to bound NATS message size on the executions query path.
func readFullOutput(path string) string {
	const maxBytes = 256 << 10
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(data) > maxBytes {
		return string(data[:maxBytes]) + "\n...[truncated]"
	}
	return string(data)
}

// loadJobExecutions attaches the most recent executions to each job entry in
// the capabilities read.
func (a *HermesAdapter) loadJobExecutions(ctx context.Context, jobs []protocol.CronJobInfo, perJob int) {
	if perJob <= 0 {
		perJob = 3
	}
	for i := range jobs {
		execs, err := a.ListExecutions(ctx, jobs[i].ID, perJob)
		if err != nil {
			if a.log != nil {
				a.log.Warn("load job executions failed",
					slog.String("error", err.Error()),
					slog.String("jobId", jobs[i].ID),
				)
			}
			continue
		}
		if len(execs) > 0 {
			// 第 1 层（capabilities 读回）只带 preview，不带完整 output，保持列表轻量
			for j := range execs {
				execs[j].Output = ""
			}
			jobs[i].Executions = execs
		}
	}
}

// sortExecutionsNewestFirst orders executions by start time descending.
func sortExecutionsNewestFirst(execs []protocol.ExecutionInfo) {
	sort.SliceStable(execs, func(i, j int) bool {
		return execs[i].StartedAt > execs[j].StartedAt
	})
}

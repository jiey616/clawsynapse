package adapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── extractJobSchedule ─────────────────────────────────────────────

func TestExtractJobScheduleString(t *testing.T) {
	raw := []byte(`"0 9 * * *"`)
	if got := extractJobSchedule(raw, ""); got != "0 9 * * *" {
		t.Errorf("got %q, want cron string", got)
	}
}

func TestExtractJobScheduleNestedObject(t *testing.T) {
	raw := []byte(`{"kind":"cron","expr":"0 9 * * *","display":"Every day at 09:00"}`)
	if got := extractJobSchedule(raw, ""); got != "Every day at 09:00" {
		t.Errorf("got %q, want nested display", got)
	}
	// display falls back to expr, then kind.
	raw = []byte(`{"kind":"interval","expr":"every 5m","display":""}`)
	if got := extractJobSchedule(raw, ""); got != "every 5m" {
		t.Errorf("got %q, want expr fallback", got)
	}
}

func TestExtractJobScheduleDisplayWins(t *testing.T) {
	raw := []byte(`{"kind":"cron","expr":"0 9 * * *","display":"Daily"}`)
	if got := extractJobSchedule(raw, "显式展示"); got != "显式展示" {
		t.Errorf("got %q, want explicit display to win", got)
	}
}

func TestExtractJobScheduleEmptyAndInvalid(t *testing.T) {
	if got := extractJobSchedule(nil, ""); got != "" {
		t.Errorf("nil raw: got %q, want empty", got)
	}
	if got := extractJobSchedule([]byte(`{not-json`), ""); got != "" {
		t.Errorf("invalid raw: got %q, want empty", got)
	}
}

// ── fetchModels fallback branches ──────────────────────────────────

func TestFetchModelsBuiltinProviderSurfaced(t *testing.T) {
	home := t.TempDir()
	cfgPath := filepath.Join(home, "config.yaml")
	// model section only (deepseek built-in), no custom_providers.
	yaml := "model:\n  default: deepseek-chat\n  provider: deepseek\n"
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	a, err := NewHermesAdapter(HermesConfig{NodeID: "n1", ConfigPath: cfgPath})
	if err != nil {
		t.Fatalf("NewHermesAdapter: %v", err)
	}
	models, err := a.fetchModels(context.Background())
	if err != nil {
		t.Fatalf("fetchModels: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("Models = %+v, want 1 builtin entry", models)
	}
	m := models[0]
	if !m.IsDefault || m.Provider != "deepseek" || m.Model != "deepseek-chat" {
		t.Errorf("Model = %+v, want deepseek/deepseek-chat default", m)
	}
}

func TestFetchModelsIDAndDefaultModelFallbacks(t *testing.T) {
	home := t.TempDir()
	cfgPath := filepath.Join(home, "config.yaml")
	// Provider without id → id falls back to name; entry without model →
	// default_model fallback; a builtin default appended as second entry.
	yaml := `model:
  default: builtin-model
  provider: builtin
custom_providers:
  - name: alpha
    base_url: https://alpha.example/v1
    api_key: sk-1
    default_model: alpha-big
  - name: beta
    base_url: https://beta.example/v1
    api_key: sk-2
`
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	a, err := NewHermesAdapter(HermesConfig{NodeID: "n1", ConfigPath: cfgPath})
	if err != nil {
		t.Fatalf("NewHermesAdapter: %v", err)
	}
	models, err := a.fetchModels(context.Background())
	if err != nil {
		t.Fatalf("fetchModels: %v", err)
	}
	if len(models) != 3 {
		t.Fatalf("Models = %+v, want 3 entries", models)
	}
	if models[0].ID != "alpha" || models[0].Model != "alpha-big" {
		t.Errorf("models[0] = %+v, want id=alpha model=alpha-big", models[0])
	}
	if models[1].ID != "beta" || models[1].Model != "" || models[1].IsDefault {
		t.Errorf("models[1] = %+v, want id=beta non-default", models[1])
	}
	if models[2].Provider != "builtin" || !models[2].IsDefault {
		t.Errorf("models[2] = %+v, want builtin default appended", models[2])
	}
}

// ── hermesConfigPath priority ──────────────────────────────────────

func TestHermesConfigPathPriority(t *testing.T) {
	// 1) explicit configPath wins.
	a := &HermesAdapter{configPath: "/explicit/config.yaml"}
	t.Setenv("HERMES_HOME", "/hermes-home")
	if got := a.hermesConfigPath(); got != filepath.Join("/explicit", "config.yaml") {
		t.Errorf("explicit config: got %q", got)
	}
	// 2) HERMES_HOME next.
	a.configPath = ""
	want := filepath.Join("/hermes-home", "config.yaml")
	if got := a.hermesConfigPath(); got != want {
		t.Errorf("HERMES_HOME: got %q, want %q", got, want)
	}
	// 3) user home fallback.
	t.Setenv("HERMES_HOME", "")
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		want := filepath.Join(home, ".hermes", "config.yaml")
		if got := a.hermesConfigPath(); got != want {
			t.Errorf("user home: got %q, want %q", got, want)
		}
	}
}

// ── nearestOutputFile ──────────────────────────────────────────────

func TestNearestOutputFile(t *testing.T) {
	root := "/out"
	jobID := "job-1"
	base := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	byKey := map[string]string{
		"job-1/" + base.Format("2006-01-02_15-04-05"):            filepath.Join(root, "job-1", "a.md"),
		"job-1/" + base.Add(4*time.Minute).Format("2006-01-02_15-04-05"): filepath.Join(root, "job-1", "b.md"),
		"job-2/" + base.Format("2006-01-02_15-04-05"):            filepath.Join(root, "job-2", "c.md"),
		"job-1/not-a-timestamp":                                  filepath.Join(root, "job-1", "d.md"),
	}

	// Exact timestamp hit.
	if got := nearestOutputFile(root, jobID, base.UnixMilli(), byKey); !strings.HasSuffix(got, "a.md") {
		t.Errorf("exact hit: got %q, want a.md", got)
	}
	// Within 5-minute window → nearest candidate: at 10:03, b.md (10:04)
	// is 1min away vs a.md (10:00) 3min away, so b.md wins.
	three := base.Add(3 * time.Minute)
	if got := nearestOutputFile(root, jobID, three.UnixMilli(), byKey); !strings.HasSuffix(got, "b.md") {
		t.Errorf("nearest within window: got %q, want b.md (1min vs 3min skew)", got)
	}
	// Outside the 5-minute window → empty.
	if got := nearestOutputFile(root, jobID, base.Add(30*time.Minute).UnixMilli(), byKey); got != "" {
		t.Errorf("outside window: got %q, want empty", got)
	}
	// Different job → empty.
	if got := nearestOutputFile(root, "job-9", base.UnixMilli(), byKey); got != "" {
		t.Errorf("other job: got %q, want empty", got)
	}
}

// ── readFullOutput clamp ───────────────────────────────────────────

func TestReadFullOutputClampsTo256KiB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.md")
	big := strings.Repeat("x", 256<<10) + "TAIL-MARKER"
	if err := os.WriteFile(path, []byte(big), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := readFullOutput(path)
	if !strings.HasSuffix(got, "\n...[truncated]") {
		t.Errorf("output not clamped (len=%d)", len(got))
	}
	if strings.Contains(got, "TAIL-MARKER") {
		t.Error("output contains content beyond the 256KiB clamp")
	}
	// Missing file → empty.
	if got := readFullOutput(filepath.Join(t.TempDir(), "missing.md")); got != "" {
		t.Errorf("missing file: got %q, want empty", got)
	}
}

// ── invalidateCache ────────────────────────────────────────────────

func TestInvalidateCacheForcesRefetch(t *testing.T) {
	fg := &capabilityFakeGateway{}
	a, _ := newCapabilityTestAdapter(t, fg)
	ctx := context.Background()

	if _, err := a.Capabilities(ctx); err != nil {
		t.Fatalf("first read: %v", err)
	}
	a.invalidateCache()
	if _, err := a.Capabilities(ctx); err != nil {
		t.Fatalf("second read after invalidate: %v", err)
	}
	fg.mu.Lock()
	defer fg.mu.Unlock()
	if fg.skillsHit != 2 {
		t.Errorf("skills fetched %d times, want 2 (invalidateCache did not bust the TTL cache)", fg.skillsHit)
	}
}

// ── Capabilities: jobs failure degrades but stays available ────────

func TestHermesCapabilitiesJobsFailureKeepsAvailable(t *testing.T) {
	var jobsHits int
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/skills", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	})
	mux.HandleFunc("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
		jobsHits++
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	home := t.TempDir()
	cfgPath := filepath.Join(home, "config.yaml")
	_ = os.WriteFile(cfgPath, []byte("model:\n  default: m\n  provider: p\n"), 0o644)
	a, err := NewHermesAdapter(HermesConfig{NodeID: "n1", BaseURL: srv.URL + "/v1", ConfigPath: cfgPath})
	if err != nil {
		t.Fatalf("NewHermesAdapter: %v", err)
	}

	res, err := a.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if !res.Available {
		t.Errorf("Available = false, want true (jobs failure only degrades)")
	}
	if !strings.Contains(res.Reason, "jobs unavailable") {
		t.Errorf("Reason = %q, want jobs-unavailable explanation", res.Reason)
	}
}

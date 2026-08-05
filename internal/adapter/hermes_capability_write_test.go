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

	"gopkg.in/yaml.v3"
)

// newWriteTestAdapter builds a HermesAdapter with a temp hermes home
// (config.yaml + skills dir) so write-back tests never touch the real files.
// The gateway base URL points at a closed port; tests that reach the config
// edit + restart path will observe restart_failed (pgrep/hermes unavailable in
// the test env) which is fine — assertions focus on config/disk mutations.
func newWriteTestAdapter(t *testing.T) (*HermesAdapter, string) {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "skills", "clawsynapse"), 0o755); err != nil {
		t.Fatalf("mkdir skills: %v", err)
	}
	cfgPath := filepath.Join(home, "config.yaml")
	cfgYAML := `model:
  default: agnes-2.0-flash
  provider: agnes
  base_url: https://apihub.agnes-ai.com/v1
custom_providers:
  - name: agnes
    base_url: https://apihub.agnes-ai.com/v1
    api_key: sk-secret
    model: agnes-2.0-flash
skills:
  external_dirs:
    - ` + filepath.ToSlash(filepath.Join(home, "skills", "clawsynapse")) + `
`
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o644); err != nil {
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
	a.restartGatewayFn = func(context.Context) error { return nil }
	return a, home
}

func loadProviders(t *testing.T, cfgPath string) []map[string]any {
	t.Helper()
	return customProviders(readConfigMap(t, cfgPath))
}

func readConfigMap(t *testing.T, cfgPath string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg map[string]any
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse config yaml: %v", err)
	}
	return cfg
}

func providerNames(providers []map[string]any) []string {
	out := make([]string, 0, len(providers))
	for _, p := range providers {
		out = append(out, strAny(p["name"]))
	}
	return out
}

func ctxForTest() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// ── model ──────────────────────────────────────────────────────────

func TestApplyModelAddAppendsProvider(t *testing.T) {
	a, home := newWriteTestAdapter(t)
	ctx, cancel := ctxForTest()
	defer cancel()

	res, err := a.ApplyCapabilitySet(ctx, &CapabilitySetRequest{
		Target: "model",
		Action: "add",
		Provider: map[string]any{
			"name": "deepseek", "base_url": "https://api.deepseek.com",
			"api_key": "sk-x", "model": "deepseek-v4-flash",
		},
	})
	if err != nil {
		t.Fatalf("ApplyCapabilitySet: %v", err)
	}
	names := providerNames(loadProviders(t, a.hermesConfigPath()))
	if len(names) != 2 || !contains(names, "agnes") || !contains(names, "deepseek") {
		t.Fatalf("providers = %v, want [agnes deepseek]", names)
	}
	// Gateway restart cannot run in unit tests → expect restart_failed, not invalid.
	if res.Error != "" && strings.Contains(res.Error, "capability.invalid") {
		t.Fatalf("unexpected invalid error: %s", res.Error)
	}
	_ = home
}

func TestApplyModelAddUpdatesExisting(t *testing.T) {
	a, _ := newWriteTestAdapter(t)
	ctx, cancel := ctxForTest()
	defer cancel()

	_, err := a.ApplyCapabilitySet(ctx, &CapabilitySetRequest{
		Target: "model",
		Action: "add",
		Provider: map[string]any{
			"name": "agnes", "base_url": "https://new.example.com/v1",
			"api_key": "sk-new", "model": "agnes-2.5",
		},
	})
	if err != nil {
		t.Fatalf("ApplyCapabilitySet: %v", err)
	}
	provs := loadProviders(t, a.hermesConfigPath())
	if len(provs) != 1 {
		t.Fatalf("providers = %d, want 1 (update not append)", len(provs))
	}
	if strAny(provs[0]["base_url"]) != "https://new.example.com/v1" || strAny(provs[0]["model"]) != "agnes-2.5" {
		t.Fatalf("provider not updated: %+v", provs[0])
	}
}

func TestApplyModelAddRequiresName(t *testing.T) {
	a, _ := newWriteTestAdapter(t)
	ctx, cancel := ctxForTest()
	defer cancel()

	res, _ := a.ApplyCapabilitySet(ctx, &CapabilitySetRequest{
		Target: "model", Action: "add",
		Provider: map[string]any{"base_url": "x"},
	})
	if res.OK || !strings.Contains(res.Error, "capability.invalid") {
		t.Fatalf("want invalid error, got ok=%v err=%q", res.OK, res.Error)
	}
}

func TestApplyModelDeleteDefaultBlocked(t *testing.T) {
	a, _ := newWriteTestAdapter(t)
	ctx, cancel := ctxForTest()
	defer cancel()

	// agnes is the current default (model.provider == agnes) → must be blocked.
	res, _ := a.ApplyCapabilitySet(ctx, &CapabilitySetRequest{
		Target: "model", Action: "delete", Model: "agnes",
	})
	if res.OK || !strings.Contains(res.Error, "cannot delete the current default") {
		t.Fatalf("want default-delete block, got ok=%v err=%q", res.OK, res.Error)
	}
}

func TestApplyModelSwitchCustomProvider(t *testing.T) {
	a, _ := newWriteTestAdapter(t)
	ctx, cancel := ctxForTest()
	defer cancel()

	// First add deepseek as custom provider.
	_, _ = a.ApplyCapabilitySet(ctx, &CapabilitySetRequest{
		Target: "model", Action: "add",
		Provider: map[string]any{
			"name": "deepseek", "base_url": "https://api.deepseek.com",
			"api_key": "sk-x", "model": "deepseek-v4-flash",
		},
	})
	// Switch default to deepseek by provider name.
	res, _ := a.ApplyCapabilitySet(ctx, &CapabilitySetRequest{
		Target: "model", Action: "switch", Model: "deepseek",
	})
	if !res.OK && !strings.Contains(res.Error, "restart_failed") {
		t.Fatalf("switch failed: ok=%v err=%q", res.OK, res.Error)
	}
	cfg := readConfigMap(t, a.hermesConfigPath())
	m, _ := cfg["model"].(map[string]any)
	if strAny(m["default"]) != "deepseek-v4-flash" {
		t.Errorf("model.default = %q, want deepseek-v4-flash (model name, not provider name)", strAny(m["default"]))
	}
	if strAny(m["provider"]) != "deepseek" {
		t.Errorf("model.provider = %q, want deepseek", strAny(m["provider"]))
	}
	if strAny(m["base_url"]) != "https://api.deepseek.com" {
		t.Errorf("model.base_url = %q, want deepseek endpoint (base_url must follow)", strAny(m["base_url"]))
	}
}

func TestApplyModelSwitchBuiltinRejected(t *testing.T) {
	a, _ := newWriteTestAdapter(t)
	ctx, cancel := ctxForTest()
	defer cancel()

	// deepseek is NOT in custom_providers (built-in) → switch must be rejected.
	res, _ := a.ApplyCapabilitySet(ctx, &CapabilitySetRequest{
		Target: "model", Action: "switch", Model: "deepseek",
	})
	if res.OK || !strings.Contains(res.Error, "built-in provider switch is unsupported") {
		t.Fatalf("want built-in switch rejection, got ok=%v err=%q", res.OK, res.Error)
	}
	// Config must be untouched.
	cfg := readConfigMap(t, a.hermesConfigPath())
	m, _ := cfg["model"].(map[string]any)
	if strAny(m["default"]) != "agnes-2.0-flash" {
		t.Errorf("config was mutated: model.default = %q", strAny(m["default"]))
	}
}

// ── skill ──────────────────────────────────────────────────────────

func writeSkillFile(t *testing.T, home, subdir, filename, content string) string {
	t.Helper()
	dir := filepath.Join(home, subdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p := filepath.Join(dir, filename)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func TestApplySkillAddInstallsToManagedDir(t *testing.T) {
	a, home := newWriteTestAdapter(t)
	ctx, cancel := ctxForTest()
	defer cancel()

	skillMd := writeSkillFile(t, home, "upload", "SKILL.md", "---\nname: demo\n---\n# demo\n")
	res, _ := a.ApplyCapabilitySet(ctx, &CapabilitySetRequest{
		Target: "skill", Action: "add", Skill: "demo-skill",
		FileLocalPaths: []string{skillMd},
	})
	if !res.OK && !strings.Contains(res.Error, "restart_failed") {
		t.Fatalf("skill add failed: ok=%v err=%q", res.OK, res.Error)
	}

	// Managed dir must be under the hermes home, not its parent.
	managedSkill := filepath.Join(home, "skills", "clawsynapse-managed", "demo-skill", "SKILL.md")
	if _, err := os.Stat(managedSkill); err != nil {
		t.Fatalf("managed skill not installed at %s: %v", managedSkill, err)
	}

	cfg := readConfigMap(t, a.hermesConfigPath())
	skills, _ := cfg["skills"].(map[string]any)
	managed := stringList(skills["clawsynapse_managed"])
	if !contains(managed, "demo-skill") {
		t.Errorf("clawsynapse_managed = %v, want demo-skill", managed)
	}
	dirs := stringList(skills["external_dirs"])
	wantDir := filepath.ToSlash(filepath.Join(home, "skills", "clawsynapse-managed", "demo-skill"))
	slashDirs := make([]string, 0, len(dirs))
	for _, d := range dirs {
		slashDirs = append(slashDirs, filepath.ToSlash(d))
	}
	if !contains(slashDirs, wantDir) {
		t.Errorf("external_dirs = %v, want %s", slashDirs, wantDir)
	}
}

func TestApplySkillUnsafeNameRejected(t *testing.T) {
	a, home := newWriteTestAdapter(t)
	ctx, cancel := ctxForTest()
	defer cancel()

	skillMd := writeSkillFile(t, home, "upload", "SKILL.md", "x")
	res, _ := a.ApplyCapabilitySet(ctx, &CapabilitySetRequest{
		Target: "skill", Action: "add", Skill: "../../evil",
		FileLocalPaths: []string{skillMd},
	})
	if res.OK || !strings.Contains(res.Error, "capability.invalid") {
		t.Fatalf("want unsafe-name rejection, got ok=%v err=%q", res.OK, res.Error)
	}
}

func TestApplySkillMissingFileIdsRejected(t *testing.T) {
	a, _ := newWriteTestAdapter(t)
	ctx, cancel := ctxForTest()
	defer cancel()

	res, _ := a.ApplyCapabilitySet(ctx, &CapabilitySetRequest{
		Target: "skill", Action: "add", Skill: "demo",
	})
	if res.OK || !strings.Contains(res.Error, "fileIds required") {
		t.Fatalf("want fileIds-required error, got ok=%v err=%q", res.OK, res.Error)
	}
}

func TestApplySkillMissingSkillMdRejected(t *testing.T) {
	a, home := newWriteTestAdapter(t)
	ctx, cancel := ctxForTest()
	defer cancel()

	// File present but not a SKILL.md → package validation fails, nothing registered.
	foo := writeSkillFile(t, home, "upload", "foo.txt", "not a skill")
	res, _ := a.ApplyCapabilitySet(ctx, &CapabilitySetRequest{
		Target: "skill", Action: "add", Skill: "demo",
		FileLocalPaths: []string{foo},
	})
	if res.OK || !strings.Contains(res.Error, "SKILL.md") {
		t.Fatalf("want missing-SKILL.md error, got ok=%v err=%q", res.OK, res.Error)
	}
	cfg := readConfigMap(t, a.hermesConfigPath())
	skills, _ := cfg["skills"].(map[string]any)
	if managed := stringList(skills["clawsynapse_managed"]); len(managed) != 0 {
		t.Errorf("managed = %v, want empty (failed skill must not be registered)", managed)
	}
}

func TestApplySkillDisableRemovesManagedEntry(t *testing.T) {
	a, home := newWriteTestAdapter(t)
	ctx, cancel := ctxForTest()
	defer cancel()

	skillMd := writeSkillFile(t, home, "upload", "SKILL.md", "x")
	_, _ = a.ApplyCapabilitySet(ctx, &CapabilitySetRequest{
		Target: "skill", Action: "add", Skill: "demo",
		FileLocalPaths: []string{skillMd},
	})
	res, _ := a.ApplyCapabilitySet(ctx, &CapabilitySetRequest{
		Target: "skill", Action: "disable", Skill: "demo",
	})
	if !res.OK && !strings.Contains(res.Error, "restart_failed") {
		t.Fatalf("disable failed: ok=%v err=%q", res.OK, res.Error)
	}
	cfg := readConfigMap(t, a.hermesConfigPath())
	skills, _ := cfg["skills"].(map[string]any)
	if managed := stringList(skills["clawsynapse_managed"]); len(managed) != 0 {
		t.Errorf("managed = %v, want empty after disable", managed)
	}
	// Files must remain on disk (recoverable).
	if _, err := os.Stat(filepath.Join(home, "skills", "clawsynapse-managed", "demo", "SKILL.md")); err != nil {
		t.Errorf("disabled skill files were deleted: %v", err)
	}
}

// ── cron (proxies gateway, no restart) ─────────────────────────────

type cronGateway struct{}

func (g *cronGateway) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"id":"job-new","name":"t"}`))
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"jobs":[]}`))
		default:
			w.WriteHeader(405)
		}
	})
	mux.HandleFunc("/api/jobs/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(204)
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	return mux
}

func newCronAdapter(t *testing.T) *HermesAdapter {
	t.Helper()
	srv := httptest.NewServer((&cronGateway{}).handler())
	t.Cleanup(srv.Close)
	a, err := NewHermesAdapter(HermesConfig{
		NodeID:     "n1",
		BaseURL:    srv.URL + "/v1",
		Model:      "hermes-agent",
		ConfigPath: filepath.Join(t.TempDir(), "config.yaml"),
	})
	if err != nil {
		t.Fatalf("NewHermesAdapter: %v", err)
	}
	a.restartGatewayFn = func(context.Context) error { return nil }
	return a
}

func TestApplyCronCreateProxiesGatewayNoRestart(t *testing.T) {
	a := newCronAdapter(t)
	ctx, cancel := ctxForTest()
	defer cancel()

	res, err := a.ApplyCapabilitySet(ctx, &CapabilitySetRequest{
		Target: "cron", Action: "create",
		Job: map[string]any{"name": "t", "schedule": "0 3 * * *", "prompt": "p"},
	})
	if err != nil {
		t.Fatalf("ApplyCapabilitySet: %v", err)
	}
	if !res.OK || res.RestartStatus != "none" {
		t.Fatalf("cron create: ok=%v restart=%q err=%q", res.OK, res.RestartStatus, res.Error)
	}
}

func TestApplyCronDeleteRequiresJobID(t *testing.T) {
	a := newCronAdapter(t)
	ctx, cancel := ctxForTest()
	defer cancel()

	res, _ := a.ApplyCapabilitySet(ctx, &CapabilitySetRequest{
		Target: "cron", Action: "delete",
	})
	if res.OK || !strings.Contains(res.Error, "jobId") {
		t.Fatalf("want jobId-required error, got ok=%v err=%q", res.OK, res.Error)
	}
}

func TestApplyCronDeleteProxiesGateway(t *testing.T) {
	a := newCronAdapter(t)
	ctx, cancel := ctxForTest()
	defer cancel()

	res, _ := a.ApplyCapabilitySet(ctx, &CapabilitySetRequest{
		Target: "cron", Action: "delete", JobID: "job-1",
	})
	if !res.OK || res.RestartStatus != "none" {
		t.Fatalf("cron delete: ok=%v restart=%q err=%q", res.OK, res.RestartStatus, res.Error)
	}
}

func TestApplyCapabilityUnknownTarget(t *testing.T) {
	a, _ := newWriteTestAdapter(t)
	ctx, cancel := ctxForTest()
	defer cancel()

	res, _ := a.ApplyCapabilitySet(ctx, &CapabilitySetRequest{
		Target: "bogus", Action: "x",
	})
	if res.OK || !strings.Contains(res.Error, "unknown target") {
		t.Fatalf("want unknown-target error, got ok=%v err=%q", res.OK, res.Error)
	}
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

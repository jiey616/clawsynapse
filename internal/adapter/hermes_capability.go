package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"clawsynapse/internal/protocol"
)

// capabilitiesSnapshot is a short-TTL cache of the last successful read so a
// busy TrustMesh frontend does not hammer the gateway / config file on every
// poll. The skill set is static between gateway restarts anyway.
type capabilitiesSnapshot struct {
	at     time.Time
	result *CapabilitiesResult
}

const capabilityCacheTTL = 30 * time.Second

// ── CapabilityProvider: read ───────────────────────────────────────

// Capabilities implements CapabilityProvider. It aggregates:
//   - skills: gateway GET /v1/skills
//   - models: local config.yaml custom_providers (gateway /v1/models only
//     ever returns a single agent name, so it cannot list providers)
//   - jobs:   gateway GET /api/jobs
//
// Any single failure degrades the whole read to available:false with a reason
// (the gateway being down is the common case). api_key is never echoed.
func (a *HermesAdapter) Capabilities(ctx context.Context) (*CapabilitiesResult, error) {
	a.capMu.Lock()
	defer a.capMu.Unlock()
	if a.capCache != nil && time.Since(a.capCache.at) < capabilityCacheTTL {
		return a.capCache.result, nil
	}

	skills, skillsErr := a.fetchSkills(ctx)
	models, modelsErr := a.fetchModels(ctx)
	jobs, jobsErr := a.fetchJobs(ctx)

	res := &CapabilitiesResult{
		Product:   "hermes",
		Available: true,
		Skills:    skills,
		Models:    models,
		Jobs:      jobs,
	}
	switch {
	case skillsErr != nil:
		res.Available = false
		res.Reason = "skills unavailable: " + skillsErr.Error()
	case modelsErr != nil:
		res.Available = false
		res.Reason = "models unavailable: " + modelsErr.Error()
	case jobsErr != nil:
		// Jobs are auxiliary; a failure degrades the reason but keeps available.
		res.Reason = "jobs unavailable: " + jobsErr.Error()
	default:
		// Attach recent executions to each job (best-effort; failures just
		// leave the executions list empty).
		if len(jobs) > 0 {
			a.loadJobExecutions(ctx, res.Jobs, 3)
		}
	}

	a.capCache = &capabilitiesSnapshot{at: time.Now(), result: res}
	return res, nil
}

// ── Read helpers ───────────────────────────────────────────────────

func (a *HermesAdapter) fetchSkills(ctx context.Context) ([]protocol.SkillInfo, error) {
	var out struct {
		Object string `json:"object"`
		Data   []struct {
			Name        string  `json:"name"`
			Description string  `json:"description"`
			Category    *string `json:"category"`
		} `json:"data"`
	}
	if _, err := a.callJSON(ctx, http.MethodGet, a.baseURL+"/skills", nil, &out); err != nil {
		return nil, err
	}
	skills := make([]protocol.SkillInfo, 0, len(out.Data))
	for _, s := range out.Data {
		cat := ""
		if s.Category != nil {
			cat = *s.Category
		}
		skills = append(skills, protocol.SkillInfo{Name: s.Name, Description: s.Description, Category: cat})
	}
	return skills, nil
}

func (a *HermesAdapter) fetchJobs(ctx context.Context) ([]protocol.CronJobInfo, error) {
	var out struct {
		Jobs []struct {
			ID              string          `json:"id"`
			Name            string          `json:"name"`
			Schedule        json.RawMessage `json:"schedule"` // string OR {kind, expr, display}
			ScheduleDisplay string          `json:"schedule_display"`
			Enabled         bool            `json:"enabled"`
			Prompt          string          `json:"prompt"`
			Skills          []string        `json:"skills"`
			NextRun         string          `json:"next_run"`
			NextRunAt       string          `json:"nextRunAt"`
		} `json:"jobs"`
	}
	if _, err := a.callJSON(ctx, http.MethodGet, a.rootURL()+"/api/jobs", nil, &out); err != nil {
		return nil, err
	}
	jobs := make([]protocol.CronJobInfo, 0, len(out.Jobs))
	for _, j := range out.Jobs {
		jobs = append(jobs, protocol.CronJobInfo{
			ID:       j.ID,
			Name:     j.Name,
			Schedule: extractJobSchedule(j.Schedule, j.ScheduleDisplay),
			Enabled:  j.Enabled,
			Prompt:   j.Prompt,
			Skills:   j.Skills,
			NextRun:  firstNonEmpty(j.NextRun, j.NextRunAt),
		})
	}
	return jobs, nil
}

// extractJobSchedule tolerates both plain string schedules and the nested
// {kind, expr, display} object the gateway uses.
func extractJobSchedule(raw json.RawMessage, display string) string {
	if s := strings.TrimSpace(display); s != "" {
		return s
	}
	if len(raw) == 0 {
		return ""
	}
	var plain string
	if err := json.Unmarshal(raw, &plain); err == nil {
		return strings.TrimSpace(plain)
	}
	var obj struct {
		Kind    string `json:"kind"`
		Expr    string `json:"expr"`
		Display string `json:"display"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		return firstNonEmpty(obj.Display, obj.Expr, obj.Kind)
	}
	return ""
}

// hermesConfigFile mirrors the parts of ~/.hermes/config.yaml that the
// capability module reads/writes. Unknown fields are preserved by editing the
// file with yaml.Node round-trips (see write side), so this struct only needs
// the fields we read.
type hermesConfigFile struct {
	Model struct {
		Default  string `yaml:"default"`
		Provider string `yaml:"provider"`
	} `yaml:"model"`
	CustomProviders []map[string]any `yaml:"custom_providers"`
}

func (a *HermesAdapter) fetchModels(ctx context.Context) ([]protocol.ModelInfo, error) {
	cfgPath := a.hermesConfigPath()
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("read hermes config: %w", err)
	}
	var cfg hermesConfigFile
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse hermes config: %w", err)
	}

	models := make([]protocol.ModelInfo, 0, len(cfg.CustomProviders))
	for i, p := range cfg.CustomProviders {
		provider := strAny(p["name"])
		model := strAny(p["model"])
		if model == "" {
			model = strAny(p["default_model"])
		}
		id := strAny(p["id"])
		if id == "" {
			id = provider
			if id == "" {
				id = fmt.Sprintf("provider-%d", i+1)
			}
		}
		isDefault := provider != "" && provider == cfg.Model.Provider &&
			(model == "" || model == cfg.Model.Default)
		models = append(models, protocol.ModelInfo{
			ID:        id,
			Provider:  provider,
			Model:     model,
			IsDefault: isDefault,
		})
	}
	// The default model may be a built-in provider (e.g. deepseek) configured
	// via the model section rather than a custom_providers entry. Always
	// surface it so the UI shows the model actually in use — this covers both
	// the legacy mode (no custom_providers) and mixed setups (custom provider
	// present while the default is a built-in provider).
	if defProvider := strings.TrimSpace(cfg.Model.Provider); defProvider != "" {
		found := false
		for _, m := range models {
			if m.Provider == defProvider {
				found = true
				break
			}
		}
		if !found {
			models = append(models, protocol.ModelInfo{
				ID:        defProvider,
				Provider:  defProvider,
				Model:     cfg.Model.Default,
				IsDefault: true,
			})
		}
	}
	return models, nil
}

// hermesConfigPath returns the config.yaml path used for capability reads and
// write-backs. Priority: configured value → $HERMES_HOME/config.yaml →
// ~/.hermes/config.yaml → /root/.hermes/config.yaml.
func (a *HermesAdapter) hermesConfigPath() string {
	if a.configPath != "" {
		return a.configPath
	}
	if h := strings.TrimSpace(os.Getenv("HERMES_HOME")); h != "" {
		return filepath.Join(h, "config.yaml")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".hermes", "config.yaml")
	}
	return "/root/.hermes/config.yaml"
}

// hermesHomeDir derives the hermes home directory from the same rules as
// hermesConfigPath (the directory that contains config.yaml), used to locate
// the managed skill root ($HERMES_HOME/skills/clawsynapse-managed).
func (a *HermesAdapter) hermesHomeDir() string {
	return filepath.Dir(a.hermesConfigPath())
}

// jsonBody is a small helper for endpoints that return raw JSON objects.
func jsonBody(ctx context.Context, body []byte, out any) error {
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, out)
}

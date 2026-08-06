package adapter

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// managedSkillsSubdir is the isolated directory where remotely-managed skills
// live. The write-back path only ever places files under this root — the blast
// radius of a malicious/compromised peer is locked inside the managed area.
const managedSkillsSubdir = "skills/clawsynapse-managed"

// managedConfigKey is the custom config.yaml key holding the list of managed
// skill directories (kept separate from the user's base external_dirs so a
// write-back never destroys hand-configured entries).
const managedConfigKey = "clawsynapse_managed"

var safeSkillNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ── CapabilityProvider: write ──────────────────────────────────────

// ApplyCapabilitySet implements CapabilityProvider, branching on target:
//
//	skill: add/update (file → managed dir) · enable/disable (managed key)
//	       → edit config.yaml + validate + restart gateway
//	model: add (custom_provider) · switch (config.model) · delete (provider,
//	       deleting the current default is rejected)
//	       → edit config.yaml + validate + restart gateway
//	cron:  create/update/delete/pause/resume/run → proxy gateway /api/jobs,
//	       no restart
func (a *HermesAdapter) ApplyCapabilitySet(ctx context.Context, req *CapabilitySetRequest) (*CapabilitySetResult, error) {
	if req == nil {
		return nil, fmt.Errorf("nil capability set request")
	}
	var res *CapabilitySetResult
	switch req.Target {
	case "skill":
		res, _ = a.applySkillSet(ctx, req)
	case "model":
		res, _ = a.applyModelSet(ctx, req)
	case "cron":
		res, _ = a.applyCronSet(ctx, req)
	default:
		return &CapabilitySetResult{
			OK:            false,
			Target:        req.Target,
			Action:        req.Action,
			RestartStatus: "none",
			Error:         "capability.invalid: unknown target " + req.Target,
		}, nil
	}
	// 写回成功后清缓存：下一次读立即反映新状态，避免 30s TTL 内读到旧值
	// （曾导致"切换模型显示已同步但实际还是旧默认"）。
	if res != nil && res.OK {
		a.invalidateCache()
	}
	return res, nil
}

// ── skill ──────────────────────────────────────────────────────────

func (a *HermesAdapter) applySkillSet(_ context.Context, req *CapabilitySetRequest) (*CapabilitySetResult, error) {
	if !safeSkillNameRe.MatchString(req.Skill) {
		return skillFail(req, "capability.invalid: unsafe skill name")
	}

	switch req.Action {
	case "add", "update":
		if len(req.FileLocalPaths) == 0 {
			return skillFail(req, "capability.invalid: fileIds required for skill add/update")
		}
		if err := a.installSkillFiles(req.Skill, req.FileLocalPaths); err != nil {
			return skillFail(req, "capability.invalid: install skill files: "+err.Error())
		}
	case "enable", "disable":
		// No file operation; just toggle the managed key below.
	default:
		return skillFail(req, "capability.invalid: unknown skill action "+req.Action)
	}

	cfg, err := a.loadConfigMap()
	if err != nil {
		return skillFail(req, "capability.unavailable: read config: "+err.Error())
	}

	skills := skillsSection(cfg)
	managed := stringList(skills[managedConfigKey])
	switch req.Action {
	case "add", "update", "enable":
		managed = appendUnique(managed, req.Skill)
	case "disable":
		managed = removeString(managed, req.Skill)
	}
	skills[managedConfigKey] = managed
	skills["external_dirs"] = mergeExternalDirs(
		stringList(skills["external_dirs"]),
		managedDirPaths(a.hermesHomeDir(), managed),
	)

	// Validate-then-persist: a marshal/unmarshal round-trip failure aborts
	// before the config is written, so a broken skill can never brick the
	// gateway. (Skill files themselves stay on disk — they are recoverable.)
	if err := a.saveConfigMap(cfg); err != nil {
		return skillFail(req, "capability.invalid: config validation failed: "+err.Error())
	}

	a.logCapabilitySet("skill", req.Action, req.Skill, "")
	return a.restartAndReport("skill", req.Action, req.Skill, "")
}

func skillFail(req *CapabilitySetRequest, msg string) (*CapabilitySetResult, error) {
	return &CapabilitySetResult{
		OK: false, Target: "skill", Action: req.Action,
		Skill: req.Skill, RestartStatus: "none", Error: msg,
	}, nil
}

// installSkillFiles copies the resolved local files into the managed skill
// directory and validates the package has a SKILL.md. A single .zip source is
// extracted (with path-traversal protection); other files are copied as-is.
func (a *HermesAdapter) installSkillFiles(skill string, localPaths []string) error {
	destDir := filepath.Join(a.hermesHomeDir(), managedSkillsSubdir, skill)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	for _, src := range localPaths {
		name := filepath.Base(src)
		if name == "" || name == "." || name == "/" || strings.Contains(name, "..") {
			return fmt.Errorf("invalid file name from transfer: %s", name)
		}
		if strings.EqualFold(filepath.Ext(name), ".zip") {
			if err := extractZip(src, destDir); err != nil {
				return err
			}
			continue
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("read %s: %w", src, err)
		}
		if err := os.WriteFile(filepath.Join(destDir, name), data, 0o644); err != nil {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(destDir, "SKILL.md")); err != nil {
		return fmt.Errorf("skill package missing SKILL.md")
	}
	return nil
}

// extractZip safely extracts a zip skill package into destDir, rejecting
// entries that escape the destination directory.
func extractZip(zipPath, destDir string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open zip %s: %w", zipPath, err)
	}
	defer zr.Close()

	for _, f := range zr.File {
		target := filepath.Join(destDir, f.Name)
		// Reject path traversal and absolute paths.
		if !strings.HasPrefix(target, filepath.Clean(destDir)+string(os.PathSeparator)) {
			return fmt.Errorf("zip entry escapes destination: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("open zip entry %s: %w", f.Name, err)
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		closeErr := out.Close()
		if err != nil {
			return fmt.Errorf("extract %s: %w", f.Name, err)
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

// ── model ──────────────────────────────────────────────────────────

func (a *HermesAdapter) applyModelSet(_ context.Context, req *CapabilitySetRequest) (*CapabilitySetResult, error) {
	cfg, err := a.loadConfigMap()
	if err != nil {
		return modelFail(req, "capability.unavailable: read config: "+err.Error())
	}

	switch req.Action {
	case "add":
		if req.Provider == nil || strAny(req.Provider["name"]) == "" {
			return modelFail(req, "capability.invalid: model add requires provider.name")
		}
		name := strAny(req.Provider["name"])
		custom := customProviders(cfg)
		idx := findProviderIndex(custom, name)
		if idx >= 0 {
			custom[idx] = req.Provider
		} else {
			custom = append(custom, req.Provider)
		}
		cfg["custom_providers"] = custom
		if d := strAny(req.Provider["default_model"]); d != "" {
			m := modelSection(cfg)
			m["default"] = d
			m["provider"] = name
			// Mirror the provider's base_url so the model section does not
			// keep a stale endpoint from a previously defaulted provider.
			syncModelBaseURL(m, req.Provider)
		}

	case "switch":
		modelName := strings.TrimSpace(req.Model)
		if modelName == "" {
			return modelFail(req, "capability.invalid: model switch requires model")
		}
		m := modelSection(cfg)
		custom := customProviders(cfg)
		matched := false

		// 1) modelName matches a custom provider name → use its model name.
		if idx := findProviderIndex(custom, modelName); idx >= 0 {
			prov := custom[idx]
			mdl := strAny(prov["model"])
			if mdl == "" {
				mdl = strAny(prov["default_model"])
			}
			if mdl != "" {
				m["default"] = mdl
			}
			m["provider"] = modelName
			syncModelBaseURL(m, prov)
			matched = true
		}
		// 2) modelName matches a provider's model name → resolve its provider.
		if !matched {
			for _, prov := range custom {
				if strAny(prov["model"]) == modelName || strAny(prov["default_model"]) == modelName {
					m["default"] = modelName
					m["provider"] = strAny(prov["name"])
					syncModelBaseURL(m, prov)
					matched = true
					break
				}
			}
		}
		// 3) Built-in provider (no custom_providers entry): we cannot reliably
		// map a provider name to its model id (hermes owns that table), so
		// refuse instead of writing a broken model.default that could stop the
		// gateway from booting. Add it as a custom provider first, or configure
		// it via deployment env + the model section.
		if !matched {
			return modelFail(req, "capability.invalid: built-in provider switch is unsupported; add the provider via model add first (or configure it through deployment env + model section)")
		}

	case "delete":
		name := strings.TrimSpace(req.Model)
		if name == "" {
			return modelFail(req, "capability.invalid: model delete requires model")
		}
		custom := customProviders(cfg)
		idx := findProviderIndex(custom, name)
		if idx < 0 {
			return modelFail(req, "capability.invalid: provider not found: "+name)
		}
		// Guard: deleting the current default model would leave the agent
		// with no runnable model — require a switch first.
		if strAny(modelSection(cfg)["provider"]) == name {
			return modelFail(req, "capability.invalid: cannot delete the current default model; switch to another model first")
		}
		custom = append(custom[:idx], custom[idx+1:]...)
		cfg["custom_providers"] = custom

	default:
		return modelFail(req, "capability.invalid: unknown model action "+req.Action)
	}

	if err := a.saveConfigMap(cfg); err != nil {
		return modelFail(req, "capability.invalid: config validation failed: "+err.Error())
	}

	a.logCapabilitySet("model", req.Action, req.Model, "")
	return a.restartAndReport("model", req.Action, req.Model, "")
}

func modelFail(req *CapabilitySetRequest, msg string) (*CapabilitySetResult, error) {
	return &CapabilitySetResult{
		OK: false, Target: "model", Action: req.Action,
		Model: req.Model, RestartStatus: "none", Error: msg,
	}, nil
}

// ── cron (proxies gateway native endpoints, no restart) ────────────

func (a *HermesAdapter) applyCronSet(ctx context.Context, req *CapabilitySetRequest) (*CapabilitySetResult, error) {
	base := a.rootURL()
	var method, path string
	var body any

	switch req.Action {
	case "create":
		method, path, body = http.MethodPost, base+"/api/jobs", req.Job
	case "update":
		if req.JobID == "" {
			return cronFail(req, "capability.invalid: cron update requires jobId")
		}
		method, path, body = http.MethodPatch, base+"/api/jobs/"+req.JobID, req.Job
	case "delete":
		if req.JobID == "" {
			return cronFail(req, "capability.invalid: cron delete requires jobId")
		}
		method, path = http.MethodDelete, base+"/api/jobs/"+req.JobID
	case "pause":
		if req.JobID == "" {
			return cronFail(req, "capability.invalid: cron pause requires jobId")
		}
		method, path = http.MethodPost, base+"/api/jobs/"+req.JobID+"/pause"
	case "resume":
		if req.JobID == "" {
			return cronFail(req, "capability.invalid: cron resume requires jobId")
		}
		method, path = http.MethodPost, base+"/api/jobs/"+req.JobID+"/resume"
	case "run":
		if req.JobID == "" {
			return cronFail(req, "capability.invalid: cron run requires jobId")
		}
		method, path = http.MethodPost, base+"/api/jobs/"+req.JobID+"/run"
	default:
		return cronFail(req, "capability.invalid: unknown cron action "+req.Action)
	}

	var out map[string]any
	if _, err := a.callJSON(ctx, method, path, body, &out); err != nil {
		return cronFail(req, "capability.unavailable: gateway /api/jobs: "+err.Error())
	}

	a.logCapabilitySet("cron", req.Action, req.JobID, "")
	return &CapabilitySetResult{
		OK: true, Target: "cron", Action: req.Action,
		JobID: req.JobID, RestartStatus: "none",
	}, nil
}

func cronFail(req *CapabilitySetRequest, msg string) (*CapabilitySetResult, error) {
	return &CapabilitySetResult{
		OK: false, Target: "cron", Action: req.Action,
		JobID: req.JobID, RestartStatus: "none", Error: msg,
	}, nil
}

// ── config.yaml editing (preserves unknown keys) ───────────────────

func (a *HermesAdapter) loadConfigMap() (map[string]any, error) {
	data, err := os.ReadFile(a.hermesConfigPath())
	if err != nil {
		return nil, err
	}
	var cfg map[string]any
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return cfg, nil
}

// saveConfigMap validates (marshal + re-parse) then atomically writes the
// config so a bad edit can never brick the gateway at the next boot.
func (a *HermesAdapter) saveConfigMap(cfg map[string]any) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	var check map[string]any
	if err := yaml.Unmarshal(data, &check); err != nil {
		return err
	}
	path := a.hermesConfigPath()
	tmp := path + ".capability-tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func skillsSection(cfg map[string]any) map[string]any {
	s, _ := cfg["skills"].(map[string]any)
	if s == nil {
		s = map[string]any{}
		cfg["skills"] = s
	}
	return s
}

func modelSection(cfg map[string]any) map[string]any {
	m, _ := cfg["model"].(map[string]any)
	if m == nil {
		m = map[string]any{}
		cfg["model"] = m
	}
	return m
}

func customProviders(cfg map[string]any) []map[string]any {
	raw, ok := cfg["custom_providers"].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func findProviderIndex(providers []map[string]any, name string) int {
	for i, p := range providers {
		if strAny(p["name"]) == name {
			return i
		}
	}
	return -1
}

// syncModelBaseURL mirrors a provider's base_url onto the model section (or
// clears a stale one) so switching providers does not leave a wrong endpoint.
func syncModelBaseURL(m map[string]any, prov map[string]any) {
	if bu := strAny(prov["base_url"]); bu != "" {
		m["base_url"] = bu
	} else {
		delete(m, "base_url")
	}
}

func stringList(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s := strAny(item); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func appendUnique(list []string, v string) []string {
	for _, item := range list {
		if item == v {
			return list
		}
	}
	return append(list, v)
}

func removeString(list []string, v string) []string {
	out := make([]string, 0, len(list))
	for _, item := range list {
		if item != v {
			out = append(out, item)
		}
	}
	return out
}

func managedDirPaths(home string, managed []string) []string {
	out := make([]string, 0, len(managed))
	for _, name := range managed {
		out = append(out, filepath.Join(home, managedSkillsSubdir, name))
	}
	return out
}

func mergeExternalDirs(base, managed []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(base)+len(managed))
	for _, d := range append(append([]string{}, base...), managed...) {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		if _, ok := seen[d]; ok {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	return out
}

// ── gateway restart ────────────────────────────────────────────────

// restartAndReport restarts the gateway process and reports the outcome.
// Session continuity survives the restart because gateway sessions are
// persisted to ~/.hermes/state.db (SQLite) — only a few seconds of downtime.
func (a *HermesAdapter) restartAndReport(target, action, skill, model string) (*CapabilitySetResult, error) {
	res := &CapabilitySetResult{
		OK: true, Target: target, Action: action, Skill: skill, Model: model,
	}
	restart := a.restartGatewayFn
	if restart == nil {
		restart = a.restartGateway
	}
	if err := restart(context.Background()); err != nil {
		res.OK = false
		res.RestartStatus = "restart_failed"
		res.Error = "capability.restart_failed: " + err.Error()
		return res, nil
	}
	res.RestartStatus = "restarted"
	return res, nil
}

// restartGateway finds the `hermes gateway run` process, kills it, waits for
// the port to free up, then re-launches it with the same environment and
// polls /health until it is back.
func (a *HermesAdapter) restartGateway(ctx context.Context) error {
	port := a.gatewayPort()

	pids, err := findGatewayPIDs()
	if err == nil {
		for _, pid := range pids {
			_ = exec.Command("kill", pid).Run()
		}
	} else if a.log != nil {
		a.log.Warn("find gateway pid failed", slog.String("error", err.Error()))
	}
	if err := waitPortClosed(ctx, port, 15*time.Second); err != nil {
		return fmt.Errorf("gateway port %s did not free: %w", port, err)
	}

	cmd := exec.Command("hermes", "gateway", "run")
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("relaunch gateway: %w", err)
	}
	_ = cmd.Process.Release()

	return waitHealthy(ctx, a.rootURL()+"/health", 30*time.Second)
}

func findGatewayPIDs() ([]string, error) {
	out, err := exec.Command("pgrep", "-f", "hermes gateway").Output()
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// gatewayPort extracts the gateway port from the adapter baseURL so restart
// targets the right listener.
func (a *HermesAdapter) gatewayPort() string {
	u, err := url.Parse(a.rootURL())
	if err != nil {
		return "8642"
	}
	if p := u.Port(); p != "" {
		return p
	}
	return "8642"
}

func waitPortClosed(ctx context.Context, port string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 500*time.Millisecond)
		if err != nil {
			return nil
		}
		_ = conn.Close()
		if err := sleepCtx(ctx, 300*time.Millisecond); err != nil {
			return err
		}
	}
	return fmt.Errorf("port %s still open after %s", port, timeout)
}

func waitHealthy(ctx context.Context, healthURL string, timeout time.Duration) error {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
		}
		if err := sleepCtx(ctx, 500*time.Millisecond); err != nil {
			return err
		}
	}
	return fmt.Errorf("gateway health not OK within %s", timeout)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func (a *HermesAdapter) logCapabilitySet(target, action, subject, fileIDs string) {
	if a.log == nil {
		return
	}
	a.log.Info("capability set applied",
		slog.String("target", target),
		slog.String("action", action),
		slog.String("subject", subject),
		slog.String("fileIds", fileIDs),
	)
}

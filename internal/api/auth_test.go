package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"clawsynapse/internal/config"
	"clawsynapse/internal/discovery"
	"clawsynapse/pkg/types"
	"gopkg.in/yaml.v3"
)

func newAuthTestServer(t *testing.T, token string) *Server {
	t.Helper()
	return &Server{
		apiToken:   token,
		peers:      discovery.NewRegistry(),
		cfg: config.Config{
			NATSServers:      []string{"nats://10.0.0.1:4222"},
			TrustMode:        "open",
			AgentAdapter:     "hermes",
			HermesGatewayKey: "sk-live-secret-123",
			HermesModel:      "hermes-agent",
			HermesTodoMode:   "runs",
			LogLevel:         "info",
			LogFormat:        "json",
		},
		configPath: filepath.Join(t.TempDir(), "config.json"),
	}
}

// TestRequireBearer_Enforced (Phase 3.4): protected endpoints demand the
// bearer token; /v1/health and /v1/auth/challenge stay open.
func TestRequireBearer_Enforced(t *testing.T) {
	const token = "test-token-abc"
	srv := newAuthTestServer(t, token)
	handler := requireBearer(token, srv.routes())

	// No token -> 401.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/peers", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401", rec.Code)
	}

	// Wrong token -> 401.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/peers", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d, want 401", rec.Code)
	}

	// Correct token -> passes auth (endpoint may still 4xx/5xx on missing
	// deps, but must not be 401).
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/peers", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("valid token rejected: %d", rec.Code)
	}

	// Open paths bypass auth entirely.
	for _, path := range []string{"/v1/health", "/v1/auth/challenge"} {
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusUnauthorized {
			t.Fatalf("%s should be open, got 401", path)
		}
	}
}

// TestLoadOrCreateAPIToken: generated once, stable across restarts, 0600.
func TestLoadOrCreateAPIToken(t *testing.T) {
	dir := t.TempDir()

	tok1, err := LoadOrCreateAPIToken(dir)
	if err != nil || len(tok1) != 64 {
		t.Fatalf("first load: tok=%q err=%v", tok1, err)
	}
	tok2, err := LoadOrCreateAPIToken(dir)
	if err != nil || tok2 != tok1 {
		t.Fatalf("restart changed token: %q vs %q (err=%v)", tok1, tok2, err)
	}
	info, err := os.Stat(filepath.Join(dir, "api_token"))
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("token file mode = %o, want no group/other bits", perm)
	}
}

// TestHandleConfigGet_RedactsSecret (Phase 3.4): the gateway key must never
// leave the node in plaintext.
func TestHandleConfigGet_RedactsSecret(t *testing.T) {
	srv := newAuthTestServer(t, "")

	rec := httptest.NewRecorder()
	srv.handleConfigGet(rec, httptest.NewRequest(http.MethodGet, "/v1/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}

	var result types.APIResult
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	raw, _ := json.Marshal(result.Data)
	if bytes.Contains(raw, []byte("sk-live-secret-123")) {
		t.Fatal("response leaked the gateway key")
	}
	cfgData := result.Data["config"].(map[string]any)
	if cfgData["hermesGatewayKey"] != config.RedactedPlaceholder {
		t.Fatalf("hermesGatewayKey = %v, want placeholder", cfgData["hermesGatewayKey"])
	}
}

// TestHandleConfigSave_Whitelist (Phase 3.4): only whitelisted fields are
// applied; identity/NATS/storage fields cannot be rewritten; the redacted
// placeholder keeps the stored secret.
func TestHandleConfigSave_Whitelist(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthTestServer(t, "")
	srv.configPath = filepath.Join(dir, "config.json")

	body := map[string]any{
		"natsServers":        []string{"nats://evil:4222"},
		"dataDir":            "/evil",
		"trustMode":          "open",
		"hermesModel":        "hermes-agent-pro",
		"hermesGatewayKey":   config.RedactedPlaceholder,
		"agentAdapter":       "codex",
		"logLevel":           "debug",
		"maxConcurrentRuns1": 1,
		"task": map[string]any{
			"maxConcurrentRuns": 4,
			"queueCapacity":     20,
			"runTimeout":        "45m",
			"queueWaitTimeout":  "4m",
		},
	}
	raw, _ := json.Marshal(body)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/config", bytes.NewReader(raw))
	srv.handleConfigSave(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("save got %d: %s", rec.Code, rec.Body.String())
	}

	// The handler merges onto a copy and persists it as YAML (apply on
	// restart); assert against the saved file, not the in-memory config.
	persisted, err := os.ReadFile(srv.configPath)
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	var saved struct {
		NATSServers      []string           `yaml:"natsServers"`
		DataDir          string             `yaml:"dataDir"`
		AgentAdapter     string             `yaml:"agentAdapter"`
		HermesGatewayKey string             `yaml:"hermesGatewayKey"`
		HermesModel      string             `yaml:"hermesModel"`
		LogLevel         string             `yaml:"logLevel"`
		Task             *config.TaskConfig `yaml:"task"`
	}
	if err := yaml.Unmarshal(persisted, &saved); err != nil {
		t.Fatalf("decode saved config: %v", err)
	}
	if saved.HermesModel != "hermes-agent-pro" {
		t.Fatalf("hermesModel = %q, want whitelisted update", saved.HermesModel)
	}
	if saved.LogLevel != "debug" {
		t.Fatalf("logLevel = %q, want whitelisted update", saved.LogLevel)
	}
	if saved.Task == nil || saved.Task.MaxConcurrentRuns != 4 {
		t.Fatalf("task = %+v, want whitelisted update", saved.Task)
	}
	if len(saved.NATSServers) == 0 || saved.NATSServers[0] != "nats://10.0.0.1:4222" {
		t.Fatalf("natsServers rewritten to %v — whitelist breach", saved.NATSServers)
	}
	if saved.DataDir == "/evil" {
		t.Fatal("dataDir rewritten — whitelist breach")
	}
	if saved.AgentAdapter != "hermes" {
		t.Fatalf("agentAdapter rewritten to %q — whitelist breach", saved.AgentAdapter)
	}
	if saved.HermesGatewayKey != "sk-live-secret-123" {
		t.Fatalf("gateway key = %q, want stored secret preserved on placeholder", saved.HermesGatewayKey)
	}
}

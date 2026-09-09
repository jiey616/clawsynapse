package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type fileConfig struct {
	NATSServers         []string `yaml:"natsServers"`
	LocalAPIAddr        string   `yaml:"localApiAddr"`
	DataDir             string   `yaml:"dataDir"`
	IdentityKeyPath     string   `yaml:"identityKeyPath"`
	IdentityPubPath     string   `yaml:"identityPubPath"`
	HeartbeatInterval   string   `yaml:"heartbeatInterval"`
	AnnounceTTL         string   `yaml:"announceTtl"`
	TrustMode           string   `yaml:"trustMode"`
	TrustAutoApprove    *bool    `yaml:"trustAutoApprove"`
	RoleAnchor          *bool    `yaml:"roleAnchor"`
	AgentAdapter        string   `yaml:"agentAdapter"`
	AgentAdapterTimeout string   `yaml:"agentAdapterTimeout"`
	AgentRole           string   `yaml:"agentRole"`
	HermesGatewayURL    string   `yaml:"hermesGatewayUrl"`
	HermesGatewayKey    string   `yaml:"hermesGatewayKey"`
	HermesModel         string   `yaml:"hermesModel"`
	HermesConfigPath    string   `yaml:"hermesConfigPath"`
	HermesTodoMode      string   `yaml:"hermesTodoMode"`
	Task                *TaskConfig `yaml:"task"`
	WebhookURL          string   `yaml:"webhookUrl"`
	LogFilePath         string   `yaml:"logFilePath"`
	LogRotateMaxSizeMB  *int     `yaml:"logRotateMaxSizeMb"`
	LogRotateMaxBackups *int     `yaml:"logRotateMaxBackups"`
	LogRotateMaxAgeDays *int     `yaml:"logRotateMaxAgeDays"`
	LogRotateCompress   *bool    `yaml:"logRotateCompress"`
	DeliverablePrefixes []string `yaml:"deliverablePrefixes"`
	TransferDir         string   `yaml:"transferDir"`
	TransferMaxFileSize *int64   `yaml:"transferMaxFileSize"`
	TransferTTL         string   `yaml:"transferTtl"`
	LogLevel            string   `yaml:"logLevel"`
	LogFormat           string   `yaml:"logFormat"`
	LogAddSource        *bool    `yaml:"logAddSource"`
}

func toFileConfig(cfg Config) fileConfig {
	las := cfg.LogAddSource
	taa := cfg.TrustAutoApprove
	ra := cfg.RoleAnchor
	mfs := cfg.TransferMaxFileSize
	var task *TaskConfig
	if cfg.Task != nil {
		t := *cfg.Task
		task = &t
	}
	return fileConfig{
		NATSServers:         cfg.NATSServers,
		LocalAPIAddr:        cfg.LocalAPIAddr,
		DataDir:             cfg.DataDir,
		IdentityKeyPath:     cfg.IdentityKeyPath,
		IdentityPubPath:     cfg.IdentityPubPath,
		HeartbeatInterval:   cfg.HeartbeatInterval,
		AnnounceTTL:         cfg.AnnounceTTL,
		TrustMode:           cfg.TrustMode,
		TrustAutoApprove:    &taa,
		AgentAdapter:        cfg.AgentAdapter,
		AgentAdapterTimeout: cfg.AgentAdapterTimeout,
		AgentRole:           cfg.AgentRole,
		HermesGatewayURL:    cfg.HermesGatewayURL,
		HermesGatewayKey:    cfg.HermesGatewayKey,
		HermesModel:         cfg.HermesModel,
		HermesConfigPath:    cfg.HermesConfigPath,
		HermesTodoMode:      cfg.HermesTodoMode,
		Task:                task,
		WebhookURL:          cfg.WebhookURL,
		LogFilePath:         cfg.LogFilePath,
		LogRotateMaxSizeMB:  &cfg.LogRotateMaxSizeMB,
		LogRotateMaxBackups: &cfg.LogRotateMaxBackups,
		LogRotateMaxAgeDays: &cfg.LogRotateMaxAgeDays,
		LogRotateCompress:   &cfg.LogRotateCompress,
		DeliverablePrefixes: cfg.DeliverablePrefixes,
		TransferDir:         cfg.TransferDir,
		TransferMaxFileSize: &mfs,
		TransferTTL:         cfg.TransferTTL,
		LogLevel:            cfg.LogLevel,
		LogFormat:           cfg.LogFormat,
		LogAddSource:        &las,
		RoleAnchor:          &ra,
	}
}

func loadConfigValues(path string, required bool) (configValues, error) {
	path, err := expandPath(path)
	if err != nil {
		return configValues{}, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !required {
			return configValues{}, nil
		}
		return configValues{}, fmt.Errorf("read config file: %w", err)
	}

	var cfg fileConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return configValues{}, fmt.Errorf("parse config file: %w", err)
	}

	values := configValues{
		NATSServers:         cloneStrings(cfg.NATSServers),
		LocalAPIAddr:        strings.TrimSpace(cfg.LocalAPIAddr),
		DataDir:             strings.TrimSpace(cfg.DataDir),
		IdentityKeyPath:     strings.TrimSpace(cfg.IdentityKeyPath),
		IdentityPubPath:     strings.TrimSpace(cfg.IdentityPubPath),
		TrustMode:           strings.TrimSpace(cfg.TrustMode),
		AgentAdapter:        strings.TrimSpace(cfg.AgentAdapter),
		AgentAdapterTimeout: parseDurationValue(cfg.AgentAdapterTimeout, 0),
		AgentRole:           strings.TrimSpace(cfg.AgentRole),
		HermesGatewayURL:    strings.TrimSpace(cfg.HermesGatewayURL),
		HermesGatewayKey:    strings.TrimSpace(cfg.HermesGatewayKey),
		HermesModel:         strings.TrimSpace(cfg.HermesModel),
		HermesConfigPath:    strings.TrimSpace(cfg.HermesConfigPath),
		HermesTodoMode:      strings.TrimSpace(cfg.HermesTodoMode),
		WebhookURL:          strings.TrimSpace(cfg.WebhookURL),
		LogFilePath:         strings.TrimSpace(cfg.LogFilePath),
		DeliverablePrefixes: cloneStrings(cfg.DeliverablePrefixes),
		TransferDir:         strings.TrimSpace(cfg.TransferDir),
		LogLevel:            strings.TrimSpace(cfg.LogLevel),
		LogFormat:           strings.TrimSpace(cfg.LogFormat),
	}
	if cfg.LogRotateMaxSizeMB != nil {
		values.LogRotateMaxSizeMB = *cfg.LogRotateMaxSizeMB
	}
	if cfg.LogRotateMaxBackups != nil {
		values.LogRotateMaxBackups = *cfg.LogRotateMaxBackups
	}
	if cfg.LogRotateMaxAgeDays != nil {
		values.LogRotateMaxAgeDays = *cfg.LogRotateMaxAgeDays
	}
	if cfg.LogRotateCompress != nil {
		values.LogRotateCompress = *cfg.LogRotateCompress
	}
	if cfg.LogAddSource != nil {
		values.LogAddSource = *cfg.LogAddSource
	}
	if cfg.TrustAutoApprove != nil {
		values.TrustAutoApprove = *cfg.TrustAutoApprove
		values.TrustAutoApproveSet = true
	}
	if cfg.RoleAnchor != nil {
		values.RoleAnchor = *cfg.RoleAnchor
		values.RoleAnchorSet = true
	}
	if cfg.Task != nil {
		if cfg.Task.MaxConcurrentRuns > 0 {
			values.TaskMaxConcurrentRuns = cfg.Task.MaxConcurrentRuns
		}
		if cfg.Task.QueueCapacity > 0 {
			values.TaskQueueCapacity = cfg.Task.QueueCapacity
		}
		if strings.TrimSpace(cfg.Task.RunTimeout) != "" {
			values.TaskRunTimeout = parseDurationValue(cfg.Task.RunTimeout, 0)
		}
		if strings.TrimSpace(cfg.Task.QueueWaitTimeout) != "" {
			values.TaskQueueWaitTimeout = parseDurationValue(cfg.Task.QueueWaitTimeout, 0)
		}
	}

	if cfg.HeartbeatInterval != "" {
		values.Heartbeat = parseDurationValue(cfg.HeartbeatInterval, 0)
	}
	if cfg.AnnounceTTL != "" {
		values.AnnounceTTL = parseDurationValue(cfg.AnnounceTTL, 0)
	}
	if cfg.TransferMaxFileSize != nil {
		values.TransferMaxFileSize = *cfg.TransferMaxFileSize
	}
	if cfg.TransferTTL != "" {
		values.TransferTTL = parseDurationValue(cfg.TransferTTL, 0)
	}

	return values, nil
}

func loadDotEnvValues() configValues {
	wd, err := os.Getwd()
	if err != nil {
		return configValues{}
	}
	root := findProjectRoot(wd)

	data, err := os.ReadFile(filepath.Join(root, ".env"))
	if err != nil {
		return configValues{}
	}

	entries := parseDotEnv(string(data))
	return loadValuesFromMap(entries, "dotenv")
}

func findProjectRoot(start string) string {
	dir := start
	for {
		if fileExists(filepath.Join(dir, ".git")) || fileExists(filepath.Join(dir, "go.mod")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return start
		}
		dir = parent
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func loadOSEnvValues() configValues {
	entries := make(map[string]string)
	for _, raw := range os.Environ() {
		key, value, ok := strings.Cut(raw, "=")
		if !ok {
			continue
		}
		entries[key] = value
	}
	return loadValuesFromMap(entries, "env")
}

// loadValuesFromMap resolves a flat key/value map (OS environ or .env) into
// config values. Canonical keys carry the CLAWSYNAPSE_ prefix; the bare
// legacy name is accepted as fallback. src names the layer ("env" or
// "dotenv") and is recorded per resolved key (suffix "-legacy" when the
// unprefixed name supplied the value).
func loadValuesFromMap(values map[string]string, src string) configValues {
	lookup := func(legacyKey string) (string, bool) {
		if v := strings.TrimSpace(values["CLAWSYNAPSE_"+legacyKey]); v != "" {
			return v, false
		}
		if v := strings.TrimSpace(values[legacyKey]); v != "" {
			return v, true
		}
		return "", false
	}
	tag := func(v *configValues, key, legacyKey string, vRaw string, legacy bool) {
		name := src
		if legacy {
			name = src + "-legacy"
		}
		_ = vRaw
		v.setSource(key, name)
	}
	cfg := configValues{
		NATSServers:         splitCSV(mustLookup(values, lookup, "NATS_SERVERS")),
		LocalAPIAddr:        strings.TrimSpace(mustLookup(values, lookup, "LOCAL_API_ADDR")),
		DataDir:             strings.TrimSpace(mustLookup(values, lookup, "DATA_DIR")),
		IdentityKeyPath:     strings.TrimSpace(mustLookup(values, lookup, "IDENTITY_KEY_PATH")),
		IdentityPubPath:     strings.TrimSpace(mustLookup(values, lookup, "IDENTITY_PUB_PATH")),
		Heartbeat:           parseDurationValue(mustLookup(values, lookup, "HEARTBEAT_INTERVAL_MS"), 0),
		AnnounceTTL:         parseDurationValue(mustLookup(values, lookup, "ANNOUNCE_TTL_MS"), 0),
		TrustMode:           strings.TrimSpace(mustLookup(values, lookup, "TRUST_MODE")),
		AgentAdapter:        strings.TrimSpace(mustLookup(values, lookup, "AGENT_ADAPTER")),
		AgentAdapterTimeout: parseDurationValue(mustLookup(values, lookup, "AGENT_ADAPTER_TIMEOUT"), 0),
		AgentRole:           strings.TrimSpace(mustLookup(values, lookup, "AGENT_ROLE")),
		TaskMaxConcurrentRuns: int(parseIntValue(mustLookup(values, lookup, "TASK_MAX_CONCURRENT_RUNS"))),
		TaskQueueCapacity:     int(parseIntValue(mustLookup(values, lookup, "TASK_QUEUE_CAPACITY"))),
		TaskRunTimeout:        parseDurationValue(mustLookup(values, lookup, "TASK_RUN_TIMEOUT"), 0),
		TaskQueueWaitTimeout:  parseDurationValue(mustLookup(values, lookup, "TASK_QUEUE_WAIT_TIMEOUT"), 0),
		WebhookURL:          strings.TrimSpace(mustLookup(values, lookup, "WEBHOOK_URL")),
		LogFilePath:         strings.TrimSpace(mustLookup(values, lookup, "LOG_FILE_PATH")),
		LogRotateMaxSizeMB:  int(parseIntValue(mustLookup(values, lookup, "LOG_ROTATE_MAX_SIZE_MB"))),
		LogRotateMaxBackups: int(parseIntValue(mustLookup(values, lookup, "LOG_ROTATE_MAX_BACKUPS"))),
		LogRotateMaxAgeDays: int(parseIntValue(mustLookup(values, lookup, "LOG_ROTATE_MAX_AGE_DAYS"))),
		LogRotateCompress:   parseBoolValue(mustLookup(values, lookup, "LOG_ROTATE_COMPRESS")),
		DeliverablePrefixes: splitCSV(mustLookup(values, lookup, "DELIVERABLE_PREFIXES")),
		TransferDir:         strings.TrimSpace(mustLookup(values, lookup, "TRANSFER_DIR")),
		TransferMaxFileSize: parseIntValue(mustLookup(values, lookup, "TRANSFER_MAX_FILE_SIZE")),
		TransferTTL:         parseDurationValue(mustLookup(values, lookup, "TRANSFER_TTL"), 0),
		LogLevel:            strings.TrimSpace(mustLookup(values, lookup, "LOG_LEVEL")),
		LogFormat:           strings.TrimSpace(mustLookup(values, lookup, "LOG_FORMAT")),
		LogAddSource:        parseBoolValue(mustLookup(values, lookup, "LOG_ADD_SOURCE")),
	}
	// Record the per-key source for every value resolved above.
	srcKeys := map[string]string{
		"natsServers":            "NATS_SERVERS",
		"localApiAddr":           "LOCAL_API_ADDR",
		"dataDir":                "DATA_DIR",
		"identityKeyPath":        "IDENTITY_KEY_PATH",
		"identityPubPath":        "IDENTITY_PUB_PATH",
		"heartbeat":              "HEARTBEAT_INTERVAL_MS",
		"announceTtl":            "ANNOUNCE_TTL_MS",
		"trustMode":              "TRUST_MODE",
		"agentAdapter":           "AGENT_ADAPTER",
		"agentAdapterTimeout":    "AGENT_ADAPTER_TIMEOUT",
		"agentRole":              "AGENT_ROLE",
		"task.maxConcurrentRuns": "TASK_MAX_CONCURRENT_RUNS",
		"task.queueCapacity":     "TASK_QUEUE_CAPACITY",
		"task.runTimeout":        "TASK_RUN_TIMEOUT",
		"task.queueWaitTimeout":  "TASK_QUEUE_WAIT_TIMEOUT",
		"webhookUrl":             "WEBHOOK_URL",
		"logFilePath":            "LOG_FILE_PATH",
		"logRotateMaxSizeMb":     "LOG_ROTATE_MAX_SIZE_MB",
		"logRotateMaxBackups":    "LOG_ROTATE_MAX_BACKUPS",
		"logRotateMaxAgeDays":    "LOG_ROTATE_MAX_AGE_DAYS",
		"logRotateCompress":      "LOG_ROTATE_COMPRESS",
		"deliverablePrefixes":    "DELIVERABLE_PREFIXES",
		"transferDir":            "TRANSFER_DIR",
		"transferMaxFileSize":    "TRANSFER_MAX_FILE_SIZE",
		"transferTtl":            "TRANSFER_TTL",
		"logLevel":               "LOG_LEVEL",
		"logFormat":              "LOG_FORMAT",
		"logAddSource":           "LOG_ADD_SOURCE",
	}
	for key, legacyKey := range srcKeys {
		if v, legacy := lookup(legacyKey); v != "" {
			name := src
			if legacy {
				name = src + "-legacy"
			}
			cfg.setSource(key, name)
		}
	}
	if v, legacy := lookup("TRUST_AUTO_APPROVE"); v != "" {
		cfg.TrustAutoApprove = parseBoolValue(v)
		cfg.TrustAutoApproveSet = true
		tag(&cfg, "trustAutoApprove", "TRUST_AUTO_APPROVE", v, legacy)
	}
	if v, legacy := lookup("ROLE_ANCHOR"); v != "" {
		cfg.RoleAnchor = parseBoolValue(v)
		cfg.RoleAnchorSet = true
		tag(&cfg, "roleAnchor", "ROLE_ANCHOR", v, legacy)
	}
	return cfg
}

// mustLookup returns the resolved value for legacyKey (CLAWSYNAPSE_ prefix
// first, bare legacy name as fallback) and records its per-key source tag.
// values is kept in the signature for readability at call sites.
func mustLookup(values map[string]string, lookup func(string) (string, bool), legacyKey string) string {
	v, _ := lookup(legacyKey)
	return v
}

func parseDurationValue(v string, fallback time.Duration) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return fallback
	}

	if d, err := time.ParseDuration(v); err == nil {
		return d
	}

	ms, err := time.ParseDuration(v + "ms")
	if err != nil {
		return fallback
	}
	return ms
}

func parseDotEnv(data string) map[string]string {
	values := make(map[string]string)
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if strings.HasPrefix(value, "\"") && strings.HasSuffix(value, "\"") {
				value = strings.Trim(value, "\"")
			}
			if strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'") {
				value = strings.Trim(value, "'")
			}
		}
		values[key] = value
	}
	return values
}

func cloneStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, item := range in {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		out = append(out, item)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func parseIntValue(v string) int64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func parseBoolValue(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	ok, err := strconv.ParseBool(v)
	if err != nil {
		return false
	}
	return ok
}

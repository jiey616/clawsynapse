package config

import (
	"testing"
)

func TestLoadValuesFromMapPrefixedWins(t *testing.T) {
	vals := map[string]string{
		"NATS_SERVERS":              "nats://legacy:4222",
		"CLAWSYNAPSE_NATS_SERVERS":  "nats://canonical:4222",
		"AGENT_ADAPTER":             "hermes",
		"CLAWSYNAPSE_ROLE_ANCHOR":   "false",
	}
	cfg := loadValuesFromMap(vals, "env")
	if len(cfg.NATSServers) != 1 || cfg.NATSServers[0] != "nats://canonical:4222" {
		t.Errorf("NATSServers = %v, want canonical", cfg.NATSServers)
	}
	if cfg.Sources["natsServers"] != "env" {
		t.Errorf("natsServers source = %q, want env (canonical wins)", cfg.Sources["natsServers"])
	}
	if cfg.AgentAdapter != "hermes" {
		t.Errorf("AgentAdapter = %q, want hermes via legacy fallback", cfg.AgentAdapter)
	}
	if cfg.Sources["agentAdapter"] != "env-legacy" {
		t.Errorf("agentAdapter source = %q, want env-legacy", cfg.Sources["agentAdapter"])
	}
	if !cfg.RoleAnchorSet || cfg.RoleAnchor {
		t.Errorf("RoleAnchor = %v set=%v, want false set", cfg.RoleAnchor, cfg.RoleAnchorSet)
	}
}

func TestLoadValuesFromMapLegacyFallback(t *testing.T) {
	vals := map[string]string{
		"NATS_SERVERS":     "nats://legacy:4222",
		"LOG_LEVEL":        "debug",
	}
	cfg := loadValuesFromMap(vals, "env")
	if len(cfg.NATSServers) != 1 || cfg.NATSServers[0] != "nats://legacy:4222" {
		t.Errorf("NATSServers = %v, want legacy fallback", cfg.NATSServers)
	}
	if cfg.Sources["natsServers"] != "env-legacy" {
		t.Errorf("natsServers source = %q, want env-legacy", cfg.Sources["natsServers"])
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}
	if cfg.Sources["logLevel"] != "env-legacy" {
		t.Errorf("logLevel source = %q, want env-legacy", cfg.Sources["logLevel"])
	}
}

func TestEnvPrefixed(t *testing.T) {
	t.Setenv("NATS_SERVERS", "nats://legacy:4222")
	t.Setenv("CLAWSYNAPSE_HERMES_MODEL", "canonical-model")

	v, legacy, found := envPrefixed("NATS_SERVERS", "")
	if !found || !legacy || v != "nats://legacy:4222" {
		t.Errorf("envPrefixed legacy = %q legacy=%v found=%v", v, legacy, found)
	}
	if v, legacy, found := envPrefixed("HERMES_MODEL", "fb"); !found || legacy || v != "canonical-model" {
		t.Errorf("envPrefixed canonical = %q legacy=%v found=%v", v, legacy, found)
	}
	if v, _, found := envPrefixed("CLAWSYNAPSE_MISSING_XYZ", "fallback-value"); found || v != "fallback-value" {
		t.Errorf("envPrefixed missing = %q found=%v, want fallback not found", v, found)
	}
}

func TestMergeConfigValuesRecordsSource(t *testing.T) {
	base := configValues{LogLevel: "info"}
	override := configValues{LogLevel: "debug", RoleAnchor: true, RoleAnchorSet: true}
	merged := mergeConfigValues(base, override, "yaml")
	if merged.LogLevel != "debug" || merged.Sources["logLevel"] != "yaml" {
		t.Errorf("logLevel = %q source=%q, want debug/yaml", merged.LogLevel, merged.Sources["logLevel"])
	}
	if merged.Sources["roleAnchor"] != "yaml" {
		t.Errorf("roleAnchor source = %q, want yaml", merged.Sources["roleAnchor"])
	}
	// Untouched keys carry no source entry (reported as default).
	if _, ok := merged.Sources["natsServers"]; ok {
		t.Errorf("natsServers should have no source entry")
	}
}

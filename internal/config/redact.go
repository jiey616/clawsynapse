package config

// RedactedPlaceholder replaces secret values in GET /v1/config responses.
// A PUT carrying this placeholder (or an empty value) keeps the stored
// secret instead of wiping it.
const RedactedPlaceholder = "__REDACTED__"

// RedactConfig returns a copy of cfg with secret fields masked for API
// exposure (Phase 3.4: GET /v1/config must not leak credentials).
func RedactConfig(cfg Config) Config {
	if cfg.HermesGatewayKey != "" {
		cfg.HermesGatewayKey = RedactedPlaceholder
	}
	// ConfigPath is marked json:"-" already; scrub it defensively anyway.
	cfg.ConfigPath = ""
	return cfg
}

// ApplyConfigWhitelist copies only operator-approved fields from src into
// dst (Phase 3.4: PUT /v1/config must not rewrite identity, trust, NATS,
// or storage layout). Secrets: a placeholder or empty value in src keeps
// dst's existing value.
func ApplyConfigWhitelist(dst *Config, src Config) {
	// Hermes connection (gateway key keeps its value on placeholder/empty).
	if src.HermesGatewayURL != "" {
		dst.HermesGatewayURL = src.HermesGatewayURL
	}
	if src.HermesGatewayKey != "" && src.HermesGatewayKey != RedactedPlaceholder {
		dst.HermesGatewayKey = src.HermesGatewayKey
	}
	if src.HermesModel != "" {
		dst.HermesModel = src.HermesModel
	}
	if src.HermesTodoMode != "" {
		dst.HermesTodoMode = src.HermesTodoMode
	}
	if src.AgentAdapterTimeout != "" {
		dst.AgentAdapterTimeout = src.AgentAdapterTimeout
	}

	// Logging.
	if src.LogLevel != "" {
		dst.LogLevel = src.LogLevel
	}
	if src.LogFormat != "" {
		dst.LogFormat = src.LogFormat
	}
	dst.LogAddSource = src.LogAddSource

	// Task run admission bounds (todo.*).
	if src.Task != nil {
		dst.Task = src.Task
	}

	// Messaging deliverability.
	if len(src.DeliverablePrefixes) > 0 {
		dst.DeliverablePrefixes = src.DeliverablePrefixes
	}

	// Trust convenience flag (trust mode itself is NOT whitelisted).
	dst.TrustAutoApprove = src.TrustAutoApprove
}

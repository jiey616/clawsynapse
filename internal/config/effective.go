package config

import (
	"log/slog"
	"strconv"
	"strings"
)

// LogEffective logs the effective configuration and where each value came
// from. Sources are: default, yaml, dotenv, env, flag; the "-legacy" suffix
// means the value was provided by the legacy unprefixed env/dotenv name
// (CLAWSYNAPSE_* is canonical). Secrets are masked.
func LogEffective(cfg Config, log *slog.Logger) {
	if log == nil {
		return
	}
	type item struct {
		key, value string
	}
	items := []item{
		{"natsServers", strings.Join(cfg.NATSServers, ",")},
		{"localApiAddr", cfg.LocalAPIAddr},
		{"dataDir", cfg.DataDir},
		{"identityKeyPath", cfg.IdentityKeyPath},
		{"trustMode", cfg.TrustMode},
		{"trustAutoApprove", boolStr(cfg.TrustAutoApprove)},
		{"roleAnchor", boolStr(cfg.RoleAnchor)},
		{"agentAdapter", cfg.AgentAdapter},
		{"agentAdapterTimeout", cfg.AgentAdapterTimeout},
		{"agentRole", cfg.AgentRole},
		{"hermesGatewayUrl", cfg.HermesGatewayURL},
		{"hermesGatewayKey", mask(cfg.HermesGatewayKey)},
		{"hermesModel", cfg.HermesModel},
		{"hermesConfigPath", cfg.HermesConfigPath},
		{"hermesTodoMode", cfg.HermesTodoMode},
		{"webhookUrl", cfg.WebhookURL},
		{"logLevel", cfg.LogLevel},
		{"logFormat", cfg.LogFormat},
		{"logFilePath", cfg.LogFilePath},
		{"deliverablePrefixes", strings.Join(cfg.DeliverablePrefixes, ",")},
		{"transferDir", cfg.TransferDir},
		{"transferTtl", cfg.TransferTTL},
		{"logAddSource", boolStr(cfg.LogAddSource)},
	}
	if cfg.Task != nil {
		items = append(items,
			item{"task.maxConcurrentRuns", intStr(cfg.Task.MaxConcurrentRuns)},
			item{"task.queueCapacity", intStr(cfg.Task.QueueCapacity)},
			item{"task.runTimeout", cfg.Task.RunTimeout},
			item{"task.queueWaitTimeout", cfg.Task.QueueWaitTimeout},
		)
	}
	for _, it := range items {
		if strings.TrimSpace(it.value) == "" {
			continue
		}
		src := "default"
		if cfg.Sources != nil {
			if s := cfg.Sources[it.key]; s != "" {
				src = s
			}
		}
		log.Info("effective config",
			slog.String("key", it.key),
			slog.String("value", it.value),
			slog.String("source", src),
		)
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func intStr(n int) string {
	if n <= 0 {
		return ""
	}
	return strconv.Itoa(n)
}

func mask(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return "****"
}

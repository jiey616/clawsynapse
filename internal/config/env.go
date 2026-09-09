package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// envPrefixed resolves a config env var with the canonical CLAWSYNAPSE_ prefix.
// The bare legacy name is kept as a fallback for existing deployments:
// CLAWSYNAPSE_<KEY> wins, then <KEY>. Returns the value, whether the legacy
// (unprefixed) name provided it, and whether anything was found.
func envPrefixed(key, fallback string) (string, bool, bool) {
	if v := strings.TrimSpace(os.Getenv("CLAWSYNAPSE_" + key)); v != "" {
		return v, false, true
	}
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v, true, true
	}
	return fallback, false, false
}

func envOr(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}

	if strings.HasSuffix(v, "ms") || strings.HasSuffix(v, "s") || strings.HasSuffix(v, "m") {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}

	ms, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return time.Duration(ms) * time.Millisecond
}

func expandPath(path string) (string, error) {
	if path == "" {
		return "", nil
	}

	if strings.HasPrefix(path, "~/") || path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			return home, nil
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}

	return filepath.Abs(path)
}

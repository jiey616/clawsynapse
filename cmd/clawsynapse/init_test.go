package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunInitWritesConfigFromFlags(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	dataDir := filepath.Join(dir, "data")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := runInit([]string{
		"--config", configPath,
		"--nats-servers", "nats://127.0.0.1:4222,nats://127.0.0.1:4223",
		"--agent-adapter", "webhook",
		"--webhook-url", "http://127.0.0.1:8080/hook",
		"--data-dir", dataDir,
		"--deliverable-prefixes", "chat,task,todo",
	}, strings.NewReader(""), &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "webhookUrl: http://127.0.0.1:8080/hook\n") {
		t.Fatalf("expected webhookUrl in config, got:\n%s", text)
	}
	if !strings.Contains(text, "agentAdapterTimeout: 10m\n") {
		t.Fatalf("expected agentAdapterTimeout in config, got:\n%s", text)
	}
	if !strings.Contains(text, "  - todo\n") {
		t.Fatalf("expected deliverable prefixes list, got:\n%s", text)
	}
	if !strings.Contains(text, "transferDir: "+filepath.Join(dataDir, "transfers")+"\n") {
		t.Fatalf("expected transferDir default, got:\n%s", text)
	}
	if strings.Contains(text, "nodeId") {
		t.Fatalf("config should not contain nodeId, got:\n%s", text)
	}
}

func TestRunInitWritesConfigWithoutNodeID(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")

	err := runInit([]string{
		"--config", configPath,
		"--data-dir", dir,
	}, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "nodeId") {
		t.Fatalf("config should not contain nodeId field")
	}
}

func TestRunInitRequiresOverwriteForExistingConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("natsServers:\n  - nats://127.0.0.1:4222\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := runInit([]string{
		"--config", configPath,
	}, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected overwrite protection error")
	}
	if !strings.Contains(err.Error(), "use --overwrite") {
		t.Fatalf("unexpected error: %v", err)
	}
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type upgradeCall struct {
	name string
	args []string
}

type stubUpgradeRunner struct {
	calls        []upgradeCall
	daemonPath   string
	daemonOut    []byte
	installerOut []byte
	installerErr error
}

func (s *stubUpgradeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	s.calls = append(s.calls, upgradeCall{name: name, args: append([]string(nil), args...)})
	if name == s.daemonPath && len(args) == 1 && args[0] == "version" {
		return s.daemonOut, nil
	}
	if name == "bash" {
		return s.installerOut, s.installerErr
	}
	return nil, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestRunUpgradeCheckReportsAvailableVersions(t *testing.T) {
	home := t.TempDir()
	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	daemonPath := filepath.Join(binDir, daemonBinaryName)
	if err := os.WriteFile(daemonPath, []byte("daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifestDir := filepath.Join(home, ".clawsynapse")
	if err := os.MkdirAll(manifestDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "manifest.json"), []byte(`{
		"product":"clawsynapse",
		"repo":"owner/repo",
		"version":"v0.3.1",
		"installDir":"`+strings.ReplaceAll(binDir, `\`, `\\`)+`",
		"components":["cli","daemon"],
		"serviceManager":"launchd"
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	oldVersion := version
	version = "v0.3.1"
	t.Cleanup(func() { version = oldVersion })

	runner := &stubUpgradeRunner{
		daemonPath: daemonPath,
		daemonOut:  []byte("v0.3.0\n"),
	}
	client := &http.Client{Timeout: time.Second, Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "api.github.com" {
			t.Fatalf("unexpected host %s", req.URL.Host)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(strings.NewReader(`{"tag_name":"v0.3.2"}`)),
			Header:     make(http.Header),
		}, nil
	})}

	deps := upgradeDeps{
		runner:     runner,
		httpClient: client,
		homeDir:    func() (string, error) { return home, nil },
		executable: func() (string, error) { return filepath.Join(binDir, cliBinaryName), nil },
		lookPath:   func(string) (string, error) { return "", os.ErrNotExist },
		localCheck: func(context.Context, string) (string, error) { return "", errors.New("unavailable") },
		now:        time.Now,
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runUpgradeWithDeps([]string{"check"}, strings.NewReader(""), &stdout, &stderr, deps); err != nil {
		t.Fatal(err)
	}

	output := stdout.String()
	if !strings.Contains(output, "cli:    v0.3.1") {
		t.Fatalf("expected cli version in output, got:\n%s", output)
	}
	if !strings.Contains(output, "daemon: v0.3.0") {
		t.Fatalf("expected daemon version in output, got:\n%s", output)
	}
	if !strings.Contains(output, "release: v0.3.2") {
		t.Fatalf("expected latest version in output, got:\n%s", output)
	}
	if !strings.Contains(output, "daemon: v0.3.0 -> v0.3.2") {
		t.Fatalf("expected daemon upgrade line, got:\n%s", output)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
}

func TestRunUpgradeApplyInvokesInstallerWithManifestDefaults(t *testing.T) {
	home := t.TempDir()
	manifestDir := filepath.Join(home, ".clawsynapse")
	if err := os.MkdirAll(manifestDir, 0o700); err != nil {
		t.Fatal(err)
	}
	installDir := filepath.Join(t.TempDir(), "bin")
	if err := os.WriteFile(filepath.Join(manifestDir, "manifest.json"), []byte(`{
		"product":"clawsynapse",
		"repo":"owner/repo",
		"version":"v1.2.2",
		"installDir":"`+strings.ReplaceAll(installDir, `\`, `\\`)+`",
		"components":["cli","daemon"]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	runner := &stubUpgradeRunner{installerOut: []byte("installed\n")}
	client := &http.Client{Timeout: time.Second, Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "raw.githubusercontent.com" {
			t.Fatalf("unexpected host %s", req.URL.Host)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(strings.NewReader("#!/usr/bin/env bash\nexit 0\n")),
			Header:     make(http.Header),
		}, nil
	})}

	deps := upgradeDeps{
		runner:     runner,
		httpClient: client,
		homeDir:    func() (string, error) { return home, nil },
		executable: func() (string, error) { return filepath.Join(t.TempDir(), cliBinaryName), nil },
		lookPath:   func(string) (string, error) { return "", os.ErrNotExist },
		localCheck: func(context.Context, string) (string, error) { return "", errors.New("unavailable") },
		now:        time.Now,
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runUpgradeWithDeps([]string{"--version", "v1.2.3", "--yes"}, nil, &stdout, &stderr, deps); err != nil {
		t.Fatal(err)
	}

	if len(runner.calls) != 1 {
		t.Fatalf("expected one installer call, got %#v", runner.calls)
	}
	call := runner.calls[0]
	if call.name != "bash" {
		t.Fatalf("expected bash call, got %#v", call)
	}
	argsText := strings.Join(call.args, " ")
	if !strings.Contains(argsText, "--all") {
		t.Fatalf("expected --all in installer args, got %#v", call.args)
	}
	if !strings.Contains(argsText, "--install-dir "+installDir) {
		t.Fatalf("expected install dir in installer args, got %#v", call.args)
	}
	if !strings.Contains(argsText, "--version v1.2.3") {
		t.Fatalf("expected target version in installer args, got %#v", call.args)
	}
	if !strings.Contains(argsText, "--repo owner/repo") {
		t.Fatalf("expected repo in installer args, got %#v", call.args)
	}

	output := stdout.String()
	if !strings.Contains(output, "Target version: v1.2.3") {
		t.Fatalf("expected upgrade plan in stdout, got:\n%s", output)
	}
	if !strings.Contains(output, "installed") {
		t.Fatalf("expected installer output in stdout, got:\n%s", output)
	}
	if !strings.Contains(output, "Upgrade completed.") {
		t.Fatalf("expected completion message in stdout, got:\n%s", output)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
}

func TestRunUpgradeCheckJSON(t *testing.T) {
	home := t.TempDir()
	manifestDir := filepath.Join(home, ".clawsynapse")
	if err := os.MkdirAll(manifestDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "manifest.json"), []byte(`{
		"product":"clawsynapse",
		"repo":"owner/repo",
		"version":"v1.0.0",
		"installDir":"/tmp/bin",
		"components":["cli"]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	oldVersion := version
	version = "v1.0.0"
	t.Cleanup(func() { version = oldVersion })

	client := &http.Client{Timeout: time.Second, Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(strings.NewReader(`{"tag_name":"v1.1.0"}`)),
			Header:     make(http.Header),
		}, nil
	})}

	deps := upgradeDeps{
		runner:     &stubUpgradeRunner{},
		httpClient: client,
		homeDir:    func() (string, error) { return home, nil },
		executable: func() (string, error) { return filepath.Join(t.TempDir(), cliBinaryName), nil },
		lookPath:   func(string) (string, error) { return "", os.ErrNotExist },
		localCheck: func(context.Context, string) (string, error) { return "", errors.New("unavailable") },
		now:        time.Now,
	}

	var stdout bytes.Buffer
	if err := runUpgradeWithDeps([]string{"check", "--json"}, strings.NewReader(""), &stdout, &bytes.Buffer{}, deps); err != nil {
		t.Fatal(err)
	}

	var result upgradeCheckResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("failed to decode JSON: %v\n%s", err, stdout.String())
	}
	if result.Current.CLI != "v1.0.0" {
		t.Fatalf("unexpected cli version: %#v", result.Current)
	}
	if result.Latest != "v1.1.0" {
		t.Fatalf("unexpected latest version: %s", result.Latest)
	}
	if result.OK {
		t.Fatal("expected ok=false when update is available")
	}
	if len(result.Updates) != 1 {
		t.Fatalf("expected one update, got %#v", result.Updates)
	}
	if result.Updates[0].Component != "cli" || result.Updates[0].Target != "v1.1.0" {
		t.Fatalf("unexpected update item: %#v", result.Updates[0])
	}
}

func TestRunUpgradeCheckPrefersLocalAPIDaemonVersion(t *testing.T) {
	home := t.TempDir()
	manifestDir := filepath.Join(home, ".clawsynapse")
	if err := os.MkdirAll(manifestDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "manifest.json"), []byte(`{
		"product":"clawsynapse",
		"repo":"owner/repo",
		"version":"v1.0.0",
		"installDir":"/tmp/bin",
		"components":["cli","daemon"]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	oldVersion := version
	version = "v1.0.0"
	t.Cleanup(func() { version = oldVersion })

	client := &http.Client{Timeout: time.Second, Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(strings.NewReader(`{"tag_name":"v1.1.0"}`)),
			Header:     make(http.Header),
		}, nil
	})}

	deps := upgradeDeps{
		runner:     &stubUpgradeRunner{},
		httpClient: client,
		homeDir:    func() (string, error) { return home, nil },
		executable: func() (string, error) { return filepath.Join(t.TempDir(), cliBinaryName), nil },
		lookPath:   func(string) (string, error) { return "", os.ErrNotExist },
		apiAddr:    "127.0.0.1:18080",
		localCheck: func(_ context.Context, addr string) (string, error) {
			if addr != "127.0.0.1:18080" {
				t.Fatalf("unexpected api addr: %s", addr)
			}
			return "v1.0.5", nil
		},
		now: time.Now,
	}

	var stdout bytes.Buffer
	if err := runUpgradeWithDeps([]string{"check"}, strings.NewReader(""), &stdout, &bytes.Buffer{}, deps); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "daemon: v1.0.5") {
		t.Fatalf("expected local api daemon version, got:\n%s", stdout.String())
	}
}

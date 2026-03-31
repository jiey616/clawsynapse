package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

type stubLogRunner struct {
	calls     []serviceCall
	responses map[string]stubServiceResult
}

type queueLogProvider struct {
	items []string
	idx   int
}

type stubLogProvider struct {
	text string
	err  error
}

func (s *stubLogRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	s.calls = append(s.calls, serviceCall{name: name, args: append([]string(nil), args...)})
	key := name + " " + joinArgs(args)
	if result, ok := s.responses[key]; ok {
		return result.out, result.err
	}
	return nil, nil
}

func (q *queueLogProvider) ReadLogs(_ context.Context, _ int) (string, error) {
	if q.idx >= len(q.items) {
		return q.items[len(q.items)-1], nil
	}
	value := q.items[q.idx]
	q.idx++
	return value, nil
}

func (s stubLogProvider) ReadLogs(_ context.Context, _ int) (string, error) {
	return s.text, s.err
}

func TestDefaultLogProviderLinuxUsesJournalctl(t *testing.T) {
	prevGOOS := serviceGOOS
	serviceGOOS = "linux"
	defer func() { serviceGOOS = prevGOOS }()

	runner := &stubLogRunner{
		responses: map[string]stubServiceResult{
			"journalctl -u " + serviceUnitName + " -n 25 --no-pager -o short-iso": {
				out: []byte("line1\nline2\n"),
			},
		},
	}

	text, err := defaultLogProvider{runner: runner}.ReadLogs(context.Background(), 25)
	if err != nil {
		t.Fatal(err)
	}
	if text != "line1\nline2\n" {
		t.Fatalf("unexpected log text: %q", text)
	}

	want := []serviceCall{
		{name: "journalctl", args: []string{"-u", serviceUnitName, "-n", "25", "--no-pager", "-o", "short-iso"}},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("unexpected calls: %#v", runner.calls)
	}
}

func TestDefaultLogProviderDarwinReadsTailFromFiles(t *testing.T) {
	prevGOOS := serviceGOOS
	prevHome := serviceUserHomeDir
	serviceGOOS = "darwin"

	dir := t.TempDir()
	serviceUserHomeDir = func() (string, error) { return dir, nil }
	defer func() {
		serviceGOOS = prevGOOS
		serviceUserHomeDir = prevHome
	}()

	logDir := filepath.Join(dir, ".clawsynapse", "log")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "clawsynapsed.stdout.log"), []byte("a\nb\nc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "clawsynapsed.stderr.log"), []byte("x\ny\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	text, err := defaultLogProvider{}.ReadLogs(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	want := "== stdout ==\n" + "b\nc" + "\n\n== stderr ==\n" + "x\ny"
	if text != want {
		t.Fatalf("unexpected log text: %q", text)
	}
}

func TestRunLogsWritesProviderOutput(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	err := runLogsWithProvider([]string{"--lines", "10"}, &stdout, &stderr, stubLogProvider{text: "hello\nworld"})
	if err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "hello\nworld\n" {
		t.Fatalf("unexpected stdout: %q", stdout.String())
	}
}

func TestRunLogsRejectsInvalidLines(t *testing.T) {
	err := runLogsWithProvider([]string{"--lines", "0"}, &bytes.Buffer{}, &bytes.Buffer{}, stubLogProvider{})
	if err == nil {
		t.Fatal("expected invalid lines error")
	}
	if err.Error() != "lines must be greater than 0" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDefaultLogProviderLinuxFallsBackToSudoNonInteractive(t *testing.T) {
	prevGOOS := serviceGOOS
	serviceGOOS = "linux"
	defer func() { serviceGOOS = prevGOOS }()

	runner := &stubLogRunner{
		responses: map[string]stubServiceResult{
			"journalctl -u " + serviceUnitName + " -n 5 --no-pager -o short-iso": {
				err: errors.New("permission denied"),
			},
			"sudo -n journalctl -u " + serviceUnitName + " -n 5 --no-pager -o short-iso": {
				out: []byte("sudo line\n"),
			},
		},
	}

	text, err := defaultLogProvider{runner: runner}.ReadLogs(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	if text != "sudo line\n" {
		t.Fatalf("unexpected text: %q", text)
	}
}

func TestNewLogContentAppendsDelta(t *testing.T) {
	delta := newLogContent("line1\n", "line1\nline2\n")
	if delta != "line2\n" {
		t.Fatalf("unexpected delta: %q", delta)
	}
}

func TestNewLogContentFallsBackToCurrentOnRotation(t *testing.T) {
	delta := newLogContent("old1\nold2\n", "new1\n")
	if delta != "new1\n" {
		t.Fatalf("unexpected delta: %q", delta)
	}
}

func TestFollowLogsWritesOnlyNewContent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	provider := &queueLogProvider{
		items: []string{
			"line1\n",
			"line1\nline2\n",
			"line1\nline2\nline3\n",
		},
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- followLogs(ctx, provider, &stdout, &stderr, 10, 5*time.Millisecond, "line1\n")
	}()

	time.Sleep(18 * time.Millisecond)
	cancel()

	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "line2\nline3\n" {
		t.Fatalf("unexpected stdout: %q", stdout.String())
	}
	if stderr.String() != "" {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}
}

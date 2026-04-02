package adapter

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestOpenCodeAdapterDeliverMessage(t *testing.T) {
	adapter, err := NewOpenCodeAdapter(OpenCodeConfig{
		NodeID: "node-alpha",
	})
	if err != nil {
		t.Fatalf("NewOpenCodeAdapter failed: %v", err)
	}

	adapter.execCmd = func(_ context.Context, args ...string) ([]byte, error) {
		// args: run <msg> --format json --session <id>
		if len(args) < 6 || args[0] != "run" {
			t.Fatalf("unexpected args: %v", args)
		}
		wantMsg := "[clawsynapse from=node-beta to=node-alpha session=session-1]\nhello"
		if args[1] != wantMsg {
			t.Fatalf("message = %q, want %q", args[1], wantMsg)
		}
		if args[2] != "--format" || args[3] != "json" {
			t.Fatalf("format args = %v, want [--format json]", args[2:4])
		}
		if args[4] != "--session" || args[5] != "session-1" {
			t.Fatalf("session args = %v, want [--session session-1]", args[4:6])
		}

		return []byte(`{"type":"result","content":"done"}` + "\n"), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	result, err := adapter.DeliverMessage(ctx, DeliverMessageRequest{
		SessionKey: "session-1",
		Message:    "hello",
		From:       "node-beta",
	})
	if err != nil {
		t.Fatalf("DeliverMessage failed: %v", err)
	}
	if !result.Success {
		t.Fatal("expected success")
	}
	if !result.Accepted {
		t.Fatal("expected accepted result")
	}
	if result.Reply != "done" {
		t.Fatalf("reply = %q, want done", result.Reply)
	}
}

func TestOpenCodeAdapterDeliverMessageError(t *testing.T) {
	adapter, err := NewOpenCodeAdapter(OpenCodeConfig{})
	if err != nil {
		t.Fatalf("NewOpenCodeAdapter failed: %v", err)
	}

	adapter.execCmd = func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte(`{"type":"error","error":"model not available"}` + "\n"), nil
	}

	result, err := adapter.DeliverMessage(context.Background(), DeliverMessageRequest{Message: "hi"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Success {
		t.Fatal("expected failure")
	}
	if result.Error != "model not available" {
		t.Fatalf("error = %q, want model not available", result.Error)
	}
}

func TestOpenCodeAdapterCommandFailure(t *testing.T) {
	adapter, err := NewOpenCodeAdapter(OpenCodeConfig{})
	if err != nil {
		t.Fatalf("NewOpenCodeAdapter failed: %v", err)
	}

	adapter.execCmd = func(_ context.Context, _ ...string) ([]byte, error) {
		return nil, errors.New("command not found")
	}

	_, err = adapter.DeliverMessage(context.Background(), DeliverMessageRequest{Message: "hi"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestOpenCodeAdapterGetStatus(t *testing.T) {
	adapter, err := NewOpenCodeAdapter(OpenCodeConfig{})
	if err != nil {
		t.Fatalf("NewOpenCodeAdapter failed: %v", err)
	}

	adapter.execCmd = func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) != 1 || args[0] != "version" {
			t.Fatalf("unexpected args: %v", args)
		}
		return []byte("opencode v0.1.0\n"), nil
	}

	status, err := adapter.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("GetStatus failed: %v", err)
	}
	if !status.Healthy {
		t.Fatal("expected healthy")
	}
}

func TestOpenCodeResolveSessionID(t *testing.T) {
	a, _ := NewOpenCodeAdapter(OpenCodeConfig{NodeID: "node-1"})

	got := a.resolveSessionID(DeliverMessageRequest{SessionKey: "task-1", From: "node-2"})
	if got != "task-1" {
		t.Fatalf("got %q, want task-1", got)
	}

	got = a.resolveSessionID(DeliverMessageRequest{From: "node-2"})
	if got != "cs-node-2-node-1" {
		t.Fatalf("got %q, want cs-node-2-node-1", got)
	}

	got = a.resolveSessionID(DeliverMessageRequest{})
	if got != "cs-_anon-node-1" {
		t.Fatalf("got %q, want cs-_anon-node-1", got)
	}
}

func TestParseOpenCodeResultNDJSON(t *testing.T) {
	data := []byte(`{"type":"thinking","content":"analyzing..."}
{"type":"text","content":"here is the answer"}
{"type":"result","content":"final result"}
`)

	result, err := parseOpenCodeResult(data)
	if err != nil {
		t.Fatalf("parseOpenCodeResult failed: %v", err)
	}
	if !result.Success {
		t.Fatal("expected success")
	}
	if result.Reply != "final result" {
		t.Fatalf("reply = %q, want final result", result.Reply)
	}
}

func TestParseOpenCodeResultTextField(t *testing.T) {
	data := []byte(`{"type":"message","text":"hello from opencode"}`)

	result, err := parseOpenCodeResult(data)
	if err != nil {
		t.Fatalf("parseOpenCodeResult failed: %v", err)
	}
	if result.Reply != "hello from opencode" {
		t.Fatalf("reply = %q, want hello from opencode", result.Reply)
	}
}

func TestParseOpenCodeResultPlainText(t *testing.T) {
	data := []byte("This is plain text output\n")

	result, err := parseOpenCodeResult(data)
	if err != nil {
		t.Fatalf("parseOpenCodeResult failed: %v", err)
	}
	if !result.Success {
		t.Fatal("expected success")
	}
	if result.Reply != "This is plain text output" {
		t.Fatalf("reply = %q, want plain text output", result.Reply)
	}
}

func TestParseOpenCodeResultEmpty(t *testing.T) {
	result, err := parseOpenCodeResult([]byte(""))
	if err != nil {
		t.Fatalf("parseOpenCodeResult failed: %v", err)
	}
	if result.Success {
		t.Fatal("expected failure for empty output")
	}
}

func TestParseOpenCodeResultErrorEvent(t *testing.T) {
	data := []byte(`{"type":"error","error":"rate limit exceeded"}`)

	result, err := parseOpenCodeResult(data)
	if err != nil {
		t.Fatalf("parseOpenCodeResult failed: %v", err)
	}
	if result.Success {
		t.Fatal("expected failure")
	}
	if result.Error != "rate limit exceeded" {
		t.Fatalf("error = %q, want rate limit exceeded", result.Error)
	}
}

func TestFormatOpenCodeCommandForLogTruncatesMessage(t *testing.T) {
	message := strings.Repeat("x", 300)

	got := formatOpenCodeCommandForLog([]string{
		"run", message, "--format", "json", "--session", "session-1",
	})

	if !strings.Contains(got, `opencode "run" "`) {
		t.Fatalf("command = %q, missing prefix", got)
	}
	if !strings.Contains(got, "truncated, 300 bytes total") {
		t.Fatalf("command = %q, missing truncation marker", got)
	}
	if !strings.Contains(got, `"--session" "session-1"`) {
		t.Fatalf("command = %q, missing session", got)
	}
}

func TestOpenCodeAdapterDeliverMessageLogsCommand(t *testing.T) {
	var records []slog.Record
	logger := slog.New(captureHandler{records: &records})

	adapter, err := NewOpenCodeAdapter(OpenCodeConfig{
		NodeID: "node-alpha",
		Logger: logger,
	})
	if err != nil {
		t.Fatalf("NewOpenCodeAdapter failed: %v", err)
	}

	adapter.execCmd = func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte(`{"type":"result","content":"done"}`), nil
	}

	_, err = adapter.DeliverMessage(context.Background(), DeliverMessageRequest{
		SessionKey: "session-1",
		Message:    strings.Repeat("m", 320),
		From:       "node-beta",
	})
	if err != nil {
		t.Fatalf("DeliverMessage failed: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("log records = %d, want 1", len(records))
	}

	var command string
	records[0].Attrs(func(a slog.Attr) bool {
		if a.Key == "command" {
			command = a.Value.String()
		}
		return true
	})
	if command == "" {
		t.Fatal("expected command attribute")
	}
	wantMarker := "truncated, " + strconv.Itoa(len(formatDeliverMessage("node-alpha", DeliverMessageRequest{
		SessionKey: "session-1",
		Message:    strings.Repeat("m", 320),
		From:       "node-beta",
	}))) + " bytes total"
	if !strings.Contains(command, wantMarker) {
		t.Fatalf("command = %q, missing truncation marker", command)
	}
	if !strings.Contains(command, `opencode "run" "`) {
		t.Fatalf("command = %q, missing command prefix", command)
	}
}

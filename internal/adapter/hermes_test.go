package adapter

import (
	"context"
	"strings"
	"testing"
)

func TestNewHermesAdapter(t *testing.T) {
	cfg := HermesConfig{
		NodeID:       "n1-test",
		Logger:       nil,
		SessionStore: nil,
	}

	a, err := NewHermesAdapter(cfg)
	if err != nil {
		t.Fatalf("NewHermesAdapter failed: %v", err)
	}
	if a == nil {
		t.Fatal("expected non-nil adapter")
	}
	if a.nodeID != "n1-test" {
		t.Errorf("expected nodeID 'n1-test', got '%s'", a.nodeID)
	}
}

func TestHermesAdapter_GetStatus_NoHermesCLI(t *testing.T) {
	a := &HermesAdapter{
		nodeID: "n1-test",
		execCmd: func(ctx context.Context, args ...string) ([]byte, error) {
			// Simulate hermes not installed
			return nil, context.DeadlineExceeded
		},
	}

	status, err := a.GetStatus(context.Background())
	if err == nil {
		t.Error("expected error when hermes is not available")
	}
	if status != nil && status.Healthy {
		t.Error("expected unhealthy status when hermes is not available")
	}
}

func TestHermesAdapter_GetStatus_Healthy(t *testing.T) {
	a := &HermesAdapter{
		nodeID: "n1-test",
		execCmd: func(ctx context.Context, args ...string) ([]byte, error) {
			return []byte("hermes v1.0.0"), nil
		},
	}

	status, err := a.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !status.Healthy {
		t.Error("expected healthy status")
	}
}

func TestHermesAdapter_GetStatus_EmptyOutput(t *testing.T) {
	a := &HermesAdapter{
		nodeID: "n1-test",
		execCmd: func(ctx context.Context, args ...string) ([]byte, error) {
			return []byte("   "), nil
		},
	}

	status, err := a.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.Healthy {
		t.Error("expected unhealthy status for empty output")
	}
}

func TestBuildHermesPrompt_ChatMessage(t *testing.T) {
	prompt := buildHermesPrompt("chat.message", "n1-sender", "sess-123", "Hello!")
	if !strings.Contains(prompt, "聊天消息") {
		t.Error("expected chat message prompt to mention 聊天消息")
	}
	if !strings.Contains(prompt, "n1-sender") {
		t.Error("expected prompt to contain sender")
	}
	if !strings.Contains(prompt, "sess-123") {
		t.Error("expected prompt to contain session key")
	}
	if !strings.Contains(prompt, "Hello!") {
		t.Error("expected prompt to contain message body")
	}
	if !strings.Contains(prompt, "clawsynapse publish") {
		t.Error("expected prompt to mention clawsynapse publish")
	}
}

func TestBuildHermesPrompt_TaskMessage(t *testing.T) {
	prompt := buildHermesPrompt("task.message", "n1-sender", "sess-456", "Build a website")
	if !strings.Contains(prompt, "任务消息") {
		t.Error("expected task message prompt to mention 任务消息")
	}
	if !strings.Contains(prompt, "task.reply") {
		t.Error("expected task.reply type in reply instructions")
	}
}

func TestBuildHermesPrompt_TodoAssigned(t *testing.T) {
	prompt := buildHermesPrompt("todo.assigned", "n1-sender", "sess-789", `{"task_id":"t1","todo_id":"d1","title":"Test"}`)
	if !strings.Contains(prompt, "Todo 任务分配") {
		t.Error("expected todo.assigned prompt to mention Todo")
	}
	if !strings.Contains(prompt, "task.comment") {
		t.Error("expected step 1 to mention task.comment")
	}
	if !strings.Contains(prompt, "todo.progress") {
		t.Error("expected step 2 to mention todo.progress")
	}
	if !strings.Contains(prompt, "todo.complete") {
		t.Error("expected step 3 to mention todo.complete")
	}
	if !strings.Contains(prompt, "transfer send") {
		t.Error("expected step 3 to mention transfer send")
	}
}

func TestBuildHermesPrompt_TaskContextResult(t *testing.T) {
	prompt := buildHermesPrompt("task.context.result", "n1-sender", "sess-abc", "Context data here")
	if !strings.Contains(prompt, "任务上下文查询结果") {
		t.Error("expected task.context.result prompt to mention 上下文查询结果")
	}
}

func TestBuildHermesPrompt_Fallback(t *testing.T) {
	prompt := buildHermesPrompt("unknown.type", "n1-sender", "sess-xyz", "Some content")
	if !strings.Contains(prompt, "unknown.type") {
		t.Error("expected fallback prompt to contain message type")
	}
	if !strings.Contains(prompt, "n1-sender") {
		t.Error("expected fallback prompt to contain sender")
	}
}

func TestParseHeader(t *testing.T) {
	msg := "[clawsynapse type=chat.message from=n1-sender to=n1-recv session=sess-123]\nHello world!"
	msgType, from, session, body := parseHeader(msg)

	if msgType != "chat.message" {
		t.Errorf("expected type 'chat.message', got '%s'", msgType)
	}
	if from != "n1-sender" {
		t.Errorf("expected from 'n1-sender', got '%s'", from)
	}
	if session != "sess-123" {
		t.Errorf("expected session 'sess-123', got '%s'", session)
	}
	if body != "Hello world!" {
		t.Errorf("expected body 'Hello world!', got '%s'", body)
	}
}

func TestParseHeader_NoHeader(t *testing.T) {
	msg := "plain text without header"
	msgType, _, _, body := parseHeader(msg)

	if msgType != "chat.message" {
		t.Errorf("expected default type 'chat.message', got '%s'", msgType)
	}
	if body != "plain text without header" {
		t.Errorf("expected body to match input, got '%s'", body)
	}
}

func TestParseHeader_MultilineBody(t *testing.T) {
	msg := "[clawsynapse type=task.message from=n1-a session=s1]\nLine 1\nLine 2\nLine 3"
	_, _, _, body := parseHeader(msg)

	if !strings.Contains(body, "Line 1") {
		t.Error("expected body to contain multiline content")
	}
	if !strings.Contains(body, "Line 3") {
		t.Error("expected body to contain last line")
	}
}

func TestParseHermesResult(t *testing.T) {
	result, err := parseHermesResult([]byte("Hermes completed the task successfully."))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Error("expected success")
	}
	if !result.Accepted {
		t.Error("expected accepted")
	}
	if !strings.Contains(result.Reply, "Hermes completed") {
		t.Errorf("expected reply to contain output, got '%s'", result.Reply)
	}
}

func TestParseHermesResult_Empty(t *testing.T) {
	result, err := parseHermesResult([]byte("   "))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Success {
		t.Error("expected failure for empty output")
	}
	if result.Error != "hermes returned empty output" {
		t.Errorf("expected empty output error, got '%s'", result.Error)
	}
}

func TestParseHermesResult_LongOutput(t *testing.T) {
	// Create output longer than 4000 characters
	longText := strings.Repeat("A", 5000)
	result, err := parseHermesResult([]byte(longText))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Error("expected success for long output")
	}
	if !strings.Contains(result.Reply, "...(truncated)") {
		t.Error("expected truncated marker in long output")
	}
	if len(result.Reply) > 4000+len("...(truncated)\n")+100 {
		t.Errorf("reply too long: %d chars", len(result.Reply))
	}
}

func TestFormatHermesCommandForLog(t *testing.T) {
	longPrompt := strings.Repeat("This is a very long prompt that should be truncated in the log output. ", 5)
	args := []string{"chat", "-q", longPrompt, "--yolo"}
	logStr := formatHermesCommandForLog(args)
	if !strings.Contains(logStr, "hermes") {
		t.Error("expected log to contain 'hermes'")
	}
	if !strings.Contains(logStr, "...(truncated") {
		t.Error("expected log to truncate long prompt")
	}
}

func TestIsHermesUnknownSessionError(t *testing.T) {
	tests := []struct {
		errMsg   string
		expected bool
	}{
		{"session not found", true},
		{"unknown session id", true},
		{"no such session", true},
		{"no recorded session for key", true},
		{"connection refused", false},
		{"timeout", false},
		{"", false},
	}

	for _, tt := range tests {
		var err error
		if tt.errMsg != "" {
			err = &testError{msg: tt.errMsg}
		}
		result := isHermesUnknownSessionError(err)
		if result != tt.expected {
			t.Errorf("isHermesUnknownSessionError(%q) = %v, want %v", tt.errMsg, result, tt.expected)
		}
	}
}

// testError is a simple error type for testing.
type testError struct {
	msg string
}

func (e *testError) Error() string {
	return e.msg
}

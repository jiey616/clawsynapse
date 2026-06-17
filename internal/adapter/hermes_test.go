package adapter

import (
	"context"
	"strings"
	"testing"

	"clawsynapse/internal/store"
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

func TestNewHermesAdapter_DefaultSystemPrompt(t *testing.T) {
	cfg := HermesConfig{NodeID: "n1-test"}
	a, err := NewHermesAdapter(cfg)
	if err != nil {
		t.Fatalf("NewHermesAdapter failed: %v", err)
	}
	if a.systemPrompt != DefaultHermesSystemPrompt {
		t.Error("expected default system prompt when none provided")
	}
}

func TestNewHermesAdapter_CustomSystemPrompt(t *testing.T) {
	custom := "Custom prompt for testing"
	cfg := HermesConfig{
		NodeID:       "n1-test",
		SystemPrompt: custom,
	}
	a, err := NewHermesAdapter(cfg)
	if err != nil {
		t.Fatalf("NewHermesAdapter failed: %v", err)
	}
	if a.systemPrompt != custom {
		t.Errorf("expected custom prompt, got '%s'", a.systemPrompt)
	}
}

func TestNewHermesAdapter_EmptySystemPromptFallsBack(t *testing.T) {
	cfg := HermesConfig{
		NodeID:       "n1-test",
		SystemPrompt: "   ",
	}
	a, err := NewHermesAdapter(cfg)
	if err != nil {
		t.Fatalf("NewHermesAdapter failed: %v", err)
	}
	if a.systemPrompt != DefaultHermesSystemPrompt {
		t.Error("expected default system prompt when empty string provided")
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

func TestHermesAdapter_DeliverMessage_UsesSystemPrompt(t *testing.T) {
	var capturedPrompt string
	a := &HermesAdapter{
		nodeID:       "n1-test",
		systemPrompt: "SYS_PROMPT",
		execCmd: func(ctx context.Context, args ...string) ([]byte, error) {
			// Capture the -q prompt argument
			for i, arg := range args {
				if arg == "-q" && i+1 < len(args) {
					capturedPrompt = args[i+1]
					break
				}
			}
			return []byte("reply from hermes"), nil
		},
	}

	req := DeliverMessageRequest{
		Type:       "chat.message",
		From:       "n1-sender",
		SessionKey: "sess-123",
		Message:    "Hello!",
	}

	result, err := a.DeliverMessage(context.Background(), req)
	if err != nil {
		t.Fatalf("DeliverMessage failed: %v", err)
	}
	if !result.Success {
		t.Error("expected success")
	}

	// Verify the prompt starts with the system prompt
	if !strings.HasPrefix(capturedPrompt, "SYS_PROMPT") {
		t.Errorf("expected prompt to start with system prompt, got: %s", capturedPrompt[:min(50, len(capturedPrompt))])
	}

	// Verify the prompt contains the structured header from formatDeliverMessage
	if !strings.Contains(capturedPrompt, "[clawsynapse") {
		t.Error("expected prompt to contain [clawsynapse header")
	}
	if !strings.Contains(capturedPrompt, "type=chat.message") {
		t.Error("expected prompt to contain message type")
	}
	if !strings.Contains(capturedPrompt, "from=n1-sender") {
		t.Error("expected prompt to contain sender")
	}
	if !strings.Contains(capturedPrompt, "session=sess-123") {
		t.Error("expected prompt to contain session key")
	}
	if !strings.Contains(capturedPrompt, "Hello!") {
		t.Error("expected prompt to contain message body")
	}
}

func TestHermesAdapter_DeliverMessage_DefaultSystemPrompt(t *testing.T) {
	var capturedPrompt string
	a := &HermesAdapter{
		nodeID:       "n1-test",
		systemPrompt: DefaultHermesSystemPrompt,
		execCmd: func(ctx context.Context, args ...string) ([]byte, error) {
			for i, arg := range args {
				if arg == "-q" && i+1 < len(args) {
					capturedPrompt = args[i+1]
					break
				}
			}
			return []byte("ok"), nil
		},
	}

	req := DeliverMessageRequest{
		Type:    "task.message",
		From:    "n1-boss",
		Message: "Do something",
	}

	_, err := a.DeliverMessage(context.Background(), req)
	if err != nil {
		t.Fatalf("DeliverMessage failed: %v", err)
	}

	// Default prompt mentions clawsynapse publish
	if !strings.Contains(capturedPrompt, "clawsynapse publish") {
		t.Error("expected default system prompt to mention clawsynapse publish")
	}
	if !strings.Contains(capturedPrompt, "type=task.message") {
		t.Error("expected structured header with message type")
	}
}

func TestHermesAdapter_DeliverMessage_SessionRetry(t *testing.T) {
	// Set up a real session store with a stale session ID so the
	// retry-on-unknown-session path actually executes.
	dir := t.TempDir()
	fsStore := store.NewFSStore(dir)
	// Pre-save a stale session mapping for our test key.
	if err := fsStore.SaveSessionState(store.SessionState{
		Adapter:    "hermes",
		SessionKey: "sess-old",
		SessionID:  "stale-session-123",
	})
	if err != nil {
		t.Fatalf("save session state: %v", err)
	}

	callCount := 0
	a := &HermesAdapter{
		nodeID:       "n1-test",
		systemPrompt: DefaultHermesSystemPrompt,
		sessionStore: fsStore,
		execCmd: func(ctx context.Context, args ...string) ([]byte, error) {
			callCount++
			// First call fails with unknown session error (stale ID)
			if callCount == 1 {
				return nil, &testError{msg: "unknown session id"}
			}
			return []byte("retry success"), nil
		},
	}

	req := DeliverMessageRequest{
		Type:       "chat.message",
		From:       "n1-sender",
		SessionKey: "sess-old",
		Message:    "test",
	}

	result, err := a.DeliverMessage(context.Background(), req)
	if err != nil {
		t.Fatalf("DeliverMessage failed: %v", err)
	}
	if !result.Success {
		t.Error("expected success after retry")
	}
	if callCount != 2 {
		t.Errorf("expected 2 calls (initial + retry), got %d", callCount)
	}
}

func TestDefaultHermesSystemPrompt_Contents(t *testing.T) {
	// Verify the default prompt covers key message types
	if !strings.Contains(DefaultHermesSystemPrompt, "chat.message") {
		t.Error("default prompt should mention chat.message")
	}
	if !strings.Contains(DefaultHermesSystemPrompt, "task.message") {
		t.Error("default prompt should mention task.message")
	}
	if !strings.Contains(DefaultHermesSystemPrompt, "todo.assigned") {
		t.Error("default prompt should mention todo.assigned")
	}
	if !strings.Contains(DefaultHermesSystemPrompt, "clawsynapse publish") {
		t.Error("default prompt should mention clawsynapse publish")
	}
	if !strings.Contains(DefaultHermesSystemPrompt, "[clawsynapse") {
		t.Error("default prompt should mention [clawsynapse header format")
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
	longPrompt := strings.Repeat("A", 500)
	args := []string{"chat", "-q", longPrompt}
	logStr := formatHermesCommandForLog(args)
	if !strings.Contains(logStr, "hermes") {
		t.Error("expected log to contain 'hermes'")
	}
	if !strings.Contains(logStr, "...(truncated") {
		t.Errorf("expected log to truncate long prompt, got: %s", logStr)
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

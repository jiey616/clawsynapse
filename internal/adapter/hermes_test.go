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

func TestHermesAdapter_DeliverMessage_Format(t *testing.T) {
	var capturedPrompt string
	a := &HermesAdapter{
		nodeID: "n1-test",
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

	// Verify prompt contains the structured header from formatDeliverMessage (bare, no system prompt)
	if !strings.HasPrefix(capturedPrompt, "[clawsynapse") {
		t.Errorf("expected prompt to start with [clawsynapse, got: %s", capturedPrompt[:min(50, len(capturedPrompt))])
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

func TestHermesAdapter_DeliverMessage_NoSystemPrompt(t *testing.T) {
	var capturedPrompt string
	a := &HermesAdapter{
		nodeID: "n1-test",
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

	result, err := a.DeliverMessage(context.Background(), req)
	if err != nil {
		t.Fatalf("DeliverMessage failed: %v", err)
	}
	if !result.Success {
		t.Error("expected success")
	}

	// Verify no system prompt — prompt starts directly with protocol header
	if !strings.HasPrefix(capturedPrompt, "[clawsynapse") {
		t.Errorf("expected prompt to start with [clawsynapse (no system prompt), got: %s",
			capturedPrompt[:min(60, len(capturedPrompt))])
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
	}); err != nil {
		t.Fatalf("save session state: %v", err)
	}

	callCount := 0
	a := &HermesAdapter{
		nodeID:       "n1-test",
		sessionStore: fsStore,
		execCmd: func(ctx context.Context, args ...string) ([]byte, error) {
			callCount++
			// First call fails with unknown session error (stale ID)
			if callCount == 1 {
				return nil, &testError{msg: "unknown session id"}
			}
			return []byte("session_id: fresh-session-456\n\nretry success"), nil
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

func TestBuildCommandArgs_SkillAndFlags(t *testing.T) {
	// Verify that -s clawsynapse, -s tm-task-plan, -Q, --resume are correct, --max-turns is NOT present
	a := &HermesAdapter{
		nodeID: "n1-test",
	}
	var capturedArgs []string
	a.execCmd = func(ctx context.Context, args ...string) ([]byte, error) {
		capturedArgs = args
		return []byte("ok"), nil
	}

	_, err := a.DeliverMessage(context.Background(), DeliverMessageRequest{
		Type:    "chat.message",
		From:    "n1-sender",
		Message: "test",
	})
	if err != nil {
		t.Fatalf("DeliverMessage failed: %v", err)
	}

	// Check that required flags are present
	if !containsArg(capturedArgs, "-q") {
		t.Error("expected -q flag")
	}
	if !containsArg(capturedArgs, "-Q") {
		t.Error("expected -Q (quiet mode) flag")
	}
	if !containsArg(capturedArgs, "-s") {
		t.Error("expected -s flag for skill")
	}
	if !containsArg(capturedArgs, "clawsynapse") {
		t.Error("expected -s clawsynapse skill name")
	}
	if !containsArg(capturedArgs, "tm-task-plan") {
		t.Error("expected -s tm-task-plan skill name")
	}
	if !containsArg(capturedArgs, "-t") {
		t.Error("expected -t flag for toolsets")
	}
	if !containsArg(capturedArgs, "--yolo") {
		t.Error("expected --yolo flag")
	}

	// Check that --max-turns is NOT present
	if containsArg(capturedArgs, "--max-turns") {
		t.Error("--max-turns should not be present")
	}

	// Without a session, --resume should NOT be present
	if containsArg(capturedArgs, "--resume") {
		t.Error("--resume should not be present without a session")
	}
}

func TestBuildCommandArgs_ResumeWithSession(t *testing.T) {
	// When a session mapping exists, --resume <id> should be present
	dir := t.TempDir()
	fsStore := store.NewFSStore(dir)
	if err := fsStore.SaveSessionState(store.SessionState{
		Adapter:    "hermes",
		SessionKey: "sess-resume-test",
		SessionID:  "20260619_100000_abc",
	}); err != nil {
		t.Fatalf("save session state: %v", err)
	}

	a := &HermesAdapter{
		nodeID:       "n1-test",
		sessionStore: fsStore,
	}
	var capturedArgs []string
	a.execCmd = func(ctx context.Context, args ...string) ([]byte, error) {
		capturedArgs = args
		return []byte("session_id: 20260619_100000_abc\n\nok"), nil
	}

	_, err := a.DeliverMessage(context.Background(), DeliverMessageRequest{
		Type:       "chat.message",
		From:       "n1-sender",
		SessionKey: "sess-resume-test",
		Message:    "test",
	})
	if err != nil {
		t.Fatalf("DeliverMessage failed: %v", err)
	}

	if !containsArg(capturedArgs, "--resume") {
		t.Error("expected --resume flag when session mapping exists")
	}

	// Verify the session ID is the value after --resume
	foundResume := false
	for i, arg := range capturedArgs {
		if arg == "--resume" && i+1 < len(capturedArgs) {
			if capturedArgs[i+1] == "20260619_100000_abc" {
				foundResume = true
			}
			break
		}
	}
	if !foundResume {
		t.Error("expected --resume 20260619_100000_abc")
	}
}

func TestParseHermesResult(t *testing.T) {
	// Quiet mode output: session_id line + blank line + reply
	input := "session_id: 20260619_120000_abc123\n\nHermes completed the task successfully."
	result, err := parseHermesResult([]byte(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Error("expected success")
	}
	if !result.Accepted {
		t.Error("expected accepted")
	}
	if result.SessionID != "20260619_120000_abc123" {
		t.Errorf("expected session ID '20260619_120000_abc123', got '%s'", result.SessionID)
	}
	if result.Reply != "Hermes completed the task successfully." {
		t.Errorf("expected clean reply, got '%s'", result.Reply)
	}
}

func TestParseHermesResult_NoSessionID(t *testing.T) {
	// Output without session_id line (e.g., older hermes version)
	result, err := parseHermesResult([]byte("plain reply without session id"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Error("expected success")
	}
	if result.SessionID != "" {
		t.Errorf("expected empty session ID, got '%s'", result.SessionID)
	}
	if result.Reply != "plain reply without session id" {
		t.Errorf("expected full output as reply, got '%s'", result.Reply)
	}
}

func TestParseHermesResult_MultiLineReply(t *testing.T) {
	input := "session_id: sess-xyz\n\nLine 1 of reply\nLine 2 of reply\nLine 3"
	result, err := parseHermesResult([]byte(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Error("expected success")
	}
	if result.SessionID != "sess-xyz" {
		t.Errorf("expected session ID 'sess-xyz', got '%s'", result.SessionID)
	}
	expected := "Line 1 of reply\nLine 2 of reply\nLine 3"
	if result.Reply != expected {
		t.Errorf("expected multi-line reply, got '%s'", result.Reply)
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
	// Output longer than 4000 characters — should NOT be truncated
	longText := strings.Repeat("A", 5000)
	input := "session_id: sess-long\n\n" + longText
	result, err := parseHermesResult([]byte(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Error("expected success for long output")
	}
	if result.SessionID != "sess-long" {
		t.Errorf("expected session ID 'sess-long', got '%s'", result.SessionID)
	}
	if result.Reply != longText {
		t.Errorf("expected full untruncated output (%d chars), got %d chars", len(longText), len(result.Reply))
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

// containsArg checks if a slice of strings contains a specific argument value.
func containsArg(args []string, target string) bool {
	for _, a := range args {
		if a == target {
			return true
		}
	}
	return false
}

// testError is a simple error type for testing.
type testError struct {
	msg string
}

func (e *testError) Error() string {
	return e.msg
}

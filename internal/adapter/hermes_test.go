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
		versionCmd: func(ctx context.Context, args ...string) ([]byte, error) {
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
		versionCmd: func(ctx context.Context, args ...string) ([]byte, error) {
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
		versionCmd: func(ctx context.Context, args ...string) ([]byte, error) {
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

func TestHermesAdapter_DeliverMessage_BackgroundMode(t *testing.T) {
	var capturedArgs []string
	a := &HermesAdapter{
		nodeID:       "n1-test",
		systemPrompt: "SYS_PROMPT",
		startCmd: func(ctx context.Context, args ...string) error {
			capturedArgs = args
			return nil
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

	// In background mode, the adapter should return Accepted=true with empty Reply
	// so the messaging layer does not send a duplicate chat.response.
	if !result.Success {
		t.Error("expected success")
	}
	if !result.Accepted {
		t.Error("expected accepted")
	}
	if result.Reply != "" {
		t.Errorf("expected empty reply in background mode, got '%s'", result.Reply)
	}

	// Verify the command starts with "chat" and includes -Q flag
	if len(capturedArgs) < 2 || capturedArgs[0] != "chat" {
		t.Errorf("expected first arg to be 'chat', got %v", capturedArgs)
	}

	// Verify -Q (quiet/programmatic mode) flag is present
	hasQuietFlag := false
	for _, arg := range capturedArgs {
		if arg == "-Q" {
			hasQuietFlag = true
			break
		}
	}
	if !hasQuietFlag {
		t.Error("expected -Q flag in command args")
	}
}

func TestHermesAdapter_DeliverMessage_UsesSystemPrompt(t *testing.T) {
	var capturedPrompt string
	a := &HermesAdapter{
		nodeID:       "n1-test",
		systemPrompt: "SYS_PROMPT",
		startCmd: func(ctx context.Context, args ...string) error {
			// Capture the -q prompt argument
			for i, arg := range args {
				if arg == "-q" && i+1 < len(args) {
					capturedPrompt = args[i+1]
					break
				}
			}
			return nil
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
		startCmd: func(ctx context.Context, args ...string) error {
			for i, arg := range args {
				if arg == "-q" && i+1 < len(args) {
					capturedPrompt = args[i+1]
					break
				}
			}
			return nil
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

func TestHermesAdapter_DeliverMessage_StartError(t *testing.T) {
	a := &HermesAdapter{
		nodeID:       "n1-test",
		systemPrompt: DefaultHermesSystemPrompt,
		startCmd: func(ctx context.Context, args ...string) error {
			return context.DeadlineExceeded
		},
	}

	req := DeliverMessageRequest{
		Type:    "chat.message",
		From:    "n1-sender",
		Message: "test",
	}

	_, err := a.DeliverMessage(context.Background(), req)
	if err == nil {
		t.Error("expected error when hermes fails to start")
	}
}

func TestHermesAdapter_DeliverMessage_WithSessionID(t *testing.T) {
	dir := t.TempDir()
	fsStore := store.NewFSStore(dir)
	if err := fsStore.SaveSessionState(store.SessionState{
		Adapter:    "hermes",
		SessionKey: "sess-existing",
		SessionID:  "hermes-session-abc",
	}); err != nil {
		t.Fatalf("save session state: %v", err)
	}

	var capturedArgs []string
	a := &HermesAdapter{
		nodeID:       "n1-test",
		systemPrompt: DefaultHermesSystemPrompt,
		sessionStore: fsStore,
		startCmd: func(ctx context.Context, args ...string) error {
			capturedArgs = args
			return nil
		},
	}

	req := DeliverMessageRequest{
		Type:       "chat.message",
		From:       "n1-sender",
		SessionKey: "sess-existing",
		Message:    "test",
	}

	result, err := a.DeliverMessage(context.Background(), req)
	if err != nil {
		t.Fatalf("DeliverMessage failed: %v", err)
	}
	if !result.Success || !result.Accepted {
		t.Error("expected success and accepted")
	}

	// Verify --session flag is present with the mapped session ID
	hasSessionFlag := false
	for i, arg := range capturedArgs {
		if arg == "--session" && i+1 < len(capturedArgs) && capturedArgs[i+1] == "hermes-session-abc" {
			hasSessionFlag = true
			break
		}
	}
	if !hasSessionFlag {
		t.Errorf("expected --session hermes-session-abc in args, got %v", capturedArgs)
	}
}

func TestHermesAdapter_DeliverMessage_NoSessionStore(t *testing.T) {
	var capturedArgs []string
	a := &HermesAdapter{
		nodeID:       "n1-test",
		systemPrompt: DefaultHermesSystemPrompt,
		sessionStore: nil,
		startCmd: func(ctx context.Context, args ...string) error {
			capturedArgs = args
			return nil
		},
	}

	req := DeliverMessageRequest{
		Type:       "chat.message",
		From:       "n1-sender",
		SessionKey: "sess-123",
		Message:    "test",
	}

	result, err := a.DeliverMessage(context.Background(), req)
	if err != nil {
		t.Fatalf("DeliverMessage failed: %v", err)
	}
	if !result.Success || !result.Accepted {
		t.Error("expected success and accepted")
	}

	// Verify no --session flag when sessionStore is nil
	for _, arg := range capturedArgs {
		if arg == "--session" {
			t.Error("expected no --session flag when sessionStore is nil")
		}
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

func TestBuildCommandArgs_QuietFlag(t *testing.T) {
	a := &HermesAdapter{
		nodeID:       "n1-test",
		systemPrompt: "test",
	}

	args := a.buildCommandArgs("test prompt", "")

	// Verify -Q flag is present
	found := false
	for _, arg := range args {
		if arg == "-Q" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected -Q flag in args: %v", args)
	}
}

func TestBuildCommandArgs_WithSession(t *testing.T) {
	a := &HermesAdapter{
		nodeID:       "n1-test",
		systemPrompt: "test",
	}

	args := a.buildCommandArgs("test prompt", "session-123")

	// Verify --session is appended
	found := false
	for i, arg := range args {
		if arg == "--session" && i+1 < len(args) && args[i+1] == "session-123" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected --session session-123 in args: %v", args)
	}
}

func TestBuildCommandArgs_NoSession(t *testing.T) {
	a := &HermesAdapter{
		nodeID:       "n1-test",
		systemPrompt: "test",
	}

	args := a.buildCommandArgs("test prompt", "")

	for _, arg := range args {
		if arg == "--session" {
			t.Errorf("expected no --session flag when sessionID is empty, got: %v", args)
		}
	}
}

func TestHermesAdapter_Shutdown(t *testing.T) {
	a := &HermesAdapter{
		nodeID:       "n1-test",
		systemPrompt: "test",
		startCmd:     func(ctx context.Context, args ...string) error { return nil },
		versionCmd:   func(ctx context.Context, args ...string) ([]byte, error) { return []byte("v1"), nil },
	}

	// Shutdown with no processes should not panic
	a.Shutdown()
}

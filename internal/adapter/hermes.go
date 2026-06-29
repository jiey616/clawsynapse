package adapter

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"clawsynapse/internal/store"
)

// HermesConfig holds configuration for the Hermes agent adapter.
type HermesConfig struct {
	NodeID       string
	Logger       *slog.Logger
	SessionStore *store.FSStore
	AgentRole    string // pm | executor | ""
}

// HermesAdapter delivers messages to a local Hermes agent via the hermes CLI.
type HermesAdapter struct {
	nodeID       string
	log          *slog.Logger
	sessionStore *store.FSStore
	agentRole    string
	execCmd      func(ctx context.Context, args ...string) ([]byte, error)
}

// NewHermesAdapter creates a Hermes adapter instance.
func NewHermesAdapter(cfg HermesConfig) (*HermesAdapter, error) {
	return &HermesAdapter{
		nodeID:       strings.TrimSpace(cfg.NodeID),
		log:          cfg.Logger,
		sessionStore: cfg.SessionStore,
		agentRole:    strings.ToLower(strings.TrimSpace(cfg.AgentRole)),
		execCmd:      defaultHermesExecCmd,
	}, nil
}

// DeliverMessage formats the incoming message with the standard ClawSynapse
// protocol header, invokes hermes chat -q, waits for completion, and
// returns the result.
func (a *HermesAdapter) DeliverMessage(ctx context.Context, req DeliverMessageRequest) (*DeliverMessageResult, error) {
	// Use the standard structured header format (same as openclaw/opencode/codex)
	formatted := formatDeliverMessage(a.nodeID, req)

	sessionID := a.loadMappedSessionID(req.SessionKey)

	// Fallback: when the message SessionKey differs from the original
	// (e.g. todo.assigned → task.context.result round-trip where the agent
	// generated a new SessionKey), look up the Hermes session via taskId
	// from Metadata, which is stable across the task lifecycle.
	if sessionID == "" {
		if taskID := extractTaskID(req.Metadata); taskID != "" {
			sessionID = a.loadMappedSessionID(taskID)
			if sessionID != "" && a.log != nil {
				a.log.Info("hermes session resolved via taskId fallback",
					slog.String("sessionKey", req.SessionKey),
					slog.String("taskId", taskID),
					slog.String("hermesSessionId", sessionID),
				)
			}
		}
	}

	out, err := a.runCommand(ctx, formatted, sessionID)
	if err != nil && sessionID != "" && isHermesUnknownSessionError(err) {
		a.deleteMappedSession(req.SessionKey)
		out, err = a.runCommand(ctx, formatted, "")
	}
	if err != nil {
		return nil, fmt.Errorf("hermes exec command: %w", err)
	}

	result, err := parseHermesResult(out)
	if err != nil {
		return nil, err
	}

	// Always save the primary SessionKey mapping.
	a.saveMappedSession(req.SessionKey, result.SessionID)

	// Also index by taskId so follow-up messages (which may carry a
	// different SessionKey) can still find the same Hermes session.
	if taskID := extractTaskID(req.Metadata); taskID != "" && taskID != req.SessionKey {
		a.saveMappedSession(taskID, result.SessionID)
	}

	return result, nil
}

// GetStatus checks whether the hermes CLI is available and working.
func (a *HermesAdapter) GetStatus(ctx context.Context) (*AgentStatus, error) {
	out, err := a.execCmd(ctx, "--version")
	if err != nil {
		return &AgentStatus{Healthy: false}, err
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return &AgentStatus{Healthy: false}, nil
	}
	return &AgentStatus{Healthy: true}, nil
}

// ── Command execution ──────────────────────────────────────────────

func (a *HermesAdapter) runCommand(ctx context.Context, prompt string, sessionID string) ([]byte, error) {
	// Build hermes chat -q command
	// -Q enables quiet mode: suppresses banner/spinner, outputs session_id + final response only
	args := []string{
		"chat", "-q", prompt,
		"-Q",
		"-t", "terminal",
		"--yolo",
	}

	// Protocol skill (clawsynapse) is always required for message delivery
	args = append(args, "-s", "clawsynapse")

	// Load business skill based on the configured agent role
	switch a.agentRole {
	case "pm":
		args = append(args, "-s", "tm-task-plan")
	case "executor":
		args = append(args, "-s", "tm-task-exec")
	}

	if sessionID = strings.TrimSpace(sessionID); sessionID != "" {
		args = append(args, "--resume", sessionID)
	}

	a.logCommand(args, sessionID)
	return a.execCmd(ctx, args...)
}

// ── Task ID extraction ─────────────────────────────────────────────

// extractTaskID returns the taskId from message metadata, if present.
// This is used as a stable fallback key when the message SessionKey
// changes across rounds (e.g. todo.assigned → task.context.result).
func extractTaskID(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	id, _ := metadata["taskId"].(string)
	return strings.TrimSpace(id)
}

// ── Session mapping (reuses store.SessionState like Codex adapter) ──

func (a *HermesAdapter) loadMappedSessionID(sessionKey string) string {
	if a.sessionStore == nil {
		return ""
	}
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return ""
	}

	st, ok, err := a.sessionStore.LoadSessionState("hermes", sessionKey)
	if err != nil {
		a.logStoreWarning("load hermes session mapping failed", sessionKey, err)
		return ""
	}
	if !ok {
		return ""
	}
	return strings.TrimSpace(st.SessionID)
}

func (a *HermesAdapter) saveMappedSession(sessionKey string, sessionID string) {
	if a.sessionStore == nil {
		return
	}
	sessionKey = strings.TrimSpace(sessionKey)
	sessionID = strings.TrimSpace(sessionID)
	if sessionKey == "" || sessionID == "" {
		return
	}

	existing, ok, err := a.sessionStore.LoadSessionState("hermes", sessionKey)
	if err != nil {
		a.logStoreWarning("load hermes session mapping failed", sessionKey, err)
		return
	}
	if ok && strings.TrimSpace(existing.SessionID) == sessionID {
		return
	}

	now := time.Now().UnixMilli()
	createdAt := now
	if ok && existing.CreatedAtMs > 0 {
		createdAt = existing.CreatedAtMs
	}

	if err := a.sessionStore.SaveSessionState(store.SessionState{
		Adapter:      "hermes",
		SessionKey:   sessionKey,
		SessionID:    sessionID,
		CreatedAtMs:  createdAt,
		UpdatedAtMs:  now,
	}); err != nil {
		a.logStoreWarning("save hermes session mapping failed", sessionKey, err)
	}
}

func (a *HermesAdapter) deleteMappedSession(sessionKey string) {
	if a.sessionStore == nil {
		return
	}
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return
	}
	if err := a.sessionStore.DeleteSessionState("hermes", sessionKey); err != nil {
		a.logStoreWarning("delete hermes session mapping failed", sessionKey, err)
	}
}

// ── Logging helpers ─────────────────────────────────────────────────

func (a *HermesAdapter) logStoreWarning(msg string, sessionKey string, err error) {
	if a.log == nil {
		return
	}
	a.log.Warn(msg,
		slog.String("sessionKey", sessionKey),
		slog.String("error", err.Error()),
	)
}

func (a *HermesAdapter) logCommand(args []string, sessionID string) {
	if a.log == nil {
		return
	}
	a.log.Info("executing hermes chat command",
		slog.String("sessionId", sessionID),
		slog.String("command", formatHermesCommandForLog(args)),
	)
}

// ── Output parsing ──────────────────────────────────────────────────

// parseHermesResult parses the output from `hermes chat -Q`.
// In quiet mode, hermes outputs:
//
//	session_id: <SESSION_ID>
//	<blank line>
//	<actual reply text>
//
// We extract the session_id for continuity and use the remaining lines as the reply.
func parseHermesResult(data []byte) (*DeliverMessageResult, error) {
	text := strings.TrimSpace(string(data))
	if text == "" {
		return &DeliverMessageResult{
			Success: false,
			Error:   "hermes returned empty output",
		}, nil
	}

	lines := strings.Split(text, "\n")

	// Extract session_id from the first line if present
	var sessionID string
	var replyLines []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if sessionID == "" && strings.HasPrefix(trimmed, "session_id:") {
			sessionID = strings.TrimSpace(strings.TrimPrefix(trimmed, "session_id:"))
			continue // skip this line, don't include in reply
		}
		replyLines = append(replyLines, line)
	}

	reply := strings.TrimSpace(strings.Join(replyLines, "\n"))
	if reply == "" {
		return &DeliverMessageResult{
			Success:  false,
			Error:    "hermes returned empty reply (only session_id line)",
			SessionID: sessionID,
		}, nil
	}

	return &DeliverMessageResult{
		Success:   true,
		Accepted:  true,
		SessionID: sessionID,
		Reply:     reply,
	}, nil
}

// ── Command formatting for logging ──────────────────────────────────

func formatHermesCommandForLog(args []string) string {
	logArgs := append([]string(nil), args...)

	// Truncate the prompt (last argument after -q)
	for i := 0; i < len(logArgs)-1; i++ {
		if logArgs[i] == "-q" && i+1 < len(logArgs) {
			logArgs[i+1] = truncateForLog(logArgs[i+1], 240)
			break
		}
	}

	parts := make([]string, 0, len(logArgs)+1)
	parts = append(parts, "hermes")
	for _, arg := range logArgs {
		parts = append(parts, strconv.Quote(arg))
	}
	return strings.Join(parts, " ")
}

// ── Error detection ─────────────────────────────────────────────────

func isHermesUnknownSessionError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "unknown session") ||
		strings.Contains(msg, "no such session") ||
		strings.Contains(msg, "no recorded session")
}

// ── Default command executor ────────────────────────────────────────

func defaultHermesExecCmd(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "hermes", args...)

	// Close stdin to prevent hermes from reading prompt from stdin
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return nil, fmt.Errorf("open /dev/null: %w", err)
	}
	defer devNull.Close()
	cmd.Stdin = devNull

	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("hermes command canceled: %w", ctxErr)
		}
		// CombinedOutput already includes stderr, no need to read exitErr.Stderr
		return nil, fmt.Errorf("hermes exited: %w", err)
	}
	return out, nil
}

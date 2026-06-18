package adapter

import (
	"bytes"
	"context"
	"errors"
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
}

// HermesAdapter delivers messages to a local Hermes agent via the hermes CLI.
type HermesAdapter struct {
	nodeID       string
	log          *slog.Logger
	sessionStore *store.FSStore
	execCmd      func(ctx context.Context, args ...string) ([]byte, error)
}

// NewHermesAdapter creates a Hermes adapter instance.
func NewHermesAdapter(cfg HermesConfig) (*HermesAdapter, error) {
	return &HermesAdapter{
		nodeID:       strings.TrimSpace(cfg.NodeID),
		log:          cfg.Logger,
		sessionStore: cfg.SessionStore,
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
	a.saveMappedSession(req.SessionKey, result.SessionID)

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
	args := []string{
		"chat", "-q", prompt,
		"-t", "terminal",
		"--yolo",
	}

	if sessionID = strings.TrimSpace(sessionID); sessionID != "" {
		args = append(args, "--session", sessionID)
	}

	a.logCommand(args, sessionID)
	return a.execCmd(ctx, args...)
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

// parseHermesResult parses the plain-text output from hermes chat.
// Hermes outputs plain text (not JSON stream), so we take the output
// as the reply directly.
func parseHermesResult(data []byte) (*DeliverMessageResult, error) {
	text := strings.TrimSpace(string(data))
	if text == "" {
		return &DeliverMessageResult{
			Success: false,
			Error:   "hermes returned empty output",
		}, nil
	}

	// Return the full output as the reply (same as openclaw/opencode/codex).
	// Hermes outputs plain text — the entire response is treated as the reply.
	return &DeliverMessageResult{
		Success:  true,
		Accepted: true,
		Reply:    text,
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

	out, err := cmd.Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("hermes command canceled: %w", ctxErr)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr := strings.TrimSpace(string(exitErr.Stderr))
			return nil, fmt.Errorf("hermes exited %s: %s", strconv.Itoa(exitErr.ExitCode()), stderr)
		}
		return nil, err
	}
	return out, nil
}

package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"clawsynapse/internal/store"
)

type OpenCodeConfig struct {
	NodeID       string
	Logger       *slog.Logger
	SessionStore *store.FSStore
}

type OpenCodeAdapter struct {
	nodeID       string
	log          *slog.Logger
	sessionStore *store.FSStore
	execCmd      func(ctx context.Context, args ...string) ([]byte, error)
}

func NewOpenCodeAdapter(cfg OpenCodeConfig) (*OpenCodeAdapter, error) {
	return &OpenCodeAdapter{
		nodeID:       strings.TrimSpace(cfg.NodeID),
		log:          cfg.Logger,
		sessionStore: cfg.SessionStore,
		execCmd:      defaultOpenCodeExecCmd,
	}, nil
}

func (a *OpenCodeAdapter) DeliverMessage(ctx context.Context, req DeliverMessageRequest) (*DeliverMessageResult, error) {
	msg := formatDeliverMessage(a.nodeID, req)
	sessionID := a.loadMappedSessionID(req.SessionKey)

	out, err := a.runCommand(ctx, msg, sessionID)
	if err != nil && sessionID != "" && isOpenCodeUnknownSessionError(err) {
		a.deleteMappedSession(req.SessionKey)
		out, err = a.runCommand(ctx, msg, "")
	}
	if err != nil {
		return nil, fmt.Errorf("opencode run command: %w", err)
	}

	result, err := parseOpenCodeResult(out)
	if err != nil {
		return nil, err
	}
	a.saveMappedSession(req.SessionKey, result.SessionID)
	return result, nil
}

func (a *OpenCodeAdapter) GetStatus(ctx context.Context) (*AgentStatus, error) {
	out, err := a.execCmd(ctx, "version")
	if err != nil {
		return &AgentStatus{Healthy: false}, err
	}
	if len(out) == 0 {
		return &AgentStatus{Healthy: false}, nil
	}
	return &AgentStatus{Healthy: true}, nil
}

type openCodeEvent struct {
	Type      string `json:"type"`
	Content   string `json:"content"`
	Text      string `json:"text"`
	Error     string `json:"error"`
	SessionID string `json:"sessionID"`
}

func parseOpenCodeResult(data []byte) (*DeliverMessageResult, error) {
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))

	var lastText string
	var lastError string
	var lastSessionID string
	var parsed bool

	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var evt openCodeEvent
		if err := json.Unmarshal(line, &evt); err != nil {
			continue
		}
		parsed = true
		if evt.Error != "" {
			lastError = evt.Error
		}
		if evt.SessionID != "" {
			lastSessionID = strings.TrimSpace(evt.SessionID)
		}
		text := firstNonEmpty(evt.Content, evt.Text)
		if text != "" {
			lastText = text
		}
	}

	if !parsed {
		text := strings.TrimSpace(string(data))
		if text == "" {
			return &DeliverMessageResult{
				Success: false,
				Error:   "opencode returned empty output",
			}, nil
		}
		return &DeliverMessageResult{
			Success:   true,
			Accepted:  true,
			SessionID: lastSessionID,
			Reply:     text,
		}, nil
	}

	if lastError != "" {
		return &DeliverMessageResult{
			Success: false,
			Error:   lastError,
		}, nil
	}

	return &DeliverMessageResult{
		Success:   true,
		Accepted:  true,
		SessionID: lastSessionID,
		Reply:     lastText,
	}, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" {
			return v
		}
	}
	return ""
}

func (a *OpenCodeAdapter) runCommand(ctx context.Context, msg string, sessionID string) ([]byte, error) {
	args := []string{"run", msg, "--format", "json"}
	if sessionID = strings.TrimSpace(sessionID); sessionID != "" {
		args = append(args, "--session", sessionID)
	}

	a.logCommand(args)
	return a.execCmd(ctx, args...)
}

func (a *OpenCodeAdapter) loadMappedSessionID(sessionKey string) string {
	if a.sessionStore == nil {
		return ""
	}
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return ""
	}

	st, ok, err := a.sessionStore.LoadSessionState("opencode", sessionKey)
	if err != nil {
		a.logStoreWarning("load opencode session mapping failed", sessionKey, err)
		return ""
	}
	if !ok {
		return ""
	}
	return strings.TrimSpace(st.SessionID)
}

func (a *OpenCodeAdapter) saveMappedSession(sessionKey string, sessionID string) {
	if a.sessionStore == nil {
		return
	}
	sessionKey = strings.TrimSpace(sessionKey)
	sessionID = strings.TrimSpace(sessionID)
	if sessionKey == "" || sessionID == "" {
		return
	}

	existing, ok, err := a.sessionStore.LoadSessionState("opencode", sessionKey)
	if err != nil {
		a.logStoreWarning("load opencode session mapping failed", sessionKey, err)
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
		Adapter:     "opencode",
		SessionKey:  sessionKey,
		SessionID:   sessionID,
		CreatedAtMs: createdAt,
		UpdatedAtMs: now,
	}); err != nil {
		a.logStoreWarning("save opencode session mapping failed", sessionKey, err)
	}
}

func (a *OpenCodeAdapter) deleteMappedSession(sessionKey string) {
	if a.sessionStore == nil {
		return
	}
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return
	}
	if err := a.sessionStore.DeleteSessionState("opencode", sessionKey); err != nil {
		a.logStoreWarning("delete opencode session mapping failed", sessionKey, err)
	}
}

func (a *OpenCodeAdapter) logStoreWarning(msg string, sessionKey string, err error) {
	if a.log == nil {
		return
	}
	a.log.Warn(msg,
		slog.String("sessionKey", sessionKey),
		slog.String("error", err.Error()),
	)
}

func isOpenCodeUnknownSessionError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unknown session") || strings.Contains(msg, "session not found")
}

func (a *OpenCodeAdapter) logCommand(args []string) {
	if a.log == nil {
		return
	}
	a.log.Info("executing opencode run command",
		slog.String("sessionID", redactedOpenCodeSessionID(args)),
		slog.String("command", formatOpenCodeCommandForLog(args)),
	)
}

func formatOpenCodeCommandForLog(args []string) string {
	logArgs := append([]string(nil), args...)
	if len(logArgs) > 1 {
		logArgs[1] = truncateForLog(logArgs[1], 240)
	}

	parts := make([]string, 0, len(logArgs)+1)
	parts = append(parts, "opencode")
	for _, arg := range logArgs {
		parts = append(parts, strconv.Quote(arg))
	}
	return strings.Join(parts, " ")
}

func redactedOpenCodeSessionID(args []string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--session" {
			return args[i+1]
		}
	}
	return ""
}

func defaultOpenCodeExecCmd(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "opencode", args...)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr := strings.TrimSpace(string(exitErr.Stderr))
			return nil, fmt.Errorf("opencode exited %s: %s", strconv.Itoa(exitErr.ExitCode()), stderr)
		}
		return nil, err
	}
	return out, nil
}

package adapter

import (
	"bytes"
	"context"
	"encoding/json"
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

type CodexConfig struct {
	NodeID       string
	Logger       *slog.Logger
	SessionStore *store.FSStore
}

type CodexAdapter struct {
	nodeID       string
	log          *slog.Logger
	sessionStore *store.FSStore
	execCmd      func(ctx context.Context, args ...string) ([]byte, error)
}

func NewCodexAdapter(cfg CodexConfig) (*CodexAdapter, error) {
	return &CodexAdapter{
		nodeID:       strings.TrimSpace(cfg.NodeID),
		log:          cfg.Logger,
		sessionStore: cfg.SessionStore,
		execCmd:      defaultCodexExecCmd,
	}, nil
}

func (a *CodexAdapter) DeliverMessage(ctx context.Context, req DeliverMessageRequest) (*DeliverMessageResult, error) {
	msg := formatDeliverMessage(a.nodeID, req)
	sessionID := a.loadMappedSessionID(req.SessionKey)

	out, err := a.runCommand(ctx, msg, sessionID)
	if err != nil && sessionID != "" && isCodexUnknownSessionError(err) {
		a.deleteMappedSession(req.SessionKey)
		out, err = a.runCommand(ctx, msg, "")
	}
	if err != nil {
		return nil, fmt.Errorf("codex exec command: %w", err)
	}

	result, err := parseCodexResult(out)
	if err != nil {
		return nil, err
	}
	a.saveMappedSession(req.SessionKey, result.SessionID)
	return result, nil
}

func (a *CodexAdapter) GetStatus(ctx context.Context) (*AgentStatus, error) {
	out, err := a.execCmd(ctx, "--version")
	if err != nil {
		return &AgentStatus{Healthy: false}, err
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return &AgentStatus{Healthy: false}, nil
	}
	return &AgentStatus{Healthy: true}, nil
}

type codexEvent struct {
	Type     string          `json:"type"`
	ThreadID string          `json:"thread_id"`
	Item     *codexEventItem `json:"item"`
	Error    string          `json:"error"`
	Message  string          `json:"message"`
}

type codexEventItem struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Text string `json:"text"`
}

func parseCodexResult(data []byte) (*DeliverMessageResult, error) {
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))

	var lastReply string
	var lastError string
	var threadID string
	var parsed bool

	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var evt codexEvent
		if err := json.Unmarshal(line, &evt); err != nil {
			continue
		}
		parsed = true
		if evt.ThreadID != "" {
			threadID = strings.TrimSpace(evt.ThreadID)
		}
		if evt.Error != "" {
			lastError = evt.Error
		}
		if evt.Type == "turn.failed" && evt.Message != "" {
			lastError = evt.Message
		}
		if evt.Item != nil && evt.Item.Type == "agent_message" {
			if text := strings.TrimSpace(evt.Item.Text); text != "" {
				lastReply = text
			}
		}
	}

	if !parsed {
		text := strings.TrimSpace(string(data))
		if text == "" {
			return &DeliverMessageResult{
				Success: false,
				Error:   "codex returned empty output",
			}, nil
		}
		return &DeliverMessageResult{
			Success:   true,
			Accepted:  true,
			SessionID: threadID,
			Reply:     text,
		}, nil
	}

	if lastError != "" {
		return &DeliverMessageResult{
			Success:   false,
			Error:     lastError,
			SessionID: threadID,
		}, nil
	}

	return &DeliverMessageResult{
		Success:   true,
		Accepted:  true,
		SessionID: threadID,
		Reply:     lastReply,
	}, nil
}

func (a *CodexAdapter) runCommand(ctx context.Context, msg string, sessionID string) ([]byte, error) {
	args := []string{"exec"}
	if sessionID = strings.TrimSpace(sessionID); sessionID != "" {
		args = append(args, "resume", "--json", "--skip-git-repo-check", sessionID, "--", msg)
	} else {
		args = append(args, "--json", "--skip-git-repo-check", "--", msg)
	}

	a.logCommand(args)
	return a.execCmd(ctx, args...)
}

func (a *CodexAdapter) loadMappedSessionID(sessionKey string) string {
	if a.sessionStore == nil {
		return ""
	}
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return ""
	}

	st, ok, err := a.sessionStore.LoadSessionState("codex", sessionKey)
	if err != nil {
		a.logStoreWarning("load codex session mapping failed", sessionKey, err)
		return ""
	}
	if !ok {
		return ""
	}
	return strings.TrimSpace(st.SessionID)
}

func (a *CodexAdapter) saveMappedSession(sessionKey string, sessionID string) {
	if a.sessionStore == nil {
		return
	}
	sessionKey = strings.TrimSpace(sessionKey)
	sessionID = strings.TrimSpace(sessionID)
	if sessionKey == "" || sessionID == "" {
		return
	}

	existing, ok, err := a.sessionStore.LoadSessionState("codex", sessionKey)
	if err != nil {
		a.logStoreWarning("load codex session mapping failed", sessionKey, err)
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
		Adapter:     "codex",
		SessionKey:  sessionKey,
		SessionID:   sessionID,
		CreatedAtMs: createdAt,
		UpdatedAtMs: now,
	}); err != nil {
		a.logStoreWarning("save codex session mapping failed", sessionKey, err)
	}
}

func (a *CodexAdapter) deleteMappedSession(sessionKey string) {
	if a.sessionStore == nil {
		return
	}
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return
	}
	if err := a.sessionStore.DeleteSessionState("codex", sessionKey); err != nil {
		a.logStoreWarning("delete codex session mapping failed", sessionKey, err)
	}
}

func (a *CodexAdapter) logStoreWarning(msg string, sessionKey string, err error) {
	if a.log == nil {
		return
	}
	a.log.Warn(msg,
		slog.String("sessionKey", sessionKey),
		slog.String("error", err.Error()),
	)
}

func isCodexUnknownSessionError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "unknown session") ||
		strings.Contains(msg, "no such session") ||
		strings.Contains(msg, "no recorded session")
}

func (a *CodexAdapter) logCommand(args []string) {
	if a.log == nil {
		return
	}
	a.log.Info("executing codex exec command",
		slog.String("sessionID", redactedCodexSessionID(args)),
		slog.String("command", formatCodexCommandForLog(args)),
	)
}

func formatCodexCommandForLog(args []string) string {
	logArgs := append([]string(nil), args...)
	// 最后一个参数是 prompt（位于 `--` 之后），截断它
	for i := len(logArgs) - 1; i >= 0; i-- {
		if logArgs[i] == "--" {
			break
		}
		// 截断 `--` 之后的最后一个非空参数
		if i == len(logArgs)-1 {
			logArgs[i] = truncateForLog(logArgs[i], 240)
		}
	}

	parts := make([]string, 0, len(logArgs)+1)
	parts = append(parts, "codex")
	for _, arg := range logArgs {
		parts = append(parts, strconv.Quote(arg))
	}
	return strings.Join(parts, " ")
}

// redactedCodexSessionID 提取 `codex exec resume ... <SESSION_ID> -- <prompt>` 中的 SESSION_ID。
func redactedCodexSessionID(args []string) string {
	if len(args) < 2 || args[0] != "exec" || args[1] != "resume" {
		return ""
	}
	// SESSION_ID 是 `--` 之前的最后一个非 flag 参数
	for i := len(args) - 1; i >= 2; i-- {
		if args[i] == "--" && i >= 1 {
			// 取 `--` 之前的最后一个非 flag token
			if i-1 >= 2 && !strings.HasPrefix(args[i-1], "-") {
				return args[i-1]
			}
		}
	}
	return ""
}

func defaultCodexExecCmd(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "codex", args...)
	// 必须显式置空 stdin，避免 codex 检测到管道 stdin 后读取并污染 prompt
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return nil, fmt.Errorf("open /dev/null: %w", err)
	}
	defer devNull.Close()
	cmd.Stdin = devNull

	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr := strings.TrimSpace(string(exitErr.Stderr))
			return nil, fmt.Errorf("codex exited %s: %s", strconv.Itoa(exitErr.ExitCode()), stderr)
		}
		return nil, err
	}
	return out, nil
}

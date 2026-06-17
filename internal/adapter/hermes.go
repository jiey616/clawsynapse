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

// DeliverMessage formats the incoming message as a prompt, invokes hermes chat -q,
// waits for completion, and returns the result.
func (a *HermesAdapter) DeliverMessage(ctx context.Context, req DeliverMessageRequest) (*DeliverMessageResult, error) {
	msg := formatDeliverMessage(a.nodeID, req)
	sessionID := a.loadMappedSessionID(req.SessionKey)

	// Parse header to extract message type
	msgType, _, _, body := parseHeader(msg)

	// Build the hermes prompt based on message type
	prompt := buildHermesPrompt(msgType, req.From, req.SessionKey, body)

	out, err := a.runCommand(ctx, prompt, sessionID)
	if err != nil && sessionID != "" && isHermesUnknownSessionError(err) {
		a.deleteMappedSession(req.SessionKey)
		out, err = a.runCommand(ctx, prompt, "")
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
		"-s", "tm-task-exec",
		"-s", "wrt-writer",
		"-t", "terminal",
		"--max-turns", "100",
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

	// Use the last 4000 characters as the reply
	// (hermes output tail typically contains the final summary)
	reply := text
	if len(reply) > 4000 {
		reply = "...(truncated)\n" + reply[len(reply)-4000:]
	}

	return &DeliverMessageResult{
		Success:  true,
		Accepted: true,
		Reply:    reply,
	}, nil
}

// ── Prompt building ─────────────────────────────────────────────────

// buildHermesPrompt constructs a hermes chat -q prompt based on the
// incoming message type. This mirrors the logic from openclaw-wrapper.py.
func buildHermesPrompt(msgType string, from string, sessionKey string, body string) string {
	target := from // reply to sender by default

	switch msgType {
	case "chat.message":
		return fmt.Sprintf(
			"你通过 ClawSynapse/TrustMesh 收到一条聊天消息。\n"+
				"发送者: %s\n"+
				"会话: %s\n"+
				"消息: %s\n\n"+
				"请理解内容，用 clawsynapse publish 回复。\n"+
				"回复格式：\n"+
				"  clawsynapse publish --type chat.message --target %s --session-key %s --message \"你的回复\"",
			from, sessionKey, body, target, sessionKey,
		)

	case "task.message":
		return fmt.Sprintf(
			"你通过 ClawSynapse/TrustMesh 收到一条任务消息。\n"+
				"发送者: %s\n"+
				"会话: %s\n"+
				"内容: %s\n\n"+
				"请理解任务内容，用 clawsynapse publish 回复。\n"+
				"  clawsynapse publish --type task.reply --target %s --session-key %s --message \"你的回复\"",
			from, sessionKey, body, target, sessionKey,
		)

	case "todo.assigned":
		return fmt.Sprintf(
			"你收到一个来自 ClawSynapse/TrustMesh 的 Todo 任务分配。这是你的主任务，请完成全部工作。\n"+
				"发送者: %s\n"+
				"会话: %s\n"+
				"内容: %s\n\n"+
				"请严格按照以下步骤执行，每一步都用 clawsynapse publish 发送：\n\n"+
				"【步骤 1 — 开工确认】发送 task.comment\n"+
				"  从内容中提取 task_id 和 todo_id，构建 JSON：\n"+
				"  {\"task_id\": \"<TASK_ID>\", \"todo_id\": \"<TODO_ID>\", \"content\": \"已收到任务，开始处理\"}\n"+
				"  clawsynapse publish --target %s --type task.comment --session-key %s --message \"<JSON>\"\n\n"+
				"【步骤 2 — 进度确认】发送 todo.progress\n"+
				"  {\"task_id\": \"<TASK_ID>\", \"todo_id\": \"<TODO_ID>\", \"message\": \"开始执行\"}\n"+
				"  clawsynapse publish --target %s --type todo.progress --session-key %s --message \"<JSON>\"\n\n"+
				"【步骤 3 — 实际工作】执行 Todo 内容，完成后：\n"+
				"  a) 创建实际交付物（代码文件、文档等），保存在本地\n"+
				"  b) 用 clawsynapse transfer send 上传文件：\n"+
				"     clawsynapse transfer send --target %s --file /path/to/file --mime-type text/plain\n"+
				"  c) 构建 todo.complete 并发送：\n"+
				"     {\"task_id\": \"<TASK_ID>\", \"todo_id\": \"<TODO_ID>\", \"result\": {\"summary\": \"一句话总结\", \"output\": \"详细输出\"}}\n"+
				"     clawsynapse publish --target %s --type todo.complete --session-key %s --message \"<JSON>\"\n\n"+
				"关键规则：\n"+
				"- 步骤 1、2 快速完成（各 1 条命令）\n"+
				"- 步骤 3 花大部分轮次构建实际作品\n"+
				"- 交付物文件用 .md 格式\n"+
				"- 先上传文件再发 todo.complete\n"+
				"- 所有 clawsynapse publish 用 --target %s --session-key %s",
			from, sessionKey, body,
			target, sessionKey,
			target, sessionKey,
			target,
			target, sessionKey,
			target, sessionKey,
		)

	case "task.context.result":
		return fmt.Sprintf(
			"你收到 ClawSynapse/TrustMesh 返回的任务上下文查询结果。\n"+
				"会话: %s\n"+
				"内容: %s\n\n"+
				"请理解其中的任务快照信息，用于后续工作。",
			sessionKey, body,
		)

	default:
		return fmt.Sprintf(
			"你通过 ClawSynapse/TrustMesh 收到一条消息。\n"+
				"类型: %s\n"+
				"发送者: %s\n"+
				"会话: %s\n"+
				"内容: %s\n\n"+
				"请理解并适当回复。",
			msgType, from, sessionKey, body,
		)
	}
}

// ── Header parsing ──────────────────────────────────────────────────

// parseHeader extracts message type, sender, session, and body from a
// ClawSynapse-formatted message string.
//
// Format:
//
//	[clawsynapse type=chat.message from=n1-xxx to=n1-yyy session=abc ...]
//	<body>
func parseHeader(message string) (msgType string, from string, session string, body string) {
	msgType = "chat.message"
	body = message

	// Find the [clawsynapse ...] header block
	if !strings.HasPrefix(message, "[clawsynapse") {
		return
	}

	closingBracket := strings.Index(message, "]")
	if closingBracket == -1 {
		return
	}

	headerStr := message[len("[clawsynapse"):closingBracket]
	body = strings.TrimSpace(message[closingBracket+1:])

	// Parse key=value pairs from the header
	fields := strings.Fields(headerStr)
	for _, field := range fields {
		eqIdx := strings.Index(field, "=")
		if eqIdx == -1 {
			continue
		}
		key := field[:eqIdx]
		value := field[eqIdx+1:]

		switch key {
		case "type":
			msgType = value
		case "from":
			from = value
		case "session":
			session = value
		}
	}

	return
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

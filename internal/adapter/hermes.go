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
	"sync"
	"time"

	"clawsynapse/internal/store"
)

// DefaultHermesSystemPrompt is the default system prompt used when no
// custom prompt is configured. It instructs hermes how to interpret
// ClawSynapse protocol headers and how to reply via clawsynapse publish.
const DefaultHermesSystemPrompt = `You are an AI node in the ClawSynapse/TrustMesh network.
When you receive a message starting with [clawsynapse ...], parse the header to understand:
- type: the message type (chat.message, task.message, todo.assigned, task.context.result, etc.)
- from: the sender node ID
- session: the session key for this conversation
- The text after the header is the message body.

Reply using: clawsynapse publish --type <reply_type> --target <sender> --session-key <session> --message "your reply"

Message type guidelines:
- chat.message → reply with type chat.message
- task.message → reply with type task.reply
- todo.assigned → acknowledge with task.comment, update progress with todo.progress, complete with todo.complete
- For todo.assigned: create deliverables locally first, upload with: clawsynapse transfer send --target <sender> --file <path> --mime-type <type>
- Always send todo.complete AFTER uploading deliverables`

// HermesConfig holds configuration for the Hermes agent adapter.
type HermesConfig struct {
	NodeID       string
	SystemPrompt string // optional custom system prompt; defaults to DefaultHermesSystemPrompt
	Logger       *slog.Logger
	SessionStore *store.FSStore
}

// hermesProcess tracks a running hermes process for lifecycle management.
type hermesProcess struct {
	cmd       *exec.Cmd
	startedAt time.Time
}

// HermesAdapter delivers messages to a local Hermes agent via the hermes CLI.
// It runs hermes chat in background (self-driven mode): the agent receives the
// message, processes it, and replies by calling clawsynapse publish on its own.
// The adapter returns Accepted=true with an empty Reply so the messaging layer
// does not send a duplicate chat.response.
type HermesAdapter struct {
	nodeID       string
	systemPrompt string
	log          *slog.Logger
	sessionStore *store.FSStore

	mu        sync.Mutex
	processes []hermesProcess

	// startCmd launches a hermes command in the background and returns
	// immediately. Returns an error only if the command fails to start.
	startCmd func(ctx context.Context, args ...string) error

	// versionCmd runs a hermes command synchronously and returns its output.
	// Used by GetStatus for the health check.
	versionCmd func(ctx context.Context, args ...string) ([]byte, error)
}

// NewHermesAdapter creates a Hermes adapter instance.
func NewHermesAdapter(cfg HermesConfig) (*HermesAdapter, error) {
	sp := strings.TrimSpace(cfg.SystemPrompt)
	if sp == "" {
		sp = DefaultHermesSystemPrompt
	}
	a := &HermesAdapter{
		nodeID:       strings.TrimSpace(cfg.NodeID),
		systemPrompt: sp,
		log:          cfg.Logger,
		sessionStore: cfg.SessionStore,
	}
	a.startCmd = a.defaultStartCmd
	a.versionCmd = defaultHermesVersionCmd
	return a, nil
}

// DeliverMessage formats the incoming message with the standard ClawSynapse
// protocol header, prepends the system prompt, and launches hermes chat
// in the background. The hermes agent self-replies via clawsynapse publish,
// so this method returns Accepted=true with an empty Reply to prevent
// the messaging layer from sending a duplicate chat.response.
func (a *HermesAdapter) DeliverMessage(ctx context.Context, req DeliverMessageRequest) (*DeliverMessageResult, error) {
	// Use the standard structured header format (same as openclaw/opencode/codex)
	formatted := formatDeliverMessage(a.nodeID, req)

	// Prepend system prompt so hermes understands the protocol
	prompt := a.systemPrompt + "\n\n" + formatted

	sessionID := a.loadMappedSessionID(req.SessionKey)
	args := a.buildCommandArgs(prompt, sessionID)

	a.logCommand(args, sessionID)

	if err := a.startCmd(ctx, args...); err != nil {
		return nil, fmt.Errorf("hermes start command: %w", err)
	}

	return &DeliverMessageResult{
		Success:  true,
		Accepted: true,
	}, nil
}

// GetStatus checks whether the hermes CLI is available and working.
func (a *HermesAdapter) GetStatus(ctx context.Context) (*AgentStatus, error) {
	out, err := a.versionCmd(ctx, "--version")
	if err != nil {
		return &AgentStatus{Healthy: false}, err
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return &AgentStatus{Healthy: false}, nil
	}
	return &AgentStatus{Healthy: true}, nil
}

// Shutdown terminates all running hermes processes and waits for them to exit.
func (a *HermesAdapter) Shutdown() {
	a.mu.Lock()
	defer a.mu.Unlock()

	for _, p := range a.processes {
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
	}
	// Wait outside the lock to avoid deadlock with the reaper goroutine.
	processes := a.processes
	a.processes = nil

	for _, p := range processes {
		_ = p.cmd.Wait()
	}
}

// ── Command construction ──────────────────────────────────────────────

func (a *HermesAdapter) buildCommandArgs(prompt string, sessionID string) []string {
	// Build hermes chat command with -Q (quiet/programmatic mode) to
	// suppress banner, spinner, and tool previews for clean output.
	args := []string{
		"chat", "-q", prompt,
		"-Q",
		"-s", "tm-task-exec",
		"-t", "terminal",
		"--max-turns", "100",
		"--yolo",
	}

	if sessionID = strings.TrimSpace(sessionID); sessionID != "" {
		args = append(args, "--session", sessionID)
	}

	return args
}

// ── Default command starter (background mode) ─────────────────────────

func (a *HermesAdapter) defaultStartCmd(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "hermes", args...)

	// Close stdin to prevent hermes from reading prompt from stdin
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return fmt.Errorf("open /dev/null: %w", err)
	}
	cmd.Stdin = devNull

	// Discard stdout and stderr — hermes replies via clawsynapse publish
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		devNull.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("hermes command canceled: %w", ctxErr)
		}
		return fmt.Errorf("hermes start: %w", err)
	}

	// Close devNull after start — the command has inherited the file descriptor
	devNull.Close()

	// Track process for lifecycle management
	a.mu.Lock()
	a.processes = append(a.processes, hermesProcess{
		cmd:       cmd,
		startedAt: time.Now(),
	})
	a.mu.Unlock()

	// Reap finished processes in background to prevent zombie processes
	go func() {
		_ = cmd.Wait()
		a.mu.Lock()
		defer a.mu.Unlock()
		for i, p := range a.processes {
			if p.cmd == cmd {
				a.processes = append(a.processes[:i], a.processes[i+1:]...)
				break
			}
		}
	}()

	return nil
}

// ── Default version command (synchronous) ─────────────────────────────

func defaultHermesVersionCmd(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "hermes", args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── Session mapping ───────────────────────────────────────────────────

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

// ── Logging helpers ───────────────────────────────────────────────────

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
	a.log.Info("starting hermes chat command (background)",
		slog.String("sessionId", sessionID),
		slog.String("command", formatHermesCommandForLog(args)),
	)
}

// ── Command formatting for logging ────────────────────────────────────

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

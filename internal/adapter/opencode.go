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
)

type OpenCodeConfig struct {
	NodeID string
	Logger *slog.Logger
}

type OpenCodeAdapter struct {
	nodeID  string
	log     *slog.Logger
	execCmd func(ctx context.Context, args ...string) ([]byte, error)
}

func NewOpenCodeAdapter(cfg OpenCodeConfig) (*OpenCodeAdapter, error) {
	return &OpenCodeAdapter{
		nodeID:  strings.TrimSpace(cfg.NodeID),
		log:     cfg.Logger,
		execCmd: defaultOpenCodeExecCmd,
	}, nil
}

func (a *OpenCodeAdapter) DeliverMessage(ctx context.Context, req DeliverMessageRequest) (*DeliverMessageResult, error) {
	msg := formatDeliverMessage(a.nodeID, req)
	sessionID := a.resolveSessionID(req)

	args := []string{"run", msg, "--format", "json", "--session", sessionID}

	a.logCommand(args)

	out, err := a.execCmd(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("opencode run command: %w", err)
	}

	return parseOpenCodeResult(out)
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

func (a *OpenCodeAdapter) resolveSessionID(req DeliverMessageRequest) string {
	if s := strings.TrimSpace(req.SessionKey); s != "" {
		return s
	}
	from := req.From
	if from == "" {
		from = "_anon"
	}
	return "cs-" + from + "-" + a.nodeID
}

type openCodeEvent struct {
	Type    string `json:"type"`
	Content string `json:"content"`
	Text    string `json:"text"`
	Error   string `json:"error"`
}

func parseOpenCodeResult(data []byte) (*DeliverMessageResult, error) {
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))

	var lastText string
	var lastError string
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
			Success:  true,
			Accepted: true,
			Reply:    text,
		}, nil
	}

	if lastError != "" {
		return &DeliverMessageResult{
			Success: false,
			Error:   lastError,
		}, nil
	}

	return &DeliverMessageResult{
		Success:  true,
		Accepted: true,
		Reply:    lastText,
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

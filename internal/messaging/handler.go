package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"clawsynapse/internal/adapter"
)

type IncomingMessage struct {
	MessageID  string
	Type       string
	AgentID    string
	From       string
	To         string
	Message    string
	SessionKey string
	Metadata   map[string]any
}

type HandlerResult struct {
	Reply string
	RunID string
}

type MessageHandler interface {
	HandleMessage(msg IncomingMessage) (HandlerResult, error)
}

type MessageHandlerFunc func(msg IncomingMessage) (HandlerResult, error)

func (f MessageHandlerFunc) HandleMessage(msg IncomingMessage) (HandlerResult, error) {
	return f(msg)
}

type DefaultMessageHandler struct {
	nodeID string
}

func NewDefaultMessageHandler(nodeID string) *DefaultMessageHandler {
	return &DefaultMessageHandler{nodeID: nodeID}
}

func (h *DefaultMessageHandler) HandleMessage(msg IncomingMessage) (HandlerResult, error) {
	return HandlerResult{Reply: fmt.Sprintf("node %s handled message from %s: %s", h.nodeID, msg.From, msg.Message)}, nil
}

type AdapterMessageHandler struct {
	adapter        adapter.AgentAdapter
	timeout        time.Duration
	acceptFeedback bool
	// taskRunTimeout bounds todo.* deliveries (queue + run). Defaults to
	// 60m — deliberately much longer than `timeout` so long runs are not
	// killed by the generic 10m adapter timeout (T1.3 timeout split).
	taskRunTimeout time.Duration
	// rootCtx parents every per-message delivery ctx. Nil = Background.
	// App wires a cancelable root (T2.6) so shutdown can interrupt
	// in-flight gateway runs after the drain grace expires.
	rootCtx context.Context
}

// HandlerOption customizes an AdapterMessageHandler at construction time.
type HandlerOption func(*AdapterMessageHandler)

// WithFeedbackDelivery lets the handler forward .response / .error messages to
// the underlying adapter. Use it for passthrough adapters (e.g. webhook) where
// the remote endpoint can consume arbitrary notifications. Do NOT use it for
// LLM-based adapters (openclaw, opencode, codex) — they would re-ingest the
// reply text as a new prompt and create feedback loops.
func WithFeedbackDelivery() HandlerOption {
	return func(h *AdapterMessageHandler) { h.acceptFeedback = true }
}

// WithTaskRunTimeout overrides the delivery timeout used for todo.* messages
// (default 60m).
func WithTaskRunTimeout(d time.Duration) HandlerOption {
	return func(h *AdapterMessageHandler) { h.taskRunTimeout = d }
}

// WithRootContext parents every delivery context (T2.6 graceful exit):
// cancelling the root aborts all in-flight adapter calls.
func WithRootContext(ctx context.Context) HandlerOption {
	return func(h *AdapterMessageHandler) { h.rootCtx = ctx }
}

func NewAdapterMessageHandler(agentAdapter adapter.AgentAdapter, timeout time.Duration, opts ...HandlerOption) *AdapterMessageHandler {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	h := &AdapterMessageHandler{
		adapter:        agentAdapter,
		timeout:        timeout,
		taskRunTimeout: 60 * time.Minute,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

func (h *AdapterMessageHandler) HandleMessage(msg IncomingMessage) (HandlerResult, error) {
	if !h.acceptFeedback && isFeedbackType(msg.Type) {
		return HandlerResult{}, nil
	}

	// Task control messages (todo.status_changed{canceled}, todo.remind
	// targeting an in-flight run) must be intercepted BEFORE the
	// silentNotifyReply check — todo.status_changed is in
	// silentNotifyTypes and would be silently ACKed without reaching the
	// cancel path (T1.3).
	if c, ok := h.adapter.(adapter.RunCanceller); ok {
		if res, handled := h.handleTaskControl(msg, c); handled {
			return res, nil
		}
	}

	// Silent status-sync notifications carry no actionable content for the
	// local agent. Feeding them to an LLM adapter only produces hallucinated
	// "review" commentary (observed in production: the PM fabricated
	// "TD_02 completed" summaries and cross-task status reports). Answer
	// them with a deterministic one-line ACK instead — the platform side
	// swallows "ACK " replies via isSilentACK, so nothing reaches the task
	// timeline and no LLM tokens are spent.
	if reply, ok := silentNotifyReply(msg); ok {
		return HandlerResult{Reply: reply}, nil
	}

	// todo.* runs (queue + execute) get the dedicated task run timeout;
	// everything else keeps the generic adapter timeout (T1.3 split).
	deliveryTimeout := h.timeout
	if h.taskRunTimeout > 0 && strings.HasPrefix(strings.TrimSpace(msg.Type), "todo.") {
		deliveryTimeout = h.taskRunTimeout
	}

	root := h.rootCtx
	if root == nil {
		root = context.Background()
	}
	ctx, cancel := context.WithTimeout(root, deliveryTimeout)
	defer cancel()

	result, err := h.adapter.DeliverMessage(ctx, adapter.DeliverMessageRequest{
		Type:       msg.Type,
		AgentID:    msg.AgentID,
		SessionKey: msg.SessionKey,
		Message:    msg.Message,
		From:       msg.From,
		Metadata:   msg.Metadata,
		MessageID:  msg.MessageID,
	})
	if err != nil {
		return HandlerResult{}, err
	}
	if result == nil {
		return HandlerResult{}, fmt.Errorf("adapter returned nil result")
	}
	if result.Error != "" {
		return HandlerResult{}, fmt.Errorf("adapter error: %s", result.Error)
	}
	if !result.Success {
		return HandlerResult{}, fmt.Errorf("adapter did not complete successfully")
	}
	if result.Reply != "" {
		reply := result.Reply
		if result.RunID != "" && !strings.Contains(reply, "runId="+result.RunID) {
			reply = fmt.Sprintf("%s (runId=%s)", reply, result.RunID)
		}
		return HandlerResult{Reply: reply, RunID: result.RunID}, nil
	}
	if result.Accepted {
		reply := "accepted"
		if result.RunID != "" {
			reply = fmt.Sprintf("accepted (runId=%s)", result.RunID)
		}
		return HandlerResult{Reply: reply, RunID: result.RunID}, nil
	}
	return HandlerResult{}, fmt.Errorf("adapter did not accept message")
}

// isFeedbackType 判断消息是否属于"反馈类"（上一条消息的响应或错误）。
// 这类消息不应投递给 LLM 类适配器，也不应触发新的回复（见 service.replyToSender）。
func isFeedbackType(t string) bool {
	return strings.HasSuffix(t, ".response") || strings.HasSuffix(t, ".error")
}

// silentNotifyTypes 平台状态同步类通知。它们是机器对机器的 FYI 消息，
// agent 收到后无需（也不应该）用 LLM 生成回复。
var silentNotifyTypes = map[string]bool{
	"task.status_changed": true,
	"todo.status_changed": true,
}

// silentNotifyReply 为静默状态通知生成确定性模板 ACK（含 todo/状态/原因摘要），
// 完全不经过 LLM，杜绝长 session 下的幻觉回复。解析失败时退化为纯类型 ACK。
func silentNotifyReply(msg IncomingMessage) (string, bool) {
	if !silentNotifyTypes[msg.Type] {
		return "", false
	}
	var p struct {
		TaskID string `json:"task_id"`
		TodoID string `json:"todo_id"`
		Status string `json:"status"`
		Cause  string `json:"cause"`
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal([]byte(msg.Message), &p)

	var b strings.Builder
	b.WriteString("ACK " + msg.Type)
	if p.TodoID != "" {
		b.WriteString(" - " + p.TodoID + " -> " + p.Status)
	} else if p.Status != "" {
		b.WriteString(" - 任务状态 -> " + p.Status)
	}
	if p.Reason != "" {
		r := []rune(strings.TrimSpace(p.Reason))
		if len(r) > 80 {
			r = append(r[:80], []rune("…")...)
		}
		b.WriteString("（" + string(r) + "）")
	}
	return b.String(), true
}

// handleTaskControl intercepts task-control messages that target in-flight
// runs instead of starting a new delivery:
//   - todo.status_changed{status: canceled | cause: user_cancel} → cancel
//     the in-flight run (POST /stop + ctx cancel) and ACK silently;
//   - todo.remind while a run is in flight → steer (inject) the reminder
//     text into that run instead of queueing a duplicate delivery;
//     no in-flight run → not handled, falls through to normal delivery.
//
// Everything else is not handled here (todo.status_changed without a
// cancel semantic falls back to the silentNotifyReply ACK).
func (h *AdapterMessageHandler) handleTaskControl(msg IncomingMessage, c adapter.RunCanceller) (HandlerResult, bool) {
	switch msg.Type {
	case "todo.status_changed":
		var p struct {
			Status string `json:"status"`
			Cause  string `json:"cause"`
		}
		_ = json.Unmarshal([]byte(msg.Message), &p)
		status := strings.ToLower(strings.TrimSpace(p.Status))
		cause := strings.ToLower(strings.TrimSpace(p.Cause))
		if status != "canceled" && status != "cancelled" && cause != "user_cancel" {
			return HandlerResult{}, false
		}
		taskKey := taskControlKey(msg)
		if err := c.CancelActive(context.Background(), taskKey); err != nil {
			// CancelActive is idempotent and best-effort; a failure must
			// not wedge the platform-side cancel flow.
			return HandlerResult{Reply: "ACK todo.status_changed - canceled"}, true
		}
		return HandlerResult{Reply: "ACK todo.status_changed - canceled"}, true
	case "todo.remind":
		taskKey := taskControlKey(msg)
		if c.ActiveRunID(taskKey) == "" {
			// Nothing in flight: let the reminder flow through the normal
			// delivery path (spec: fall back instead of swallowing).
			return HandlerResult{}, false
		}
		if err := c.SteerActive(context.Background(), taskKey, msg.Message); err != nil {
			if errors.Is(err, adapter.ErrNoActiveRun) {
				return HandlerResult{}, false
			}
			// Steer failed but a run IS in flight — still ack to avoid a
			// duplicate delivery racing the in-flight run.
			return HandlerResult{Reply: "ACK todo.remind - steered"}, true
		}
		return HandlerResult{Reply: "ACK todo.remind - steered"}, true
	default:
		return HandlerResult{}, false
	}
}

// taskControlKey derives the task identity for cancel/steer: the platform
// sets SessionKey to the task id; metadata taskId/task_id is the fallback.
func taskControlKey(msg IncomingMessage) string {
	if key := strings.TrimSpace(msg.SessionKey); key != "" {
		return key
	}
	if msg.Metadata != nil {
		for _, k := range []string{"taskId", "task_id", "todoId", "todo_id"} {
			if v, ok := msg.Metadata[k].(string); ok {
				if v = strings.TrimSpace(v); v != "" {
					return v
				}
			}
		}
	}
	return ""
}

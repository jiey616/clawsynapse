package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"clawsynapse/internal/store"
)

// HermesConfig holds configuration for the Hermes agent adapter.
type HermesConfig struct {
	NodeID       string
	Logger       *slog.Logger
	SessionStore *store.FSStore
	AgentRole    string // pm | executor | ""
	// Gateway API connection (hermes gateway run, API Server enabled).
	BaseURL string // e.g. http://127.0.0.1:8642/v1
	APIKey  string // API_SERVER_KEY
	Model   string // advertised agent name from GET /v1/models
	// ConfigPath is the hermes config.yaml path used by capability read
	// (custom_providers) and write-back (skills.external_dirs / model).
	// Empty means "$HERMES_HOME/config.yaml" or "~/.hermes/config.yaml".
	ConfigPath string
	// TodoMode controls how todo.* messages are delivered:
	//   - "runs" (default): POST /v1/runs, polled to terminal state
	//   - "responses": POST /v1/responses with session_id continuation
	TodoMode string
	// MaxConcurrentRuns caps the number of in-flight runs deliveries
	// (default 8, leaving 2 spare slots under the gateway's global limit
	// of 10). Zero or negative falls back to the default.
	MaxConcurrentRuns int
	// Task bounds task run admission/queueing for the TaskCoordinator.
	// Zero-value fields fall back to the coordinator defaults
	// (8 concurrent / 100 queue / 60m run / 5m queue wait).
	Task TaskConfig
	// TaskStore persists the task run lifecycle (task_runs/). When nil the
	// TaskCoordinator stays disabled and todo.* deliveries keep the
	// legacy T0.6 gate path.
	TaskStore *store.TaskStore
}

// HermesAdapter delivers messages to a local Hermes agent via the Gateway
// HTTP API (a long-running `hermes gateway run` process).
//
// Message routing:
//   - chat.message            -> POST /v1/responses (stateful, previous_response_id continuation)
//   - task.*                 -> POST /v1/responses (same dialogue/stateful flow as chat)
//   - meeting.*              -> POST /v1/responses (dialogue/stateful flow, like chat)
//   - todo.*                 -> POST /v1/runs or POST /v1/responses (configurable via TodoMode)
type HermesAdapter struct {
	nodeID       string
	log          *slog.Logger
	sessionStore *store.FSStore
	agentRole    string

	httpClient *http.Client
	baseURL    string
	apiKey     string
	model      string
	configPath string
	todoMode   string // "runs" or "responses"

	capMu    sync.Mutex
	capCache *capabilitiesSnapshot

	// taskCoord serializes and bounds todo.* runs executions. Nil until a
	// TaskStore is provided via HermesConfig (T1.5 wires the runs path).
	taskCoord *TaskCoordinator

	// restartGatewayFn is the gateway restart implementation. Overridable in
	// tests to avoid spawning real processes; defaults to restartGateway.
	restartGatewayFn func(ctx context.Context) error

	// Poll tuning. Defaults: 1s initial backoff, 5s cap, 12m hard deadline.
	// Overridable in tests to keep them fast.
	pollInterval    time.Duration
	pollIntervalMax time.Duration
	pollDeadline    time.Duration

	// runSem gates concurrent runs deliveries (Phase 0 stopgap). It is held
	// from before run creation until the run reaches a terminal state.
	// Replaced by the TaskCoordinator in the lifecycle phase.
	runSem chan struct{}
	// backoff429 are the retry delays for gateway-level 429 rejections.
	backoff429 []time.Duration
}

// NewHermesAdapter creates a Hermes adapter instance backed by the Gateway API.
func NewHermesAdapter(cfg HermesConfig) (*HermesAdapter, error) {
	baseURL := strings.TrimSpace(cfg.BaseURL)
	if baseURL == "" {
		baseURL = "http://127.0.0.1:8642/v1"
	}
	baseURL = strings.TrimRight(baseURL, "/")

	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		model = "hermes-agent"
	}

	todoMode := strings.ToLower(strings.TrimSpace(cfg.TodoMode))
	if todoMode == "" {
		todoMode = "runs"
	}
	if todoMode != "runs" && todoMode != "responses" {
		todoMode = "runs"
	}

	maxRuns := cfg.MaxConcurrentRuns
	if maxRuns <= 0 {
		maxRuns = 8
	}

	// Task lifecycle coordination (T1.2). Boot recovery converts running
	// records left by a previous process into failed (the node lost its
	// polling rights; better a redispatchable failure than a ghost).
	var taskCoord *TaskCoordinator
	if cfg.TaskStore != nil {
		if err := cfg.TaskStore.EnsureLayout(); err != nil {
			return nil, fmt.Errorf("task store layout: %w", err)
		}
		if recovered, err := cfg.TaskStore.RecoverStaleRunningTasks(); err != nil {
			log := cfg.Logger
			if log == nil {
				log = slog.Default()
			}
			log.Warn("task run recovery failed", "err", err)
		} else if recovered > 0 {
			log := cfg.Logger
			if log == nil {
				log = slog.Default()
			}
			log.Info("recovered stale running task records", "count", recovered)
		}
		taskCoord = NewTaskCoordinator(cfg.Task, cfg.TaskStore, cfg.Logger)
	}

	return &HermesAdapter{
		nodeID:       strings.TrimSpace(cfg.NodeID),
		log:          cfg.Logger,
		sessionStore: cfg.SessionStore,
		agentRole:    strings.ToLower(strings.TrimSpace(cfg.AgentRole)),
		// Timeout is driven by the caller-supplied context, not a fixed client timeout.
		httpClient:      &http.Client{Timeout: 0},
		baseURL:         baseURL,
		apiKey:          strings.TrimSpace(cfg.APIKey),
		model:           model,
		configPath:      strings.TrimSpace(cfg.ConfigPath),
		todoMode:        todoMode,
		pollInterval:    time.Second,
		pollIntervalMax: 5 * time.Second,
		pollDeadline:    12 * time.Minute,
		runSem:          make(chan struct{}, maxRuns),
		backoff429:      []time.Duration{time.Second, 2 * time.Second, 4 * time.Second},
		taskCoord:       taskCoord,
	}, nil
}

// DeliverMessage formats the incoming message with the standard ClawSynapse
// protocol header, then routes it to the appropriate Gateway endpoint based
// on the message type (chat vs task).
func (a *HermesAdapter) DeliverMessage(ctx context.Context, req DeliverMessageRequest) (*DeliverMessageResult, error) {
	// Use the standard structured header format (same as openclaw/opencode/codex)
	formatted := formatDeliverMessage(a.nodeID, req)

	if isRunsMessage(req.Type) && a.todoMode == "runs" {
		return a.deliverViaRuns(ctx, formatted, req)
	}
	if isRunsMessage(req.Type) || isTaskType(req.Type) {
		return a.deliverTaskViaResponses(ctx, formatted, req)
	}
	return a.deliverViaResponses(ctx, formatted, req)
}

// GetStatus checks whether the Hermes Gateway API is reachable.
func (a *HermesAdapter) GetStatus(ctx context.Context) (*AgentStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.rootURL()+"/health", nil)
	if err != nil {
		return &AgentStatus{Healthy: false}, err
	}
	resp, err := a.httpClient.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return &AgentStatus{Healthy: false}, ctxErr
		}
		return &AgentStatus{Healthy: false}, err
	}
	defer resp.Body.Close()
	return &AgentStatus{Healthy: resp.StatusCode >= 200 && resp.StatusCode < 300}, nil
}

// ── RunCanceller (T1.3): cancel / steer in-flight task runs ──────────

// stopRun asks the gateway to stop a run. 404/409 are tolerated (the run
// may have already terminated); other errors are logged, not propagated —
// the local ctx cancel is the authoritative abort.
func (a *HermesAdapter) stopRun(ctx context.Context, runID string) error {
	if strings.TrimSpace(runID) == "" {
		return nil
	}
	_, err := a.callJSON(ctx, http.MethodPost, a.baseURL+"/runs/"+url.PathEscape(strings.TrimSpace(runID))+"/stop", nil, nil)
	if err != nil {
		a.logGateway("runs-stop-failed", runID, false)
	}
	return err
}

// CancelActive implements RunCanceller. Looks up the in-flight handle for
// taskKey: with a published runID → POST /runs/{id}/stop (5s budget) then
// cancel the local run ctx; queued without a runID → cancel only; nothing
// in flight → nil (idempotent).
func (a *HermesAdapter) CancelActive(ctx context.Context, taskKey string) error {
	if a.taskCoord == nil {
		return nil
	}
	h := a.taskCoord.lookup(taskKey)
	if h == nil {
		return nil
	}
	if runID := h.getRunID(); runID != "" {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = a.stopRun(stopCtx, runID)
	}
	h.cancel()
	return nil
}

// SteerActive implements RunCanceller: inject text into the in-flight run.
func (a *HermesAdapter) SteerActive(ctx context.Context, taskKey string, input string) error {
	if a.taskCoord == nil {
		return ErrNoActiveRun
	}
	h := a.taskCoord.lookup(taskKey)
	if h == nil || h.getRunID() == "" {
		return ErrNoActiveRun
	}
	body := map[string]string{"input": input}
	_, err := a.callJSON(ctx, http.MethodPost, a.baseURL+"/runs/"+url.PathEscape(h.getRunID())+"/steer", body, nil)
	return err
}

// ActiveRunID implements RunCanceller: current in-flight runID for taskKey.
func (a *HermesAdapter) ActiveRunID(taskKey string) string {
	if a.taskCoord == nil {
		return ""
	}
	if h := a.taskCoord.lookup(taskKey); h != nil {
		return h.getRunID()
	}
	return ""
}

// ── Routing helpers ───────────────────────────────────────────────

// isRunsMessage reports whether the message type belongs to the long-running
// Runs flow (polled to a terminal state). Only `todo.*` messages take this path;
// `chat.*`, `task.*` and `meeting.*` all go through the stateful Responses API.
func isRunsMessage(msgType string) bool {
	t := strings.TrimSpace(msgType)
	return strings.HasPrefix(t, "todo.")
}

// isTaskType reports whether the message belongs to the task-collaboration
// flow (`task.*`). These run through the Responses API but continue on the
// task-level session (session_id = task id), sharing one hermes session with
// the `todo.*` runs flow so execution and dialogue context stay connected.
func isTaskType(msgType string) bool {
	t := strings.TrimSpace(msgType)
	return strings.HasPrefix(t, "task.")
}

// chatSessionKey derives the stable mapping key for chat (dialogue) messages.
// Prefers the upstream-provided SessionKey (set by Trustmesh to a stable value
// within a conversation); falls back to a per-source key so the same sender
// naturally continues the dialogue.
func (a *HermesAdapter) chatSessionKey(req DeliverMessageRequest) string {
	if k := strings.TrimSpace(req.SessionKey); k != "" {
		return k
	}
	from := strings.TrimSpace(req.From)
	if from == "" {
		from = "_anon"
	}
	return "cs-" + from + "-" + a.nodeID
}

// taskSessionKey derives the stable task identifier used for session
// continuation. TrustMesh sets SessionKey to the task id, so this is the
// task-level key. Falls back to the metadata taskId; returns "" (no
// continuation) when neither is present.
func (a *HermesAdapter) taskSessionKey(req DeliverMessageRequest) string {
	if k := strings.TrimSpace(req.SessionKey); k != "" {
		return k
	}
	if t := extractTaskID(req.Metadata); t != "" {
		return t
	}
	return "" // no task identifier: start a fresh session, never share "default"
}

// ── Task dialogue: Responses API with session_id continuation ───────

// deliverTaskViaResponses handles `task.*` (and, in TodoMode=responses,
// `todo.*`) messages through the Responses API, chaining them onto one
// task-scoped conversation.
//
// The task id is passed as the `conversation` field — hermes' named-conversation
// mechanism, which links responses server-side. The adapter previously sent it
// as `session_id`, but that is only an echo-back correlation label: verified
// against a live gateway, `session_id` is ignored (every call created a fresh
// random-UUID session and `response_store.conversations` stayed empty), while
// `conversation` correctly chains calls into a single session.
//
// Unlike chat, there is no previous_response_id mapping to persist: hermes owns
// the chaining once the conversation is named.
func (a *HermesAdapter) deliverTaskViaResponses(ctx context.Context, formatted string, req DeliverMessageRequest) (*DeliverMessageResult, error) {
	sid := a.taskSessionKey(req)

	body := responsesRequest{Model: a.model, Input: formatted}
	if sid != "" {
		body.Conversation = sid
	}

	a.logGateway("responses-task", "task:"+sid, sid != "")

	var resp responsesResponse
	status, err := a.callJSONIdempotent(ctx, http.MethodPost, a.baseURL+"/responses", body, &resp, idemKeyOf(req.MessageID, ""))
	// Unknown/lost task conversation: drop the conversation name and retry
	// once on a fresh session (mirrors the chat path's unknown-session retry).
	if err != nil && sid != "" && isGatewayUnknownSessionError(status, err.Error()) {
		a.logGateway("responses-task-retry", "task:"+sid, false)
		body.Conversation = ""
		status, err = a.callJSONIdempotent(ctx, http.MethodPost, a.baseURL+"/responses", body, &resp, idemKeyOf(req.MessageID, "-r1"))
	}
	if err != nil {
		return nil, fmt.Errorf("hermes gateway responses(task): %w", err)
	}

	reply := extractResponseText(resp)
	if strings.TrimSpace(reply) == "" {
		return &DeliverMessageResult{
			Success: false,
			Error:   "hermes returned empty reply",
		}, nil
	}

	if isModelProviderError(reply) {
		// Feedback confirmation carries no new work; swallow it.
		if isFeedbackLike(req.Type) {
			return &DeliverMessageResult{Success: true, Accepted: true, Reply: "ok"}, nil
		}
		// Session-history rejection (e.g. deepseek's strict "empty tool_calls
		// array" check): the task conversation history is poisoned — retry
		// once on a fresh conversation (mirrors the chat path's retry).
		if sid != "" {
			a.logGateway("responses-task-retry-fresh", "task:"+sid, false)
			body.Conversation = ""
			var resp2 responsesResponse
			if _, err2 := a.callJSONIdempotent(ctx, http.MethodPost, a.baseURL+"/responses", body, &resp2, idemKeyOf(req.MessageID, "-r2")); err2 == nil {
				reply2 := extractResponseText(resp2)
				if strings.TrimSpace(reply2) != "" && !isModelProviderError(reply2) {
					return &DeliverMessageResult{
						Success:  true,
						Accepted: true,
						Reply:    reply2,
					}, nil
				}
			}
			return &DeliverMessageResult{
				Success: false,
				Error:   "对话上下文被模型服务拒绝，已自动重开会话，请重新发送该消息",
			}, nil
		}
		return &DeliverMessageResult{
			Success: false,
			Error:   "模型服务暂时不可用，请稍后重试",
		}, nil
	}

	return &DeliverMessageResult{
		Success:  true,
		Accepted: true,
		Reply:    reply,
	}, nil
}

// ── Dialogue: Responses API (stateful, auto-continuation) ──────────

func (a *HermesAdapter) deliverViaResponses(ctx context.Context, formatted string, req DeliverMessageRequest) (*DeliverMessageResult, error) {
	chatKey := "chat:" + a.chatSessionKey(req)
	prevID := a.loadMappedSessionID(chatKey)

	body := responsesRequest{Model: a.model, Input: formatted}
	if prevID != "" {
		body.PreviousResponseID = prevID
	}

	a.logGateway("responses", chatKey, prevID != "")

	var resp responsesResponse
	status, err := a.callJSONIdempotent(ctx, http.MethodPost, a.baseURL+"/responses", body, &resp, idemKeyOf(req.MessageID, ""))
	if err != nil && prevID != "" && isGatewayUnknownSessionError(status, err.Error()) {
		a.deleteMappedSession(chatKey)
		body.PreviousResponseID = ""
		a.logGateway("responses-retry", chatKey, false)
		status, err = a.callJSONIdempotent(ctx, http.MethodPost, a.baseURL+"/responses", body, &resp, idemKeyOf(req.MessageID, "-r1"))
	}
	if err != nil {
		return nil, fmt.Errorf("hermes gateway responses: %w", err)
	}

	reply := extractResponseText(resp)
	if strings.TrimSpace(reply) == "" {
		return &DeliverMessageResult{
			Success: false,
			Error:   "hermes returned empty reply",
		}, nil
	}

	// The model provider rejected the conversation history (e.g. deepseek's
	// strict "empty tool_calls array" check). Hermes digests the 400 into the
	// reply text; rather than leaking the raw error to the user, drop the
	// session mapping and retry once on a fresh conversation (which has no
	// poisoned history). If the retry also fails, degrade gracefully.
	if isModelProviderError(reply) && prevID != "" {
		// Feedback confirmation (a .response/.error ack of a message this node
		// already sent — e.g. backend ack of todo.complete). The underlying
		// task work is already done; a fresh-conversation retry would re-run
		// the whole task and produce duplicate deliveries (extra review books,
		// re-submitted todo.complete). Swallow the confirmation instead.
		if isFeedbackLike(req.Type) {
			return &DeliverMessageResult{Success: true, Accepted: true, Reply: "ok"}, nil
		}
		a.deleteMappedSession(chatKey)
		body.PreviousResponseID = ""
		a.logGateway("responses-retry-fresh", chatKey, false)
		var resp2 responsesResponse
		if _, err2 := a.callJSONIdempotent(ctx, http.MethodPost, a.baseURL+"/responses", body, &resp2, idemKeyOf(req.MessageID, "-r2")); err2 == nil {
			reply2 := extractResponseText(resp2)
			if strings.TrimSpace(reply2) != "" && !isModelProviderError(reply2) {
				if resp2.ID != "" {
					a.saveMappedSession(chatKey, resp2.ID)
				}
				return &DeliverMessageResult{
					Success:  true,
					Accepted: true,
					Reply:    reply2,
				}, nil
			}
		}
		return &DeliverMessageResult{
			Success: false,
			Error:   "对话上下文被模型服务拒绝，已自动重开会话，请重新发送该消息",
		}, nil
	}
	if isModelProviderError(reply) {
		return &DeliverMessageResult{
			Success: false,
			Error:   "模型服务暂时不可用，请稍后重试",
		}, nil
	}

	if resp.ID != "" {
		a.saveMappedSession(chatKey, resp.ID)
	}

	return &DeliverMessageResult{
		Success:  true,
		Accepted: true,
		Reply:    reply,
	}, nil
}

// ── Task flow: Runs API (long-running, polled) ────────────────────

func (a *HermesAdapter) deliverViaRuns(ctx context.Context, formatted string, req DeliverMessageRequest) (*DeliverMessageResult, error) {
	// Runs execution needs a stable task identity for idempotency, the
	// single-task mutex and session continuation. Without one every message
	// would share the bare "task:" key and pollute a common session — refuse
	// instead (the handler turns this into an .error reply to the platform).
	// Note: the task.* responses path deliberately keeps its current behavior
	// (empty id → fresh session, no error); tasks must stay isolated per task,
	// so falling back to the sender id would be worse than no continuation.
	taskID := a.taskSessionKey(req)
	if strings.TrimSpace(taskID) == "" {
		return nil, fmt.Errorf("hermes runs execution requires a valid taskId or sessionKey")
	}
	taskKey := "task:" + taskID
	prevID := a.loadMappedSessionID(taskKey)

	// NOTE(§7.1): continuation field name for /v1/runs is to be verified
	// against the live gateway (session_id vs previous_response_id).
	body := runCreateRequest{Input: formatted, Model: a.model}
	if prevID != "" {
		body.SessionID = prevID
	}

	// Gate concurrent runs deliveries before creating anything on the
	// gateway. The slot is held until this delivery reaches a terminal state
	// (including fresh-run retries), so gateway-side concurrency stays ≤
	// MaxConcurrentRuns at all times.
	select {
	case a.runSem <- struct{}{}:
		defer func() { <-a.runSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	a.logGateway("runs-create", taskKey, prevID != "")

	idemKey := idemKeyOf(req.MessageID, "")
	created, status, err := a.createRunWithRetry(ctx, body, idemKey)
	if err != nil && prevID != "" && isGatewayUnknownSessionError(status, err.Error()) {
		a.deleteMappedSession(taskKey)
		body.SessionID = ""
		a.logGateway("runs-create-retry", taskKey, false)
		created, status, err = a.createRunWithRetry(ctx, body, idemKeyOf(req.MessageID, "-r1"))
	}
	if err != nil {
		return nil, fmt.Errorf("hermes gateway runs create: %w", err)
	}

	runID := strings.TrimSpace(created.RunID)
	if runID == "" {
		runID = strings.TrimSpace(created.ID)
	}
	if runID == "" {
		return nil, fmt.Errorf("hermes gateway runs create: missing run_id in response")
	}

	// Persist the continuation id returned at creation time so follow-up
	// task messages can resume this run's session.
	if sid := strings.TrimSpace(created.SessionID); sid != "" {
		a.saveMappedSession(taskKey, sid)
	}

	final, err := a.pollRun(ctx, runID)
	if err != nil {
		// Caller ctx cancelled/expired (or the adapter itself cancelled the
		// run): make sure the gateway-side run is stopped too, otherwise it
		// keeps burning tokens with nobody polling it (T1.3).
		if runCtxErr := ctx.Err(); runCtxErr != nil {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer stopCancel()
			_ = a.stopRun(stopCtx, runID)
		}
		return nil, fmt.Errorf("hermes gateway runs poll: %w", err)
	}

	if runFailed(final.Status) {
		runText := extractRunText(*final)
		// Model-provider rejection (e.g. deepseek empty tool_calls) is a
		// session-history problem — drop the task mapping and retry once on a
		// fresh run instead of surfacing the raw 400 to the user.
		if isModelProviderError(runText) && prevID != "" {
			// Feedback confirmation: the underlying work is already done;
			// a fresh run would re-execute the task and duplicate deliveries.
			if isFeedbackLike(req.Type) {
				return &DeliverMessageResult{Success: true, Accepted: true, RunID: runID, Reply: "ok"}, nil
			}
			a.deleteMappedSession(taskKey)
			a.logGateway("runs-retry-fresh", taskKey, false)
			fresh := runCreateRequest{Input: formatted, Model: a.model}
			var created2 runCreateResponse
			if _, err2 := a.callJSONIdempotent(ctx, http.MethodPost, a.baseURL+"/runs", fresh, &created2, idemKeyOf(req.MessageID, "-r2")); err2 == nil {
				runID2 := strings.TrimSpace(created2.RunID)
				if runID2 == "" {
					runID2 = strings.TrimSpace(created2.ID)
				}
				if runID2 != "" {
					if sid := strings.TrimSpace(created2.SessionID); sid != "" {
						a.saveMappedSession(taskKey, sid)
					}
					if final2, err2 := a.pollRun(ctx, runID2); err2 == nil && final2 != nil {
						if runFailed(final2.Status) {
							return &DeliverMessageResult{
								Success: false,
								RunID:   runID2,
								Error:   "任务上下文被模型服务拒绝，已自动重开任务，请重新提交",
							}, nil
						}
						reply2 := extractRunText(*final2)
						if strings.TrimSpace(reply2) != "" {
							if sessionID := strings.TrimSpace(final2.SessionID); sessionID != "" {
								a.saveMappedSession(taskKey, sessionID)
							}
							return &DeliverMessageResult{
								Success:  true,
								Accepted: true,
								RunID:    runID2,
								Reply:    reply2,
							}, nil
						}
					}
				}
			}
			return &DeliverMessageResult{
				Success: false,
				RunID:   runID,
				Error:   "任务上下文被模型服务拒绝，已自动重开任务，请重新提交",
			}, nil
		}
		return &DeliverMessageResult{
			Success: false,
			RunID:   runID,
			Error:   "hermes run " + final.Status + ": " + runText,
		}, nil
	}

	reply := extractRunText(*final)
	if strings.TrimSpace(reply) == "" || strings.TrimSpace(reply) == "(empty)" {
		// Agent produced no usable text (hermes "(empty)" sentinel or blank).
		// Retry once on a fresh run — the model often recovers on a retry.
		if retried, ok := a.retryFreshRun(ctx, taskKey, formatted, runID, idemKeyOf(req.MessageID, "-r3")); ok {
			return retried, nil
		}
		return &DeliverMessageResult{
			Success: false,
			RunID:   runID,
			Error:   "hermes run completed with empty output",
		}, nil
	}

	if sessionID := strings.TrimSpace(final.SessionID); sessionID != "" {
		a.saveMappedSession(taskKey, sessionID)
	}

	return &DeliverMessageResult{
		Success:  true,
		Accepted: true,
		RunID:    runID,
		Reply:    reply,
	}, nil
}

// runTerminalStatuses are the /v1/runs statuses that mean the run is finished.
//
// Verified against a live gateway (hermes v0.21.0): the full status set is
// queued/started/running/stopping/completed/failed/cancelled/interrupted and
// the terminal set is {completed, failed, cancelled, interrupted}. "stopped"
// and "error" are accepted defensively but have never been observed; "canceled"
// (US spelling) is accepted defensively alongside the observed "cancelled".
// "stopping" is deliberately NOT terminal — the gateway is still transitioning.
// Any status not listed here is treated as in-progress (with stuck-run and
// deadline guards in pollRun).
var runTerminalStatuses = map[string]bool{
	"completed":   true,
	"failed":      true,
	"cancelled":   true,
	"canceled":    true,
	"interrupted": true,
	"stopped":     true,
	"error":       true,
}

// isTerminalRunStatus reports whether a /v1/runs status means the run is over.
func isTerminalRunStatus(status string) bool {
	return runTerminalStatuses[strings.ToLower(strings.TrimSpace(status))]
}

// runFailed reports whether a terminal run status represents a failure rather
// than a successful completion.
func runFailed(status string) bool {
	s := strings.ToLower(strings.TrimSpace(status))
	return isTerminalRunStatus(s) && s != "completed"
}

// pollRun polls GET /v1/runs/{id} until it reaches a terminal state.
//
// Polling interval backs off from 1s to a 5s cap. Resilience rules:
//   - up to 3 consecutive poll-request errors are tolerated (gateway restart
//     window) before giving up; the backoff does not grow on errors;
//   - a hard 12-minute deadline caps polling regardless of the caller's ctx
//     (a caller-level cancellation still surfaces immediately as ctx.Err());
//   - 5 consecutive polls returning the same non-terminal status are treated
//     as a stuck run and fail fast instead of burning the whole timeout.
func (a *HermesAdapter) pollRun(ctx context.Context, runID string) (*runStatusResponse, error) {
	pollCtx, cancel := context.WithTimeout(ctx, a.pollDeadline)
	defer cancel()

	interval := a.pollInterval
	consecErrs := 0
	lastStatus := ""
	unchanged := 0
	for {
		select {
		case <-pollCtx.Done():
			if ctx.Err() != nil {
				// Caller-level cancellation/timeout: surface immediately so
				// the caller can decide (e.g. stop the run on the gateway).
				return nil, ctx.Err()
			}
			if lastStatus != "" {
				return nil, fmt.Errorf("run %s polling deadline exceeded (%s), last status=%s", runID, a.pollDeadline, lastStatus)
			}
			return nil, pollCtx.Err()
		default:
		}

		var st runStatusResponse
		_, err := a.callJSON(pollCtx, http.MethodGet, a.baseURL+"/runs/"+runID, nil, &st)
		if err != nil {
			if pollCtx.Err() != nil {
				continue // loop head handles ctx.Done uniformly
			}
			consecErrs++
			if consecErrs >= 3 {
				return nil, fmt.Errorf("run %s poll failed %d times consecutively: %w", runID, consecErrs, err)
			}
			select {
			case <-pollCtx.Done():
				continue
			case <-time.After(interval): // do not grow the backoff on errors
			}
			continue
		}
		consecErrs = 0

		if isTerminalRunStatus(st.Status) {
			return &st, nil
		}

		if status := strings.TrimSpace(st.Status); status == lastStatus {
			unchanged++
			if unchanged >= 5 {
				return nil, fmt.Errorf("run %s stuck in status %q after %d polls", runID, status, unchanged)
			}
		} else {
			lastStatus = status
			unchanged = 0
		}

		select {
		case <-pollCtx.Done():
			continue
		case <-time.After(interval):
		}
		if interval < a.pollIntervalMax {
			interval *= 2
		} else {
			interval = a.pollIntervalMax
		}
	}
}

// isGatewayRateLimited reports whether a callJSON error is the gateway's
// global concurrency rejection (HTTP 429 "Too many concurrent runs").
func isGatewayRateLimited(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "429") || strings.Contains(s, "too many concurrent")
}

// createRunWithRetry POSTs the run create request, retrying gateway-level
// 429 rejections with the configured backoff sequence (default 1s/2s/4s).
// It returns the HTTP status of the last attempt so callers can apply their
// own error classification (e.g. unknown-session retry).
func (a *HermesAdapter) createRunWithRetry(ctx context.Context, body runCreateRequest, idemKey string) (*runCreateResponse, int, error) {
	var created runCreateResponse
	for attempt := 0; ; attempt++ {
		// 429 backoff retries reuse the same Idempotency-Key: a 429 means
		// the gateway rejected the request without creating anything.
		status, err := a.callJSONIdempotent(ctx, http.MethodPost, a.baseURL+"/runs", body, &created, idemKey)
		if err == nil || !isGatewayRateLimited(err) {
			return &created, status, err
		}
		if attempt >= len(a.backoff429) {
			return nil, status, fmt.Errorf("rate limited after %d retries: %w", attempt, err)
		}
		a.logGateway("runs-create-429-retry", fmt.Sprintf("attempt=%d", attempt+1), false)
		select {
		case <-ctx.Done():
			return nil, status, ctx.Err()
		case <-time.After(a.backoff429[attempt]):
		}
	}
}

// retryFreshRun retries a run once on a fresh session (no continuation
// history), used when the previous run produced no usable text — e.g. the
// hermes "(empty)" sentinel or a blank output. It returns (nil, false) when
// the retry cannot produce a usable result, so the caller falls back to its
// own error path.
func (a *HermesAdapter) retryFreshRun(ctx context.Context, taskKey, formatted, prevRunID, idemKey string) (*DeliverMessageResult, bool) {
	a.deleteMappedSession(taskKey)
	a.logGateway("runs-retry-fresh", taskKey, false)

	fresh := runCreateRequest{Input: formatted, Model: a.model}
	var created runCreateResponse
	if _, err := a.callJSONIdempotent(ctx, http.MethodPost, a.baseURL+"/runs", fresh, &created, idemKey); err != nil {
		return nil, false
	}
	runID := strings.TrimSpace(created.RunID)
	if runID == "" {
		runID = strings.TrimSpace(created.ID)
	}
	if runID == "" {
		return nil, false
	}
	if sid := strings.TrimSpace(created.SessionID); sid != "" {
		a.saveMappedSession(taskKey, sid)
	}

	final, err := a.pollRun(ctx, runID)
	if err != nil || final == nil {
		return nil, false
	}

	if runFailed(final.Status) {
		return &DeliverMessageResult{
			Success: false,
			RunID:   runID,
			Error:   "任务上下文被模型服务拒绝，已自动重开任务，请重新提交",
		}, true
	}

	reply := extractRunText(*final)
	if strings.TrimSpace(reply) == "" || strings.TrimSpace(reply) == "(empty)" {
		return nil, false
	}

	if sessionID := strings.TrimSpace(final.SessionID); sessionID != "" {
		a.saveMappedSession(taskKey, sessionID)
	}
	return &DeliverMessageResult{
		Success:  true,
		Accepted: true,
		RunID:    runID,
		Reply:    reply,
	}, true
}

// ── HTTP transport ────────────────────────────────────────────────

// callJSON performs a JSON request against the gateway. It returns the HTTP
// status code and an error (which embeds the response body on non-2xx).
func (a *HermesAdapter) callJSON(ctx context.Context, method, url string, reqBody any, out any) (int, error) {
	return a.callJSONWithHeaders(ctx, method, url, reqBody, out, nil)
}

// callJSONWithHeaders is callJSON with extra request headers (used for the
// gateway Idempotency-Key, T1.4).
func (a *HermesAdapter) callJSONWithHeaders(ctx context.Context, method, url string, reqBody any, out any, headers map[string]string) (int, error) {
	var bodyReader io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return 0, fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}
	if a.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+a.apiKey)
	}

	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, ctxErr
		}
		return 0, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("gateway returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode response: %w", err)
		}
	}
	return resp.StatusCode, nil
}

// idemKeyOf builds an Idempotency-Key from the upstream message id. Empty
// messageID → "" (no header; the gateway then behaves as before).
// suffix distinguishes retry attempts within one HandleMessage ("-r1", ...).
func idemKeyOf(messageID, suffix string) string {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return ""
	}
	return messageID + suffix
}

// callJSONIdempotent performs a JSON request carrying
// `Idempotency-Key: <idemKey>`. A 409 (same key, different payload — e.g.
// the retry body changed) degrades to one retry WITHOUT the header, matching
// the pre-T1.4 behavior. Empty idemKey sends no header at all.
func (a *HermesAdapter) callJSONIdempotent(ctx context.Context, method, url string, reqBody any, out any, idemKey string) (int, error) {
	if idemKey == "" {
		return a.callJSON(ctx, method, url, reqBody, out)
	}
	status, err := a.callJSONWithHeaders(ctx, method, url, reqBody, out, map[string]string{"Idempotency-Key": idemKey})
	if err != nil && status == http.StatusConflict {
		// Same key already registered with a different payload: drop the
		// header and retry once so the delivery still lands.
		return a.callJSON(ctx, method, url, reqBody, out)
	}
	return status, err
}

func (a *HermesAdapter) rootURL() string {
	u := strings.TrimRight(a.baseURL, "/")
	if strings.HasSuffix(u, "/v1") {
		u = strings.TrimSuffix(u, "/v1")
	}
	return u
}

// ── Task ID extraction ─────────────────────────────────────────────

// extractTaskID returns the taskId from message metadata, if present.
// Accepts both "taskId" and "task_id" payload spellings. This is used as a
// stable fallback key when the message SessionKey changes across rounds
// (e.g. todo.assigned → task.context.result).
func extractTaskID(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	for _, key := range []string{"taskId", "task_id"} {
		if id, ok := metadata[key].(string); ok {
			if id = strings.TrimSpace(id); id != "" {
				return id
			}
		}
	}
	return ""
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
		Adapter:     "hermes",
		SessionKey:  sessionKey,
		SessionID:   sessionID,
		CreatedAtMs: createdAt,
		UpdatedAtMs: now,
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

func (a *HermesAdapter) logGateway(operation, sessionKey string, hasContinuation bool) {
	if a.log == nil {
		return
	}
	a.log.Info("hermes gateway call",
		slog.String("operation", operation),
		slog.String("sessionKey", sessionKey),
		slog.Bool("continuation", hasContinuation),
	)
}

// ── Error detection ─────────────────────────────────────────────────

// isGatewayUnknownSessionError inspects a non-2xx gateway response to decide
// whether a continuation id (previous_response_id / session_id) is stale and
// should be dropped before retrying.
func isGatewayUnknownSessionError(statusCode int, body string) bool {
	if statusCode < 400 {
		return false
	}
	b := strings.ToLower(body)
	return strings.Contains(b, "not found") ||
		strings.Contains(b, "unknown session") ||
		strings.Contains(b, "no such session") ||
		strings.Contains(b, "no recorded session") ||
		strings.Contains(b, "session not found") ||
		strings.Contains(b, "invalid session")
}

// isModelProviderError reports whether a reply text is a model-provider
// rejection that hermes digested into the response (e.g. deepseek's strict
// "empty tool_calls array" 400). These are session-history problems, not user
// input problems — the adapter recovers by starting a fresh conversation.
func isModelProviderError(text string) bool {
	b := strings.ToLower(text)
	return strings.Contains(b, "tool_calls") ||
		strings.Contains(b, "invalid_request_error") ||
		strings.Contains(b, "badrequesterror") ||
		strings.Contains(b, "http 400") ||
		strings.Contains(b, "non-retryable client error") ||
		strings.Contains(b, "api call failed") ||
		strings.Contains(b, "context_length_exceeded")
}

// isFeedbackLike reports whether the incoming message is a confirmation/ACK
// (message type ends in .response or .error) — i.e. the backend's reply to a
// message this node already sent. These carry no new work; when a model-provider
// history rejection hits such a message, retrying fresh would re-execute the
// whole task (duplicate deliveries), so the adapter swallows the confirmation
// instead of re-running.
func isFeedbackLike(msgType string) bool {
	return strings.HasSuffix(msgType, ".response") || strings.HasSuffix(msgType, ".error")
}

// ── Request / response types ───────────────────────────────────────

type responsesRequest struct {
	Model              string `json:"model"`
	Input              string `json:"input"`
	PreviousResponseID string `json:"previous_response_id,omitempty"`
	// Conversation names a hermes conversation; the server chains responses
	// that share the same name. This is the supported way to continue a
	// task-scoped dialogue — `session_id` is only echoed back and does not
	// drive continuation.
	Conversation string `json:"conversation,omitempty"`
}

type responsesResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	// OutputText is not returned by the gateway (verified against a live one:
	// the top level carries id/model/object/output/status/usage/created_at
	// only). It is kept purely as tolerance in case a future gateway adds it;
	// extractResponseText always falls through to parsing Output below.
	OutputText string `json:"output_text"`
	Output     []struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Status  string `json:"status"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
}

type runCreateRequest struct {
	Model string `json:"model,omitempty"`
	Input string `json:"input"`
	// Continuation id for task context. Field name TBD (§7.1).
	SessionID string `json:"session_id,omitempty"`
}

type runCreateResponse struct {
	RunID     string `json:"run_id"`
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
}

type runStatusResponse struct {
	RunID     string `json:"run_id"`
	Status    string `json:"status"`
	SessionID string `json:"session_id"`
	Output    string `json:"output"`
	Result    string `json:"result"`
	Response  string `json:"response"`
	Content   string `json:"content"`
	Messages  []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

// ── Response text extraction (tolerant of field variations) ─────────

func extractResponseText(r responsesResponse) string {
	if strings.TrimSpace(r.OutputText) != "" {
		return strings.TrimSpace(r.OutputText)
	}
	var parts []string
	for _, o := range r.Output {
		if o.Role != "" && o.Role != "assistant" && o.Type != "message" {
			continue
		}
		for _, c := range o.Content {
			if strings.TrimSpace(c.Text) == "" {
				continue
			}
			if c.Type == "" || c.Type == "output_text" || c.Type == "text" {
				parts = append(parts, c.Text)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func extractRunText(r runStatusResponse) string {
	for _, f := range []string{r.Output, r.Result, r.Response, r.Content} {
		if strings.TrimSpace(f) != "" {
			return strings.TrimSpace(f)
		}
	}
	for _, m := range r.Messages {
		if strings.TrimSpace(m.Content) != "" {
			return strings.TrimSpace(m.Content)
		}
	}
	return ""
}

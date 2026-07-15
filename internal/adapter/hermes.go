package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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
	// Gateway API connection (hermes gateway run, API Server enabled).
	BaseURL string // e.g. http://127.0.0.1:8642/v1
	APIKey  string // API_SERVER_KEY
	Model   string // advertised agent name from GET /v1/models
}

// HermesAdapter delivers messages to a local Hermes agent via the Gateway
// HTTP API (a long-running `hermes gateway run` process).
//
// Message routing:
//   - chat.message            -> POST /v1/responses (stateful, previous_response_id continuation)
//   - task.*                 -> POST /v1/responses (same dialogue/stateful flow as chat)
//   - todo.*                 -> POST /v1/runs      (long-running, polled to terminal state)
type HermesAdapter struct {
	nodeID       string
	log          *slog.Logger
	sessionStore *store.FSStore
	agentRole    string

	httpClient *http.Client
	baseURL    string
	apiKey     string
	model      string
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

	return &HermesAdapter{
		nodeID:       strings.TrimSpace(cfg.NodeID),
		log:          cfg.Logger,
		sessionStore: cfg.SessionStore,
		agentRole:    strings.ToLower(strings.TrimSpace(cfg.AgentRole)),
		// Timeout is driven by the caller-supplied context, not a fixed client timeout.
		httpClient: &http.Client{Timeout: 0},
		baseURL:    baseURL,
		apiKey:     strings.TrimSpace(cfg.APIKey),
		model:      model,
	}, nil
}

// DeliverMessage formats the incoming message with the standard ClawSynapse
// protocol header, then routes it to the appropriate Gateway endpoint based
// on the message type (chat vs task).
func (a *HermesAdapter) DeliverMessage(ctx context.Context, req DeliverMessageRequest) (*DeliverMessageResult, error) {
	// Use the standard structured header format (same as openclaw/opencode/codex)
	formatted := formatDeliverMessage(a.nodeID, req)

	if isRunsMessage(req.Type) {
		return a.deliverViaRuns(ctx, formatted, req)
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

// ── Routing helpers ───────────────────────────────────────────────

// isRunsMessage reports whether the message type belongs to the long-running
// Runs flow (polled to a terminal state). Only `todo.*` messages take this path;
// `task.*` and `chat.*` both go through the stateful Responses API.
func isRunsMessage(msgType string) bool {
	t := strings.TrimSpace(msgType)
	return strings.HasPrefix(t, "todo.")
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

// taskSessionKey derives the stable mapping key for task messages.
// Prefers SessionKey, then the stable taskId from metadata.
func (a *HermesAdapter) taskSessionKey(req DeliverMessageRequest) string {
	if k := strings.TrimSpace(req.SessionKey); k != "" {
		return k
	}
	if t := extractTaskID(req.Metadata); t != "" {
		return t
	}
	return "default"
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
	status, err := a.callJSON(ctx, http.MethodPost, a.baseURL+"/responses", body, &resp)
	if err != nil && prevID != "" && isGatewayUnknownSessionError(status, err.Error()) {
		a.deleteMappedSession(chatKey)
		body.PreviousResponseID = ""
		a.logGateway("responses-retry", chatKey, false)
		status, err = a.callJSON(ctx, http.MethodPost, a.baseURL+"/responses", body, &resp)
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
	taskKey := "task:" + a.taskSessionKey(req)
	prevID := a.loadMappedSessionID(taskKey)

	// NOTE(§7.1): continuation field name for /v1/runs is to be verified
	// against the live gateway (session_id vs previous_response_id).
	body := runCreateRequest{Input: formatted}
	if prevID != "" {
		body.SessionID = prevID
	}

	a.logGateway("runs-create", taskKey, prevID != "")

	var created runCreateResponse
	status, err := a.callJSON(ctx, http.MethodPost, a.baseURL+"/runs", body, &created)
	if err != nil && prevID != "" && isGatewayUnknownSessionError(status, err.Error()) {
		a.deleteMappedSession(taskKey)
		body.SessionID = ""
		a.logGateway("runs-create-retry", taskKey, false)
		status, err = a.callJSON(ctx, http.MethodPost, a.baseURL+"/runs", body, &created)
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
		return nil, fmt.Errorf("hermes gateway runs poll: %w", err)
	}

	switch final.Status {
	case "failed", "stopped", "cancelled", "error":
		return &DeliverMessageResult{
			Success: false,
			RunID:   runID,
			Error:   "hermes run " + final.Status + ": " + extractRunText(*final),
		}, nil
	}

	reply := extractRunText(*final)
	if strings.TrimSpace(reply) == "" {
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

// pollRun polls GET /v1/runs/{id} until it reaches a terminal state or the
// context is cancelled. Polling interval backs off from 1s to a 5s cap.
func (a *HermesAdapter) pollRun(ctx context.Context, runID string) (*runStatusResponse, error) {
	interval := time.Second
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		var st runStatusResponse
		_, err := a.callJSON(ctx, http.MethodGet, a.baseURL+"/runs/"+runID, nil, &st)
		if err != nil {
			return nil, err
		}

		switch strings.ToLower(strings.TrimSpace(st.Status)) {
		case "completed":
			return &st, nil
		case "failed", "stopped", "cancelled", "error":
			return &st, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
		if interval < 5*time.Second {
			interval *= 2
		} else {
			interval = 5 * time.Second
		}
	}
}

// ── HTTP transport ────────────────────────────────────────────────

// callJSON performs a JSON request against the gateway. It returns the HTTP
// status code and an error (which embeds the response body on non-2xx).
func (a *HermesAdapter) callJSON(ctx context.Context, method, url string, reqBody any, out any) (int, error) {
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

func (a *HermesAdapter) rootURL() string {
	u := strings.TrimRight(a.baseURL, "/")
	if strings.HasSuffix(u, "/v1") {
		u = strings.TrimSuffix(u, "/v1")
	}
	return u
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

// ── Request / response types ───────────────────────────────────────

type responsesRequest struct {
	Model              string `json:"model"`
	Input              string `json:"input"`
	PreviousResponseID string `json:"previous_response_id,omitempty"`
}

type responsesResponse struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
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

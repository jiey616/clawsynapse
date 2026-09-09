package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"clawsynapse/internal/store"
)

// fakeGateway is an in-memory Hermes Gateway API mock.
type fakeGateway struct {
	mu sync.Mutex

	responsesPrev         []string // previous_response_id received on each /v1/responses call
	responsesConversation []string // conversation received on each /v1/responses call
	responsesInstructions []string // instructions received on each /v1/responses call
	runsSessionID         []string // session_id received on each /v1/runs call
	runsModel             []string // model received on each /v1/runs call
	runsInstructions      []string // instructions received on each /v1/runs call

	runStatusIdx map[string]int

	healthCode int

	unknownResponses bool // simulate unknown-session on first continuation (responses)
	unknownRuns      bool // simulate unknown-session on first continuation (runs)
	unknownTask      bool // simulate unknown-session on first task conversation continuation
	urDone           bool
	urRunsDone       bool
	urTaskDone       bool

	// responsesReplies optionally scripts the OutputText returned by each
	// /v1/responses call (popped in order; falls back to "hello").
	responsesReplies []string

	// T1.3 cancel/steer support
	runsRunID   string              // run id returned by POST /v1/runs (default "run-1")
	runStatusSeq map[string][]string // per-run GET status sequences (default legacy ["running","completed"])
	stopCalls   []string            // run ids passed to POST /v1/runs/{id}/stop
	steerCalls  []string            // run ids passed to POST /v1/runs/{id}/steer

	// T1.4 idempotency support
	runsIdem      []string // Idempotency-Key received on each /v1/runs POST ("" = absent)
	responsesIdem []string // Idempotency-Key received on each /v1/responses POST ("" = absent)
	conflictRuns  bool     // first /v1/runs POST carrying a key answers 409
	conflictDone  bool
}

func (fg *fakeGateway) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		code := fg.healthCode
		if code == 0 {
			code = 200
		}
		w.WriteHeader(code)
	})

	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		var req responsesRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		fg.mu.Lock()
		fg.responsesPrev = append(fg.responsesPrev, req.PreviousResponseID)
		fg.responsesConversation = append(fg.responsesConversation, req.Conversation)
		fg.responsesInstructions = append(fg.responsesInstructions, req.Instructions)
		fg.responsesIdem = append(fg.responsesIdem, r.Header.Get("Idempotency-Key"))
		id := fmt.Sprintf("resp-%d", len(fg.responsesPrev))
		fg.mu.Unlock()

		if fg.unknownResponses && !fg.urDone && req.PreviousResponseID != "" {
			fg.urDone = true
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"session not found"}`))
			return
		}

		if fg.unknownTask && !fg.urTaskDone && req.Conversation != "" {
			fg.urTaskDone = true
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"session not found"}`))
			return
		}

		reply := "hello"
		fg.mu.Lock()
		if len(fg.responsesReplies) > 0 {
			reply = fg.responsesReplies[0]
			fg.responsesReplies = fg.responsesReplies[1:]
		}
		fg.mu.Unlock()

		_ = json.NewEncoder(w).Encode(responsesResponse{
			ID:         id,
			Status:     "completed",
			OutputText: reply,
		})
	})

	mux.HandleFunc("/v1/runs", func(w http.ResponseWriter, r *http.Request) {
		var req runCreateRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		fg.mu.Lock()
		fg.runsSessionID = append(fg.runsSessionID, req.SessionID)
		fg.runsModel = append(fg.runsModel, req.Model)
		fg.runsInstructions = append(fg.runsInstructions, req.Instructions)
		fg.runsIdem = append(fg.runsIdem, r.Header.Get("Idempotency-Key"))
		fg.mu.Unlock()

		// T1.4: same key, different payload → 409 once, so the adapter can
		// prove its degrade-to-headerless retry.
		if fg.conflictRuns {
			fg.mu.Lock()
			hadKey := r.Header.Get("Idempotency-Key") != ""
			fire := hadKey && !fg.conflictDone
			if fire {
				fg.conflictDone = true
			}
			fg.mu.Unlock()
			if fire {
				w.WriteHeader(409)
				_, _ = w.Write([]byte(`{"error":"idempotency key conflict"}`))
				return
			}
		}

		if fg.unknownRuns && !fg.urRunsDone && req.SessionID != "" {
			fg.urRunsDone = true
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"session not found"}`))
			return
		}

		_ = json.NewEncoder(w).Encode(runCreateResponse{
			RunID:     fg.runsRunIDOrDefault(),
			SessionID: "sess-1",
		})
	})

	mux.HandleFunc("/v1/runs/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/v1/runs/")
		switch {
		case strings.HasSuffix(rest, "/stop"):
			id := strings.TrimSuffix(rest, "/stop")
			fg.mu.Lock()
			fg.stopCalls = append(fg.stopCalls, id)
			fg.mu.Unlock()
			w.WriteHeader(200)
			return
		case strings.HasSuffix(rest, "/steer"):
			id := strings.TrimSuffix(rest, "/steer")
			fg.mu.Lock()
			fg.steerCalls = append(fg.steerCalls, id)
			fg.mu.Unlock()
			w.WriteHeader(200)
			return
		}

		runID := rest
		fg.mu.Lock()
		if fg.runStatusIdx == nil {
			fg.runStatusIdx = map[string]int{}
		}
		i := fg.runStatusIdx[runID]
		fg.runStatusIdx[runID]++
		seq := fg.runStatusSeq[runID]
		fg.mu.Unlock()

		st := "completed"
		if len(seq) > 0 {
			if i < len(seq) {
				st = seq[i]
			} else {
				st = seq[len(seq)-1]
			}
		} else if runID == "run-1" {
			// legacy default: first poll running, then completed
			if i < 1 {
				st = "running"
			}
		}
		_ = json.NewEncoder(w).Encode(runStatusResponse{
			RunID:  runID,
			Status: st,
			Output: "task done",
		})
	})

	return mux
}

func (fg *fakeGateway) runsRunIDOrDefault() string {
	if fg.runsRunID != "" {
		return fg.runsRunID
	}
	return "run-1"
}

func (fg *fakeGateway) recordedStopCalls() []string {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	return append([]string(nil), fg.stopCalls...)
}

func (fg *fakeGateway) recordedSteerCalls() []string {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	return append([]string(nil), fg.steerCalls...)
}

func (fg *fakeGateway) recordedRunsIdem() []string {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	return append([]string(nil), fg.runsIdem...)
}

func (fg *fakeGateway) recordedResponsesIdem() []string {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	return append([]string(nil), fg.responsesIdem...)
}

func newTestAdapter(t *testing.T, fg *fakeGateway) *HermesAdapter {
	t.Helper()
	srv := httptest.NewServer(fg.handler())
	t.Cleanup(srv.Close)

	a, err := NewHermesAdapter(HermesConfig{
		NodeID:       "n1",
		BaseURL:      srv.URL + "/v1",
		Model:        "hermes-agent",
		SessionStore: store.NewFSStore(t.TempDir()),
	})
	if err != nil {
		t.Fatalf("NewHermesAdapter failed: %v", err)
	}
	return a
}

func testCtx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// ── Routing ──────────────────────────────────────────────────────

func TestIsRunsMessage(t *testing.T) {
	cases := map[string]bool{
		"chat.message":        false,
		"task.message":        false,
		"meeting.invite":      false,
		"meeting.message":     false,
		"todo.assigned":       true,
		"task.context.result": false,
		"todo.response":       true,
		"":                    false,
		"chat.response":       false,
	}
	for msgType, want := range cases {
		if got := isRunsMessage(msgType); got != want {
			t.Errorf("isRunsMessage(%q) = %v, want %v", msgType, got, want)
		}
	}
}

// ── Run status machine ───────────────────────────────────────────

// TestRunTerminalStatuses pins the terminal/failure classification of every
// /v1/runs status observed on hermes v0.21.0 plus defensive extras.
func TestRunTerminalStatuses(t *testing.T) {
	cases := []struct {
		status   string
		terminal bool
		failed   bool
	}{
		{"completed", true, false},
		{"failed", true, true},
		{"cancelled", true, true},  // gateway-observed spelling
		{"canceled", true, true},   // US spelling, defensive
		{"interrupted", true, true}, // /stop terminal state (was the "hangs 10m" bug)
		{"stopped", true, true},
		{"error", true, true},
		{"queued", false, false},
		{"started", false, false},
		{"running", false, false},
		{"stopping", false, false}, // transitioning, NOT terminal
		{"", false, false},
		{"unknown-future-status", false, false},
		{"Interrupted", true, true}, // case-insensitive lookup
		{" Completed", true, false},
	}
	for _, c := range cases {
		if got := isTerminalRunStatus(c.status); got != c.terminal {
			t.Errorf("isTerminalRunStatus(%q) = %v, want %v", c.status, got, c.terminal)
		}
		if got := runFailed(c.status); got != c.failed {
			t.Errorf("runFailed(%q) = %v, want %v", c.status, got, c.failed)
		}
	}
}

// ── Dialogue: Responses API ──────────────────────────────────────

func TestDeliverViaResponses_FirstTurn(t *testing.T) {
	fg := &fakeGateway{}
	a := newTestAdapter(t, fg)
	ctx, cancel := testCtx(t)
	defer cancel()

	req := DeliverMessageRequest{Type: "chat.message", From: "alice", SessionKey: "c1", Message: "hi"}
	res, err := a.DeliverMessage(ctx, req)
	if err != nil {
		t.Fatalf("DeliverMessage failed: %v", err)
	}
	if !res.Success || res.Reply != "hello" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(fg.responsesPrev) != 1 || fg.responsesPrev[0] != "" {
		t.Errorf("first turn should send no previous_response_id, got %v", fg.responsesPrev)
	}
	if got := a.loadMappedSessionID("chat:c1"); got != "resp-1" {
		t.Errorf("expected chat:c1 -> resp-1, got %q", got)
	}
}

func TestDeliverViaResponses_Continuation(t *testing.T) {
	fg := &fakeGateway{}
	a := newTestAdapter(t, fg)
	ctx, cancel := testCtx(t)
	defer cancel()

	// First turn establishes mapping chat:c1 -> resp-1
	if _, err := a.DeliverMessage(ctx, DeliverMessageRequest{Type: "chat.message", SessionKey: "c1", Message: "hi"}); err != nil {
		t.Fatal(err)
	}
	// Second turn should continue with previous_response_id = resp-1
	res, err := a.DeliverMessage(ctx, DeliverMessageRequest{Type: "chat.message", SessionKey: "c1", Message: "again"})
	if err != nil {
		t.Fatalf("DeliverMessage failed: %v", err)
	}
	if res.Reply != "hello" {
		t.Fatalf("unexpected reply: %q", res.Reply)
	}
	if len(fg.responsesPrev) != 2 {
		t.Fatalf("expected 2 responses calls, got %d", len(fg.responsesPrev))
	}
	if fg.responsesPrev[1] != "resp-1" {
		t.Errorf("expected continuation with previous_response_id=resp-1, got %q", fg.responsesPrev[1])
	}
}

func TestDeliverViaResponses_NewSessionIsolated(t *testing.T) {
	fg := &fakeGateway{}
	a := newTestAdapter(t, fg)
	ctx, cancel := testCtx(t)
	defer cancel()

	if _, err := a.DeliverMessage(ctx, DeliverMessageRequest{Type: "chat.message", SessionKey: "c1", Message: "hi"}); err != nil {
		t.Fatal(err)
	}
	// Different SessionKey => independent conversation (no previous_response_id)
	if _, err := a.DeliverMessage(ctx, DeliverMessageRequest{Type: "chat.message", SessionKey: "c2", Message: "hi"}); err != nil {
		t.Fatal(err)
	}
	if len(fg.responsesPrev) != 2 {
		t.Fatalf("expected 2 responses calls, got %d", len(fg.responsesPrev))
	}
	if fg.responsesPrev[1] != "" {
		t.Errorf("new session should not carry previous_response_id, got %q", fg.responsesPrev[1])
	}
}

func TestDeliverViaResponses_FallbackKey(t *testing.T) {
	fg := &fakeGateway{}
	a := newTestAdapter(t, fg)
	ctx, cancel := testCtx(t)
	defer cancel()

	// No SessionKey => fallback key cs-<from>-<nodeID> = cs-alice-n1
	if _, err := a.DeliverMessage(ctx, DeliverMessageRequest{Type: "chat.message", From: "alice", Message: "hi"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DeliverMessage(ctx, DeliverMessageRequest{Type: "chat.message", From: "alice", Message: "again"}); err != nil {
		t.Fatal(err)
	}
	if len(fg.responsesPrev) != 2 {
		t.Fatalf("expected 2 responses calls, got %d", len(fg.responsesPrev))
	}
	if fg.responsesPrev[1] != "resp-1" {
		t.Errorf("fallback key should continue same sender, got prevID %q", fg.responsesPrev[1])
	}
	if got := a.loadMappedSessionID("chat:cs-alice-n1"); got != "resp-2" {
		t.Errorf("expected fallback mapping updated to resp-2 after continuation, got %q", got)
	}
}

func TestDeliverViaResponses_UnknownSessionRetry(t *testing.T) {
	fg := &fakeGateway{unknownResponses: true}
	a := newTestAdapter(t, fg)
	ctx, cancel := testCtx(t)
	defer cancel()

	// Establish mapping chat:c1 -> resp-1
	if _, err := a.DeliverMessage(ctx, DeliverMessageRequest{Type: "chat.message", SessionKey: "c1", Message: "hi"}); err != nil {
		t.Fatal(err)
	}
	// Next turn carries prevID=resp-1 which is now "unknown" on the gateway;
	// adapter should drop the mapping and retry without it.
	res, err := a.DeliverMessage(ctx, DeliverMessageRequest{Type: "chat.message", SessionKey: "c1", Message: "again"})
	if err != nil {
		t.Fatalf("DeliverMessage failed: %v", err)
	}
	if res.Reply != "hello" {
		t.Fatalf("expected successful retry reply, got %+v", res)
	}
	// prevID sequence: first-turn(""), retry-turn(prevID resp-1 -> 404), retry("")
	if len(fg.responsesPrev) != 3 {
		t.Fatalf("expected 3 responses calls (first + retry-with + retry-without), got %d: %v", len(fg.responsesPrev), fg.responsesPrev)
	}
	if fg.responsesPrev[1] != "resp-1" {
		t.Errorf("retry attempt should carry stale prevID, got %q", fg.responsesPrev[1])
	}
	if fg.responsesPrev[2] != "" {
		t.Errorf("final retry should drop prevID, got %q", fg.responsesPrev[2])
	}
	if got := a.loadMappedSessionID("chat:c1"); got != "resp-3" {
		t.Errorf("mapping should be rebuilt after retry, got %q", got)
	}
}

func TestDeliverViaResponses_Meeting(t *testing.T) {
	fg := &fakeGateway{}
	a := newTestAdapter(t, fg)
	ctx, cancel := testCtx(t)
	defer cancel()

	// meeting.* must route to the stateful Responses API (not Runs), and
	// continue the same dialogue via previous_response_id like chat.
	if _, err := a.DeliverMessage(ctx, DeliverMessageRequest{Type: "meeting.invite", SessionKey: "m1", Message: "standup"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DeliverMessage(ctx, DeliverMessageRequest{Type: "meeting.message", SessionKey: "m1", Message: "notes"}); err != nil {
		t.Fatal(err)
	}
	if len(fg.responsesPrev) != 2 {
		t.Fatalf("expected 2 /v1/responses calls for meeting, got %d: %v", len(fg.responsesPrev), fg.responsesPrev)
	}
	if fg.responsesPrev[1] != "resp-1" {
		t.Errorf("meeting continuation should carry previous_response_id=resp-1, got %q", fg.responsesPrev[1])
	}
	if len(fg.runsSessionID) != 0 {
		t.Errorf("meeting must NOT hit /v1/runs, got %d runs calls", len(fg.runsSessionID))
	}
	if got := a.loadMappedSessionID("chat:m1"); got != "resp-2" {
		t.Errorf("expected meeting:m1 -> resp-2 mapping, got %q", got)
	}
}

// ── Task flow: Runs API ──────────────────────────────────────────

func TestDeliverViaRuns_Polling(t *testing.T) {
	fg := &fakeGateway{}
	a := newTestAdapter(t, fg)
	ctx, cancel := testCtx(t)
	defer cancel()

	res, err := a.DeliverMessage(ctx, DeliverMessageRequest{Type: "todo.message", SessionKey: "t1", Message: "do it"})
	if err != nil {
		t.Fatalf("DeliverMessage failed: %v", err)
	}
	if !res.Success || res.Reply != "task done" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.RunID != "run-1" {
		t.Errorf("expected RunID run-1, got %q", res.RunID)
	}
	if fg.runsSessionID[0] != "" {
		t.Errorf("first run should not carry session_id, got %q", fg.runsSessionID[0])
	}
	if got := a.loadMappedSessionID("task:t1"); got != "sess-1" {
		t.Errorf("expected task:t1 -> sess-1, got %q", got)
	}
}

func TestDeliverViaRuns_Continuation(t *testing.T) {
	fg := &fakeGateway{}
	a := newTestAdapter(t, fg)
	ctx, cancel := testCtx(t)
	defer cancel()

	if _, err := a.DeliverMessage(ctx, DeliverMessageRequest{Type: "todo.message", SessionKey: "t1", Message: "do it"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DeliverMessage(ctx, DeliverMessageRequest{Type: "todo.message", SessionKey: "t1", Message: "continue"}); err != nil {
		t.Fatal(err)
	}
	if len(fg.runsSessionID) != 2 {
		t.Fatalf("expected 2 runs calls, got %d", len(fg.runsSessionID))
	}
	if fg.runsSessionID[1] != "sess-1" {
		t.Errorf("task continuation should carry session_id=sess-1, got %q", fg.runsSessionID[1])
	}
}

func TestDeliverViaRuns_UnknownSessionRetry(t *testing.T) {
	fg := &fakeGateway{unknownRuns: true}
	a := newTestAdapter(t, fg)
	ctx, cancel := testCtx(t)
	defer cancel()

	if _, err := a.DeliverMessage(ctx, DeliverMessageRequest{Type: "todo.message", SessionKey: "t1", Message: "do it"}); err != nil {
		t.Fatal(err)
	}
	res, err := a.DeliverMessage(ctx, DeliverMessageRequest{Type: "todo.message", SessionKey: "t1", Message: "continue"})
	if err != nil {
		t.Fatalf("DeliverMessage failed: %v", err)
	}
	if res.Reply != "task done" {
		t.Fatalf("expected successful retry reply, got %+v", res)
	}
	if len(fg.runsSessionID) != 3 {
		t.Fatalf("expected 3 runs calls, got %d: %v", len(fg.runsSessionID), fg.runsSessionID)
	}
	if fg.runsSessionID[1] != "sess-1" {
		t.Errorf("retry attempt should carry stale session_id, got %q", fg.runsSessionID[1])
	}
	if fg.runsSessionID[2] != "" {
		t.Errorf("final retry should drop session_id, got %q", fg.runsSessionID[2])
	}
}

// ── GetStatus ────────────────────────────────────────────────────

func TestHermesAdapter_GetStatus_Healthy(t *testing.T) {
	fg := &fakeGateway{healthCode: 200}
	a := newTestAdapter(t, fg)
	status, err := a.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !status.Healthy {
		t.Error("expected healthy")
	}
}

func TestHermesAdapter_GetStatus_Unhealthy(t *testing.T) {
	fg := &fakeGateway{healthCode: 500}
	a := newTestAdapter(t, fg)
	status, err := a.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.Healthy {
		t.Error("expected unhealthy for 500")
	}
}

// ── Header format preserved (bare, no system prompt) ─────────────

func TestDeliverViaResponses_HeaderFormat(t *testing.T) {
	fg := &fakeGateway{}
	a := newTestAdapter(t, fg)
	ctx, cancel := testCtx(t)
	defer cancel()

	// Capture the formatted prompt passed to the gateway by inspecting the
	// request body via a spy server.
	spy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req responsesRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !strings.HasPrefix(req.Input, "[clawsynapse") {
			t.Errorf("expected gateway input to start with [clawsynapse, got %q", req.Input[:min(40, len(req.Input))])
		}
		if !strings.Contains(req.Input, "type=chat.message") {
			t.Error("expected header to contain type=chat.message")
		}
		if !strings.Contains(req.Input, "from=alice") {
			t.Error("expected header to contain from=alice")
		}
		if !strings.Contains(req.Input, "session=c1") {
			t.Error("expected header to contain session=c1")
		}
		if !strings.Contains(req.Input, "Hello!") {
			t.Error("expected header to contain message body")
		}
		_ = json.NewEncoder(w).Encode(responsesResponse{ID: "resp-1", Status: "completed", OutputText: "ok"})
	}))
	defer spy.Close()

	a.baseURL = spy.URL + "/v1"
	if _, err := a.DeliverMessage(ctx, DeliverMessageRequest{Type: "chat.message", From: "alice", SessionKey: "c1", Message: "Hello!"}); err != nil {
		t.Fatalf("DeliverMessage failed: %v", err)
	}
}

// ── TodoMode=responses ────────────────────────────────────────────

func TestDeliverViaResponses_TodoMode(t *testing.T) {
	fg := &fakeGateway{}
	srv := httptest.NewServer(fg.handler())
	t.Cleanup(srv.Close)

	a, err := NewHermesAdapter(HermesConfig{
		NodeID:       "n1",
		BaseURL:      srv.URL + "/v1",
		Model:        "hermes-agent",
		SessionStore: store.NewFSStore(t.TempDir()),
		TodoMode:     "responses",
	})
	if err != nil {
		t.Fatalf("NewHermesAdapter failed: %v", err)
	}

	ctx, cancel := testCtx(t)
	defer cancel()

	// todo.* should use responses API instead of runs
	res, err := a.DeliverMessage(ctx, DeliverMessageRequest{Type: "todo.message", SessionKey: "t1", Message: "do it"})
	if err != nil {
		t.Fatalf("DeliverMessage failed: %v", err)
	}
	// The fake gateway's /v1/responses returns "hello"; "task done" is what the
	// runs endpoint returns, so this also proves the responses path was taken.
	if !res.Success || res.Reply != "hello" {
		t.Fatalf("unexpected result: %+v", res)
	}
	// Should NOT have created any runs
	if len(fg.runsSessionID) > 0 {
		t.Errorf("expected no runs calls in responses mode, got %d", len(fg.runsSessionID))
	}
	// Should have called responses API
	if len(fg.responsesPrev) == 0 {
		t.Errorf("expected responses API calls in responses mode, got 0")
	}
}

// TestDeliverTaskUsesConversation guards the continuation contract: the task id
// must be sent as `conversation`. hermes chains responses that share a
// conversation name, whereas `session_id` is only echoed back and does not
// drive continuation (verified against a live gateway).
func TestDeliverTaskUsesConversation(t *testing.T) {
	fg := &fakeGateway{}
	srv := httptest.NewServer(fg.handler())
	t.Cleanup(srv.Close)

	a, err := NewHermesAdapter(HermesConfig{
		NodeID:       "n1",
		BaseURL:      srv.URL + "/v1",
		Model:        "hermes-agent",
		SessionStore: store.NewFSStore(t.TempDir()),
		TodoMode:     "responses",
	})
	if err != nil {
		t.Fatalf("NewHermesAdapter failed: %v", err)
	}

	ctx, cancel := testCtx(t)
	defer cancel()

	for _, msgType := range []string{"task.message", "todo.assigned"} {
		if _, err := a.DeliverMessage(ctx, DeliverMessageRequest{
			Type: msgType, SessionKey: "task-abc", Message: "do it",
		}); err != nil {
			t.Fatalf("%s: DeliverMessage failed: %v", msgType, err)
		}
	}

	// Both a task.* and a todo.* message must land on the same conversation so
	// execution and dialogue share one hermes session.
	if len(fg.responsesConversation) != 2 {
		t.Fatalf("expected 2 conversation values, got %v", fg.responsesConversation)
	}
	for i, got := range fg.responsesConversation {
		if got != "task-abc" {
			t.Errorf("call %d: conversation = %q, want %q", i, got, "task-abc")
		}
	}
}

// TestDeliverTaskNoTaskIDOmitsConversation ensures a message with no task
// identifier does not send an empty conversation field.
func TestDeliverTaskNoTaskIDOmitsConversation(t *testing.T) {
	fg := &fakeGateway{}
	srv := httptest.NewServer(fg.handler())
	t.Cleanup(srv.Close)

	a, err := NewHermesAdapter(HermesConfig{
		NodeID:       "n1",
		BaseURL:      srv.URL + "/v1",
		Model:        "hermes-agent",
		SessionStore: store.NewFSStore(t.TempDir()),
		TodoMode:     "responses",
	})
	if err != nil {
		t.Fatalf("NewHermesAdapter failed: %v", err)
	}

	ctx, cancel := testCtx(t)
	defer cancel()

	if _, err := a.DeliverMessage(ctx, DeliverMessageRequest{
		Type: "todo.assigned", Message: "do it",
	}); err != nil {
		t.Fatalf("DeliverMessage failed: %v", err)
	}
	if len(fg.responsesConversation) != 1 || fg.responsesConversation[0] != "" {
		t.Errorf("expected empty conversation, got %v", fg.responsesConversation)
	}
}

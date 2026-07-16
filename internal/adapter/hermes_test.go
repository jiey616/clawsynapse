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

	responsesPrev []string // previous_response_id received on each /v1/responses call
	runsSessionID []string // session_id received on each /v1/runs call

	runStatusIdx map[string]int

	healthCode int

	unknownResponses bool // simulate unknown-session on first continuation (responses)
	unknownRuns      bool // simulate unknown-session on first continuation (runs)
	urDone           bool
	urRunsDone       bool
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
		id := fmt.Sprintf("resp-%d", len(fg.responsesPrev))
		fg.mu.Unlock()

		if fg.unknownResponses && !fg.urDone && req.PreviousResponseID != "" {
			fg.urDone = true
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"session not found"}`))
			return
		}

		_ = json.NewEncoder(w).Encode(responsesResponse{
			ID:         id,
			Status:     "completed",
			OutputText: "hello",
		})
	})

	mux.HandleFunc("/v1/runs", func(w http.ResponseWriter, r *http.Request) {
		var req runCreateRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		fg.mu.Lock()
		fg.runsSessionID = append(fg.runsSessionID, req.SessionID)
		fg.mu.Unlock()

		if fg.unknownRuns && !fg.urRunsDone && req.SessionID != "" {
			fg.urRunsDone = true
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"session not found"}`))
			return
		}

		_ = json.NewEncoder(w).Encode(runCreateResponse{
			RunID:     "run-1",
			SessionID: "sess-1",
		})
	})

	mux.HandleFunc("/v1/runs/", func(w http.ResponseWriter, r *http.Request) {
		seq := []string{"running", "completed"}
		fg.mu.Lock()
		if fg.runStatusIdx == nil {
			fg.runStatusIdx = map[string]int{}
		}
		i := fg.runStatusIdx["run-1"]
		fg.runStatusIdx["run-1"]++
		fg.mu.Unlock()
		st := "completed"
		if i < len(seq) {
			st = seq[i]
		}
		_ = json.NewEncoder(w).Encode(runStatusResponse{
			RunID:  "run-1",
			Status: st,
			Output: "task done",
		})
	})

	return mux
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

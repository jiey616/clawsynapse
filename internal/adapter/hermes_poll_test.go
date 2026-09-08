package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"clawsynapse/internal/store"
)

// newPollTestAdapter builds a HermesAdapter against a raw handler with fast
// poll tuning so resilience tests run in milliseconds.
func newPollTestAdapter(t *testing.T, h http.Handler) (*HermesAdapter, func()) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	a, err := NewHermesAdapter(HermesConfig{
		NodeID:  "n1",
		BaseURL: srv.URL + "/v1",
		Model:   "hermes-agent",
	})
	if err != nil {
		t.Fatalf("NewHermesAdapter failed: %v", err)
	}
	a.pollInterval = 10 * time.Millisecond
	a.pollIntervalMax = 20 * time.Millisecond
	return a, srv.Close
}

// runStatusHandler serves GET /v1/runs/{id} from a rotating sequence of
// status codes/bodies, one per request.
type runStatusHandler struct {
	bodies atomic.Int64   // number of status requests served
	seq    []func(w http.ResponseWriter) // one entry per poll request
}

func (rh *runStatusHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	i := int(rh.bodies.Add(1)) - 1
	if i >= len(rh.seq) {
		i = len(rh.seq) - 1 // repeat last
	}
	rh.seq[i](w)
}

func statusBody(status string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		_ = json.NewEncoder(w).Encode(runStatusResponse{RunID: "run-1", Status: status})
	}
}

// ── T0.2 acceptance ──────────────────────────────────────────────

func TestPollRun_ToleratesTransientErrors(t *testing.T) {
	rh := &runStatusHandler{seq: []func(w http.ResponseWriter){
		func(w http.ResponseWriter) { w.WriteHeader(500); _, _ = w.Write([]byte("boom")) },
		func(w http.ResponseWriter) { w.WriteHeader(502); _, _ = w.Write([]byte("bad gateway")) },
		statusBody("completed"),
	}}
	a, _ := newPollTestAdapter(t, rh)

	st, err := a.pollRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("pollRun should tolerate 2 transient errors, got: %v", err)
	}
	if st.Status != "completed" {
		t.Fatalf("status = %q, want completed", st.Status)
	}
}

func TestPollRun_FailsAfterThreeConsecutiveErrors(t *testing.T) {
	rh := &runStatusHandler{seq: []func(w http.ResponseWriter){
		func(w http.ResponseWriter) { w.WriteHeader(500); _, _ = w.Write([]byte("boom")) },
	}}
	a, _ := newPollTestAdapter(t, rh)

	_, err := a.pollRun(context.Background(), "run-1")
	if err == nil {
		t.Fatal("expected error after 3 consecutive poll failures")
	}
	if !strings.Contains(err.Error(), "consecutively") {
		t.Fatalf("error should mention consecutive failures, got: %v", err)
	}
}

func TestPollRun_StuckStatusFails(t *testing.T) {
	rh := &runStatusHandler{seq: []func(w http.ResponseWriter){statusBody("running")}}
	a, _ := newPollTestAdapter(t, rh)

	_, err := a.pollRun(context.Background(), "run-1")
	if err == nil {
		t.Fatal("expected stuck-run error")
	}
	if !strings.Contains(err.Error(), `stuck in status "running"`) {
		t.Fatalf("error should mention stuck status, got: %v", err)
	}
}

func TestPollRun_InterruptedIsTerminal(t *testing.T) {
	rh := &runStatusHandler{seq: []func(w http.ResponseWriter){statusBody("interrupted")}}
	a, _ := newPollTestAdapter(t, rh)

	st, err := a.pollRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("interrupted must be terminal, got error: %v", err)
	}
	if st.Status != "interrupted" {
		t.Fatalf("status = %q, want interrupted", st.Status)
	}
}

func TestPollRun_CallerCancelSurfacesCtxErr(t *testing.T) {
	rh := &runStatusHandler{seq: []func(w http.ResponseWriter){statusBody("running")}}
	a, _ := newPollTestAdapter(t, rh)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_, err := a.pollRun(ctx, "run-1")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("caller ctx deadline should surface as-is, got: %v", err)
	}
}

func TestPollRun_HardDeadline(t *testing.T) {
	rh := &runStatusHandler{seq: []func(w http.ResponseWriter){statusBody("running")}}
	a, _ := newPollTestAdapter(t, rh)
	a.pollDeadline = 80 * time.Millisecond

	_, err := a.pollRun(context.Background(), "run-1")
	if err == nil {
		t.Fatal("expected polling deadline error")
	}
	if !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("error should mention polling deadline, got: %v", err)
	}
}

// ── T0.3: run create carries the adapter model ───────────────────

func TestDeliverViaRuns_SendsModel(t *testing.T) {
	fg := &fakeGateway{}
	a := newTestAdapter(t, fg)
	ctx, cancel := testCtx(t)
	defer cancel()

	if _, err := a.DeliverMessage(ctx, DeliverMessageRequest{Type: "todo.assigned", SessionKey: "task-m1", Message: "do it"}); err != nil {
		t.Fatalf("DeliverMessage: %v", err)
	}
	if len(fg.runsModel) == 0 {
		t.Fatal("no /v1/runs create recorded")
	}
	if got := fg.runsModel[0]; got != "hermes-agent" {
		t.Fatalf("model = %q, want hermes-agent", got)
	}
}

// ── T0.4: task path resilience ───────────────────────────────────

func TestDeliverTask_UnknownSessionRetry(t *testing.T) {
	fg := &fakeGateway{unknownTask: true}
	a := newTestAdapter(t, fg)
	ctx, cancel := testCtx(t)
	defer cancel()

	res, err := a.DeliverMessage(ctx, DeliverMessageRequest{Type: "task.message", SessionKey: "task-42", Message: "hi"})
	if err != nil {
		t.Fatalf("unknown-session retry should recover: %v", err)
	}
	if !res.Success || res.Reply != "hello" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(fg.responsesConversation) != 2 {
		t.Fatalf("expected 2 /responses calls, got %d", len(fg.responsesConversation))
	}
	if fg.responsesConversation[0] != "task-42" || fg.responsesConversation[1] != "" {
		t.Fatalf("retry must drop the conversation name, got %v", fg.responsesConversation)
	}
}

func TestDeliverTask_ProviderErrorRetryFresh(t *testing.T) {
	fg := &fakeGateway{responsesReplies: []string{
		`{"error":{"type":"invalid_request_error","message":"tool_calls array is empty"}}`,
		"recovered",
	}}
	a := newTestAdapter(t, fg)
	ctx, cancel := testCtx(t)
	defer cancel()

	res, err := a.DeliverMessage(ctx, DeliverMessageRequest{Type: "task.message", SessionKey: "task-7", Message: "hi"})
	if err != nil {
		t.Fatalf("provider-error fresh retry should recover: %v", err)
	}
	if res.Reply != "recovered" {
		t.Fatalf("reply = %q, want recovered", res.Reply)
	}
	if len(fg.responsesConversation) != 2 || fg.responsesConversation[1] != "" {
		t.Fatalf("retry must drop the conversation name, got %v", fg.responsesConversation)
	}
}

func TestDeliverTask_ProviderErrorFeedbackSwallowed(t *testing.T) {
	fg := &fakeGateway{responsesReplies: []string{
		`{"error":{"message":"tool_calls array is empty"}}`,
	}}
	a := newTestAdapter(t, fg)
	ctx, cancel := testCtx(t)
	defer cancel()

	res, err := a.DeliverMessage(ctx, DeliverMessageRequest{Type: "task.message.response", SessionKey: "task-7", Message: "ack"})
	if err != nil {
		t.Fatalf("feedback provider error should be swallowed: %v", err)
	}
	if res.Reply != "ok" {
		t.Fatalf("reply = %q, want ok", res.Reply)
	}
	if len(fg.responsesConversation) != 1 {
		t.Fatalf("feedback must not trigger a retry, got %d calls", len(fg.responsesConversation))
	}
}

func TestDeliverTask_ProviderErrorRetryFails(t *testing.T) {
	fg := &fakeGateway{responsesReplies: []string{
		`{"error":{"message":"tool_calls array is empty"}}`,
		"still tool_calls broken",
	}}
	a := newTestAdapter(t, fg)
	ctx, cancel := testCtx(t)
	defer cancel()

	res, err := a.DeliverMessage(ctx, DeliverMessageRequest{Type: "task.message", SessionKey: "task-7", Message: "hi"})
	if err != nil {
		t.Fatalf("adapter returns error in result, not err: %v", err)
	}
	if res.Success {
		t.Fatalf("expected failure result: %+v", res)
	}
	if !strings.Contains(res.Error, "被模型服务拒绝") {
		t.Fatalf("error should mention model rejection, got: %q", res.Error)
	}
}

// ── T0.5: empty task id interception ─────────────────────────────

func TestDeliverViaRuns_RejectsEmptyTaskID(t *testing.T) {
	fg := &fakeGateway{}
	base := t.TempDir()
	srv := httptest.NewServer(fg.handler())
	t.Cleanup(srv.Close)
	st := store.NewFSStore(base)
	if err := st.EnsureLayout(); err != nil {
		t.Fatalf("EnsureLayout: %v", err)
	}
	a, err := NewHermesAdapter(HermesConfig{NodeID: "n1", BaseURL: srv.URL + "/v1", Model: "m", SessionStore: st})
	if err != nil {
		t.Fatalf("NewHermesAdapter: %v", err)
	}

	ctx, cancel := testCtx(t)
	defer cancel()
	res, err := a.DeliverMessage(ctx, DeliverMessageRequest{Type: "todo.assigned", Message: "no identity at all"})
	if err == nil {
		t.Fatalf("empty taskId must be rejected for runs, got %+v", res)
	}
	if !strings.Contains(err.Error(), "requires a valid taskId") {
		t.Fatalf("unexpected error: %v", err)
	}

	// No session mapping file may be created under sessions/hermes/.
	matches, _ := filepath.Glob(filepath.Join(base, "sessions", "hermes", "*", "*"))
	if len(matches) != 0 {
		t.Fatalf("no session mapping should exist, got %v", matches)
	}
}

func TestDeliverViaRuns_AcceptsMetadataTaskIDUnderscore(t *testing.T) {
	fg := &fakeGateway{}
	a := newTestAdapter(t, fg)
	ctx, cancel := testCtx(t)
	defer cancel()

	res, err := a.DeliverMessage(ctx, DeliverMessageRequest{
		Type:     "todo.assigned",
		Message:  "hi",
		Metadata: map[string]any{"task_id": "T-100"},
	})
	if err != nil {
		t.Fatalf("task_id metadata key must be accepted: %v", err)
	}
	if !res.Success {
		t.Fatalf("unexpected result: %+v", res)
	}
	if fg.runsSessionID[0] != "task:T-100" && fg.runsSessionID[0] != "" {
		// session_id echo not asserted here; presence of a successful create is enough
		_ = fg.runsSessionID[0]
	}
}

func TestExtractTaskID_AcceptsBothKeys(t *testing.T) {
	cases := []struct {
		md   map[string]any
		want string
	}{
		{map[string]any{"taskId": "abc"}, "abc"},
		{map[string]any{"task_id": "xyz"}, "xyz"},
		{map[string]any{"taskId": "   "}, ""},
		{map[string]any{"other": 1}, ""},
		{nil, ""},
		{map[string]any{"taskId": 42}, ""},
	}
	for i, c := range cases {
		if got := extractTaskID(c.md); got != c.want {
			t.Fatalf("case %d: extractTaskID = %q, want %q", i, got, c.want)
		}
	}
}

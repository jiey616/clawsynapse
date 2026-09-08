package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

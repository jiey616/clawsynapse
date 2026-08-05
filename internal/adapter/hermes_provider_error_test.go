package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"clawsynapse/internal/store"
)

func TestIsModelProviderError(t *testing.T) {
	cases := map[string]bool{
		"HTTP 400: Invalid 'messages[75].tool_calls': empty array. Expected an array with minimum length 1, but got an empty array instead.": true,
		"openai.BadRequestError: Error code: 400 - {'error': {'message': 'Invalid ...'}}": true,
		"Error code: 400 - {'type': 'invalid_request_error'}":                            true,
		"WARNING agent.conversation_loop: API call failed (attempt 1/3)":                 true,
		"Non-retryable client error: Error code: 400":                                    true,
		"正常回复内容，没有任何问题": false,
		"this is a normal assistant reply": false,
		"":                            false,
	}
	for text, want := range cases {
		if got := isModelProviderError(text); got != want {
			t.Errorf("isModelProviderError(%q) = %v, want %v", text, got, want)
		}
	}
}

// providerErrorGateway mocks a gateway whose first /v1/responses call returns a
// hermes-digested model provider error (empty tool_calls 400), and subsequent
// calls succeed. It records the previous_response_id of each call.
type providerErrorGateway struct {
	calls       int
	prevIDs     []string
	errFirst    bool
	firstReply  string
	secondReply string
}

func (fg *providerErrorGateway) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		var req responsesRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		fg.calls++
		fg.prevIDs = append(fg.prevIDs, req.PreviousResponseID)

		id := fmt.Sprintf("resp-%d", fg.calls)
		reply := fg.secondReply
		// The provider error fires on the SECOND gateway call (a session with
		// history); the retry (third call) must succeed.
		if fg.errFirst && fg.calls == 2 {
			reply = fg.firstReply
		}
		if reply == "" {
			reply = "ok"
		}
		_ = json.NewEncoder(w).Encode(responsesResponse{
			ID:         id,
			Status:     "completed",
			OutputText: reply,
		})
	})
	return mux
}

func newProviderErrorAdapter(t *testing.T, fg *providerErrorGateway) *HermesAdapter {
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
		t.Fatalf("NewHermesAdapter: %v", err)
	}
	return a
}

func TestDeliverViaResponsesRetriesFreshOnProviderError(t *testing.T) {
	fg := &providerErrorGateway{
		errFirst:    true,
		firstReply:  "HTTP 400: Invalid 'messages[75].tool_calls': empty array. Expected an array with minimum length 1, but got an empty array instead.",
		secondReply: "正常回复",
	}
	a := newProviderErrorAdapter(t, fg)

	// First delivery establishes a session.
	req := DeliverMessageRequest{Type: "chat.message", Message: "hi", SessionKey: "s1"}
	r1, err := a.DeliverMessage(context.Background(), req)
	if err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if !r1.Success {
		t.Fatalf("first delivery not ok: %+v", r1)
	}

	// Second delivery: session has history, gateway returns the provider
	// error → adapter must clear the mapping and retry fresh, succeeding.
	r2, err := a.DeliverMessage(context.Background(), req)
	if err != nil {
		t.Fatalf("second delivery: %v", err)
	}
	if !r2.Success {
		t.Fatalf("second delivery should recover, got %+v", r2)
	}
	if r2.Reply != "正常回复" {
		t.Fatalf("reply = %q, want 正常回复", r2.Reply)
	}

	// Calls: 1st ok (prev empty), 2nd error (prev resp-1), 3rd retry fresh (prev must be empty).
	if len(fg.prevIDs) < 3 {
		t.Fatalf("expected 3 gateway calls, got %d: %v", len(fg.prevIDs), fg.prevIDs)
	}
	if fg.prevIDs[2] != "" {
		t.Errorf("retry call kept stale previous_response_id %q, want empty (fresh session)", fg.prevIDs[2])
	}
}

func TestDeliverViaResponsesDegradesWhenRetryStillFails(t *testing.T) {
	fg := &providerErrorGateway{
		errFirst:    true,
		firstReply:  "HTTP 400: Invalid 'messages[1].tool_calls': empty array",
		secondReply: "HTTP 400: Invalid 'messages[1].tool_calls': empty array", // retry also fails
	}
	a := newProviderErrorAdapter(t, fg)

	req := DeliverMessageRequest{Type: "chat.message", Message: "hi", SessionKey: "s2"}
	_, _ = a.DeliverMessage(context.Background(), req) // seed session

	r, err := a.DeliverMessage(context.Background(), req)
	if err != nil {
		t.Fatalf("delivery: %v", err)
	}
	if r.Success {
		t.Fatalf("expected failure, got success: %+v", r)
	}
	if strings.Contains(r.Error, "HTTP 400") || strings.Contains(r.Error, "tool_calls") {
		t.Fatalf("raw provider error leaked to user: %q", r.Error)
	}
}

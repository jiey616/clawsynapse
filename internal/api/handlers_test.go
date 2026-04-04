package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"clawsynapse/internal/adapter"
	"clawsynapse/internal/discovery"
	"clawsynapse/pkg/types"
)

type stubAgentAdapter struct {
	status *adapter.AgentStatus
	err    error
}

func (s stubAgentAdapter) DeliverMessage(_ context.Context, _ adapter.DeliverMessageRequest) (*adapter.DeliverMessageResult, error) {
	return nil, nil
}

func (s stubAgentAdapter) GetStatus(_ context.Context) (*adapter.AgentStatus, error) {
	return s.status, s.err
}

func TestHandleHealthIncludesAdapterStatus(t *testing.T) {
	peers := discovery.NewRegistry()
	peers.Upsert(types.Peer{NodeID: "n1-localnodeid0000000000000000000000"})
	peers.Upsert(types.Peer{NodeID: "n1-remotepeer0000000000000000000000"})

	srv := &Server{
		peers:       peers,
		adapter:     stubAgentAdapter{status: &adapter.AgentStatus{Healthy: true}},
		adapterName: "openclaw",
		self: SelfInfo{
			NodeID:              "n1-localnodeid0000000000000000000000",
			DID:                 "did:key:z6MkexampleLocalDid",
			IdentityFingerprint: "sha256:1234abcd5678ef90",
			TrustMode:           "tofu",
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	rec := httptest.NewRecorder()
	srv.handleHealth(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var result types.APIResult
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	adapterData, ok := result.Data["adapter"].(map[string]any)
	if !ok {
		t.Fatalf("expected adapter data map, got %#v", result.Data["adapter"])
	}
	selfData, ok := result.Data["self"].(map[string]any)
	if !ok {
		t.Fatalf("expected self data map, got %#v", result.Data["self"])
	}
	if selfData["nodeId"] != "n1-localnodeid0000000000000000000000" {
		t.Fatalf("expected self nodeId, got %#v", selfData["nodeId"])
	}
	if selfData["did"] != "did:key:z6MkexampleLocalDid" {
		t.Fatalf("expected self did, got %#v", selfData["did"])
	}
	if selfData["identityFingerprint"] != "sha256:1234abcd5678ef90" {
		t.Fatalf("expected self identityFingerprint, got %#v", selfData["identityFingerprint"])
	}
	if selfData["trustMode"] != "tofu" {
		t.Fatalf("expected self trustMode tofu, got %#v", selfData["trustMode"])
	}
	if result.Data["peersCount"] != float64(1) {
		t.Fatalf("expected peersCount 1, got %#v", result.Data["peersCount"])
	}
	if adapterData["name"] != "openclaw" {
		t.Fatalf("expected adapter name openclaw, got %#v", adapterData["name"])
	}
	if adapterData["healthy"] != true {
		t.Fatalf("expected adapter healthy true, got %#v", adapterData["healthy"])
	}
	if _, exists := adapterData["error"]; exists {
		t.Fatalf("expected no adapter error, got %#v", adapterData["error"])
	}
}

func TestHandlePeersExcludesSelfNode(t *testing.T) {
	peers := discovery.NewRegistry()
	peers.Upsert(types.Peer{NodeID: "n1-localnodeid0000000000000000000000"})
	peers.Upsert(types.Peer{NodeID: "n1-remotepeer0000000000000000000000", AuthStatus: types.AuthAuthenticated, TrustStatus: types.TrustTrusted})

	srv := &Server{
		peers: peers,
		self: SelfInfo{
			NodeID: "n1-localnodeid0000000000000000000000",
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/peers", nil)
	rec := httptest.NewRecorder()
	srv.handlePeers(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var result types.APIResult
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	items, ok := result.Data["items"].([]any)
	if !ok {
		t.Fatalf("expected items slice, got %#v", result.Data["items"])
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 peer, got %#v", result.Data["items"])
	}
	peer, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("expected peer item map, got %#v", items[0])
	}
	if peer["nodeId"] != "n1-remotepeer0000000000000000000000" {
		t.Fatalf("expected remote peer only, got %#v", peer["nodeId"])
	}
}

func TestHandleHealthIncludesAdapterError(t *testing.T) {
	srv := &Server{
		peers:       discovery.NewRegistry(),
		adapter:     stubAgentAdapter{status: &adapter.AgentStatus{Healthy: false}, err: errors.New("openclaw unavailable")},
		adapterName: "openclaw",
		self: SelfInfo{
			NodeID:              "n1-localnodeid0000000000000000000000",
			DID:                 "did:key:z6MkexampleLocalDid",
			IdentityFingerprint: "sha256:1234abcd5678ef90",
			TrustMode:           "explicit",
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	rec := httptest.NewRecorder()
	srv.handleHealth(rec, req)

	var result types.APIResult
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	adapterData, ok := result.Data["adapter"].(map[string]any)
	if !ok {
		t.Fatalf("expected adapter data map, got %#v", result.Data["adapter"])
	}
	if adapterData["healthy"] != false {
		t.Fatalf("expected adapter healthy false, got %#v", adapterData["healthy"])
	}
	if adapterData["error"] != "openclaw unavailable" {
		t.Fatalf("expected adapter error, got %#v", adapterData["error"])
	}
}

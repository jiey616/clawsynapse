package adapter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWebhookAdapterRequiresURL(t *testing.T) {
	_, err := NewWebhookAdapter(WebhookConfig{NodeID: "n1"})
	if err == nil {
		t.Fatal("expected error for empty url")
	}
}

func TestWebhookDeliverMessageSuccess(t *testing.T) {
	var received webhookPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Fatalf("expected application/json, got %s", ct)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &received); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	a, err := NewWebhookAdapter(WebhookConfig{NodeID: "node-1", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	result, err := a.DeliverMessage(context.Background(), DeliverMessageRequest{
		Type:       "chat",
		AgentID:    "agent-42",
		From:       "peer-a",
		SessionKey: "sess-1",
		Message:    "hello world",
		Metadata:   map[string]any{"key": "val"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success, got error: %s", result.Error)
	}
	if !result.Accepted {
		t.Fatal("expected accepted")
	}
	if result.Reply != `{"ok":true}` {
		t.Fatalf("unexpected reply: %s", result.Reply)
	}

	if received.NodeID != "node-1" {
		t.Fatalf("expected nodeId node-1, got %s", received.NodeID)
	}
	if received.From != "peer-a" {
		t.Fatalf("expected from peer-a, got %s", received.From)
	}
	if received.Message != "hello world" {
		t.Fatalf("expected message 'hello world', got %s", received.Message)
	}
	if received.Type != "chat" {
		t.Fatalf("expected type chat, got %s", received.Type)
	}
	if received.AgentID != "agent-42" {
		t.Fatalf("expected agentId agent-42, got %s", received.AgentID)
	}
	if received.SessionKey != "sess-1" {
		t.Fatalf("expected sessionKey sess-1, got %s", received.SessionKey)
	}
}

func TestWebhookDeliverMessageServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	a, err := NewWebhookAdapter(WebhookConfig{NodeID: "n1", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	result, err := a.DeliverMessage(context.Background(), DeliverMessageRequest{
		Message: "test",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Success {
		t.Fatal("expected failure for 500 response")
	}
}

func TestWebhookGetStatusHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("expected GET, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a, err := NewWebhookAdapter(WebhookConfig{NodeID: "n1", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	status, err := a.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !status.Healthy {
		t.Fatal("expected healthy")
	}
}

func TestWebhookGetStatusUnhealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	a, err := NewWebhookAdapter(WebhookConfig{NodeID: "n1", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	status, err := a.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.Healthy {
		t.Fatal("expected unhealthy for 503")
	}
}

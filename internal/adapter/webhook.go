package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
)

type WebhookConfig struct {
	NodeID string
	URL    string
	Logger *slog.Logger
}

type WebhookAdapter struct {
	nodeID string
	url    string
	log    *slog.Logger
	client *http.Client
}

func NewWebhookAdapter(cfg WebhookConfig) (*WebhookAdapter, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("webhook url is required")
	}
	return &WebhookAdapter{
		nodeID: cfg.NodeID,
		url:    cfg.URL,
		log:    cfg.Logger,
		client: &http.Client{},
	}, nil
}

type webhookPayload struct {
	NodeID     string         `json:"nodeId"`
	Type       string         `json:"type"`
	AgentID    string         `json:"agentId,omitempty"`
	From       string         `json:"from"`
	SessionKey string         `json:"sessionKey,omitempty"`
	Message    string         `json:"message"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

func (a *WebhookAdapter) DeliverMessage(ctx context.Context, req DeliverMessageRequest) (*DeliverMessageResult, error) {
	payload := webhookPayload{
		NodeID:     a.nodeID,
		Type:       req.Type,
		AgentID:    req.AgentID,
		From:       req.From,
		SessionKey: req.SessionKey,
		Message:    req.Message,
		Metadata:   req.Metadata,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal webhook payload: %w", err)
	}

	if a.log != nil {
		a.log.Info("sending webhook",
			slog.String("url", a.url),
			slog.String("from", req.From),
			slog.String("type", req.Type),
		)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create webhook request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("webhook request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return &DeliverMessageResult{
			Success:  true,
			Accepted: true,
			Reply:    string(respBody),
		}, nil
	}

	return &DeliverMessageResult{
		Success: false,
		Error:   fmt.Sprintf("webhook returned status %d: %s", resp.StatusCode, string(respBody)),
	}, nil
}

func (a *WebhookAdapter) GetStatus(ctx context.Context) (*AgentStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.url, nil)
	if err != nil {
		return &AgentStatus{Healthy: false}, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return &AgentStatus{Healthy: false}, err
	}
	resp.Body.Close()
	return &AgentStatus{Healthy: resp.StatusCode < 500}, nil
}

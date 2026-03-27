package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"clawsynapse/internal/protocol"
	"clawsynapse/pkg/types"
)

type stubDashboardClient struct {
	results map[string]types.APIResult
	err     error
}

func (s stubDashboardClient) Get(_ context.Context, endpoint string) (types.APIResult, error) {
	if s.err != nil {
		return types.APIResult{}, s.err
	}
	return s.results[endpoint], nil
}

type stubLogProvider struct {
	text string
	err  error
}

func (s stubLogProvider) ReadLogs(_ context.Context, _ int) (string, error) {
	return s.text, s.err
}

func TestLoadDashboardSnapshotDecodesHealthPeersAndMessages(t *testing.T) {
	client := stubDashboardClient{
		results: map[string]types.APIResult{
			"/v1/health": {
				OK: true,
				Data: map[string]any{
					"peersCount": 2,
					"nats": map[string]any{
						"connected": true,
						"status":    "CONNECTED",
						"inMsgs":    10,
						"outMsgs":   5,
					},
				},
			},
			"/v1/peers": {
				OK: true,
				Data: map[string]any{
					"items": []map[string]any{
						{
							"nodeId":      "node-beta",
							"authStatus":  "authenticated",
							"trustStatus": "trusted",
						},
					},
				},
			},
			"/v1/messages": {
				OK: true,
				Data: map[string]any{
					"items": []map[string]any{
						{
							"id":      "msg-1",
							"type":    "chat.message",
							"from":    "node-beta",
							"to":      "node-alpha",
							"content": "hello",
							"ts":      123,
						},
					},
				},
			},
		},
	}

	snapshot, err := loadDashboardSnapshot(context.Background(), client, stubLogProvider{text: "line-a\nline-b"})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Health.PeersCount != 2 {
		t.Fatalf("expected peers count 2, got %d", snapshot.Health.PeersCount)
	}
	if !snapshot.Health.NATS.Connected {
		t.Fatal("expected nats connected")
	}
	if len(snapshot.Peers) != 1 || snapshot.Peers[0].NodeID != "node-beta" {
		t.Fatalf("unexpected peers: %#v", snapshot.Peers)
	}
	if len(snapshot.Messages) != 1 || snapshot.Messages[0].Content != "hello" {
		t.Fatalf("unexpected messages: %#v", snapshot.Messages)
	}
	if snapshot.Logs != "line-a\nline-b" {
		t.Fatalf("unexpected logs: %q", snapshot.Logs)
	}
}

func TestDashboardViewRendersStructuredPanels(t *testing.T) {
	model := dashboardModel{
		apiAddr:     "127.0.0.1:18080",
		width:       120,
		height:      32,
		activeTab:   1,
		lastUpdated: time.Unix(0, 0),
		snapshot: dashboardSnapshot{
			Health: dashboardHealth{
				PeersCount: 1,
				NATS: dashboardNATS{
					Connected: true,
					Status:    "CONNECTED",
					InMsgs:    10,
					OutMsgs:   5,
				},
			},
			Peers: []types.Peer{
				{
					NodeID:       "node-beta",
					AuthStatus:   "authenticated",
					TrustStatus:  "trusted",
					AgentProduct: "openclaw",
					Version:      "v0.0.3",
				},
			},
		},
	}

	view := fmt.Sprint(model.View())
	for _, needle := range []string{
		"ClawSynapse Dashboard",
		"Peer Detail",
		"node-beta",
		"Keys: q quit",
	} {
		if !strings.Contains(view, needle) {
			t.Fatalf("expected view to contain %q, got:\n%s", needle, view)
		}
	}
}

func TestMessageListLinesFitPanelWidth(t *testing.T) {
	model := dashboardModel{
		snapshot: dashboardSnapshot{
			Messages: []protocol.MessageEnvelope{
				{
					Type:    "conversation.message",
					From:    "node-with-a-very-long-name",
					To:      "trustmesh-dev",
					Content: `{"conversation_id":"abcd","summary":"long preview body for the dashboard list"}`,
					Ts:      1711111111111,
				},
			},
		},
	}

	lines := model.messageListLines(6, 54)
	for _, line := range lines {
		if got := visibleLen(parseTaggedOnly(line)); got > 54 {
			t.Fatalf("line exceeds width: got %d want <= 54, line=%q", got, line)
		}
	}
}

func TestFormatMessageRowCollapsesEmbeddedNewlinesInPreview(t *testing.T) {
	cols := messageColumnsForWidth(54)
	row := formatMessageRow(" ", protocol.MessageEnvelope{
		Type:    "conversation.message",
		From:    "node-jack",
		To:      "trustmesh-dev",
		Content: "line one\n\nline two\tline three",
		Ts:      1711111111111,
	}, cols)

	if strings.Contains(row, "\n") {
		t.Fatalf("expected single-line preview, got %q", row)
	}
	if !strings.Contains(row, "line one li") {
		t.Fatalf("expected normalized preview prefix, got %q", row)
	}
	if strings.Contains(row, "line one\n") {
		t.Fatalf("expected newlines to be removed from preview, got %q", row)
	}
}

func TestRenderPanelWrapsWideCharactersWithoutCorruption(t *testing.T) {
	panel := renderPanel("Message Detail", 36, 8, []string{
		"这是一个包含中文和 ASCII JSON 的长消息内容，用来验证换行不会打断字符边界。",
	})

	if strings.Contains(panel, "\ufffd") {
		t.Fatalf("panel contains replacement rune, got:\n%s", panel)
	}
}

func TestRenderPanelKeepsLayoutForMultilineContent(t *testing.T) {
	panel := renderPanel("Message Detail", 32, 8, []string{
		taggedLine(dashboardTagSection, arrowR+" Content"),
		"  first line\n\n  second line\n  third line",
	})

	if got := countLines(panel); got != 8 {
		t.Fatalf("expected panel height 8, got %d\n%s", got, panel)
	}
	lines := strings.Split(panel, "\n")
	for i, line := range lines[1 : len(lines)-1] {
		plain := stripANSI(line)
		if !strings.HasPrefix(plain, boxV) || !strings.HasSuffix(plain, boxV) {
			t.Fatalf("content line %d lost panel border: %q", i+1, plain)
		}
	}
}

func parseTaggedOnly(line string) string {
	_, text := parseTaggedLine(line)
	return text
}

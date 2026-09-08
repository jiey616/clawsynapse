package adapter

import (
	"testing"
	"time"
)

// ── T1.4 acceptance: Idempotency-Key on runs + responses paths ──────

func newIdemTestAdapter(t *testing.T, fg *fakeGateway) *HermesAdapter {
	t.Helper()
	a := newTestAdapter(t, fg)
	a.pollInterval = 10 * time.Millisecond
	a.pollIntervalMax = 20 * time.Millisecond
	return a
}

// TestDeliverViaRuns_IdempotencyKey：runs 路径首次 create 携带消息 ID 键。
func TestDeliverViaRuns_IdempotencyKey(t *testing.T) {
	fg := &fakeGateway{}
	a := newIdemTestAdapter(t, fg)

	if _, err := a.DeliverMessage(t.Context(), DeliverMessageRequest{
		Type:       "todo.assigned",
		SessionKey: "task-idem",
		Message:    "do it",
		MessageID:  "msg-1",
	}); err != nil {
		t.Fatalf("DeliverMessage: %v", err)
	}

	got := fg.recordedRunsIdem()
	if len(got) != 1 || got[0] != "msg-1" {
		t.Fatalf("runs idem keys = %v, want [msg-1]", got)
	}
}

// TestDeliverTaskResponses_IdempotencyKey：task responses 路径同样携带。
func TestDeliverTaskResponses_IdempotencyKey(t *testing.T) {
	fg := &fakeGateway{}
	a := newIdemTestAdapter(t, fg)

	if _, err := a.DeliverMessage(t.Context(), DeliverMessageRequest{
		Type:       "task.message",
		SessionKey: "task-idem",
		Message:    "hello",
		MessageID:  "msg-2",
	}); err != nil {
		t.Fatalf("DeliverMessage: %v", err)
	}

	got := fg.recordedResponsesIdem()
	if len(got) != 1 || got[0] != "msg-2" {
		t.Fatalf("responses idem keys = %v, want [msg-2]", got)
	}
}

// TestDeliverChatResponses_IdempotencyKey：chat responses 路径同样携带。
func TestDeliverChatResponses_IdempotencyKey(t *testing.T) {
	fg := &fakeGateway{}
	a := newIdemTestAdapter(t, fg)

	if _, err := a.DeliverMessage(t.Context(), DeliverMessageRequest{
		Type:       "chat.message",
		SessionKey: "cs-1",
		Message:    "hello",
		MessageID:  "msg-3",
	}); err != nil {
		t.Fatalf("DeliverMessage: %v", err)
	}

	got := fg.recordedResponsesIdem()
	if len(got) != 1 || got[0] != "msg-3" {
		t.Fatalf("responses idem keys = %v, want [msg-3]", got)
	}
}

// TestDeliverViaRuns_UnknownSessionRetryKeySuffix：同一次投递内的重试
// 使用 -r1 递增后缀，绝不与首次键冲突。
func TestDeliverViaRuns_UnknownSessionRetryKeySuffix(t *testing.T) {
	fg := &fakeGateway{unknownRuns: true}
	a := newIdemTestAdapter(t, fg)
	// Pre-seed a stale continuation id so the first create carries a
	// non-empty session_id — otherwise the gateway mock never fires the
	// unknown-session 404 (fresh adapter + single delivery ⇒ prevID == "").
	a.saveMappedSession("task:task-idem", "sess-1")

	if _, err := a.DeliverMessage(t.Context(), DeliverMessageRequest{
		Type:       "todo.assigned",
		SessionKey: "task-idem",
		Message:    "do it",
		MessageID:  "msg-4",
	}); err != nil {
		t.Fatalf("DeliverMessage: %v", err)
	}

	got := fg.recordedRunsIdem()
	if len(got) != 2 || got[0] != "msg-4" || got[1] != "msg-4-r1" {
		t.Fatalf("runs idem keys = %v, want [msg-4 msg-4-r1]", got)
	}
}

// TestDeliverViaRuns_409DegradesWithoutHeader：409（同键异 payload）→
// 去掉 header 重试一次并成功。
func TestDeliverViaRuns_409DegradesWithoutHeader(t *testing.T) {
	fg := &fakeGateway{conflictRuns: true}
	a := newIdemTestAdapter(t, fg)

	if _, err := a.DeliverMessage(t.Context(), DeliverMessageRequest{
		Type:       "todo.assigned",
		SessionKey: "task-idem",
		Message:    "do it",
		MessageID:  "msg-5",
	}); err != nil {
		t.Fatalf("DeliverMessage after 409 degrade: %v", err)
	}

	got := fg.recordedRunsIdem()
	if len(got) != 2 || got[0] != "msg-5" || got[1] != "" {
		t.Fatalf("runs idem keys = %v, want [msg-5 <no-header>]", got)
	}
}

// TestDeliver_NoMessageIDNoHeader：无 messageID 的旧调用方不携带 header，
// 行为与 T1.4 之前一致。
func TestDeliver_NoMessageIDNoHeader(t *testing.T) {
	fg := &fakeGateway{}
	a := newIdemTestAdapter(t, fg)

	if _, err := a.DeliverMessage(t.Context(), DeliverMessageRequest{
		Type:       "todo.assigned",
		SessionKey: "task-idem",
		Message:    "do it",
	}); err != nil {
		t.Fatalf("DeliverMessage: %v", err)
	}

	got := fg.recordedRunsIdem()
	if len(got) != 1 || got[0] != "" {
		t.Fatalf("runs idem keys = %v, want [<no-header>]", got)
	}
}

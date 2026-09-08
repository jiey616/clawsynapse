package messaging

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"clawsynapse/internal/adapter"
)

// fakeCancellerAdapter implements AgentAdapter + RunCanceller for handler
// dispatch tests.
type fakeCancellerAdapter struct {
	deliverCalls atomic.Int32
	cancelCalls  atomic.Int32
	steerCalls   atomic.Int32
	sleep        time.Duration // DeliverMessage simulated work
	activeRunID  string
	lastCancel   string
	lastSteer    string
	steerErr     error
}

func (f *fakeCancellerAdapter) DeliverMessage(ctx context.Context, req adapter.DeliverMessageRequest) (*adapter.DeliverMessageResult, error) {
	f.deliverCalls.Add(1)
	if f.sleep > 0 {
		select {
		case <-time.After(f.sleep):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &adapter.DeliverMessageResult{Success: true, Accepted: true, Reply: "delivered"}, nil
}

func (f *fakeCancellerAdapter) GetStatus(ctx context.Context) (*adapter.AgentStatus, error) {
	return &adapter.AgentStatus{Healthy: true}, nil
}

func (f *fakeCancellerAdapter) CancelActive(ctx context.Context, taskKey string) error {
	f.cancelCalls.Add(1)
	f.lastCancel = taskKey
	return nil
}

func (f *fakeCancellerAdapter) SteerActive(ctx context.Context, taskKey string, input string) error {
	f.steerCalls.Add(1)
	f.lastSteer = taskKey
	return f.steerErr
}

func (f *fakeCancellerAdapter) ActiveRunID(taskKey string) string {
	return f.activeRunID
}

// TestHandleMessage_CancelStatusChanged 是 T1.3 验收①的 handler 侧：
// todo.status_changed{canceled} 必须在 silentNotifyReply 之前被分流到
// CancelActive，且不产生 DeliverMessage。
func TestHandleMessage_CancelStatusChanged(t *testing.T) {
	fake := &fakeCancellerAdapter{}
	h := NewAdapterMessageHandler(fake, time.Minute)

	res, err := h.HandleMessage(IncomingMessage{
		MessageID:  "m1",
		Type:       "todo.status_changed",
		SessionKey: "task-1",
		Message:    `{"task_id":"t1","todo_id":"td1","status":"canceled","cause":"user_cancel","reason":"不要做了"}`,
	})
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if res.Reply != "ACK todo.status_changed - canceled" {
		t.Fatalf("reply = %q, want silent cancel ACK", res.Reply)
	}
	if got := fake.cancelCalls.Load(); got != 1 {
		t.Fatalf("cancel calls = %d, want 1", got)
	}
	if fake.lastCancel != "task-1" {
		t.Fatalf("cancel key = %q, want task-1", fake.lastCancel)
	}
	if got := fake.deliverCalls.Load(); got != 0 {
		t.Fatalf("deliver calls = %d, want 0", got)
	}
}

// TestHandleMessage_StatusChangedNonCancelFallsToSilentACK：非取消语义的
// status_changed 保持既有静默 ACK 行为，不触发 CancelActive。
func TestHandleMessage_StatusChangedNonCancelFallsToSilentACK(t *testing.T) {
	fake := &fakeCancellerAdapter{}
	h := NewAdapterMessageHandler(fake, time.Minute)

	res, err := h.HandleMessage(IncomingMessage{
		MessageID:  "m1",
		Type:       "todo.status_changed",
		SessionKey: "task-1",
		Message:    `{"task_id":"t1","status":"running"}`,
	})
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if !strings.HasPrefix(res.Reply, "ACK todo.status_changed") {
		t.Fatalf("reply = %q, want silent ACK prefix", res.Reply)
	}
	if got := fake.cancelCalls.Load(); got != 0 {
		t.Fatalf("cancel calls = %d, want 0", got)
	}
	if got := fake.deliverCalls.Load(); got != 0 {
		t.Fatalf("deliver calls = %d, want 0", got)
	}
}

// TestHandleMessage_RemindSteersInFlight：todo.remind 且有在途 run →
// steer 注入，不新建投递（验收③）。
func TestHandleMessage_RemindSteersInFlight(t *testing.T) {
	fake := &fakeCancellerAdapter{activeRunID: "run-9"}
	h := NewAdapterMessageHandler(fake, time.Minute)

	res, err := h.HandleMessage(IncomingMessage{
		MessageID:  "m1",
		Type:       "todo.remind",
		SessionKey: "task-1",
		Message:    "催办：请加快进度",
	})
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if res.Reply != "ACK todo.remind - steered" {
		t.Fatalf("reply = %q, want steered ACK", res.Reply)
	}
	if got := fake.steerCalls.Load(); got != 1 {
		t.Fatalf("steer calls = %d, want 1", got)
	}
	if fake.lastSteer != "task-1" {
		t.Fatalf("steer key = %q, want task-1", fake.lastSteer)
	}
	if got := fake.deliverCalls.Load(); got != 0 {
		t.Fatalf("deliver calls = %d, want 0 (no duplicate delivery)", got)
	}
}

// TestHandleMessage_RemindNoInFlightFallsThrough：无在途 run 的 remind
// 落回正常投递路径（验收③后半）。
func TestHandleMessage_RemindNoInFlightFallsThrough(t *testing.T) {
	fake := &fakeCancellerAdapter{}
	h := NewAdapterMessageHandler(fake, time.Minute)

	res, err := h.HandleMessage(IncomingMessage{
		MessageID:  "m1",
		Type:       "todo.remind",
		SessionKey: "task-1",
		Message:    "催办：请加快进度",
	})
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if res.Reply != "delivered" {
		t.Fatalf("reply = %q, want normal delivery result", res.Reply)
	}
	if got := fake.deliverCalls.Load(); got != 1 {
		t.Fatalf("deliver calls = %d, want 1", got)
	}
	if got := fake.steerCalls.Load(); got != 0 {
		t.Fatalf("steer calls = %d, want 0", got)
	}
}

// TestHandleMessage_TaskRunTimeoutSplit：todo.* 使用 taskRunTimeout，
// 其他类型仍受通用 timeout 约束（T1.3 超时拆分）。
func TestHandleMessage_TaskRunTimeoutSplit(t *testing.T) {
	fake := &fakeCancellerAdapter{sleep: 200 * time.Millisecond}
	h := NewAdapterMessageHandler(fake, 50*time.Millisecond, WithTaskRunTimeout(2*time.Second))

	if _, err := h.HandleMessage(IncomingMessage{
		MessageID:  "m1",
		Type:       "todo.assigned",
		SessionKey: "task-1",
		Message:    "do it",
	}); err != nil {
		t.Fatalf("todo delivery killed by generic timeout: %v", err)
	}

	if _, err := h.HandleMessage(IncomingMessage{
		MessageID:  "m2",
		Type:       "chat.message",
		SessionKey: "cs-1",
		Message:    "hello",
	}); err == nil {
		t.Fatalf("chat delivery should hit the 50ms generic timeout")
	}
}

// TestHandleMessage_PlainAdapterNoDispatch：未实现 RunCanceller 的适配器
// 不受分流影响（todo.status_changed 照旧静默 ACK）。
func TestHandleMessage_PlainAdapterNoDispatch(t *testing.T) {
	fake := &fakeCancellerAdapter{}
	h := NewAdapterMessageHandler(fake, time.Minute)

	res, err := h.HandleMessage(IncomingMessage{
		MessageID:  "m1",
		Type:       "task.status_changed",
		SessionKey: "task-1",
		Message:    `{"status":"running"}`,
	})
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if !strings.HasPrefix(res.Reply, "ACK task.status_changed") {
		t.Fatalf("reply = %q, want silent ACK", res.Reply)
	}
	if got := fake.deliverCalls.Load(); got != 0 {
		t.Fatalf("deliver calls = %d, want 0", got)
	}
}

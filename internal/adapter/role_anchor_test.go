package adapter

import (
	"strings"
	"testing"
)

// TestRoleAnchorText：锚点内容契约（角色回显 + 四条执行纪律）。
func TestRoleAnchorText(t *testing.T) {
	withRole := roleAnchorText("executor")
	if !strings.Contains(withRole, "节点执行角色：executor") {
		t.Fatalf("anchor missing role line: %q", withRole)
	}
	for _, want := range []string{"压缩裁剪", "重新读取技能文件", "推进到下一步", "输出契约", "todo.ask"} {
		if !strings.Contains(withRole, want) {
			t.Fatalf("anchor missing %q", want)
		}
	}

	noRole := roleAnchorText("  ")
	if strings.Contains(noRole, "节点执行角色") {
		t.Fatal("empty role must not render the role line")
	}
}

// TestDeliverViaRuns_RoleAnchor：runs create 携带锚点；开关关闭时不发送。
func TestDeliverViaRuns_RoleAnchor(t *testing.T) {
	fg := &fakeGateway{}
	a := newIdemTestAdapter(t, fg)
	defer a.Close()
	a.roleAnchor = true
	a.agentRole = "executor"

	if _, err := a.DeliverMessage(t.Context(), DeliverMessageRequest{
		Type:       "todo.assigned",
		SessionKey: "task-anchor",
		Message:    "do it",
	}); err != nil {
		t.Fatalf("DeliverMessage: %v", err)
	}
	if len(fg.runsInstructions) != 1 || !strings.Contains(fg.runsInstructions[0], "节点执行角色：executor") {
		t.Fatalf("runs instructions = %v, want anchored", fg.runsInstructions)
	}

	// Disabled: empty string means the JSON field is omitted (omitempty).
	fg2 := &fakeGateway{}
	a2 := newIdemTestAdapter(t, fg2)
	defer a2.Close()
	if _, err := a2.DeliverMessage(t.Context(), DeliverMessageRequest{
		Type:       "todo.assigned",
		SessionKey: "task-anchor-2",
		Message:    "do it",
	}); err != nil {
		t.Fatalf("DeliverMessage: %v", err)
	}
	if len(fg2.runsInstructions) != 1 || fg2.runsInstructions[0] != "" {
		t.Fatalf("disabled anchor must send no instructions, got %v", fg2.runsInstructions)
	}
}

// TestDeliverTaskResponses_RoleAnchor：task responses 首呼同样携带锚点。
func TestDeliverTaskResponses_RoleAnchor(t *testing.T) {
	fg := &fakeGateway{}
	a := newIdemTestAdapter(t, fg)
	defer a.Close()
	a.roleAnchor = true
	a.agentRole = "pm"

	if _, err := a.DeliverMessage(t.Context(), DeliverMessageRequest{
		Type:       "task.assigned",
		SessionKey: "task-anchor-3",
		Message:    "hello",
	}); err != nil {
		t.Fatalf("DeliverMessage: %v", err)
	}
	if len(fg.responsesInstructions) != 1 || !strings.Contains(fg.responsesInstructions[0], "节点执行角色：pm") {
		t.Fatalf("responses instructions = %v, want anchored", fg.responsesInstructions)
	}
}

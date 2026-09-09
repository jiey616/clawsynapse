package adapter

import "strings"

// roleAnchorText renders the per-request "instructions" anchor injected on
// every gateway create (Phase 3.2). Context compression can prune loaded
// skill content mid-conversation, which historically stranded agents on
// step 1 of multi-step pipelines; this anchor is ephemeral (not part of
// the stored session history) so it survives pruning on every turn.
//
// It is deliberately advisory, not overriding: the live-verified gateway
// passes instructions through to the model, but the agent's SOUL persona
// still takes precedence -- the anchor reinforces execution discipline
// without trying to redefine identity.
func roleAnchorText(role string) string {
	role = strings.TrimSpace(role)
	var b strings.Builder
	b.WriteString("[角色锚点 · 每轮系统提醒 | 本条为临时指令，不属于会话历史]\n")
	if role != "" {
		b.WriteString("节点执行角色：" + role + "。\n")
	}
	b.WriteString(`1) 本会话绑定的业务技能（SKILL.md 及其 references）是任务执行的唯一流程依据。对话上下文可能被压缩裁剪：若你对技能流程步骤的记忆不完整，必须先重新读取技能文件再行动，禁止凭残缺记忆输出。
2) 每轮开始先判定当前处于技能流程的第几步；完成当前步骤后必须立即推进到下一步，禁止停留在已完成步骤上重复输出，或只给计划不调用工具。
3) 最终产出必须符合技能规定的输出契约（文件路径、格式、必含字段）；中间产物按技能要求落盘。
4) 与平台的协议交互（todo.assigned/progress/complete/fail/comment、todo.ask 确认）以收到的任务消息为准，不得自行发明消息类型或跳过确认环节。`)
	return b.String()
}

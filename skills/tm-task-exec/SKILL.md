---
name: tm-task-exec
description: >
  执行 Agent 专用 skill：接收 Todo 任务，执行工作，回报进度和结果。
  收到 todo.assigned 后，通过 ClawSynapse 向 TrustMesh 回传
  todo.progress / todo.complete / todo.fail / task.comment。
compatibility: Requires clawsynapse CLI
metadata:
  author: TrustMesh
  version: "2.5"
allowed-tools:
  - "Bash(clawsynapse:*)"
---

# TrustMesh 执行 Agent 任务执行 Skill

你是执行 Agent。你的职责是：接收分派的 Todo，执行具体工作，向 TrustMesh 回报进度和结果。

## 一、工作流

```
收到 todo.assigned
  │
  1. 阅读 Todo 的 title 和 description，理解要做什么
  2. 一开始处理就先发送 1 条 task.comment，作为开工记录
  3. 开始执行，发送 todo.progress 报告进度
  4. 执行过程中，把 task.comment 当作默认工作日志持续发送
  5. 在关键里程碑发送 todo.progress
  6. 成功前先发总结 comment，再发送 todo.complete（附带结果）
  7. 失败前先发总结 comment，再发送 todo.fail（附带错误原因）
```

### 关键规则

1. **收到 todo.assigned 才开始工作。** 不要主动寻找任务。
2. **及时回报进度。** 在关键里程碑发送 `todo.progress`，让 PM 和用户了解执行状态。
2.1. **`task.comment` 是默认工作日志，不是可选补充。** 把它当作草稿和工作笔记来发，不需要等整理完成，也不需要润色。
2.2. **开工即发 comment。** 收到 `todo.assigned` 并开始处理后，先发 1 条 `task.comment`，说明你准备检查什么、先做什么。
2.3. **每个明显步骤后继续发 comment。** 读完代码、执行命令、完成一段修改、做出关键判断、发现风险、遇到阻塞后，都应补 1 条 `task.comment`。
2.4. **拿不准要不要发时，默认发。** 宁可多发简短 comment，也不要长时间沉默。
2.5. **`todo.progress` 负责里程碑，`task.comment` 负责过程。** `todo.progress` 用于状态推进；`task.comment` 用于记录观察、动作、决定、问题和下一步。
3. **结果要具体。** `todo.complete` 的 result 应包含有意义的 summary 和 output；如果有文件需要交付，通过 `clawsynapse transfer send --metadata taskId=... todoId=...` 上传。
4. **失败要说明原因。** `todo.fail` 的 error 应清晰描述失败原因，帮助诊断。
5. **所有回报都走 ClawSynapse。** 不要在聊天界面直接输出结果。

## 二、incoming 消息格式

消息通过 ClawSynapse 到达，带有 header：

```text
[clawsynapse from=<senderNodeId> to=<yourNodeId> session=<sessionKey>]
<message body>
```

- `from=` 是 TrustMesh 节点，**这是你所有回复的 target**
- `to=` 是你自己的 node ID，**永远不要用作 target**
- `session=` 用作 `--session-key`

### todo.assigned payload

```json
{
  "task_id": "task_123",
  "todo_id": "todo_2",
  "title": "实现后端登录接口",
  "description": "完成邮箱密码登录 API",
  "task_context": {
    "title": "用户认证模块重构",
    "description": "将现有 session 认证迁移到 JWT 方案",
    "todos": [
      { "todo_id": "todo_1", "order": 1, "title": "设计认证流程", "status": "done", "assignee_name": "Architect" },
      { "todo_id": "todo_2", "order": 2, "title": "实现后端登录接口", "status": "pending", "assignee_name": "CodeAgent", "is_current": true },
      { "todo_id": "todo_3", "order": 3, "title": "编写集成测试", "status": "pending", "assignee_name": "Tester" }
    ]
  },
  "prior_results": [
    {
      "todo_id": "todo_1",
      "title": "设计认证流程",
      "summary": "采用 RS256 + refresh token，access token 15min 过期",
      "output": "详细设计内容..."
    }
  ]
}
```

**字段说明：**
- `task_context`：任务全局上下文。仅在你**首次参与此任务**时包含；如果你之前已完成过该任务的其他 Todo（同一 session），此字段省略（因为你的会话中已有这些信息）。
- `prior_results`：前序 Todo 的执行结果。首次参与时包含所有前序结果；再次参与时仅包含**其他 Agent** 完成的结果（你自己做过的结果已在会话中）。

### task.context.query（按需拉取任务上下文）

如果执行过程中需要了解任务最新状态（如其他 Todo 的最新结果、新增的交付物），可以主动查询：

```bash
payload="$(jq -nc --arg task_id "$TASK_ID" '{task_id: $task_id}')"

clawsynapse publish \
  --target "$TARGET_NODE" \
  --type task.context.query \
  --session-key "$SESSION_KEY" \
  --message "$payload"
```

TrustMesh 会回复 `task.context.result`，包含完整的任务快照（task_context + 所有已完成 Todo 的结果）。其中每个结果对象的 `artifacts[]` 字段即为该前序 Todo 产出的文件，每项含 `file_name` / `mime_type` / `download_url`——用 `curl` 下载这些 `download_url` 即可拿到前序 Todo 的实际产物文件（与 `attached_files` 里的用户上传文件区分开）。`todo.assigned` 里的 `prior_results[]` 同样携带 `artifacts[]`，无需额外查询即可直接取用。

### 用户上传附件文件（attached_files，source=user_upload）

`attached_files` **只包含用户上传/引用的项目文件（任务的输入材料），不包含前序 Todo 的产物**。每个条目 `source` 恒为 `"user_upload"`：

```json
{
  "task_id": "task_123",
  "attached_files": [
    {
      "id": "pf_abc123",
      "file_name": "需求文档.pdf",
      "file_size": 204800,
      "mime_type": "application/pdf",
      "source": "user_upload",
      "download_url": "http://192.168.1.100:8080/api/v1/files/agent/pf_abc123?token=eyJhbGci..."
    }
  ]
}
```

**⚠️ 区分两类文件（重要）：**

| 文件来源 | 所在字段 | source 值 | 含义 |
|---|---|---|---|
| 用户上传 | `attached_files[]` | `user_upload` | 任务输入材料，你应参考/使用 |
| 前序 Todo 产物 | `prior_results[].artifacts[]`（以及 `task.context.query` 返回的 `all_results[].artifacts[]`） | （无 source 字段，字段名即表明身份） | 其他 Todo 执行产出的**文件结果**，你应下载并作为前序交付物衔接 |

**不要**把 `attached_files` 当成前序产物；**不要**在 `attached_files` 里找前序 Todo 的输出。前序产物走下方独立通道。

**如何获取文件内容：**

`download_url` 是临时有效的 HTTP 下载链接（有效期约 10 分钟）。使用 `curl` 下载：

```bash
curl -s "<download_url>" -o /tmp/task-file.pdf
```

对于文本文件，可以直接读入变量：

```bash
FILE_CONTENT="$(curl -s "<download_url>")"
```

**重要规则：**
- `download_url` 有时效性，收到 `todo.assigned` 后应**立即下载**所有附件
- 下载失败（过期或网络问题）时，仍应基于文件名和任务描述继续工作，在 `task.comment` 中说明"附件文件 <文件名> 下载失败：<原因>"
- 多个文件时，按顺序逐个下载
- 下载完成后，在 `task.comment` 中说明已读取的文件列表和简要内容概要

### todo.status_changed payload（状态通知）

```json
{
  "task_id": "task_123",
  "todo_id": "todo_1",
  "status": "in_progress",
  "actor_node_id": "node-dev-001",
  "cause": "todo.progress",
  "version": 7,
  "message": "接口已完成参数校验，开始接入 JWT"
}
```

## 三、发送消息

### 核心规则

1. **CRITICAL — 永远不要发给自己。** `--target` 必须是 incoming header 中 `from` 的值（TrustMesh 节点）。
2. 使用 `clawsynapse publish` 发送所有消息。
3. `--session-key` 使用 incoming header 中的 `session` 值。
4. payload 用 `jq -nc` 构建，避免手动转义。

### todo.progress — 报告进度

在执行过程中的关键节点发送。首次发送时，TrustMesh 会自动将 Todo 状态从 `pending` 推进为 `in_progress`。

```bash
# TARGET_NODE = incoming header 中 from 的值（TrustMesh 节点）
# ⚠️ 绝对不能是你自己的 node ID
TARGET_NODE="trustmesh-server"  # ← 替换为实际 from 值
SESSION_KEY="task_123"         # ← 替换为 incoming header 中 session 的值

payload="$(jq -nc \
  --arg task_id "task_123" \
  --arg todo_id "todo_1" \
  --arg message "接口已完成参数校验，开始接入 JWT" \
  '{
    task_id: $task_id,
    todo_id: $todo_id,
    message: $message
  }')"

clawsynapse publish \
  --target "$TARGET_NODE" \
  --type todo.progress \
  --session-key "$SESSION_KEY" \
  --message "$payload"
```

### todo.complete — 报告完成

Todo 执行成功时发送。`todo.complete` 只包含文本结果，不包含文件引用。文件交付通过独立的 `transfer send` 完成（见下文）。

```bash
TARGET_NODE="trustmesh-server"  # ← 替换为实际 from 值
SESSION_KEY="task_123"         # ← 替换为 incoming header 中 session 的值

payload="$(jq -nc \
  --arg task_id "task_123" \
  --arg todo_id "todo_1" \
  --arg summary "登录接口已完成" \
  --arg output "实现了注册、登录、JWT 校验" \
  '{
    task_id: $task_id,
    todo_id: $todo_id,
    result: {
      summary: $summary,
      output: $output
    }
  }')"

clawsynapse publish \
  --target "$TARGET_NODE" \
  --type todo.complete \
  --session-key "$SESSION_KEY" \
  --message "$payload"
```

result 字段说明：
- `summary`：一句话总结完成了什么
- `output`：详细描述执行结果
- `metadata`：执行元数据（可选），如使用的模型、耗时等

### 文件交付

文件交付与 `todo.complete` 完全解耦。只需用 `clawsynapse transfer send` 并通过 `--metadata` 关联 task 和 todo，TrustMesh 会自动接收文件并创建交付物。

**你可以在任何时候发送文件**——执行过程中、完成前、完成后、甚至任务结束后修订文件时。

**触发条件**：任何需要交付给用户的文件都必须上传，包括但不限于：
- 生成的代码文件
- 报告、文档（PDF、Markdown 等）
- 配置文件
- 截图、日志
- 导出的数据文件

**规则**：

- `--target` 使用 incoming header 中 `from` 的值（TrustMesh 节点）
- `--metadata` 必须包含 `taskId`，`todoId` 可选但推荐
- 同一 `transferId` 重复发送会覆盖旧文件（可用于修订）
- **如果文件上传失败，在 `todo.complete` 中说明，或发送 `todo.fail`**

示例：

```bash
TARGET_NODE="trustmesh-server"  # ← 替换为实际 from 值
TASK_ID="task_123"
TODO_ID="todo_1"

# 上传文件，通过 --metadata 关联到 task 和 todo
clawsynapse transfer send \
  --target "$TARGET_NODE" \
  --file /tmp/login-api-report.pdf \
  --mime-type application/pdf \
  --metadata "taskId=$TASK_ID" \
  --metadata "todoId=$TODO_ID"
```

多个文件时，对每个文件分别执行一次 `transfer send`，每次都带上 `--metadata taskId=...`。

**推荐流程**：先上传文件，再发送 `todo.complete`。但顺序不是强制的——文件可以在任何时候发送。

### todo.fail — 报告失败

Todo 执行失败时发送。

```bash
TARGET_NODE="trustmesh-server"  # ← 替换为实际 from 值
SESSION_KEY="task_123"         # ← 替换为 incoming header 中 session 的值

payload="$(jq -nc \
  --arg task_id "task_123" \
  --arg todo_id "todo_1" \
  --arg error "Google OAuth 凭证缺失，无法完成 OAuth 接入" \
  '{
    task_id: $task_id,
    todo_id: $todo_id,
    error: $error
  }')"

clawsynapse publish \
  --target "$TARGET_NODE" \
  --type todo.fail \
  --session-key "$SESSION_KEY" \
  --message "$payload"
```

### task.comment — 发送评论

把 `task.comment` 当作默认工作日志来发送。它不是“整理好的说明文”，而是执行过程中的草稿、观察和笔记。短句也可以，不需要等全部想清楚才发。评论不影响 Todo 状态，但它是任务可观测性的主要来源。

**发送原则**

- 开工就发，不要等到第一个里程碑再发
- 每个明显步骤都发 1 条：分析、命令、修改、实验、判断、阻塞、回退、下一步
- 如果不确定这条信息值不值得发，默认发
- 可以简短，不要求完整，不要求润色
- `todo.progress` 说“到哪个阶段了”，`task.comment` 说“刚刚做了什么、为什么这么做、接下来做什么”

**必发时机**

- 收到 `todo.assigned` 并开始处理时
- 阅读相关代码、文档、接口后，形成第一轮判断时
- 每次执行关键命令、完成关键修改、完成一次验证后
- 方案变化、做出关键决策、发现风险时
- 遇到阻塞、需要假设前进、等待外部条件时
- 发送 `todo.complete` 或 `todo.fail` 之前

**建议频率**

- 短任务：至少 2 条 comment（开工 1 条，结束前总结 1 条）
- 中等任务：至少 4 条 comment（开工、分析、执行、结束前总结）
- 长任务：每 5 到 10 分钟至少 1 条，或每个明显步骤 1 条

**推荐内容结构**

- 刚检查了什么
- 观察到了什么
- 决定怎么做
- 刚执行了什么动作
- 结果如何
- 下一步是什么

**可直接套用的简短模板**

- `已开始处理，先检查 <模块/文件>，确认现状后再决定改法。`
- `检查了 <文件/接口>，发现 <现象>，判断应优先复用 <现有实现>。`
- `刚执行了 <命令/操作>，结果是 <结果>，接下来处理 <下一步>。`
- `发现风险：<风险>。当前决定先按 <方案> 推进，并补充验证。`
- `遇到阻塞：<问题>。当前假设是 <假设>，下一步用 <方法> 验证。`
- `结束前记录：已完成 <内容>，剩余关注点是 <风险/边界>。`

**推荐做法：先定义 helper，后续反复调用**

```bash
TARGET_NODE="trustmesh-server"  # ← 替换为实际 from 值
SESSION_KEY="task_123"          # ← 替换为 incoming header 中 session 的值
TASK_ID="task_123"
TODO_ID="todo_1"

send_comment() {
  local content="$1"
  local payload
  payload="$(jq -nc \
    --arg task_id "$TASK_ID" \
    --arg todo_id "$TODO_ID" \
    --arg content "$content" \
    '{
      task_id: $task_id,
      todo_id: $todo_id,
      content: $content
    }')"

  clawsynapse publish \
    --target "$TARGET_NODE" \
    --type task.comment \
    --session-key "$SESSION_KEY" \
    --message "$payload"
}
```

示例：

```bash
send_comment "已开始处理，先检查 auth 模块和登录相关 handler。"
send_comment "检查了现有 auth 模块，发现已有 JWT 签发逻辑，决定复用而不是重写。"
send_comment "刚完成登录 handler 的参数校验，接下来接入 JWT 签发并补错误处理。"
```

```bash
TARGET_NODE="trustmesh-server"  # ← 替换为实际 from 值
SESSION_KEY="task_123"         # ← 替换为 incoming header 中 session 的值

payload="$(jq -nc \
  --arg task_id "task_123" \
  --arg todo_id "todo_1" \
  --arg content "分析了现有的 auth 模块，发现已有 JWT 签发逻辑，决定复用而非重写。接下来实现登录 handler。" \
  '{
    task_id: $task_id,
    todo_id: $todo_id,
    content: $content
  }')"

clawsynapse publish \
  --target "$TARGET_NODE" \
  --type task.comment \
  --session-key "$SESSION_KEY" \
  --message "$payload"
```

字段说明：
- `task_id`：必填，关联的任务 ID
- `todo_id`：可选，关联到具体的 Todo
- `content`：必填，评论内容（支持多行文本）

### 获取结构化发送结果

```bash
clawsynapse --json publish \
  --target "$TARGET_NODE" \
  --type todo.progress \
  --session-key "$SESSION_KEY" \
  --message "$payload"
```

## 四、Guardrails

- **永远不要用你自己的 node ID 作为 `--target`。** target 是 TrustMesh 节点（incoming `from`）。
- **不要发送 `conversation.reply`、`task.create`。** 这些是 PM Agent 的消息类型，不是执行 Agent 的。你只能发送 `todo.progress`、`todo.complete`、`todo.fail`、`task.comment`、`task.context.query` 五种消息类型。
- **Todo 终态不可逆。** 已经 `done` 或 `failed` 的 Todo 不能再更新，服务端会拒绝（`TODO_ALREADY_DONE` / `TODO_ALREADY_FAILED`）。
- **不要把 `task.comment` 当成可选项。** 对执行 Agent 来说，它是默认工作日志；长时间无 comment 视为过程缺失。
- **不要等整理完再发 comment。** comment 可以是草稿、短句、阶段性判断；过度记录优于缺失记录。
- **不要只发 `todo.progress` 不发 `task.comment`。** 里程碑更新前后，应至少有一条相关 comment 解释上下文。
- 业务内容必须通过 `clawsynapse publish` 或 `clawsynapse transfer send` 发送；不要在聊天界面复述 payload、结果摘要或过程说明。
- 在聊天界面中，除发送确认外不要输出任何业务内容。成功发送后只允许输出一行极简确认：`ACK <message_type>`。
- 如果当前是在等待新的任务消息、上下文结果或外部条件，只输出：`WAITING`。
- 如果发送失败或命令报错，只输出：`ERR <reason>` 或 `ERR publish failed: <code>`。
- 不要输出多余解释，不要粘贴 JSON，不要重复总结任务。
- 不要丢弃 `--session-key`。
- 不要发送协议中未定义的字段。
- payload 较大时用 `jq -nc` 构建。

## 五、常见错误码

| 错误码 | 含义 |
|--------|------|
| `BAD_PAYLOAD` | JSON 结构或必填字段无效 |
| `VALIDATION_ERROR` | 业务参数校验失败 |
| `FORBIDDEN` | 无权执行该操作（可能不是该 Todo 的指派 Agent） |
| `NOT_FOUND` | 目标 Task 或 Todo 不存在 |
| `TODO_FINALIZED` | Todo 已结束，不再接受更新 |
| `TODO_ALREADY_DONE` | Todo 已完成 |
| `TODO_ALREADY_FAILED` | Todo 已失败 |

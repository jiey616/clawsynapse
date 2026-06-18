# ClawSynapse 适配器实现方案与区别

## 1. 架构总览

```
clawsynapse node
├── app.go: newAgentAdapter() ── 根据 agentAdapter 配置项 switch 创建适配器实例
├── messaging/handler.go: AdapterMessageHandler ── 调用适配器的适配层
│   ├── 超时控制 (default 10分钟)
│   ├── 反馈类型过滤 (LLM 适配器丢弃 .response/.error)
│   └── runId 附加到回复
└── messaging/service.go: maybeDeliver() ── 消息路由 + replyToSender
```

所有适配器实现同一接口：

```go
type AgentAdapter interface {
    DeliverMessage(ctx context.Context, req DeliverMessageRequest) (*DeliverMessageResult, error)
    GetStatus(ctx context.Context) (*AgentStatus, error)
}
```

---

## 2. 各适配器详解

### 2.1 DefaultAdapter — 回声/调试适配器

```
chat.message → DefaultAdapter.DeliverMessage → 直接回声消息内容
```

| 特性 | 实现 |
|------|------|
| **类型** | 纯本地，无外部依赖 |
| **命令** | 无 |
| **消息格式** | 原始消息，不做任何包装 |
| **会话管理** | 无 |
| **Session Store** | 不需要 |
| **系统提示词** | 无 |
| **输出解析** | N/A |
| **角色机制** | 无 |
| **配置项** | `agentAdapter: default` |
| **stdin 处理** | N/A |

**实现细节**：生成随机 `runId`，回复原始消息内容。`GetStatus` 始终返回健康。

**适用场景**：调试、测试消息路由、无需 LLM 的节点。

---

### 2.2 OpenClawAdapter — 透传 CLI 适配器

```
chat.message → formatDeliverMessage() 生成协议头 → openclaw agent --message "..." --json --session-id "..." → 解析 JSON 响应
```

| 特性 | 实现 |
|------|------|
| **类型** | CLI 同步阻塞 |
| **命令** | `openclaw agent --message "<prompt>" --json --session-id "<id>" [--agent <agentId>]` |
| **消息格式** | `[clawsynapse type=... from=... to=... session=...]\n<msg>` |
| **会话管理** | SessionKey → 直接作为 CLI 参数（无映射层） |
| **Session Store** | **不需要** — session 无需持久化映射 |
| **系统提示词** | 无 — 消息原样传给 openclaw |
| **输出解析** | 单 JSON 对象 `{runId, status, result.payloads[0].text, error}` |
| **角色机制** | `--agent <AgentID>` 透传给 openclaw CLI |
| **配置项** | `agentAdapter: openclaw` |
| **stdin 处理** | 默认（无特殊处理） |

**实现细节**：
- 错误处理：检查 `exitErr.Stderr` 提取详细错误信息
- 回复提取：取 `result.payloads[0].text`
- Session 派生规则：`cs-<from>-<nodeID>`（当未提供 SessionKey 时）

**独特之处**：是唯一不需要 Session Store 的 LLM 适配器。Session 完全由 openclaw CLI 自身管理，clawsynapse 只做透传。

---

### 2.3 OpenCodeAdapter — NDJSON 流 + 会话重试

```
chat.message → formatDeliverMessage() 生成协议头 → opencode run "<prompt>" --format json [--session <id>]
              → 解析 NDJSON 流输出 → 提取 sessionID + content/text
              → 未知 session 时自动重试（清除旧映射 → 新建会话）
```

| 特性 | 实现 |
|------|------|
| **类型** | CLI 同步阻塞 + 会话重试 |
| **命令** | `opencode run "<prompt>" --format json [--session <id>]` |
| **消息格式** | `[clawsynapse type=... from=... to=... session=...]\n<msg>` |
| **会话管理** | SessionKey → SessionID 映射（两层 ID 体系，存储在 `sessions/opencode/`） |
| **Session Store** | **需要** — session 映射持久化 |
| **系统提示词** | 无 |
| **输出解析** | NDJSON 行流，每行一个 `{type, content, text, error, sessionID}` 事件 |
| **角色机制** | 无 |
| **配置项** | `agentAdapter: opencode` |
| **stdin 处理** | 默认（无特殊处理） |

**核心会话重试逻辑**：
```
loadMappedSessionID(SessionKey)
    ↓
runCommand(msg, sessionID)
    ↓ (如果失败且错误包含 "unknown session")
deleteMappedSession(SessionKey)
    ↓
runCommand(msg, "")  ← 新建会话重试
    ↓
saveMappedSession(SessionKey, newSessionID)
```

**输出解析策略**：
- 遍历所有 NDJSON 行，取**最后一条**有效事件
- 回复字段优先级：`content > text`
- 自动跳过无法解析的 JSON 行

---

### 2.4 CodexAdapter — NDJSON 流 + 双模式命令

```
chat.message → formatDeliverMessage() 生成协议头 → 
  有 session: codex exec resume --json --skip-git-repo-check <sessionID> -- "<prompt>"
  无 session: codex exec --json --skip-git-repo-check -- "<prompt>"
              → 解析 NDJSON 流 → 提取 thread_id + agent_message
              → 未知 session 时自动重试
```

| 特性 | 实现 |
|------|------|
| **类型** | CLI 同步阻塞 + 双模式（新建/恢复）+ 会话重试 |
| **命令（新建）** | `codex exec --json --skip-git-repo-check -- "<prompt>"` |
| **命令（恢复）** | `codex exec resume --json --skip-git-repo-check <sessionID> -- "<prompt>"` |
| **消息格式** | `[clawsynapse type=... from=... to=... session=...]\n<msg>` |
| **会话管理** | SessionKey → threadID 映射（两层 ID 体系，存储在 `sessions/codex/`） |
| **Session Store** | **需要** — session 映射持久化 |
| **系统提示词** | 无 |
| **输出解析** | NDJSON 流，每行 `{type, thread_id, item{id, type, text}, error}`。回复取 `item.type == "agent_message"` |
| **角色机制** | 无 |
| **配置项** | `agentAdapter: codex` |
| **stdin 处理** | **必须显式置空** `/dev/null` — 否则 codex 会从管道读取 stdin 污染 prompt |

**独特之处**：
- 唯一使用**双模式命令**的适配器（新建 vs 恢复线程）
- 回复提取仅看 `item.type == "agent_message"` 的事件（而非所有文本事件）
- 错误检测更丰富：`turn.failed` 事件、`error` 字段、stderr 均会检查

**stdin 处理原因**：Codex CLI 会检测是否有管道输入，若存在则会读取 stdin 作为额外 prompt 输入，导致指令语义被污染。必须显式将 stdin 设为 `/dev/null`。

---

### 2.5 HermesAdapter — 纯文本输出，无角色绑定

```
chat.message → formatDeliverMessage() 生成协议头
              → hermes chat -q "<prompt>" -t terminal --yolo [--session <id>]
              → 解析纯文本输出 → 完整输出作为回复（不做截断）
              → 未知 session 时自动重试
```

| 特性 | 实现 |
|------|------|
| **类型** | CLI 同步阻塞 + 会话重试 |
| **命令** | `hermes chat -q "<prompt>" -t terminal --yolo [--session <id>]` |
| **消息格式** | `[clawsynapse type=... from=... to=... session=...]\n<msg>`（裸传协议头，与 openclaw/codex 一致） |
| **会话管理** | SessionKey → SessionID 映射（两层 ID 体系，存储在 `sessions/hermes/`） |
| **Session Store** | **需要** — session 映射持久化 |
| **系统提示词** | **无** — 与其他 CLI 适配器一致，裸传协议头 |
| **输出解析** | **纯文本**（非 JSON），完整输出作为回复（不做截断） |
| **角色机制** | **无** — 不通过 `-s` 预设角色技能 |
| **最大轮数** | **无限制** — 不设 `--max-turns`，由 hermes 默认值控制 |
| **配置项** | `agentAdapter: hermes`（无额外配置项） |
| **stdin 处理** | `/dev/null`（与 codex 相同原因） |

**与其他适配器的主要差异**：

1. **纯文本输出** — 不同于其他适配器的结构化 JSON 输出，hermes 输出是自然语言。完整输出作为回复，不做截断。

2. **无角色绑定** — 与 openclaw 不同（通过 `--agent` 切换角色），hermes 适配器不预设角色技能。hermes 的命令参数保持最小化：仅 `-t terminal`（工具集）和 `--yolo`（自动审批）。

3. **无 max-turns 限制** — 不做轮数硬限制，由 hermes CLI 自身的默认值（90）控制，避免 clawsynapse 层面的过度约束。
- 默认为空：裸传协议头，与其他 CLI 适配器行为一致

---

### 2.6 WebhookAdapter — HTTP 异步适配器

```
chat.message → 组装 JSON payload → HTTP POST → 3 层解析:
    1. JSON 字符串 → 作为 reply
    2. 结构化对象 {reply, message, runId, sessionId, error, accepted, success}
    3. 原始文本 → 作为 reply
```

| 特性 | 实现 |
|------|------|
| **类型** | HTTP POST（异步+同步均可） |
| **命令** | `POST <webhookUrl>` with JSON body |
| **消息格式** | **原始 JSON 结构体**（不使用 `formatDeliverMessage` 协议头） |
| **会话管理** | 无 — 由外部服务自行管理 |
| **Session Store** | 不需要 |
| **系统提示词** | 无 |
| **输出解析** | 三级备选：JSON 字符串 → 结构化 webhookResponse → 原始文本 |
| **角色机制** | 完整 payload 透传（包括 AgentID） |
| **配置项** | `agentAdapter: webhook`, `webhookUrl` |
| **stdin 处理** | N/A |

**独特之处**：
- **唯一不遵循 CLI 模式**的适配器 — 使用 HTTP 而非子进程
- **唯一不生成 ClawSynapse 协议头** — 发送原始 `webhookPayload` JSON：
  ```json
  {"nodeId":"...", "type":"chat.message", "from":"...", "sessionKey":"...", "message":"...", "metadata":{...}}
  ```
- **唯一接收反馈消息** — `WithFeedbackDelivery()` 让 handler 不丢弃 `.response`/`.error` 类型
- `GetStatus` 使用 HTTP GET 检查（状态码 < 500 视为健康）
- 响应体限制 4096 字节

---

## 3. 横切对比表

### 3.1 核心架构

| 维度 | default | openclaw | opencode | codex | hermes | webhook |
|------|---------|----------|----------|-------|--------|---------|
| **传输方式** | 内存回声 | CLI 子进程 | CLI 子进程 | CLI 子进程 | CLI 子进程 | HTTP POST |
| **执行模式** | 同步 | 同步阻塞 | 同步阻塞 | 同步阻塞 | 同步阻塞 | 同步 HTTP |
| **消息包装** | 无 | 协议头 | 协议头 | 协议头 | 协议头（可选前缀系统提示词） | 原始 JSON |
| **Session Store** | 不需要 | 不需要 | sessions/opencode/ | sessions/codex/ | sessions/hermes/ | 不需要 |
| **stdin 处理** | N/A | 默认 | 默认 | /dev/null | /dev/null | N/A |
| **外部依赖** | 无 | openclaw CLI | opencode CLI | codex CLI | hermes CLI | HTTP 服务 |

### 3.2 会话管理

| 维度 | openclaw | opencode | codex | hermes |
|------|----------|----------|-------|--------|
| **Session ID 体系** | 单层：SessionKey直接作为参数 | 两层：SessionKey → SessionID 映射 | 两层：SessionKey → threadID 映射 | 两层：SessionKey → SessionID 映射 |
| **未知会话处理** | N/A | 删除映射 + 重试（新会话） | 删除映射 + 重试（新会话） | 删除映射 + 重试（新会话） |
| **SessionKey 为空时** | 自动派生 `cs-<from>-<nodeID>` | 不传 session 参数（新会话） | 不传 session 参数（新会话） | 不传 session 参数（新会话） |
| **持久化方式** | 不持久化（CLI 管理） | FSStore JSON 文件 | FSStore JSON 文件 | FSStore JSON 文件 |

Session 映射的数据结构：

```json
{
    "schemaVersion": 1,
    "adapter": "hermes",
    "sessionKey": "abc123",
    "sessionId": "hermes-session-uuid-xxx",
    "createdAtMs": 1718000000000,
    "updatedAtMs": 1718000000000
}
```

存储路径：`<BaseDir>/sessions/<adapter>/<hash[:2]>/<sha256>.json`

### 3.3 输出解析策略

| 适配器 | 输出格式 | 解析方式 | 回复提取规则 |
|--------|---------|---------|-------------|
| **openclaw** | 单 JSON 对象 | `json.Unmarshal` | `result.payloads[0].text` |
| **opencode** | NDJSON 行流 | 逐行 `json.Unmarshal` | 最后一行有效事件的 `content` > `text` |
| **codex** | NDJSON 行流 | 逐行 `json.Unmarshal` | 最后一行 `item.type == "agent_message"` 的 `item.text` |
| **hermes** | 纯文本 | 原始字符串 | 完整输出（不做截断） |
| **webhook** | JSON | 三级备选 | 字符串 → 结构化 → 原始文本 |

### 3.4 特殊参数/标志

| 适配器 | 特殊标志 | 用途 |
|--------|---------|------|
| **openclaw** | `--agent <id>` | 角色切换（透传） |
| | `--json` | 强制 JSON 输出 |
| **opencode** | `--format json` | NDJSON 输出格式 |
| | `--session <id>` | 恢复已有会话 |
| **codex** | `--skip-git-repo-check` | 跳过 Git 检查（避免交互） |
| | `resume <id>` | 恢复已有线程 |
| | `-- <prompt>` | `--` 分隔符将 flag 与 prompt 分开 |
| **hermes** | `-q` | 非交互查询模式 |
| | `-t terminal` | 终端模式（工具集） |
| | `--yolo` | 自动批准所有操作 |
| | `--session <id>` | 恢复已有会话 |

### 3.5 配置方式

```yaml
# config.example.yaml
agentAdapter: default    # default | openclaw | opencode | codex | webhook | hermes
agentAdapterTimeout: 10m
webhookUrl: ""           # webhook 适配器专用
```

| 环境变量 | CLI flag | 对应配置项 |
|----------|----------|-----------|
| `AGENT_ADAPTER` | `--agent-adapter` | `agentAdapter` |
| `AGENT_ADAPTER_TIMEOUT` | `--agent-adapter-timeout` | `agentAdapterTimeout` |
| `WEBHOOK_URL` | `--webhook-url` | `webhookUrl` |

---

## 4. 消息流对比

### 4.1 LLM CLI 适配器（openclaw / opencode / codex）的通用流程

```
[入站消息: type=chat.message, from=A, sessionKey=K]
    │
    ▼
AdapterMessageHandler.HandleMessage()
    ├── isFeedbackType? → 如果是 .response/.error → 静默丢弃
    ├── context.WithTimeout(10m)
    │
    ▼
DeliverMessage(req)
    ├── formatDeliverMessage(nodeID, req)
    │     → "[clawsynapse type=chat.message from=A to=MyNode session=K]\n你好"
    ├── loadMappedSessionID(sessionKey) → 查找已映射的外部 session
    ├── runCommand(prompt, sessionID)
    │     → 执行外部 CLI（阻塞等待 stdout）
    │     → 如果未知 session 错误 → 删除映射 + 重试（新会话）
    ├── saveMappedSession(sessionKey, newSessionID)
    │
    ▼
返回 DeliverMessageResult{Reply: "..."}
    │
    ▼
Handler 附加 runId → 返回 HandlerResult{Reply: "...(runId=xxx)"}
    │
    ▼
maybeDeliver() → replyToSender(env, reply) → 发布 chat.response 类型回复
```

### 4.2 Hermes 适配器的特殊流程

```
[入站消息: type=chat.message, from=A, sessionKey=K]
    │
    ▼
DeliverMessage(req)
    ├── 组装: formatDeliverMessage(nodeID, req)
    │     → "[clawsynapse type=chat.message ...]\n你好"
    ├── loadMappedSessionID → 查找已有 hermes session
    ├── runCommand(fullPrompt, sessionID)
    │     → hermes chat -q "<prompt>" -t terminal --yolo
    │     → hermes 内部可能执行多轮 tool call
    │     → stdout 捕获最终输出
    ├── parseHermesResult(out) → 完整输出作为回复
    ├── saveMappedSession
    │
    ▼
返回 DeliverMessageResult{Reply: "完整纯文本"}
    │
    ▼
Handler → replyToSender → chat.response（注意：可能与 hermes 自驱的 publish 冲突）
```

**关键差异**：hermes 与其他 CLI 适配器在消息格式和命令构造上完全一致——裸传协议头，不做任何前置注入。

### 4.3 Webhook 适配器的独立流程

```
[入站消息: type=chat.message, from=A, sessionKey=K]
    │
    ▼
AdapterMessageHandler.HandleMessage()
    ├── isFeedbackType? → WithFeedbackDelivery 时不过滤
    │
    ▼
DeliverMessage(req)
    ├── 组装 JSON: {nodeId, type, agentId, from, sessionKey, message, metadata}
    ├── HTTP POST → <webhookUrl>
    ├── 状态码非 2xx → 返回 Error
    ├── 解析响应 JSON
    │     ├── 尝试解析为 JSON 字符串 → Reply
    │     ├── 尝试解析为 webhookResponse 结构体 → Reply/Error/Accepted/RunID/SessionID
    │     └── 兜底：原始文本 → Reply
    │
    ▼
返回结果 → Handler → replyToSender
```

---

## 5. 设计模式总结

### 5.1 共享辅助函数（定义在 openclaw.go 中）

| 函数 | 用途 | 使用者 |
|------|------|--------|
| `formatDeliverMessage()` | 生成 `[clawsynapse ...]` 协议头 | openclaw, opencode, codex, hermes |
| `appendHeaderAttr()` | 追加协议头属性 | 同上 |
| `appendMetadataHeaderAttrs()` | 追加元数据属性 | 同上 |
| `truncateForLog()` | 日志截断（240 字节） | 各适配器日志格式化函数 |

> 注意：**webhook 不使用这些函数**，它发送原始 JSON。

### 5.2 Session 管理模式的演化

```
Level 0 (openclaw):    无 session 映射 — SessionKey 直接作为 CLI 参数
Level 1 (opencode):    引入 SessionKey → SessionID 映射 + FSStore 持久化
Level 2 (codex):      继承 Level 1 + 双模式命令（新建/恢复）
Level 3 (hermes):      继承 Level 1 + 可选系统提示词 + 固定风格参数
```

### 5.3 每个适配器的"唯一的卖点"

| 适配器 | 独有特性 |
|--------|---------|
| **default** | 零依赖回声器，用于死信/测试 |
| **openclaw** | 最简透传，无持久化开销，`--agent` 角色切换 |
| **opencode** | 首个引入 session 映射的适配器，NDJSON 流解析 |
| **codex** | 双模式命令（新建/恢复线程），`agent_message` 过滤 |
| **hermes** | 可选系统提示词，纯文本输出，`-s` 风格参数 |
| **webhook** | HTTP 而非 CLI，三级响应解析，接收反馈消息 |

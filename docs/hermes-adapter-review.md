# Hermes 适配器代码审查报告

> 审查日期：2026-08-31
> 审查对象：`internal/adapter/hermes*.go`（6 个源文件 + 5 个测试文件，约 3400 行）
> 对照基准：Hermes Agent（NousResearch/hermes-agent）官方文档
> 相关文档：`docs/hermes-integration-analysis.md`（既有架构与问题分析，本文为其补充，重点补上**官方 API 契约的权威比对结论**）

---

## 0. 审查方法说明

既有文档 `docs/hermes-integration-analysis.md` 是通过**读代码**得出的分析，因此它的 §3.3「版本兼容性风险」只能指出「源码里有 `NOTE(§7.1)` 标注说续接字段名待验证」，却无法给出答案。

本次审查在代码阅读之外，增加了**对 Hermes Agent 官方文档的联网核对**，因此能够把该文档里标记为「待验证 / 有风险」的部分**定案**。

**结论可信度标注约定**：

- ✅ **【官方确认】** — 有官方文档原文支撑
- ⚠️ **【推断】** — 无官方文档直接支撑，基于代码或间接证据
- ❓ **【未确认】** — 官方文档未覆盖，需实测

---

## 1. 结论摘要

| 编号 | 严重度 | 问题 | 状态 |
|------|--------|------|------|
| P0-1 | 🔴 阻断 | `/v1/runs` 会话续接用错字段，任务多轮上下文实际未续接 | ✅ **实测定案（2026-09-09）：结论反转，`session_id` 才是续接字段，现实现正确**（见下） |
| P0-2 | 🔴 阻断 | run 状态枚举与官方不符，`stopping` 态被误判为终态 | ✅ **已修复** |
| P0-3 | 🔴 阻断 | `/v1/responses` 顶层 `output_text` 不存在 | ✅✅ **实测确认** · 已加注释 |
| P0-4 | 🔴 阻断 | `task.*` 用 `session_id` 续接，会话完全没接上 | ✅✅ **实测确认 · 已修复** |
| P1-1 | 🟠 高 | `docs/adapter-comparison.md` 描述的是**已废弃的 CLI 实现** | ✅ 代码确认 |
| P1-2 | 🟠 高 | `custom_providers` 是遗留格式，新版 hermes 会读空 | ✅ 官方确认 |
| P1-3 | 🟠 高 | 新增 `TodoMode=responses` 路径缺少容错与长任务保护 | ✅ 代码确认 |
| P1-4 | 🟠 高 | 测试 mock 自我印证，无法发现上述契约错误 | ✅ 代码确认 |
| P1-5 | 🟠 高 | **`isFeedbackLike` 是死代码**——hermes 不启用反馈投递 | ✅✅ **实测确认** |
| P2-1 | 🟡 中 | `restartAndReport` 丢弃 ctx，改用 `context.Background()` | ✅ 代码确认 |
| P2-2 | 🟡 中 | `deliverTaskViaResponses` 缺少 unknown-session 重试 | ✅ 代码确认 |
| P2-3 | 🟡 中 | `/v1/runs` 未传 `model`，与 `/v1/responses` 不一致 | ✅ 代码确认 |
| P2-4 | 🟡 中 | 配置文件写入后无 fsync，异常掉电可能丢配置 | ✅ 代码确认 |
| P3-1 | 🟢 低 | 死代码：`sortExecutionsNewestFirst`、`jsonBody`、`boolAny`、`agentRole` 字段 | ✅ 代码确认 |
| P3-2 | 🟢 低 | `filePreview` 后两个 `else if` 分支不可达 | ✅ 代码确认 |

> 图例：✅ 有依据 · ✅✅ 已在真实运行环境实测验证 · 「已修复」指 2026-08-31 的本次修复，详见「9. 修复记录」。

**一句话总结**：适配器的**整体架构是合理的**（路由分层、会话映射、失效重开、配置原子写入、zip 路径穿越防护都做得不错），但**对 Hermes 官方 API 契约的若干关键假设是错的**，其中最严重的一处会导致「任务多轮对话上下文根本没有接上」这一静默功能缺陷——不报错，但结果是错的。

---

## 2. P0：与官方 API 契约不符

### P0-1 🔴 `/v1/runs` 会话续接用错字段 —— 任务多轮上下文断裂

**位置**：`internal/adapter/hermes.go:757-761`

```go
type runCreateRequest struct {
	Input string `json:"input"`
	// Continuation id for task context. Field name TBD (§7.1).
	SessionID string `json:"session_id,omitempty"`
}
```

**官方契约** ✅（来源：`/docs/user-guide/features/api-server`）：

`POST /v1/runs` 的请求字段为 `input`(必填)、`session_id`(可选)、`instructions`、`conversation_history`、`previous_response_id`、`model`/`provider`/`model_options`。

关键点在于**两者的语义完全不同**：

- `previous_response_id` —— **驱动会话续接**
- `session_id` —— 官方定义是「外部 UI 用来关联自己 conversation ID 的**回显标签**」，**不驱动续接**

**影响**：

`deliverViaRuns`（`hermes.go:307`）在后续轮次会把上一轮的 session id 塞进 `session_id` 发出去。按官方语义，这个值只是被回声记录，**hermes 不会据此恢复上一轮上下文**。结果是：

- 每一轮 `todo.*` 任务都在**全新的上下文**里执行
- 表面现象：任务能跑通、不报错，但多轮任务「记不住前面做了什么」
- 这正是既有文档 §3.3 里那句 `NOTE(§7.1): continuation field name ... is to be verified` 悬而未决的疑问——**现在可以定案了**

**修复建议**：

```go
type runCreateRequest struct {
	Input              string `json:"input"`
	PreviousResponseID string `json:"previous_response_id,omitempty"`
	SessionID          string `json:"session_id,omitempty"` // 仅作关联标签，不驱动续接
	Model              string `json:"model,omitempty"`
}
```

并相应调整 `deliverViaRuns` 中续接 id 的存取逻辑。注意：runs 的创建响应里若返回了 response id，需要把它作为下一轮的 `previous_response_id` 持久化，而不是当前的 `session_id`。

> ⚠️ 由于本地无法启动真实 gateway（环境无 Go 工具链、无 hermes 运行时），**建议在真机上用一个两轮任务实测验证**：第一轮让 agent 记住一个记号，第二轮问它记号是什么。修复前后对比即可确认。

#### ✅ 2026-09-09 真机实测定案（T2.4）——结论与本文档上述推断**相反**

在生产容器（`clawsynapse`，hermes gateway v1.0.35）上执行了文档建议的两轮暗号实验：

| 轮次 | 请求字段 | 结果 |
|---|---|---|
| 第一轮 | `POST /v1/runs {"input":"记住暗号是苹果"}` | completed，快照 `session_id` == `run_id`（无独立 response id） |
| 第二轮 A | `previous_response_id: <第一轮 run_id>` | ❌ **新会话**（快照 session_id 变成自己的 run_id），回复"我没有收到过任何暗号设定" |
| 第二轮 B | `session_id: <第一轮 run_id>` | ✅ **续接成功**（快照 session_id 保持第一轮的值），回复"苹果" |

结论：**当前网关上 `/v1/runs` 的续接由 `session_id` 驱动，`previous_response_id` 被静默忽略**。官方文档所描述的"session_id 仅回显标签"语义与 v1.0.35 实测行为不符（文档可能滞后于实现，或实现已变更）。本文档先前"改用 previous_response_id"的修复建议**不采纳**——现实现（沿用 `session_id`）恰是实测唯一可用的续接方式。

处置：`runCreateRequest` 注释更新为实测定案、`NOTE(§7.1)` 已删除；集成测试继续按 `session_id` 断言（`hermes_test.go` "task continuation should carry session_id"）。

---

### P0-2 🔴 run 状态枚举与官方不符 —— `stopping` 被误判为终态 ✅ 已修复

**状态**：已于 2026-08-31 修复（见「9. 修复记录」）。以下保留原始分析以备追溯。

**实测契约** ✅✅（真实 gateway，非文档推断）：

对一个真实 run 完整轮询得到的字段与状态流转：

```jsonc
// POST /v1/runs → {"run_id": "run_3b85...", "status": "started"}   // 创建时不返回 session_id
// GET  /v1/runs/{id} 期间: {"status": "running", "session_id": "<回显我们的输入>", ...}
// GET  /v1/runs/{id} 终态:
{
  "object": "hermes.run", "run_id": "run_3b85...", "status": "completed",
  "created_at": 1788153376.45, "updated_at": 1788153531.08,
  "session_id": "cs-run-probe-task1",   // 仅回显，非续接驱动
  "model": "hermes-agent", "last_event": "run.completed",
  "output": "OK",                        // 输出文本字段，字符串
  "usage": {"input_tokens": 17400, "output_tokens": 3, "total_tokens": 17403}
}
```

确认：`completed` 是终态值 ✅；输出字段是 `output`（字符串）✅ —— 适配器的 `extractRunText` 首选 `Output`，**解析是对的**。

**原问题**：`stopped`/`error` 是无文档也未被实测到的臆测值；调用方的终态判定同样缺 `stopping`。

**已改为**：显式终态集合 + 保守兜底（未列出的状态一律视为进行中），`pollRun` 与三处调用方判定统一走 `isTerminalRunStatus` / `runFailed`。

---

### P0-3 🔴 `/v1/responses` 顶层 `output_text` 字段不存在（实测确认）

**实测契约** ✅✅：真实 gateway 的 `/v1/responses` 响应**顶层字段只有**
`id`、`object`、`model`、`status`、`created_at`、`output`、`usage`。

```jsonc
{
  "id": "resp_...", "object": "response", "model": "hermes-agent",
  "status": "completed", "created_at": ..., "usage": {...},
  "output": [
    {"type": "message", "role": "assistant",
     "content": [{"type": "output_text", "text": "OK"}]}
  ]
}
```

**`"output_text" in response` → `False`**，确认顶层无此字段。

**影响**：`responsesResponse.OutputText` 恒为空，代码永远走 fallback 分支解析
`output[].content[].text` —— **功能正常**，因此这不是功能缺陷，而是类型定义与真实契约不符，
会误导维护者以为该字段是主要来源。

**处理**：字段保留作为未来版本的容错，但已加注释说明实测结果，避免后续误判为主要解析路径。

---

### P0-4 🔴 `task.*` 用 `session_id` 续接 —— 会话完全没接上 ✅ 已修复

**状态**：已于 2026-08-31 修复并实测验证（见「9. 修复记录」）。

**这是本次审查最严重的问题**：不是「可能没生效」，而是**实证完全失效**。

**证据链**（全部来自运行中的真实容器）：

1. `response_store.db` 的 `conversations` 表**为空**，而 `responses` 表已有 **88** 条记录 —— 88 次调用之间**毫无关联**
2. `state.db.sessions` 中，每次调用都生成**新的随机 UUID**（`6fcdf433-…`、`70036d1d-…`、`c0e0e0ef-…`），`session_key` 列**恒为 `None`**
3. 我们传的 `32d35bcd…`（task id）从未出现在任何会话记录里

**平台侧其实是对的**：同一任务的所有消息（`todo.assigned`、`task.mention`、`task.context.result`）
共享同一个 `sessionKey`，且该值就等于 `task_id`。是我们用错了字段名。

**对照实验 —— `conversation` 字段有效**：

```
POST /v1/responses {"conversation":"cs-conv-probe-1", ...} × 2 次
→ conversations 表写入 ('cs-conv-probe-1', 'resp_e3bf...')
→ 两次调用共用 1 个 session，message_count = 4   ← 续接成功
```

而适配器原有调用各自建 session（`message_count` 分别为 10 / 32 / 28 …），互不相关。

**已改为**：`responsesRequest` 用 `conversation` 承载 task id，替换原先的 `session_id`。

---

## 3. P1：文档漂移与配置风险

### P1-1 🟠 `docs/adapter-comparison.md` 描述的是已废弃的 CLI 实现

`docs/adapter-comparison.md:155-184` 对 Hermes 适配器的描述：

```
chat.message → formatDeliverMessage() 生成协议头
              → hermes chat -q "<prompt>" -t terminal --yolo [--session <id>]
              → 解析纯文本输出 → 完整输出作为回复（不做截断）
```

| 文档说法 | 当前实现 |
|---------|---------|
| 传输方式：CLI 子进程 | **HTTP**（Gateway API） |
| 命令：`hermes chat -q ... --yolo` | 不存在，改为 `POST /v1/responses` |
| 输出解析：纯文本 | JSON（`output[].content[].text`） |
| 配置项：`agentAdapter: hermes`（**无额外配置项**） | 现有 5 项：`hermesGatewayUrl`/`hermesGatewayKey`/`hermesModel`/`hermesConfigPath`/`hermesTodoMode` |
| 角色机制：无 | 有 `agentRole`（pm/executor），由 entrypoint 装配技能 |

总览表（`adapter-comparison.md:229/234`）也仍把 hermes 归为「CLI 子进程 / 外部依赖 hermes CLI」。

**影响**：这份文档已是**误导性**的。新维护者照它理解会完全搞错集成方式——比如照着「无额外配置项」去排查网关连不上，根本找不到 `hermesGatewayUrl`。

**修复建议**：重写该文档的 Hermes 章节，与 `docs/hermes-integration-analysis.md` 保持一致，或直接交叉引用后者。

---

### P1-2 🟠 `custom_providers` 是遗留格式，新版 hermes 会读空

**位置**：`internal/adapter/hermes_capability.go:177`

```go
CustomProviders []map[string]any `yaml:"custom_providers"`
```

**官方契约** ✅：`custom_providers` 是**遗留的顶层列表格式**；官方新格式为 `providers:` 字典（config v12），由 `hermes update` 自动迁移。字段映射关系为 `base_url`→`api`、`api_mode`→`transport`、`default_model`。

**影响**：一旦用户的 hermes 升级并完成 config 迁移，模型列表（`fetchModels`）会**静默返回空数组**——能力面板显示「无可用模型」，且不报错。

同时 `applyModelSet`（`hermes_capability_write.go:232`）仍在写 `cfg["custom_providers"]`：

```go
cfg["custom_providers"] = custom
```

在新格式下这个写入会被 hermes 忽略，导致「模型切换显示成功但实际未生效」。

**修复建议**：`fetchModels` / `applyModelSet` 需要同时兼容两种格式——优先读 `providers` 字典，读不到再回退 `custom_providers`；写入时按检测到的格式写回。建议在读取时记录一条 warning 日志标明当前用的是哪种格式，便于现场排查。

---

### P1-3 🟠 新增 `TodoMode=responses` 路径缺少容错与长任务保护

工作区中有**未提交**的改动（`git diff` 可见，涉及 `hermes.go`/`app.go`/`config.go`/`sources.go`/`config.example.yaml`），新增了 `TodoMode` 配置项，允许 `todo.*` 走 `/v1/responses` 而非 `/v1/runs`：

```go
if isRunsMessage(req.Type) && a.todoMode == "runs" {
	return a.deliverViaRuns(ctx, formatted, req)
}
if isRunsMessage(req.Type) || isTaskType(req.Type) {
	return a.deliverTaskViaResponses(ctx, formatted, req)
}
```

这个改动的**意图是好的**（runs 路径存在 P0-1 的续接问题，改用 responses 是合理的绕行方案），但新路径存在三个问题：

1. **没有 unknown-session 重试** —— `deliverViaResponses`（chat 路径）有完整的 `isGatewayUnknownSessionError` → 删除映射 → 重试逻辑；`deliverTaskViaResponses` 只调用一次 `callJSON`，失败就直接返回错误。会话过期时任务会直接失败。

2. **没有 provider error 重试** —— chat 路径在 `isModelProviderError` 时会重开会话重试；task 路径检测到错误后只返回「模型服务暂时不可用」，不重试。

3. **长任务缺少轮询保护** —— `todo.*` 原本走 runs 的轮询模型（适合长任务）。改用 responses 后变成单次 HTTP 请求，受调用方 ctx 超时（默认 10 分钟，见 `resolveAgentAdapterTimeout`）约束。执行时间超限的任务会被直接截断。

4. **`taskSessionKey` 语义变更未同步文档** —— 该改动把 fallback 从 `return "default"` 改成了 `return ""`，注释说明是「避免不同任务共享 default 会话」。这是**正确的修复**（原实现会让所有无 taskId 的消息共享一个会话，互相污染），但 `config.example.yaml` 里 `hermesTodoMode: runs` 的注释未说明这个语义变化。

**建议**：
- 为 `deliverTaskViaResponses` 补齐与 chat 路径同等的重试保护
- 在 `config.example.yaml` 中注明 `responses` 模式不适合长任务
- 这批改动目前**全部未提交**，建议提交前先补测试

---

### P1-4 🟠 测试 mock 自我印证，无法发现契约错误

**位置**：`internal/adapter/hermes_test.go:39-104`

测试用 `httptest` 搭了一个 `fakeGateway`，但这个 mock 是**照着适配器的假设写的**：

```go
_ = json.NewEncoder(w).Encode(responsesResponse{
	ID:         id,
	Status:     "completed",
	OutputText: "hello",        // ← mock 直接返回适配器以为存在的字段
})
```

以及：

```go
_ = json.NewEncoder(w).Encode(runCreateResponse{
	RunID:     "run-1",
	SessionID: "sess-1",        // ← mock 直接回声 session_id，假装续接成功
})
```

**问题**：mock 会把 `session_id` 原样回声、并返回 `OutputText`，于是 P0-1/P0-3 这类**契约错误在测试中完全不可见**——测试验证的是「适配器是否与自己的假设一致」，而不是「适配器是否与真实 gateway 一致」。

现有 40+ 个测试用例覆盖了路由、续接、重试、能力读写、cron 代理等，覆盖率看起来不错，但**对契约偏差的免疫力为零**。

**建议**：
- 引入基于官方 OpenAPI/schema 的契约测试，或录制真实 gateway 的响应样本作为 fixture
- 至少增加一个「golden file」测试：把真实 gateway 的响应 JSON 存为 testdata，断言解析结果
- 在 CI 中跑一次真实 hermes gateway 的冒烟集成测试（既有文档 §7.2 也有同样建议，此处确认其必要性）

---

### P1-5 🟠 `isFeedbackLike` 是死代码 —— hermes 从不接收反馈类消息

**位置**：`internal/adapter/hermes.go` 三处 `isFeedbackLike()` 分支；`internal/app/app.go:127`

```go
// app.go
var handlerOpts []messaging.HandlerOption
if cfg.AgentAdapter == "webhook" {          // ← 只有 webhook
    handlerOpts = append(handlerOpts, messaging.WithFeedbackDelivery())
}
```

```go
// handler.go:80
if !h.acceptFeedback && isFeedbackType(msg.Type) {
    return HandlerResult{}, nil            // ← hermes 在这里就被丢弃了
}
```

`isFeedbackType` = 以 `.response` / `.error` 结尾。hermes 适配器**没有** `acceptFeedback`，
所以这类消息在进入适配器之前就被丢弃，`hermes.go` 里三处 `isFeedbackLike()` 分支
（包括未提交改动中新增的「吞掉确认、避免重复投递」逻辑）**在生产环境永远不会执行**。

**实测佐证** ✅✅：某容器入站消息 108 条，其中反馈类 30 条（`task.response` 22、
`todo.error` 5、`task.error` 2、`chat.response` 1），非反馈类 78 条；
而网关调用恰好 **78** 次（25 `runs-create` + 53 `responses`）——30 条反馈类一条都没进适配器。

> 这也说明那三处「避免重复投递」的防护是**基于一个不成立的前提**写的。若将来确实需要
> 让 hermes 接收反馈消息，必须先在 `app.go` 为 hermes 打开 `WithFeedbackDelivery()`，
> 但这些分支才有意义——而那又可能引发注释里担心的「把回复当新 prompt」循环，需一并评估。

---

## 4. P2：代码层面问题

### P2-1 🟡 `restartAndReport` 丢弃 ctx，改用 `context.Background()`

**位置**：`internal/adapter/hermes_capability_write.go:541`

```go
if err := restart(context.Background()); err != nil {
```

外层 `ApplyCapabilitySet` 是有 ctx 的，但传下去的是 `context.Background()`。后果：

- 调用方超时或取消后，**重启流程仍在继续**（最长 15s 等端口关闭 + 30s 等健康，共 45s）
- 无法被取消，也无法继承调用方的 deadline
- 与 `AGENTS.md` §4「Concurrency and state」中「For long-running loops or subscriptions, support context cancellation」的要求相悖

**建议**：直接传 `ctx`。若担心 ctx 已有较短 deadline，可用 `context.WithoutCancel(ctx)` + 显式 `WithTimeout` 来解耦取消与超时。

---

### P2-2 🟡 `deliverTaskViaResponses` 缺少 unknown-session 重试

见 P1-3 第 1 点。chat 路径（`hermes.go:233-238`）有完整的重试保护，task 路径没有，属于**保护能力不对等**。

---

### P2-3 🟡 `/v1/runs` 未传 `model`，与 `/v1/responses` 不一致

**位置**：`internal/adapter/hermes.go:327`、`379`、`499`

```go
body := runCreateRequest{Input: formatted}   // 没有 Model
```

而 `/v1/responses` 路径是：

```go
body := responsesRequest{Model: a.model, Input: formatted}
```

官方 `/v1/runs` 支持 `model`/`provider`/`model_options` 字段。当前实现下 runs 会使用 gateway 的默认模型，**用户在 clawsynapse 侧配置的 `hermesModel` 对 `todo.*` 消息不生效**。

**建议**：`runCreateRequest` 增加 `Model` 字段并在三处构造点传入 `a.model`。

---

### P2-4 🟡 配置文件写入后无 fsync

**位置**：`internal/adapter/hermes_capability_write.go:400-415`

```go
func (a *HermesAdapter) saveConfigMap(cfg map[string]any) error {
	data, err := yaml.Marshal(cfg)
	// ... validate ...
	tmp := path + ".capability-tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
```

**做得好的地方** ✅：marshal → re-parse 校验 → 写临时文件 → rename，这个「先校验再落盘 + 原子替换」的顺序是对的，能保证坏配置不会写进去。

**不足**：`os.WriteFile` 后没有 `Sync()`，`os.Rename` 前也没有对临时文件 fsync。异常掉电时可能出现「rename 已完成但数据未落盘」，导致 `config.yaml` 损坏或为空——而这恰恰是这段代码想避免的后果。

另外，rename 失败时临时文件 `.capability-tmp` 会残留。

**建议**：写入后 `f.Sync()` 再 close、再 rename；rename 失败时清理临时文件。

---

## 5. P3：代码整洁度

| 位置 | 问题 |
|------|------|
| `hermes_capability_exec.go:286` | `sortExecutionsNewestFirst` 定义后从未被调用（死代码）——✅ 已删除（2026-09-09） |
| `hermes_capability.go:263` | `jsonBody` 定义后从未被调用（死代码）——✅ 已删除（2026-09-09） |
| `capability.go:87` | `boolAny` 定义后从未被调用（死代码）——✅ 已删除（2026-09-09） |
| `hermes.go:50,92` | ~~`agentRole` 字段赋值后从未被读取~~ ——**已过时**：Phase 3.2 角色锚点（`roleAnchorText(a.agentRole)`）已使用该字段，保留 |
| `hermes_capability_exec.go:229-235` | `filePreview` 中第 2 个 `else if` 分支**不可达**：若 `strings.Index(content, "## Response") < 0`，则 `"\n## Response\n"` 必然也找不到——✅ 已删除；第 3 个分支（单井号 `"\n# Response\n"`）经复核**可达**，保留 |
| `adapter.go:23` | `DeliverMessageResult.SessionID` 字段 hermes 适配器从未填充——**保留**：codex/opencode 适配器正常填充，属接口契约而非死代码 |

> 说明：Go 允许未使用的包级函数，所以这些死代码**不影响编译**，但会误导维护者以为这些能力已生效。

---

## 6. 对既有分析文档的补充与更正

`docs/hermes-integration-analysis.md` §3 的问题清单整体是准确的，本次审查补充以下几点：

**更正**：§3.2 称「`kill` 发送 SIGKILL，无法优雅关闭」——不准确。`hermes_capability_write.go:560` 用的是 `exec.Command("kill", pid)`，不带信号参数时默认发送 **SIGTERM(15)**，属于优雅关闭。真实风险是 `pgrep -f "hermes gateway"` 的模糊匹配可能命中无关进程（`pgrep -f` 匹配完整命令行，若其他进程命令行含该字符串会被误杀）。

**确认**：§3.3「版本兼容性风险」中列出的三个不确定点，本次已通过官方文档核实，结论见上文 P0-1 / P0-2 / P0-3——其中 `NOTE(§7.1)` 的续接字段疑问已于 2026-09-09 通过真机实测定案（见 P0-1 附录）：**实测结论为 `session_id` 续接、`previous_response_id` 被忽略，与本节早期"定案为 previous_response_id"的文档推断相反**，代码维持 `session_id` 不变。

**确认**：§3.7 指出 `Capabilities` 在 `capMu` 锁内串行执行 3 个 HTTP 请求（`hermes_capability.go:48-56`）。确实如此，且违反了 `AGENTS.md` §4「Keep lock scopes tight」。建议改用 `sync.RWMutex` + singleflight，或把网络 IO 移出临界区。

**补充**：§3.6 提到 `extractZip` 在 Windows 上的路径分隔符隐患。经核对，`hermes.go:177` 用的是 `filepath.Join(destDir, f.Name)` + `filepath.Clean(destDir)` + `os.PathSeparator`，`filepath.Join` 在 Windows 上会把 `/` 归一化为 `\`，因此**该防护在 Windows 上是有效的**，风险低于该文档的评估。但 `managedSkillsSubdir = "skills/clawsynapse-managed"` 在 Windows 下会生成带 `\` 的路径写入 YAML，属于可移植性小瑕疵。

---

## 7. 建议的修复顺序

**第一批（阻断性，建议立即处理）**

1. **实测确认契约** —— 在能跑真实 hermes gateway 的环境上，抓一次 `POST /v1/runs`、`GET /v1/runs/{id}`、`POST /v1/responses` 的真实请求/响应样本。这是后续所有修复的前提，不要仅凭文档改代码。✅ 2026-09-09 已完成（见 P0-1 附录）
2. ~~修正 P0-1 `/v1/runs` 续接字段为 `previous_response_id`~~ —— **实测推翻**：`session_id` 才是续接字段，现实现正确，无需修改
3. 修正 P0-4 `task.*` 续接改用 `conversation` 或 `previous_response_id`
4. 补齐 P0-2 状态枚举，区分终态/进行中

**第二批（正确性）**

5. 处理 P1-2 `custom_providers` → `providers` 双格式兼容
6. 补齐 P1-3 中 `deliverTaskViaResponses` 的重试保护
7. 修正 P2-3 `/v1/runs` 缺失 `model` 字段

**第三批（文档与工程）**

8. 重写 P1-1 `docs/adapter-comparison.md` Hermes 章节
9. 补充 P1-4 契约测试（golden file / 真实 gateway 冒烟）
10. 提交当前未提交的 `TodoMode` 改动，并补齐其测试
11. 清理 P3 死代码，修正 P2-1 ctx 传递与 P2-4 fsync

---

## 8. 审查局限说明

> **本节记录的是首轮审查（2026-08-31 上午）的局限**。当日后续通过「在 golang 容器内编译 +
> 对接运行中的真实 gateway」补上了实测，局限已基本消除，结论见「9. 修复记录」。

首轮审查未能完成的部分：

- **未执行编译与测试** —— 本机无 Go 工具链（`go: command not found`）
- **未对接真实 gateway** —— P0 系列结论当时仅基于**官方文档**
- **工作区存在未提交改动** —— 且审查过程中 `hermes.go` 还发生过一次变更（818 → 832 行）

后续已解决：

- **编译与测试**：改用 `docker run --rm -v <repo>:/src golang:1.25` 在容器内执行
  `gofmt` / `go vet` / `go test ./...`，全量测试通过
- **真实 gateway**：直接对运行中的容器做 API 探针，拿到了 `/v1/runs` 与
  `/v1/responses` 的完整真实契约（见 P0-2 / P0-3 / P0-4）
- **官方文档自身存在不一致** —— api-server 页与 programmatic-integration 页对 run 状态
  的描述有冲突（`started` vs `running`/`queued`）。**以实测为准**：观察到的是
  `started → running → completed`

---

## 9. 修复记录（2026-08-31）

### 已修复

| 问题 | 改动 |
|------|------|
| P0-4 会话续接失效 | `responsesRequest` 新增 `Conversation` 字段并用于 `deliverTaskViaResponses`，替换原先无效的 `SessionID` |
| P0-2 run 状态枚举 | 新增 `runTerminalStatuses` / `isTerminalRunStatus` / `runFailed`，`pollRun` 与三处调用方判定统一走该集合，未列出的状态一律视为进行中 |
| P0-3 `output_text` | 加注释说明实测结果（顶层无此字段），保留作未来版本容错 |
| 测试缺陷 | 修 `TestDeliverViaResponses_TodoMode`（期望 `task done` 但实际走 responses 返回 `hello`，**原本就是失败的**）；新增 `TestDeliverTaskUsesConversation`、`TestDeliverTaskNoTaskIDOmitsConversation` |

### 实测验证结果

| 验证项 | 结果 |
|--------|------|
| 全量测试 `go test ./...` | 全部通过 |
| `conversation,omitempty` 已编入二进制 | 新产物 1 处命中，8/17 旧产物 0 处 |
| 探针：`conversation` 建立关联 | `conversations` 表写入映射，2 次调用共用 1 个 session（`message_count=4`）|
| 对照：旧 `session_id` | 88 条 response 无任何关联，每次生成新 UUID，`session_key` 恒为 None |
| c3 / c4 部署 | 均 healthy，gateway RUNNING |

### 未修复（需决策）

- **P0-1 `/v1/runs` 续接字段**：本次未改。原因见下。
- **P1-5 `isFeedbackLike` 死代码**：仅记录，未改。需要产品侧先决定 hermes 是否应该接收反馈消息。

### 关于 P0-1 为何未随本次修复

实测发现 `/v1/runs` 的响应**不返回任何可用于续接的 response id**：

```jsonc
POST /v1/runs → {"run_id": "run_3b85...", "status": "started"}          // 无 session_id
GET  /v1/runs/{id} → {..., "session_id": "<回显我们的输入>", "output": "OK"}
```

官方说 `/v1/runs` 的续接字段是 `previous_response_id`，但响应里拿不到 response id；
而 run 的真实会话 id 是 `run_<uuid>`。因此「把 `session_id` 换成 `previous_response_id`」
这个建议在当前实测数据下**无法直接落地**，需要进一步探测（例如把上一轮的 `run_id`
作为 `session_id` 传入是否真的会复用会话）。

**建议**：与其修补 runs 路径，不如把全部容器切到 `TodoMode=responses`（P0-4 已修复，
续接机制已经实测可用），runs 路径退化为遗留分支。目前仍有 3 个容器在 runs 模式：
`clawsynapse`、`clawsynapse-default`、`trustmesh-clawsynapse`（未设 `HERMES_TODO_MODE`）。

### 待办

- 待真实任务流量经过后，确认 `conversations` 表出现以 task id 命名的条目（重启后尚无流量，
  未能端到端验证；现有证据为「单测证明适配器会发 conversation」+「探针证明 hermes 认这个字段」）
- `bin-fresh/`（25MB）建议加入 `.dockerignore`
- `internal/adapter/hermes_provider_error_test.go` 未通过 `gofmt`（**既存问题**，与本次改动无关）
- 本次改动仍未提交；未提交的 TodoMode 改动也仍在工作区，建议一并整理后提交

---

## 附：参考来源

- Hermes Agent 仓库：https://github.com/NousResearch/hermes-agent
- 官方文档站：https://hermes-agent.nousresearch.com/docs/
- API Server 契约：https://hermes-agent.nousresearch.com/docs/user-guide/features/api-server
- CLI 命令参考：https://hermes-agent.nousresearch.com/docs/reference/cli-commands
- 程序化集成：https://hermes-agent.nousresearch.com/docs/developer-guide/programmatic-integration

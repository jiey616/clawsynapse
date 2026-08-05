---
summary: "ClawSynapse 与 TrustMesh 之间的节点能力查询与写回契约（技能 / 模型 / 定时任务）"
title: "节点能力查询与写回契约 (capability)"
---

# 节点能力查询与写回契约（capability）

最后更新：2026-08-03
状态：草案 v2（待 TrustMesh 确认）

## 目录

- [背景与目标](#背景与目标)
- [网络边界（重要前提）](#网络边界重要前提)
- [模块与 Subject](#模块与-subject)
- [公共封套](#公共封套)
- [一、读：能力查询](#一读能力查询)
- [二、写：能力写回](#二写能力写回)
- [三、本地触发端点（TrustMesh 调用）](#三本地触发端点trustmesh-调用)
- [四、横切规则](#四横切规则)
- [五、错误码](#五错误码)
- [六、TrustMesh 侧配合项](#六trustmesh-侧配合项)
- [七、未决 / 二期项](#七未决--二期项)

## 背景与目标

TrustMesh 希望在 Agent 详情页为 hermes 节点展示「产品徽章 + 能力 Tab」，并支持在云端对节点技能、推理模型、定时任务进行**查询与写回**。

本契约定义 ClawSynapse 侧需新增的协议消息、本地 HTTP 端点与写回流程。能力查询/写回均通过 **NATS 网格协议消息**穿透内网，TrustMesh 只调用其旁挂 daemon 的本地 HTTP 端点。

契约覆盖三类能力 CRUD：

| target | 读来源 | 写机制 | 重启 gateway |
|--------|--------|--------|--------------|
| `skill` | gateway `GET /v1/skills` | 改 `config.yaml` `skills.external_dirs`（managed 键）+ 托管目录 | 是 |
| `model` | `config.yaml` `custom_providers`（gateway `/v1/models` 恒返回 1 个 agent 名，列不出 provider） | 改 `config.yaml` `custom_providers` + `model` 默认 | 是 |
| `cron` | gateway `GET /api/jobs` | **直接代理 gateway 原生 `/api/jobs` 端点** | 否 |

> 经 hermes 0.16.0 源码核查：gateway 无 `admin_config_rw` 能力（仅只读 `/v1/skills`、`/v1/models`、`/v1/capabilities`），也无热重载；技能/模型写回必须改 `config.yaml` 并重启 gateway。cron 则是 gateway 原生支持的 CRUD（自带 `/api/jobs` 增删改查、pause/resume/run），因此无需重启、风险更低。

## 网络边界（重要前提）

- **TrustMesh 在云端，ClawSynapse 节点在内网**，云端访问不到节点本地 API。
- 所有跨节点访问走 **NATS 网格协议消息**（`capability.*`）。
- TrustMesh 只能访问其**旁挂 daemon** 的本地 HTTP 端点；该端点内部封装「NATS 请求 → 等响应 → 返回」，对 TrustMesh 屏蔽网格细节。
- 写回涉及「接收远端文件 → 落盘 → 启用 → 重启加载执行」的 RCE 形状能力，授权与隔离要求高于只读查询。

## 模块与 Subject

新增模块 `capability`，遵循 `clawsynapse.<module>.<scope>.<action>` 规范：

| 类别 | Subject | messageType | 说明 |
|------|---------|-------------|------|
| 能力查询 | `clawsynapse.capability.<targetNodeId>.query` | `capability.query` | 查询目标节点能力 |
| 能力查询 | `clawsynapse.capability.<targetNodeId>.response` | `capability.response` | 返回能力清单 |
| 能力写回 | `clawsynapse.capability.<targetNodeId>.set` | `capability.set` | 写回技能/模型/cron |
| 能力写回 | `clawsynapse.capability.<targetNodeId>.set_response` | `capability.set_response` | 写回结果 |

## 公共封套

复用 `docs/protocol.md` 的公共消息封套，关键字段：

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `messageId` | `string` | 是 | 消息唯一 ID |
| `messageType` | `string` | 是 | 见上表 |
| `from` | `string` | 是 | 发起方 nodeId |
| `to` | `string` | 是 | 目标 nodeId |
| `requestId` | `string` | 是 | 关联 query / set 与 response |
| `ts` | `number` | 是 | Unix 毫秒时间戳 |
| `signature` | `string` | 是 | Ed25519 签名（状态变更消息必须） |
| `protocolVersion` | `string` | 否 | 默认 `v1` |

所有 `capability.*` 消息参与签名（`messageType` + `subject` + `from` + `to` + `ts` + `sha256(payload)`），`set` / `set_response` 须带签名，沿用现有签名与重放保护。

## 一、读：能力查询

### 1.1 `capability.query`

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `requestId` | `string` | 是 | 请求关联 ID |
| `nodeId` | `string` | 否 | 目标节点；省略时由 subject 的 `<targetNodeId>` 决定 |

### 1.2 `capability.response`

| 字段 | 类型 | 说明 |
|------|------|------|
| `requestId` | `string` | 对应 query 的 `requestId` |
| `product` | `string` | 节点产品标识，如 `hermes` / `clawsynapse` |
| `available` | `boolean` | 是否可查询（gateway 不可达时为 `false`） |
| `skills` | `object[]` | 来源 gateway `GET /v1/skills`：`{name, description, category}` |
| `models` | `object[]` | 来源 `config.yaml` `custom_providers`：`{id, provider, model, isDefault}`；**`api_key` 不回显** |
| `jobs` | `object[]` | 来源 gateway `GET /api/jobs`：`{id, name, schedule, enabled, prompt, skills?, nextRun?}` |
| `reason` | `string` | `available:false` 时说明原因 |

- 非 hermes 适配器：`available:false`，`skills`/`models`/`jobs` 为空。
- gateway 不可达：`available:false` + `reason`（如 `"gateway unreachable"`）。

### 1.3 读处理流程（节点侧）

1. 收到 `capability.query` → **信任校验**（仅信任对等方可查，非信任回 `capability.denied`）。
2. 调用适配器 `Capabilities(ctx)`：
   - `skill`：并发透传 gateway `GET /v1/skills`（短 TTL 缓存）。
   - `model`：解析本地 `config.yaml` 的 `custom_providers`（**非 gateway**）。
   - `cron`：代理 gateway `GET /api/jobs`。
3. 组装 `capability.response` 回发。

## 二、写：能力写回

### 2.1 `capability.set`

统一消息，`target` 区分三类能力：

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `requestId` | `string` | 是 | 请求关联 ID |
| `target` | `string` | 是 | `skill` \| `model` \| `cron` |
| `action` | `string` | 是 | 见下「动作映射」 |
| `skill` | `string` | target=skill | 技能名 |
| `fileIds` | `string[]` | target=skill 的 add/update | 通过文件传输通道已传到节点的 `fileId` |
| `model` | `string` | target=model | 模型/provider 标识 |
| `provider` | `object` | target=model 的 add | `{api_mode, transport, model, default_model, api_key?}` |
| `job` | `object` | target=cron 的 create/update | cron job 配置 |
| `jobId` | `string` | target=cron 的 update/delete/pause/resume/run | 目标 job ID |

### 2.2 动作映射

| target | action | 语义 | 实现 |
|--------|--------|------|------|
| `skill` | `add` | 部署新技能 | `fileId` → 托管目录；注册 `external_dirs` managed 键 |
| `skill` | `update` | 覆盖技能内容 | `fileId` → 覆盖托管目录文件 |
| `skill` | `enable` | 启用技能 | 注册 `external_dirs` managed 键 |
| `skill` | `disable` | 停用技能 | 注销 managed 键（保留文件，可恢复） |
| `model` | `add` | 新增 provider | 写 `custom_providers` 条目 |
| `model` | `switch` | 设默认模型 | 写 `config.model` |
| `model` | `delete` | 删除 provider | 删 `custom_providers` 条目（**删当前默认在校验阶段拦截**） |
| `cron` | `create` | 新建定时任务 | 代理 `POST /api/jobs` |
| `cron` | `update` | 改定时任务 | 代理 `PATCH /api/jobs/{id}` |
| `cron` | `delete` | 删定时任务 | 代理 `DELETE /api/jobs/{id}` |
| `cron` | `pause` | 暂停 | 代理 `POST /api/jobs/{id}/pause` |
| `cron` | `resume` | 恢复 | 代理 `POST /api/jobs/{id}/resume` |
| `cron` | `run` | 立即运行 | 代理 `POST /api/jobs/{id}/run` |

> 一期 `skill` 不含 `delete`（保留文件、可恢复）；`model`/`cron` 的删除语义由校验与 gateway 原生端点保证。

### 2.3 `capability.set_response`

| 字段 | 类型 | 说明 |
|------|------|------|
| `requestId` | `string` | 对应 set 的 `requestId` |
| `ok` | `boolean` | 是否成功 |
| `target` | `string` | `skill` \| `model` \| `cron` |
| `action` | `string` | 执行的动作 |
| `skill` | `string` | 影响到的技能名（如有） |
| `model` | `string` | 影响到的模型（如有） |
| `jobId` | `string` | 影响到的 job（如有） |
| `restartStatus` | `string` | `skill`/`model` 写回的重启结果：`none`（cron 无需重启）/ `restarted` / `restart_failed` |
| `error` | `string` | 失败原因（含错误码，如 `capability.invalid`） |

### 2.4 写回流程（节点侧）

**`skill` / `model`（改 config + 重启）**

1. 信任校验（仅信任对等方）→ 否则回 `capability.denied`。
2. 按 `fileId` 取本地文件（仅 `skill` add/update）→ 落到托管目录 `~/.hermes/skills/clawsynapse-managed/<skill>/`（**禁止任意路径**）。
3. 维护 `skills.clawsynapse_managed` 键；应用前计算有效 `external_dirs = 原 base + managed`（保留用户 base，不破坏式覆盖）。
4. **先校验**：YAML 解析 + 技能文件结构（或 `model` provider 字段合法性）→ 失败则**回滚、不重启**、`response` 报 `capability.invalid`。
5. `model` 写回额外校验：禁止删除当前默认模型（须先 `switch` 再删）。
6. 成功 → 定位 `:8642` 上的 gateway PID → kill → 同 env 重 exec `hermes gateway run` → health 检查 → 回报 `restartStatus`。

**`cron`（代理 gateway 原生端点，无重启）**

1. 信任校验。
2. 按 `action` 直接代理对应 gateway `/api/jobs` 端点（Bearer 鉴权复用 `HermesGatewayKey`）。
3. 返回 gateway 结果 → `restartStatus: "none"`。

### 2.5 文件传输前置

`skill` 的 `add` / `update` 需要先走 ClawSynapse **现有文件传输通道**（`clawsynapse.transfer.*`）把技能文件（含 `SKILL.md` 及脚本）传到节点，拿到 `fileId`，再在 `capability.set` 中携带 `fileIds`。协议消息内**不内联大文件**。

`model` / `cron` 不依赖文件传输：`model` 的 `add` 在 `provider` 字段内联配置（含 `api_key`），`cron` 在 `job` 字段内联 job 配置。

## 三、本地触发端点（TrustMesh 调用）

旁挂 daemon 新增同步阻塞端点，封装 NATS 请求/响应：

### 3.1 查询

```
GET /v1/peers/{nodeId}/capabilities
```

- 内部发 `capability.query` → 等 `capability.response`（**5s 超时**）。
- 超时 / 拒绝 / peer 离线 → 返回 `{available:false, reason:"..."}`，**HTTP 仍 200**（前端降级）。
- 响应体对齐 `capability.response`，含 `ts`，使用 `types.APIResult` 风格。

### 3.2 写回

```
POST /v1/peers/{nodeId}/capabilities
Content-Type: application/json

{ "target": "skill|model|cron", "action": "...", "skill"?:"", "fileIds"?:[], "model"?:"", "provider"?:{...}, "job"?:"", "jobId"?:"..." }
```

- 内部发 `capability.set` → 等 `capability.set_response`（**5s 超时**）。
- 超时 / 拒绝 / peer 离线 → 返回 `{ok:false, error:"..."}`，HTTP 仍 200。

## 四、横切规则

- **权限**：读写均**仅信任对等方**（复用现有签名验证 + trust-mode 检查）。非信任节点的 `query` / `set` 回 `capability.denied`。
- **托管目录隔离**（写回兜底，不增加协议复杂度）：`skill` 新/改只落到 `~/.hermes/skills/clawsynapse-managed/<skill>/`，绝不接受任意路径，把 RCE 爆炸半径锁死在托管区。
- **写回审计日志**（write 兜底，不增加协议复杂度）：每次 `capability.set` 记 `sender / action / target / skill|model|jobId / fileId / ts`；`api_key` 脱敏后记录。
- **密钥不回显**：`models` 读响应与 `provider` 写回均不明文返回 `api_key`。
- **重启安全性**：gateway 会话落盘 `~/.hermes/state.db`（SQLite），`/v1/responses` 的 `previous_response_id` 续聊血缘重启后从磁盘恢复，仅数秒不可用窗口。

## 五、错误码

| 错误码 | 含义 |
|--------|------|
| `capability.denied` | 未授权（非信任对等方） |
| `capability.unavailable` | peer 离线 / gateway 不可达 |
| `capability.invalid` | 校验失败（已回滚，未重启） |
| `capability.restart_failed` | 重启后 health 未通过 |
| `capability.timeout` | 网格响应超时（本地端点降级用） |

## 六、TrustMesh 侧配合项

- 将原方案 §4.1.3 的 `GetSkills` / `GetModels` 合并为单调用 `GET /v1/peers/{nodeId}/capabilities`。
- 技能写回 UI：先上传文件 → 再发 `{action, skill, fileIds}`。
- 模型写回 UI：`add` 内联 provider 配置；`switch` 设默认；`delete` 删除（当前默认禁止）。
- cron 写回 UI：直接增删改查，无文件上传、无重启等待。
- 原方案 §五 的 4 个待确认契约问题本草案已全部作答：作用域（按节点 + 网格）、鉴权（仅信任对等方）、schema（如上）、写回（本期含 skill/model/cron 三类 CRUD）。

## 七、未决 / 二期项

- **`capability.query` 权限收紧**：当前为「仅信任对等方」；若未来需 admin 白名单或独立写密钥，横向扩展即可，不影响消息结构。
- **trustmesh 直接跨节点推送文件**：一期依赖节点先经文件传输通道收文件，再 `capability.set` 引用 `fileId`。
- **AgentProduct 硬编码 bug**：`discovery/service.go` 当前把 announce 的 `AgentProduct` 硬编码为 `"clawsynapse"`，hermes 节点上报错误；能力展示/写回上线前需按 `agentAdapter` 配置修正，否则徽章显示错误。
- **`tm-task-exec` 技能缺失**：executor 角色引用了不存在的技能，能力展示上线后会当场暴露空技能清单；需先补齐。
- **toolset 开关、MCP server 管理、记忆管理** 等更多写回目标，机制同 `skill`（改 config + 重启），可后续扩展。

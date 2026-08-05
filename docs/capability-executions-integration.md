# 定时任务执行记录/结果上送 TrustMesh — 对接文档

> 面向 **TrustMesh 侧** 的修改说明。ClawSynapse 侧已实现并实测通过，
> 本文档说明 TrustMesh 需要对接的接口、数据结构与前端展示建议。
> 日期：2026-08-04

## 1. 背景与目标

TrustMesh 平台需要把远端 ClawSynapse 节点（hermes）定时任务的**执行记录**
（状态/时间/耗时）与**执行结果**（模型产出的 markdown 报告）展示给用户。

数据源位于目标节点的 hermes gateway：
- 执行状态：`/root/.hermes/cron/executions.db`（SQLite，表 `executions`）
- 执行结果：`/root/.hermes/cron/output/<jobId>/<时间戳>.md`（markdown 报告）

gateway 本身**没有** executions 读取 API（已探测全部 404），因此由
ClawSynapse 适配器直读并上送。ClawSynapse 侧已实现两层能力并实测通过：

| 层 | 能力 | 用途 |
|---|---|---|
| 第 1 层 | `capability.response` 的 `jobs[].executions` | 列表页展示"最近执行"（每 job 最近 3 条） |
| 第 2 层 | `GET /v1/peers/{nodeId}/cron/executions` | 详情页展示完整执行历史 + 结果预览 |

## 2. TrustMesh 需要对接的接口

### 2.1 执行摘要随 capabilities 读回（第 1 层）

现有能力查询响应中，`jobs` 数组的每一项新增字段：

```jsonc
// GET /api/agents/{agentId}/capabilities （现有端点）
{
  "ok": true,
  "code": "ok",
  "data": {
    "product": "hermes",
    "available": true,
    "skills": [ ... ],
    "models": [ ... ],
    "jobs": [
      {
        "id": "73f3e3475cb2",
        "name": "测试任务",
        "schedule": "0 9 * * *",
        "enabled": true,
        "prompt": "回复\"测试任务\"",
        "nextRun": "",
        "executions": [                          // ← 新增
          {
            "executionId": "34abab5f8eac42a29501df30e7a46bb0",
            "jobId": "73f3e3475cb2",
            "status": "completed",               // running | completed | failed | unknown
            "startedAtMs": 1785828520991,
            "finishedAtMs": 1785828524286,
            "durationMs": 3295,
            "error": "",
            "outputFile": "/root/.hermes/cron/output/73f3e3475cb2/2026-08-04_07-28-44.md",
            "outputPreview": "# Cron Job: 测试任务\n\n## Response\n\n测试任务\n..."
          }
        ]
      }
    ]
  }
}
```

- `executions`：最近 3 条（按开始时间倒序），旧节点/无执行时为**空数组**。
- `outputPreview`：结果 markdown 前 400 字符（含截断标记 `\n...`）。

### 2.2 执行历史详情查询（第 2 层，新增端点）

```
GET /v1/peers/{nodeId}/cron/executions?jobId=<jobId>&limit=<N>
```

- `nodeId`：目标节点 ID（与 capabilities 一致）
- `jobId`：可选，为空返回全部 job 的执行
- `limit`：可选，默认 20，上限 100
- 该请求由 TrustMesh 后端通过旁挂节点（本地 daemon）发起，走 NATS
  `capability.executions` 消息（签名 + 信任校验，与现有 capability 相同机制）
- 超时（5s）或目标节点不支持时，返回 HTTP 200 + `executions: []` + `error` 字段：

```jsonc
{
  "ok": true,
  "code": "ok",
  "data": {
    "executions": [ /* 同 2.1 的 executions 元素 */ ],
    "error": "capability.timeout: context deadline exceeded"   // 正常时为空/缺省
  }
}
```

## 3. 数据结构说明

`ExecutionInfo`（执行记录）字段：

| 字段 | 类型 | 说明 |
|---|---|---|
| `executionId` | string | 执行唯一 ID（gateway executions.db id） |
| `jobId` | string | 所属定时任务 ID |
| `status` | string | `running` / `completed` / `failed` / `unknown` |
| `startedAtMs` | int64 | 开始时间（epoch ms） |
| `finishedAtMs` | int64 | 结束时间（运行中为 0/缺省） |
| `durationMs` | int64 | 耗时（finished - started） |
| `error` | string | 失败原因（成功后为空） |
| `outputFile` | string | 结果 markdown 文件路径（节点内） |
| `outputPreview` | string | 结果前 400 字符（列表/详情通用） |

## 4. 前端展示建议

在"定时任务"管理区增加**执行记录**展示：

1. **任务列表**：利用 `capabilities` 里每个 job 的 `executions[0]`，显示
   - 状态徽章（completed 绿色 / failed 红色 / running 蓝色）
   - 最近执行时间 + 耗时
2. **执行历史**：点击任务 → 调用 2.2 详情接口，列表展示全部执行（状态/时间/耗时/错误）
3. **结果查看**：点击某条执行 → 弹窗渲染 `outputPreview`（或详情接口返回的完整 markdown）
   - 前端需支持 **markdown 渲染**（建议用已有 markdown 组件）
   - `[SILENT]` 开头的输出表示"无新内容"（hermes cron 约定），可提示"本次无新内容"

## 5. 兼容性与注意事项

- **旧节点**（未升级 ClawSynapse）：`executions` 字段缺省 → 前端按"无执行记录"展示即可
- **运行中**的执行：`finishedAtMs: 0`，前端显示"执行中"，可轮询刷新
- **失败**的执行：`status: failed` + `error` 原因，`outputPreview` 可能为空
- **只读安全**：ClawSynapse 仅对 executions.db 做只读 SELECT，不影响 gateway
- **NATS 消息**：`capability.executions`（query）与 `capability.executions_response`（response），
  签名与信任校验与既有 capability 消息一致（TrustMesh 旁挂节点已具备发送能力，无需改动）

## 6. 已实测验证（ClawSynapse 侧）

- 真实节点（n1-9881…）`capabilities` 读回：`测试任务` job 带 1 条 completed 执行 + 结果预览 ✅
- `GET /v1/peers/{nodeId}/cron/executions?jobId=73f3e3475cb2`：
  `executionId/status/startedAtMs/finishedAtMs/durationMs(3295ms)/outputFile/outputPreview` 全部正确 ✅
- 单测 6 个（读 db、按 job 过滤、无 db 空列表、limit 钳制、attached to jobs、时间解析）✅

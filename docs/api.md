---
summary: "ClawSynapse 本地 HTTP API 完整接口参考"
title: "ClawSynapse API Reference"
---

# ClawSynapse API Reference

最后更新：2026-03-17

`clawsynapsed` 在本地暴露 HTTP API，供 Agent、CLI 或外部系统调用。默认监听 `127.0.0.1:18080`，可通过 `LOCAL_API_ADDR` 或 `--local-api-addr` 配置。

## 通用约定

### 统一响应格式

所有接口返回 `Content-Type: application/json`，统一使用 `APIResult` 结构：

```json
{
  "ok": true,
  "code": "模块.动作",
  "message": "人类可读的描述",
  "data": {},
  "ts": 1710000000000
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `ok` | bool | 操作是否成功 |
| `code` | string | 机器可读的结果码，格式 `模块.动作`（如 `auth.challenge_accepted`） |
| `message` | string | 人类可读的结果描述 |
| `data` | object | 业务数据，失败时可能包含上下文信息 |
| `ts` | int64 | 响应时间戳，Unix 毫秒 |

### 错误处理

请求参数错误返回 `400 Bad Request`，响应中 `ok: false`：

```json
{
  "ok": false,
  "code": "invalid_argument",
  "message": "invalid json payload",
  "ts": 1710000000000
}
```

---

## 接口列表

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/v1/health` | 健康检查 |
| `GET` | `/v1/peers` | 获取已发现的 peer 列表 |
| `POST` | `/v1/publish` | 向目标节点发布消息 |
| `GET` | `/v1/messages` | 获取最近收到的消息 |
| `POST` | `/v1/auth/challenge` | 向目标节点发起认证握手 |
| `GET` | `/v1/trust/pending` | 获取待处理的信任请求 |
| `POST` | `/v1/trust/request` | 向目标节点发起信任请求 |
| `POST` | `/v1/trust/approve` | 批准信任请求 |
| `POST` | `/v1/trust/reject` | 拒绝信任请求 |
| `POST` | `/v1/trust/revoke` | 撤销对目标节点的信任 |

---

## GET /v1/health

健康检查，返回服务状态和 NATS 连接信息。

**响应**

```json
{
  "ok": true,
  "code": "health.ok",
  "message": "service healthy",
  "data": {
    "peersCount": 3,
    "nats": {
      "name": "clawsynapse-node-alpha",
      "serverUrl": "nats://220.168.146.21:9414",
      "connected": true,
      "status": "CONNECTED",
      "connectedAt": 1710000000000,
      "lastDisconnectAt": 0,
      "lastReconnectAt": 0,
      "disconnects": 0,
      "reconnects": 0,
      "lastError": "",
      "inMsgs": 1024,
      "outMsgs": 512,
      "inBytes": 65536,
      "outBytes": 32768
    }
  },
  "ts": 1710000000000
}
```

`data.nats` 字段说明：

| 字段 | 类型 | 说明 |
|------|------|------|
| `name` | string | NATS 连接名称 |
| `serverUrl` | string | 当前连接的服务器地址 |
| `connected` | bool | 是否已连接 |
| `status` | string | 连接状态（`CONNECTED`, `DISCONNECTED`, `CLOSED` 等） |
| `connectedAt` | int64 | 连接建立时间，Unix 毫秒 |
| `lastDisconnectAt` | int64 | 最后一次断开时间 |
| `lastReconnectAt` | int64 | 最后一次重连时间 |
| `disconnects` | int64 | 断开次数 |
| `reconnects` | int64 | 重连次数 |
| `lastError` | string | 最后一次错误信息 |
| `inMsgs` | uint64 | 接收消息数 |
| `outMsgs` | uint64 | 发送消息数 |
| `inBytes` | uint64 | 接收字节数 |
| `outBytes` | uint64 | 发送字节数 |

---

## GET /v1/peers

获取已发现的 peer 节点列表。

**响应**

```json
{
  "ok": true,
  "code": "peers.ok",
  "message": "peers fetched",
  "data": {
    "items": [
      {
        "nodeId": "node-beta",
        "agentProduct": "openclaw",
        "version": "2026.3.9",
        "capabilities": ["chat", "tools"],
        "inbox": "clawsynapse.msg.node-beta.inbox",
        "authStatus": "authenticated",
        "trustStatus": "trusted",
        "lastSeenMs": 1710000000000,
        "metadata": { "hostname": "server-2" }
      }
    ]
  },
  "ts": 1710000000000
}
```

`data.items[]` 字段说明（Peer 结构）：

| 字段 | 类型 | 说明 |
|------|------|------|
| `nodeId` | string | 节点 ID |
| `agentProduct` | string | Agent 产品标识（如 `openclaw`） |
| `version` | string | Agent 版本号 |
| `capabilities` | string[] | 能力列表（如 `chat`, `tools`） |
| `inbox` | string | 节点 inbox subject |
| `authStatus` | string | 认证状态：`unknown`, `seen`, `auth_pending`, `authenticated`, `rejected`, `expired` |
| `trustStatus` | string | 信任状态：`none`, `pending`, `trusted`, `rejected`, `revoked` |
| `lastSeenMs` | int64 | 最后一次心跳时间，Unix 毫秒 |
| `metadata` | object | 附加元数据 |

---

## POST /v1/publish

向目标节点发布消息。守护进程负责路由、签名和投递。

**请求**

```json
{
  "targetNode": "node-beta",
  "type": "chat.message",
  "message": "请汇总最新报告",
  "sessionKey": "nats:node-alpha:node-beta",
  "metadata": { "priority": "high" }
}
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `targetNode` | string | 是 | 目标节点 ID |
| `type` | string | 否 | 消息类型（如 `chat.message`, `task.assign`） |
| `message` | string | 是 | 消息正文 |
| `sessionKey` | string | 否 | 会话标识，用于关联上下文 |
| `metadata` | object | 否 | 附加元数据 |

**成功响应**

```json
{
  "ok": true,
  "code": "msg.published",
  "message": "message published",
  "data": {
    "targetNode": "node-beta",
    "messageId": "msg-abc123",
    "sessionKey": "nats:node-alpha:node-beta"
  },
  "ts": 1710000000000
}
```

**失败响应**

```json
{
  "ok": false,
  "code": "msg.publish_failed",
  "message": "peer not found: node-beta",
  "data": {
    "targetNode": "node-beta"
  },
  "ts": 1710000000000
}
```

---

## GET /v1/messages

获取最近收到的消息（最多 100 条）。

**响应**

```json
{
  "ok": true,
  "code": "msg.recent",
  "message": "recent messages fetched",
  "data": {
    "items": [
      {
        "id": "msg-abc123",
        "type": "chat.message",
        "from": "node-beta",
        "to": "node-alpha",
        "content": "报告已完成",
        "sessionKey": "nats:node-alpha:node-beta",
        "ts": 1710000000000,
        "sig": "base64-signature",
        "metadata": {},
        "protocolVersion": "1.0"
      }
    ]
  },
  "ts": 1710000000000
}
```

`data.items[]` 字段说明（MessageEnvelope 结构）：

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | string | 消息唯一 ID |
| `type` | string | 消息类型 |
| `from` | string | 发送方节点 ID |
| `to` | string | 接收方节点 ID |
| `content` | string | 消息正文 |
| `sessionKey` | string | 会话标识 |
| `ts` | int64 | 消息时间戳，Unix 毫秒 |
| `sig` | string | 消息签名（Base64） |
| `metadata` | object | 附加元数据 |
| `protocolVersion` | string | 协议版本号 |

---

## POST /v1/auth/challenge

向目标节点发起 challenge-response 认证握手。

**请求**

```json
{
  "targetNode": "node-beta"
}
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `targetNode` | string | 是 | 目标节点 ID |

**成功响应**

```json
{
  "ok": true,
  "code": "auth.challenge_accepted",
  "message": "challenge completed",
  "data": {
    "targetNode": "node-beta",
    "status": "authenticated"
  },
  "ts": 1710000000000
}
```

**失败响应**

```json
{
  "ok": false,
  "code": "auth.challenge_failed",
  "message": "peer not found: node-beta",
  "data": {
    "targetNode": "node-beta"
  },
  "ts": 1710000000000
}
```

---

## GET /v1/trust/pending

获取待处理的信任请求列表。

**响应**

```json
{
  "ok": true,
  "code": "trust.pending",
  "message": "pending trust requests fetched",
  "data": {
    "items": [
      {
        "requestId": "req-xyz789",
        "from": "node-beta",
        "to": "node-alpha",
        "direction": "incoming",
        "reason": "需要协作完成任务",
        "receivedAtMs": 1710000000000
      }
    ]
  },
  "ts": 1710000000000
}
```

`data.items[]` 字段说明（TrustPendingState 结构）：

| 字段 | 类型 | 说明 |
|------|------|------|
| `requestId` | string | 请求唯一 ID |
| `from` | string | 请求发起方节点 ID |
| `to` | string | 请求接收方节点 ID |
| `direction` | string | 方向：`incoming`（收到的）或 `outgoing`（发出的） |
| `reason` | string | 请求理由 |
| `receivedAtMs` | int64 | 请求接收时间，Unix 毫秒 |

---

## POST /v1/trust/request

向目标节点发起信任请求。

**请求**

```json
{
  "targetNode": "node-beta",
  "reason": "需要协作完成任务",
  "capabilities": ["chat", "tools"]
}
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `targetNode` | string | 是 | 目标节点 ID |
| `reason` | string | 否 | 请求理由 |
| `capabilities` | string[] | 否 | 请求的能力列表 |

**成功响应**

```json
{
  "ok": true,
  "code": "trust.requested",
  "message": "trust request sent",
  "data": {
    "targetNode": "node-beta",
    "requestId": "req-xyz789"
  },
  "ts": 1710000000000
}
```

**失败响应**

```json
{
  "ok": false,
  "code": "trust.request_failed",
  "message": "peer not found: node-beta",
  "data": {
    "targetNode": "node-beta"
  },
  "ts": 1710000000000
}
```

---

## POST /v1/trust/approve

批准一个信任请求。

**请求**

```json
{
  "requestId": "req-xyz789",
  "reason": "已确认身份"
}
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `requestId` | string | 是 | 信任请求 ID |
| `reason` | string | 否 | 批准理由 |

**成功响应**

```json
{
  "ok": true,
  "code": "trust.responded",
  "message": "trust decision sent",
  "data": {
    "requestId": "req-xyz789",
    "decision": "approve"
  },
  "ts": 1710000000000
}
```

---

## POST /v1/trust/reject

拒绝一个信任请求。

**请求**

```json
{
  "requestId": "req-xyz789",
  "reason": "未知节点"
}
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `requestId` | string | 是 | 信任请求 ID |
| `reason` | string | 否 | 拒绝理由 |

**成功响应**

```json
{
  "ok": true,
  "code": "trust.responded",
  "message": "trust decision sent",
  "data": {
    "requestId": "req-xyz789",
    "decision": "reject"
  },
  "ts": 1710000000000
}
```

---

## POST /v1/trust/revoke

撤销对目标节点的信任。

**请求**

```json
{
  "targetNode": "node-beta",
  "reason": "节点行为异常"
}
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `targetNode` | string | 是 | 目标节点 ID |
| `reason` | string | 否 | 撤销理由 |

**成功响应**

```json
{
  "ok": true,
  "code": "trust.revoked",
  "message": "trust revoked",
  "data": {
    "targetNode": "node-beta"
  },
  "ts": 1710000000000
}
```

---

## Go 客户端

`internal/api` 包提供了 `Client` 结构，可在 Go 代码中直接调用本地 API：

```go
c := api.NewClient("127.0.0.1:18080", 5*time.Second)

// GET 请求
result, err := c.Get(ctx, "/v1/peers")

// POST 请求
result, err := c.Post(ctx, "/v1/publish", map[string]any{
    "targetNode": "node-beta",
    "message":    "hello",
})
```

`Client` 自动处理 JSON 序列化/反序列化，返回值统一为 `types.APIResult`。当 HTTP 状态码 >= 400 时，返回 error 且 `result.OK` 为 `false`。

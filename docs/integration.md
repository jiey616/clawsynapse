---
summary: "ClawSynapse 集成与适配：Adapter 接口、OpenClaw 接入与 Webhook 接入"
title: "ClawSynapse Integration"
---

# ClawSynapse Integration

最后更新：2026-03-17

本文档介绍如何通过 Agent Adapter 适配层将不同 Agent 产品接入 ClawSynapse。

本地 HTTP API 的完整接口说明见 [docs/api.md](api.md)。CLI 用法见 [docs/cli.md](cli.md)。消息协议与 subject 命名规范见 [docs/protocol.md](protocol.md)。

## Agent Adapter

当守护进程收到发往本节点的消息时，通过 `AgentAdapter` 接口投递给本地 Agent。不同 Agent 产品实现该接口即可接入。

```go
type AgentAdapter interface {
    DeliverMessage(ctx context.Context, req DeliverMessageRequest) (*DeliverMessageResult, error)
    GetStatus(ctx context.Context) (*AgentStatus, error)
}
```

### DeliverMessageRequest

| 字段 | 类型 | 说明 |
|------|------|------|
| `Type` | string | 消息类型（如 `chat.message`, `task.assign`） |
| `SessionKey` | string | 会话标识，用于关联上下文 |
| `Message` | string | 消息正文 |
| `From` | string | 发送方节点 ID |
| `Metadata` | map[string]any | 附加元数据 |

### DeliverMessageResult

| 字段 | 类型 | 说明 |
|------|------|------|
| `Success` | bool | 投递是否成功 |
| `Accepted` | bool | Agent 是否接受了消息 |
| `RunID` | string | Agent 运行 ID（如适用） |
| `Reply` | string | Agent 的回复内容 |
| `Error` | string | 错误描述（失败时） |

### AgentStatus

| 字段 | 类型 | 说明 |
|------|------|------|
| `Healthy` | bool | Agent 是否健康 |

### 已实现的适配器

| 适配器 | `AGENT_ADAPTER` 值 | 说明 |
|--------|---------------------|------|
| `DefaultAdapter` | `default` | 内置默认，echo 回显，用于调试和测试 |
| `OpenClawAdapter` | `openclaw` | 调用本地 OpenClaw CLI 投递消息 |
| `WebhookAdapter` | `webhook` | 通过 HTTP POST 将消息 JSON 转发到外部 URL |

通过环境变量 `AGENT_ADAPTER` 或命令行 `--agent-adapter` 选择适配器。

---

## OpenClaw 接入

`OpenClawAdapter` 通过调用本地 `openclaw` CLI 将消息投递给 OpenClaw Agent。

### 消息格式

投递时将请求格式化为带 header 的文本：

```
[clawsynapse type=chat.message from=<peer-node-id> to=<local-node-id> session=sess-1 key=value]
消息正文内容
```

header 中包含 `type`、`from`、`to`、`session` 以及 metadata 中的键值对。

### 会话映射

- 优先使用请求中的 `SessionKey`
- 如果为空，自动生成 `cs-{from}-{localNodeId}` 格式的会话 ID

### 网关通信验证

CLI 验证：

```bash
openclaw gateway run
OPENCLAW_GATEWAY_TOKEN=your-gateway-token \
  openclaw agent --message "你好"
```

WebSocket 调用流程：

1. 连接本地 Gateway
2. 响应 `connect.challenge`
3. 发送 `connect`
4. 发送 `chat.send`
5. 在运行结束后调用 `chat.history` 获取最终回复

简化示例：

```javascript
const ws = new WebSocket("ws://127.0.0.1:18789");

ws.onmessage = (event) => {
  const msg = JSON.parse(event.data);

  if (msg.event === "connect.challenge") {
    ws.send(JSON.stringify({
      type: "req",
      id: "1",
      method: "connect",
      params: {
        minProtocol: 3,
        maxProtocol: 3,
        client: { id: "gateway-client", version: "0.0.1", platform: "macos", mode: "backend" },
        role: "operator",
        scopes: ["operator.read", "operator.write"],
        auth: { token: "your-gateway-token" }
      }
    }));
  }

  if (msg.type === "res" && msg.ok && msg.payload?.type === "hello-ok") {
    ws.send(JSON.stringify({
      type: "req",
      id: "2",
      method: "chat.send",
      params: {
        message: "你好",
        sessionKey: "nats:n1-localnodeid:n1-11223344556677889900aabbccddeeff",
        idempotencyKey: crypto.randomUUID()
      }
    }));
  }
};
```

---

## Webhook 接入

`WebhookAdapter` 在收到订阅消息后，将消息以 JSON 格式通过 HTTP POST 转发到指定的 webhook URL，适用于与外部系统或自研 Agent 的松耦合集成。

### 配置

环境变量：

```bash
AGENT_ADAPTER=webhook
WEBHOOK_URL=https://example.com/hooks/clawsynapse
```

命令行：

```bash
clawsynapsed --agent-adapter webhook --webhook-url https://example.com/hooks/clawsynapse
```

YAML 配置文件（`~/.clawsynapse/config.yaml`）：

```yaml
agentAdapter: webhook
webhookUrl: https://example.com/hooks/clawsynapse
```

### Webhook Payload

每次消息投递时，`WebhookAdapter` 向配置的 URL 发送 `POST` 请求，`Content-Type: application/json`，body 格式：

```json
{
  "nodeId": "n1-2f4c6e8a0b1d3f557799aabbccddeeff",
  "type": "chat.message",
  "from": "n1-11223344556677889900aabbccddeeff",
  "sessionKey": "nats:n1-11223344556677889900aabbccddeeff:n1-2f4c6e8a0b1d3f557799aabbccddeeff",
  "message": "请汇总最新报告",
  "metadata": { "priority": "high" }
}
```

字段说明：

| 字段 | 类型 | 说明 |
|------|------|------|
| `nodeId` | string | 本地节点 ID，由本地 DID 自动派生 |
| `type` | string | 消息类型（如 `chat.message`, `task.assign`） |
| `from` | string | 发送方节点 ID |
| `sessionKey` | string | 会话标识，可能为空 |
| `message` | string | 消息正文 |
| `metadata` | object | 附加元数据，可能为空 |

### 响应约定

- **2xx**：视为投递成功，响应 body 作为 `Reply` 返回给发送方
- **非 2xx**：视为投递失败，状态码和响应 body 记入错误信息

### 健康检查

`GetStatus` 对 webhook URL 发送 `GET` 请求：

- 状态码 < 500 → healthy
- 状态码 >= 500 或连接失败 → unhealthy

### 使用场景

- 自研 Agent 暴露 HTTP 接口接收消息
- 转发消息到 Slack、飞书、企业微信等即时通讯平台
- 对接工作流引擎（n8n、Zapier 等）
- 消息归档、审计日志

---

## 扩展方向

- 其他 Agent 产品通过适配层实现统一接入
- 后续可补充事件流接口（SSE/WebSocket），支持双向通信

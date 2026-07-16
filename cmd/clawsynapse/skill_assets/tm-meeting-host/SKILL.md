---
name: tm-meeting-host
description: >
  会议主持人（PM）专用 skill：组织会议、发出会议邀请（meeting.invite）、
  主持并推进议程、在会议中归纳决议与待办，并通过 ClawSynapse 将行动项
  以 todo.* 分派给相关执行 Agent，最后发出 meeting.summary 纪要。
compatibility: Requires clawsynapse CLI
metadata:
  author: TrustMesh
  version: "1.0"
allowed-tools:
  - "Bash(clawsynapse:*)"
---

# TrustMesh 会议主持人 Skill

你是会议主持人（Host / PM）。你的职责是组织会议、推动讨论、沉淀决议，
并把行动项转化为可分派的 Todo 任务。

> 注意：本文件为脚手架，host 的具体会议协议（邀请字段、纪要模板、行动项
> 分派规则）请按你的实际 TrustMesh 会议规范 refinement。

## 一、工作流

```
收到会议需求（chat/task 提及需要开会，或收到 meeting.request）
  │
  1. 明确会议主题、目标、参与人（TrustMesh 节点 ID）
  2. 发送 meeting.invite 邀请各参与节点（附主题/时间/议程）
  3. 会议进行中：推动议程、记录关键讨论与决定
  4. 归纳决议（decision）与待办（action item）
  5. 将 action item 以 todo.assigned 分派给执行 Agent（带 title/description）
  6. 发送 meeting.summary 会议纪要给所有参与人
```

### 关键规则

1. **会议邀请走 `meeting.invite`。** 使用
   `clawsynapse send --type meeting.invite --to <nodeId> --session-key <key>`
   发出邀请，明确主题、时间、议程与主持人。
2. **决议与待办要可追踪。** 每个 action item 必须通过 `todo.assigned` 分派，
   并附带清晰的 `title` 与 `description`。
3. **纪要要具体。** `meeting.summary` 应包含：参会人、决议清单、行动项及
   负责人（对应 Todo ID）、未决事项（open questions）。
4. **所有对外沟通走 ClawSynapse，不要只在聊天界面输出。**

## 二、incoming 消息格式

消息通过 ClawSynapse 到达，带有 header：

```text
[clawsynapse from=<senderNodeId> to=<yourNodeId> session=<sessionKey>]
<message body>
```

- `from=` 是 TrustMesh 节点，**这是你所有回复的 target**
- `to=` 是你自己的 node ID，**永远不要用作 target**
- `session=` 用作 `--session-key`

### 相关消息类型

- `meeting.request`：有人请求你组织/主持一场会议
- `meeting.invite`：其他节点发来的会议邀请（你是参与人时由 participant skill 处理）
- `meeting.response`：参与人对邀请的回应（accept/decline）
- `meeting.message`：会议进行中的发言/讨论

## 三、outgoing 消息

| 类型 | 用途 |
|------|------|
| `meeting.invite` | 向参与人发出会议邀请 |
| `meeting.summary` | 发出会议纪要 |
| `todo.assigned` | 将行动项分派给执行 Agent |

## 四、示例

```bash
# 邀请执行 Agent 参加评审会
clawsynapse send --type meeting.invite --to executor-node-1 \
  --session-key meet-review-001 \
  --body "主题：v1.0.25 评审；时间：今天 15:00；议程：发布说明、回归范围"

# 分派行动项
clawsynapse send --type todo.assigned --to executor-node-1 \
  --session-key task-042 \
  --metadata taskId=task-042 \
  --body "title: 补充 release notes 的镜像拉取说明\ndescription: 在 v1.0.25 notes 中说明多架构镜像来源(TCR/GHCR)"

# 发出纪要
clawsynapse send --type meeting.summary --to executor-node-1 \
  --session-key meet-review-001 \
  --body "决议：v1.0.25 可发布。行动项：task-042 由 executor-node-1 认领。"
```

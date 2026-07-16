---
name: tm-meeting-participant
description: >
  会议参与人（执行 Agent）专用 skill：接收会议邀请（meeting.invite）、
  参与讨论并发言（meeting.message）、回应邀请（meeting.response），
  认领会议中分派给自己的行动项（todo.*），并向主持人回报进展。
compatibility: Requires clawsynapse CLI
metadata:
  author: TrustMesh
  version: "1.0"
allowed-tools:
  - "Bash(clawsynapse:*)"
---

# TrustMesh 会议参与人 Skill

你是会议参与人（Participant / 执行 Agent）。你的职责是响应会议邀请、
在会议中贡献内容、认领并执行分派给你的行动项，并向主持人回报进展。

> 注意：本文件为脚手架，participant 的具体会议协议（回应该字段、发言格式、
> 行动项认领规则）请按你的实际 TrustMesh 会议规范 refinement。

## 一、工作流

```
收到 meeting.invite
  │
  1. 阅读主题/时间/议程，判断是否可参加
  2. 发送 meeting.response 回应 accept / decline（附理由）
  3. 会议进行中：按议程参与讨论，发表 meeting.message
  4. 收到 todo.assigned（会议行动项）后：开工即发 task.comment
  5. 执行行动项，按里程碑发送 todo.progress
  6. 完成发 todo.complete，失败发 todo.fail
```

### 关键规则

1. **先回应邀请。** 收到 `meeting.invite` 后尽快发送 `meeting.response`，
   说明是否参加及原因。
2. **参与讨论要具体。** `meeting.message` 用于发表观点、补充信息、提出风险，
   不要沉默。
3. **行动项走 Todo 协议。** 会议中分派给你的任务以 `todo.assigned` 到达，
   按执行 Agent 工作流（见 tm-task-exec）认领、执行、回报。
4. **所有对外沟通走 ClawSynapse，不要只在聊天界面输出。**

## 二、incoming 消息格式

消息通过 ClawSynapse 到达，带有 header：

```text
[clawsynapse from=<senderNodeId> to=<yourNodeId> session=<sessionKey>]
<message body>
```

- `from=` 是 TrustMesh 节点（通常是主持人），**这是你所有回复的 target**
- `to=` 是你自己的 node ID，**永远不要用作 target**
- `session=` 用作 `--session-key`

### 相关消息类型

- `meeting.invite`：主持人发来的会议邀请
- `meeting.message`：会议进行中的发言/讨论
- `meeting.summary`：主持人发出的会议纪要
- `todo.assigned`：会议中分派给你的行动项

## 三、outgoing 消息

| 类型 | 用途 |
|------|------|
| `meeting.response` | 回应邀请（accept/decline） |
| `meeting.message` | 会议中发言/讨论 |
| `todo.progress` / `todo.complete` / `todo.fail` | 回报行动项进展与结果 |
| `task.comment` | 行动项执行过程中的工作日志 |

## 四、示例

```bash
# 回应邀请
clawsynapse send --type meeting.response --to pm-node-1 \
  --session-key meet-review-001 --body "accept：准时参加"

# 会议发言
clawsynapse send --type meeting.message --to pm-node-1 \
  --session-key meet-review-001 \
  --body "提醒：v1.0.25 多架构镜像首次构建约 4h，建议错峰打 tag 避免 TCR 限流"

# 认领并执行行动项（沿用 Todo 协议）
clawsynapse send --type task.comment --to pm-node-1 \
  --session-key task-042 --metadata taskId=task-042 \
  --body "开工：开始补充 release notes 镜像拉取说明"
clawsynapse send --type todo.complete --to pm-node-1 \
  --session-key task-042 --metadata taskId=task-042 \
  --body "result: 已在 v1.0.25 notes 补充 TCR/GHCR 镜像来源说明"
```

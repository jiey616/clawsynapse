---
name: clawsynapse
description: >
  Use this skill when the user wants to contact, message, or collaborate with
  another person, agent, or node — even if they don't mention "ClawSynapse"
  directly. Covers sending messages ("tell Alex...", "ask node-2 to..."),
  assigning tasks, checking who is online, and managing trust between peers.
  If the user refers to someone by name and wants to communicate with them,
  this is the skill to use.
compatibility: Requires clawsynapse CLI access
metadata:
  author: jiey616
  version: "2.0"
allowed-tools:
  - "Bash(clawsynapse:*)"
---

# ClawSynapse

You have access to `clawsynapse`, a CLI tool for communicating with other AI agents on the ClawSynapse peer-to-peer network.

## Response Contract

When handling ClawSynapse communication tasks, follow these response rules strictly:

- Do NOT restate payloads, clarifications, task summaries, or Todo plans in the chat UI.
- Outside of a send confirmation, do NOT output any business content in the chat UI.
- After a successful send, output exactly one line: `ACK <message_type>`.
- If waiting for a new user reply or a follow-up system message, output exactly: `WAITING`.
- If a command fails, output exactly `ERR <reason>` or `ERR publish failed: <code>`.
- Do NOT output extra explanation.
- Do NOT paste JSON.
- Do NOT repeat what was sent.

## When to Use This Skill

Use clawsynapse whenever the user wants to:
- Send a message to someone not in this conversation ("give Alex a message", "tell node-2 that...")
- Assign a task to another agent ("ask Alex to do xxx")
- Ask another agent a question ("check with Alex on the current status")
- Check who is online ("which nodes are available")

Node names (like "Alex", "node-2") correspond to peers on the network.

## Current CLI Scope

This skill is focused on peer communication workflows. The current `clawsynapse` CLI also includes other top-level commands such as `init`, `service`, `dashboard`, `logs`, `upgrade`, and `version`, but those are not the primary tools for this skill.

Messaging-related commands currently available in the CLI are:

- `health`
- `peers`
- `messages`
- `publish`
- `auth challenge`
- `trust pending|request|approve|reject|revoke`
- `transfer send|get|delete|list`
- `transfers`

## First Step: Resolve the Target

If the user mentions a name but not a node ID, run:
```bash
clawsynapse --json peers
```
Match the name against node IDs in the result. If no match is found, ask the user to clarify.

## Examples

Here is how to translate common user requests into clawsynapse actions:

**User:** "给 Alex 发个消息，让他准备一下周会材料"
```bash
# Step 1: resolve "Alex" to a node ID
clawsynapse --json peers
# Step 2: send the message (assuming Alex's node ID is "alex")
# Use the global --json flag to capture the sessionKey for follow-up messages
clawsynapse --json publish --target alex --message "[request] Please prepare materials for the weekly meeting."
# Capture the returned sessionKey and reuse it for all follow-up messages in the same task
```

**User:** "问一下 node-2 现在进度怎么样"
```bash
# Start a normal conversation and keep the returned sessionKey
clawsynapse --json publish --target node-2 --message "[request] What is your current progress?"
```

**User:** "有哪些节点在线？"
```bash
clawsynapse peers
```

**User:** "告诉 Bob 任务完成了，结果是 42"
```bash
clawsynapse publish --target bob --message "[reply] Task completed. The result is 42."
```

## Incoming Message Format

When a peer sends you a message, it is delivered directly to you with a structured header:

```
[clawsynapse from=<senderNodeId> to=<localNodeId> session=<sessionKey>]
<message body>
```

Example:

```
[clawsynapse from=node-2 to=node-1 session=task-abc]
[request] What is the current system status?
```

**You do NOT need to poll or check the inbox** — messages are delivered to you automatically. When you see the `[clawsynapse ...]` header, you know:
- The message came from another agent on the ClawSynapse network
- `from=` tells you which node sent it — use this as the `--target` when replying
- `to=` is your own node ID
- `session=` identifies the conversation session — **when present, you MUST include `--session-key <sessionKey>` in your reply** to keep the task in the same conversation thread

## Replying to Messages

**The sender is a remote agent on another machine — it CANNOT see your text output.** You MUST use `clawsynapse publish` to send any response. Never reply by simply outputting text.

Example — you receive:
```
[clawsynapse from=node-2 to=node-1 session=task-abc]
[request] How many files are in the workspace?
```

❌ WRONG — node-2 will never see this:
```
There are 42 files in the workspace.
```

❌ WRONG — missing `--session-key`, breaks conversation continuity:
```bash
clawsynapse publish --target node-2 --message "[reply] There are 42 files in the workspace."
```

✅ CORRECT — includes `--session-key` to keep the same session:
```bash
clawsynapse publish --target node-2 --message "[reply] There are 42 files in the workspace." --session-key task-abc
```

## Commands Used By This Skill

### Messaging

```bash
# Inspect recent messages manually
clawsynapse messages

# Publish a message to another agent
clawsynapse publish --target <nodeId> --message "your message"

# Publish with a specific message type (default: chat.message)
clawsynapse publish --target <nodeId> --type task.assign --message "your message"

# Publish with session key (for conversation continuity)
clawsynapse publish --target <nodeId> --message "your message" --session-key "session-123"

# Publish with metadata
clawsynapse publish --target <nodeId> --message "your message" --metadata key1=value1 --metadata key2=value2
```

### Network & Discovery

```bash
# List discovered peers
clawsynapse peers

# Get raw JSON output
clawsynapse --json peers

# Check local API and network status
clawsynapse health
```

### Trust Management

```bash
# View pending trust requests
clawsynapse trust pending

# Send a trust request to a peer
clawsynapse trust request --target <nodeId> --reason "collaboration on project X"

# Send a trust request with repeatable capability flags
clawsynapse trust request --target <nodeId> --capability chat --capability tools

# Approve a trust request
clawsynapse trust approve --request-id <requestId>

# Reject a trust request
clawsynapse trust reject --request-id <requestId> --reason "unknown peer"

# Revoke trust for a peer
clawsynapse trust revoke --target <nodeId> --reason "no longer needed"
```

**Response notes for `trust request`:**
- `trust.ok: request sent` — trust request successfully submitted; wait for approval.
- `trust.request_failed: trust.already_trusted: peer already trusted` — the two nodes already have a trust relationship; no further action needed.
- `trust.request_failed: trust.self_request` — cannot send a trust request to yourself.
- Check `clawsynapse --json peers` and look for `"trustStatus": "trusted"` to confirm the relationship after a request.

### Authentication

```bash
# The only auth subcommand currently exposed by the CLI
clawsynapse auth challenge --target <nodeId>
```

**Response notes for `auth challenge`:**
- On success: `auth.challenge_accepted: challenge completed` with `targetNode` and `status: authenticated`.
- On failure: `auth.challenge_failed: <reason>` — the challenge handshake did not complete. Common causes: peer offline, network partition, or NATS disconnection.
- Always verify with `clawsynapse health` (check `natsConnected` and `natsStatus`) if a challenge fails.

### File Transfer

```bash
# Send a file to a peer
clawsynapse transfer send --target <nodeId> --file /path/to/file

# Get transfer details
clawsynapse transfer get --id <transferId>

# Delete a transfer record
clawsynapse transfer delete --id <transferId>

# List transfers
clawsynapse transfer list
clawsynapse transfers
```

## OpenClaw Messaging Rule

- **OpenClaw agents MUST use `clawsynapse publish` for peer-to-peer communication.**
- Rationale: this skill relies on session continuity through `sessionKey` and explicit `[reply]` / `[notify]` messages.
- If you need a follow-up, send another `publish` in the same `--session-key`.

## Message Intent Tags

When sending messages, prefix the body with an intent tag so the receiving agent knows how to handle it:

| Tag | Usage |
|-----|-------|
| `[request]` | Asking the peer to do something or answer a question |
| `[reply]` | Responding to a previous `[request]` |
| `[notify]` | One-way notification, no response expected |
| `[data]` | Sending structured data or payload |
| `[end]` | Closing the conversation — do not reply to this |

Example:
```bash
clawsynapse publish --target node-2 --message "[request] Can you summarize the latest logs?"
```

## Global Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--api-addr` | `127.0.0.1:18080` | Local API address |
| `--timeout` | `5s` | Local API timeout |
| `--json` | `false` | Output raw JSON response |

## Collaboration Rules

### Receiving Messages

1. **Messages arrive automatically** — You will receive messages with the `[clawsynapse ...]` header. Do NOT run `clawsynapse messages` to check inbox — that is only for manual inspection.
2. **Always reply via `clawsynapse publish`** — See "Replying to Messages" above. Never output text as a reply. **If the incoming message has `session=`, always include `--session-key` in your reply.**
3. **Auto-handle when safe** — Simple queries, status checks, and public information can be answered directly via `publish` without asking the user.
4. **Notify user when needed** — The following scenarios require user confirmation:
   - Sending files or sensitive data to a peer
   - Modifying local files or configuration
   - Making decisions on behalf of the user
   - Accessing the user's private information
5. **Send exactly once per turn unless there is a real state change** — Do not send the same answer twice. Do not send both a content reply and a second "closing" message unless the conversation truly needs both.

### Sending Messages

1. **User-initiated only** — Only **proactively** send messages (starting a new conversation) when the user explicitly asks. Do not autonomously contact other nodes. However, **replying to received messages** is NOT user-initiated — you should reply via `clawsynapse publish` as described in "Receiving Messages" above.
2. **Resolve peer first** — If the user does not specify a node ID, run `clawsynapse --json peers` to list discovered peers, then let the user choose or match by context.
3. **Keep messages concise** — One topic per message.
4. **Include context** — The receiving agent has no access to your conversation history. Provide enough background for the message to be self-contained.
5. **Use `clawsynapse publish` for business replies** — Do not place the actual reply content in the chat UI.

### Conversation Lifecycle

1. **Start** — Use `clawsynapse --json publish` for the first message. The response contains a `sessionKey` — save it for all follow-up messages in this task.
2. **Continue** — All subsequent messages in the same task MUST include `--session-key <sessionKey>` to maintain conversation continuity.
3. **Respond** — When replying to a received message that has `session=`, always include `--session-key` with that value.
4. **Progress** — If a collaboration exceeds 2 round-trips, give the user a progress update.
5. **Completion** — Judge by role:
   - **Initiator**: complete when the reply satisfies your original need.
   - **Responder**: complete when you have sent the requested information.
6. **Close** — Do NOT automatically send `[end]` after every `[reply]`. Only send `[end]` when the user explicitly wants to close the thread or the remote side has asked to close it. When you receive `[end]`, do not reply.
7. **Timeout** — If no reply within 60 seconds, inform the user and ask whether to retry.

### Trust Management

1. **Handshake requests** — Present the peer's info and reason to the user. Let the user decide.
2. **Never auto-approve** — Do not automatically approve any trust request.

## Important Notes

- Use `clawsynapse` commands only in this skill. Do not tell the user to run or manage background services here.
- Peers must be discovered and trusted before messaging (unless trust mode is `open`).
- Use `--json` flag when you need to parse results programmatically.
- `auth` currently only supports `challenge` from the CLI.
- `trust request` supports repeatable `--capability` flags.
- Do NOT send fields that are not defined by the protocol or CLI.
- **Binary location**: `clawsynapse` is a standalone Go binary (`clawsynapse.exe` on Windows). It may NOT be in PATH. On this system it lives at `D:\AiWorkspace\clawsynapse\clawsynapse.exe` (WSL: `/mnt/d/AiWorkspace/clawsynapse/clawsynapse.exe`). If `clawsynapse: command not found` is returned, use the full path or add its directory to PATH.
- **WSL2 terminal quirk**: When running `clawsynapse` commands via Hermes terminal on WSL2, the shell startup `cd C:\Windows\system32` fails because the path does not exist under WSL's filesystem. Workaround: pass `workdir='.'` in `terminal()` calls from `execute_code`, or run commands from the user's WSL home directory (`/home/jiey`).

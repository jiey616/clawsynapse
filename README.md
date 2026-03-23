# ClawSynapse

Language: **English** | [简体中文](./README.zh-CN.md)

ClawSynapse is a local networking layer for multi-agent interoperability.
It runs as an independent Go daemon (`clawsynapsed`) on the same machine as the agent product, connects outward to NATS, and bridges inward to local agent APIs through adapters.

## What It Provides

- Cross-agent messaging over a shared NATS bus
- Peer discovery and node registry
- Authentication and trust workflow
- Signed message flow and replay protection
- Local HTTP API for integration with CLI/skills/tools

## Architecture

```text
Agent <-> Local ClawSynapse Daemon <-> NATS <-> Remote ClawSynapse Daemon <-> Remote Agent
```

## Quick Start

Requirements:

- A running NATS server

Recommended path:

```bash
curl -fsSL https://raw.githubusercontent.com/yuanjun5681/clawsynapse/main/scripts/install.sh | bash
clawsynapse init
clawsynapse service restart
clawsynapse health
```

### 1. Install the CLI and Daemon

The recommended production setup is:

- install `clawsynapsed` as a long-running OS service
- keep runtime configuration in `~/.clawsynapse/config.yaml`
- use `clawsynapse` only as the local management CLI

Install CLI and daemon together by default:

```bash
# One-line install from GitHub Release
curl -fsSL https://raw.githubusercontent.com/yuanjun5681/clawsynapse/main/scripts/install.sh | bash

# Or install from local dist/ (after make dist)
./scripts/install.sh
```

Install only the CLI:

```bash
curl -fsSL https://raw.githubusercontent.com/yuanjun5681/clawsynapse/main/scripts/install.sh | \
  bash -s -- --cli
```

Install only the daemon service:

```bash
curl -fsSL https://raw.githubusercontent.com/yuanjun5681/clawsynapse/main/scripts/install.sh | \
  bash -s -- --daemon --node-id node-alpha
```

What the installer does for daemon mode:

- installs `clawsynapsed`
- bootstraps `~/.clawsynapse/config.yaml` if it does not already exist
- when running in a TTY and `nodeId` is missing, prompts for `nodeId`, NATS servers, adapter, and related values
- registers the daemon as a service:
  - Linux: `systemd`
  - macOS: `launchd`
- starts the service unless `--no-start` is passed

Use explicit flags for non-interactive installs:

```bash
curl -fsSL https://raw.githubusercontent.com/yuanjun5681/clawsynapse/main/scripts/install.sh | \
  bash -s -- --node-id node-alpha --nats-servers nats://127.0.0.1:4222 --agent-adapter openclaw
```

Uninstall examples:

```bash
# Remove CLI + daemon with the default mode
./scripts/install.sh --uninstall

# Remove daemon service and binary
./scripts/install.sh --daemon --uninstall

# Remove everything, including ~/.clawsynapse state
./scripts/install.sh --all --uninstall --purge
```

Use `--check-config` to print the resolved configuration and exit:

```bash
clawsynapsed --node-id node-alpha --check-config
```

Re-run the configuration wizard later with:

```bash
clawsynapse init
clawsynapse init --overwrite --node-id node-alpha --nats-servers nats://127.0.0.1:4222
clawsynapse service restart
```

Recommended operational flow after install:

```bash
# 1. Write or update ~/.clawsynapse/config.yaml
clawsynapse init

# Optional: verify installed binary versions
clawsynapse version
clawsynapsed --version

# 2. Apply the config to the daemon service
clawsynapse service restart

# 3. Verify the daemon is healthy
clawsynapse health
```

Launch the terminal dashboard:

```bash
clawsynapse dashboard
```

Read recent service logs:

```bash
clawsynapse logs
clawsynapse logs --follow
```

Automated releases are tag-driven. Push a semantic version tag such as `v0.0.4`, and GitHub Actions will run tests, build `dist/`, generate `checksums.txt`, write release notes, and publish the GitHub Release assets used by the one-line installer.

### 2. Install the Agent Skill

Give the following prompt to your AI agent (e.g. OpenClaw / Claude Code) so it can automatically install the ClawSynapse skill:

```text
Install the ClawSynapse agent skill:

1. Fetch the SKILL.md from https://github.com/yuanjun5681/clawsynapse/blob/main/skills/clawsynapse/SKILL.md and install it as a skill.

2. Save the following to your memory: This machine is a node on the ClawSynapse agent communication network. When the user wants to send a message, assign a task, or ask a question to another person or agent, use the clawsynapse skill. Run `clawsynapse peers` to discover available nodes.
```

Once installed, the agent will be able to send and receive messages, discover peers, and manage trust on the ClawSynapse network.

### 3. Manage Nodes with the CLI

```bash
# Open the terminal dashboard
clawsynapse dashboard

# Read recent service logs
clawsynapse logs
clawsynapse logs --follow

# Check daemon health
clawsynapse health

# List discovered peers
clawsynapse peers

# Send a message to a remote node
clawsynapse publish --target node-beta --message "hello from alpha"

# Authenticate a peer
clawsynapse auth challenge --target node-beta

# Trust workflow
clawsynapse trust request --target node-beta --reason "collaboration"
clawsynapse trust pending
clawsynapse trust approve --request-id <req-id>
clawsynapse trust reject --request-id <req-id>
clawsynapse trust revoke --target node-beta

# View recent messages
clawsynapse messages
```

Global flags: `--api-addr host:port`, `--timeout duration`, `--json` (raw JSON output).

If your CLI workflows need deliverable message types under `chat.*`, `task.*`, `todo.*`, or `conversation.*`, start the daemon with:

```bash
clawsynapsed --node-id node-alpha --deliverable-prefixes chat,task,todo,conversation
```

Service management after one-line install:

```bash
# Linux
sudo systemctl status clawsynapsed.service
sudo journalctl -u clawsynapsed.service -f

# macOS
launchctl print gui/$(id -u)/io.github.yuanjun5681.clawsynapse.clawsynapsed
```

## Configuration

Configuration precedence: `CLI flags > OS environment variables > project-root .env > ~/.clawsynapse/config.yaml > defaults`

Default main config file: `~/.clawsynapse/config.yaml`

The one-line installer preserves an existing config file and only bootstraps it on first daemon install.

The project-root `.env` file is loaded automatically for local development.

Starter templates are available at `config.example.yaml` and `.env.example`.

Common environment variables:

- `NATS_SERVERS` (comma-separated)
- `NODE_ID`
- `LOCAL_API_ADDR`
- `DATA_DIR`
- `IDENTITY_KEY_PATH`
- `IDENTITY_PUB_PATH`
- `HEARTBEAT_INTERVAL_MS`
- `ANNOUNCE_TTL_MS`
- `TRUST_MODE` (`open` | `tofu` | `explicit`)
- `DELIVERABLE_PREFIXES` (recommended for CLI-driven deliverables: `chat,task,todo,conversation`)

## Documentation

- [Overview](./docs/overview.md)
- [Concepts](./docs/concepts.md)
- [Messaging](./docs/messaging.md)
- [Trust](./docs/trust.md)
- [Integration](./docs/integration.md)
- [CLI](./docs/cli.md)
- [Operations](./docs/operations.md)

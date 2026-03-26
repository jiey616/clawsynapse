# ClawSynapse

Language: **English** | [简体中文](./README.zh-CN.md)

ClawSynapse is a local networking layer for multi-agent communication.
It runs a local daemon (`clawsynapsed`) on your machine, connects to NATS, and gives you a CLI (`clawsynapse`) to discover peers, send messages, and manage trust.

## Quick Start

One command installs the CLI and daemon:

```bash
curl -fsSL https://raw.githubusercontent.com/yuanjun5681/clawsynapse/main/scripts/install.sh | bash
```

The installer will guide you through the required setup in an interactive terminal when it needs to create the daemon config for the first time.

## Install

Default install includes both the CLI and daemon:

```bash
curl -fsSL https://raw.githubusercontent.com/yuanjun5681/clawsynapse/main/scripts/install.sh | bash
```

Install only the CLI:

```bash
curl -fsSL https://raw.githubusercontent.com/yuanjun5681/clawsynapse/main/scripts/install.sh | \
  bash -s -- --cli
```

Install only the daemon:

```bash
curl -fsSL https://raw.githubusercontent.com/yuanjun5681/clawsynapse/main/scripts/install.sh | \
  bash -s -- --daemon --node-id node-alpha
```

For non-interactive installs, pass the values explicitly:

```bash
curl -fsSL https://raw.githubusercontent.com/yuanjun5681/clawsynapse/main/scripts/install.sh | \
  bash -s -- --node-id node-alpha --nats-servers nats://127.0.0.1:4222 --agent-adapter openclaw
```

The installer creates `~/.clawsynapse/config.yaml` on first daemon install and keeps your existing config on later upgrades. If the config file already exists, interactive installs skip those prompts.

## Upgrade

Upgrade to the latest release by running the same installer again:

```bash
curl -fsSL https://raw.githubusercontent.com/yuanjun5681/clawsynapse/main/scripts/install.sh | bash
```

Install a specific release:

```bash
curl -fsSL https://raw.githubusercontent.com/yuanjun5681/clawsynapse/main/scripts/install.sh | \
  bash -s -- --version v0.0.x
```

After upgrading, restart the daemon to make sure the new version is active:

```bash
clawsynapse version
clawsynapsed --version
clawsynapse service restart
clawsynapse health
```

## Common Commands

Check status:

```bash
clawsynapse health
clawsynapse peers
```

Open the terminal dashboard and logs:

```bash
clawsynapse dashboard
clawsynapse logs
clawsynapse logs --follow
```

Send a message:

```bash
clawsynapse publish --target node-beta --message "hello from alpha"
```

Start authentication and trust workflow:

```bash
clawsynapse auth challenge --target node-beta
clawsynapse trust request --target node-beta --reason "collaboration"
clawsynapse trust pending
clawsynapse trust approve --request-id <req-id>
```

Read recent messages:

```bash
clawsynapse messages
```

## Configuration

Main config file:

```text
~/.clawsynapse/config.yaml
```

Re-run the config wizard at any time:

```bash
clawsynapse init
clawsynapse init --overwrite --node-id node-alpha --nats-servers nats://127.0.0.1:4222
clawsynapse service restart
```

If you only want to inspect the resolved daemon config:

```bash
clawsynapsed --node-id node-alpha --check-config
```

## Uninstall

Remove CLI and daemon:

```bash
./scripts/install.sh --uninstall
```

Remove only the daemon:

```bash
./scripts/install.sh --daemon --uninstall
```

Remove everything, including local state:

```bash
./scripts/install.sh --all --uninstall --purge
```

## More Docs

- [Overview](./docs/overview.md)
- [CLI](./docs/cli.md)
- [Operations](./docs/operations.md)
- [Messaging](./docs/messaging.md)
- [Trust](./docs/trust.md)
- [Integration](./docs/integration.md)

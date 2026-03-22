# ClawSynapse

语言： [English](./README.md) | **简体中文**

ClawSynapse 是一个面向多 Agent 互联的本地通信网络层。
它以独立 Go 守护进程（`clawsynapsed`）运行在与 Agent 相同的设备上，对外连接 NATS，对内通过适配层调用本地 Agent API。

## 提供能力

- 基于共享 NATS 总线的跨 Agent 消息通信
- 节点发现与 peer 注册表
- 身份认证与信任流程
- 消息签名与重放保护
- 面向 CLI/技能/工具集成的本地 HTTP API

## 架构

```text
Agent <-> Local ClawSynapse Daemon <-> NATS <-> Remote ClawSynapse Daemon <-> Remote Agent
```

## 快速开始

环境要求：

- 可用的 NATS 服务

推荐主路径：

```bash
curl -fsSL https://raw.githubusercontent.com/yuanjun5681/clawsynapse/main/scripts/install.sh | bash
clawsynapse init
clawsynapse service restart
clawsynapse health
```

### 1. 一键安装 CLI 和守护服务

推荐的生产部署方式是：

- `clawsynapsed` 作为操作系统长期服务运行
- 运行配置统一放在 `~/.clawsynapse/config.yaml`
- `clawsynapse` 仅作为本地管理 CLI 使用

默认同时安装 CLI 和守护服务：

```bash
# 从 GitHub Release 一键安装
curl -fsSL https://raw.githubusercontent.com/yuanjun5681/clawsynapse/main/scripts/install.sh | bash

# 或从本地 dist/ 安装（需先 make dist）
./scripts/install.sh
```

只安装 CLI：

```bash
curl -fsSL https://raw.githubusercontent.com/yuanjun5681/clawsynapse/main/scripts/install.sh | \
  bash -s -- --cli
```

只安装后台守护服务：

```bash
curl -fsSL https://raw.githubusercontent.com/yuanjun5681/clawsynapse/main/scripts/install.sh | \
  bash -s -- --daemon --node-id node-alpha
```

安装脚本在 daemon 模式下会执行：

- 安装 `clawsynapsed`
- 如果 `~/.clawsynapse/config.yaml` 不存在则自动生成
- 如果在交互式终端运行且缺少 `nodeId`，会提示填写 `nodeId`、NATS、适配器等关键参数
- 注册到系统服务：
  - Linux: `systemd`
  - macOS: `launchd`
- 默认安装后立即启动；传 `--no-start` 则只安装不启动

如果是非交互安装，建议显式传参：

```bash
curl -fsSL https://raw.githubusercontent.com/yuanjun5681/clawsynapse/main/scripts/install.sh | \
  bash -s -- --node-id node-alpha --nats-servers nats://127.0.0.1:4222 --agent-adapter openclaw
```

卸载示例：

```bash
# 按默认模式移除 CLI + daemon
./scripts/install.sh --uninstall

# 移除 daemon 服务和二进制
./scripts/install.sh --daemon --uninstall

# 连同 ~/.clawsynapse 配置和数据一起清理
./scripts/install.sh --all --uninstall --purge
```

使用 `--check-config` 打印最终配置后退出（调试用）：

```bash
clawsynapsed --node-id node-alpha --check-config
```

如果后续需要重新配置，可运行：

```bash
clawsynapse init
clawsynapse init --overwrite --node-id node-alpha --nats-servers nats://127.0.0.1:4222
clawsynapse service restart
```

安装后的推荐操作流程：

```bash
# 1. 生成或更新 ~/.clawsynapse/config.yaml
clawsynapse init

# 可选：确认已安装的二进制版本
clawsynapse version
clawsynapsed --version

# 2. 重启 daemon 服务使配置生效
clawsynapse service restart

# 3. 检查 daemon 健康状态
clawsynapse health
```

启动终端监控界面：

```bash
clawsynapse dashboard
```

查看最近服务日志：

```bash
clawsynapse logs
clawsynapse logs --follow
```

Release 发布已支持自动化。推送类似 `v0.0.4` 这样的语义化 tag 后，GitHub Actions 会自动执行测试、构建 `dist/`、生成 `checksums.txt`、生成 release notes，并发布一键安装脚本依赖的 GitHub Release 资产。

### 2. 安装 Agent Skill

将以下提示词发送给你的 AI Agent（如 OpenClaw / Claude Code），即可自动安装 ClawSynapse skill：

```text
安装 ClawSynapse agent skill：

1. 从 https://github.com/yuanjun5681/clawsynapse/blob/main/skills/clawsynapse/SKILL.md 获取 SKILL.md 并安装为 skill。

2. 将以下内容保存到你的记忆中：这台机器是 ClawSynapse Agent 通信网络上的一个节点。当用户想要给其他人或 Agent 发消息、布置任务、提问时，使用 clawsynapse skill。运行 `clawsynapse peers` 可查看可用节点。
```

安装完成后，Agent 即可通过 ClawSynapse 网络收发消息、发现节点、管理信任关系。

### 3. 使用 CLI 管理节点

```bash
# 打开终端监控界面
clawsynapse dashboard

# 查看最近服务日志
clawsynapse logs
clawsynapse logs --follow

# 检查守护进程健康状态
clawsynapse health

# 列出已发现的节点
clawsynapse peers

# 向远程节点发送消息
clawsynapse publish --target node-beta --message "hello from alpha"

# 对节点发起认证
clawsynapse auth challenge --target node-beta

# 信任流程
clawsynapse trust request --target node-beta --reason "collaboration"
clawsynapse trust pending
clawsynapse trust approve --request-id <req-id>
clawsynapse trust reject --request-id <req-id>
clawsynapse trust revoke --target node-beta

# 查看最近消息
clawsynapse messages
```

全局参数：`--api-addr host:port`、`--timeout duration`、`--json`（输出原始 JSON，便于脚本集成）。

如果 CLI 工作流需要投递 `chat.*`、`task.*`、`todo.*`、`conversation.*` 这几类消息，请在启动 daemon 时补充参数：

```bash
clawsynapsed --node-id node-alpha --deliverable-prefixes chat,task,todo,conversation
```

一键安装后的服务管理方式：

```bash
# Linux
sudo systemctl status clawsynapsed.service
sudo journalctl -u clawsynapsed.service -f

# macOS
launchctl print gui/$(id -u)/io.github.yuanjun5681.clawsynapse.clawsynapsed
```

## 配置

配置优先级：`CLI 参数 > OS 环境变量 > 项目根目录 .env > ~/.clawsynapse/config.yaml > 默认值`

默认主配置文件：`~/.clawsynapse/config.yaml`

一键安装脚本在首次安装 daemon 时会生成该配置文件；如果文件已存在，则保留原文件不覆盖。

项目根目录下的 `.env` 会在开发时自动加载。

可直接参考仓库里的 `config.example.yaml` 和 `.env.example` 模板。

常用环境变量：

- `NATS_SERVERS`（逗号分隔）
- `NODE_ID`
- `LOCAL_API_ADDR`
- `DATA_DIR`
- `IDENTITY_KEY_PATH`
- `IDENTITY_PUB_PATH`
- `HEARTBEAT_INTERVAL_MS`
- `ANNOUNCE_TTL_MS`
- `TRUST_MODE`（`open` | `tofu` | `explicit`）
- `DELIVERABLE_PREFIXES`（CLI 投递场景建议配置为 `chat,task,todo,conversation`）

## 文档

- [总览](./docs/overview.md)
- [核心概念](./docs/concepts.md)
- [消息与协议](./docs/messaging.md)
- [信任与认证](./docs/trust.md)
- [集成与适配](./docs/integration.md)
- [CLI 使用](./docs/cli.md)
- [运行与配置](./docs/operations.md)

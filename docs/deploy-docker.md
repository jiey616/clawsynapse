# ClawSynapse + Hermes Agent — Docker 部署指南

> 本文档命令使用新版 Compose 语法 `docker compose ...`。如果你的服务器只有旧版独立 `docker-compose`（v1.x），请把命令替换成 `docker-compose ...`（带连字符）。

---

## 前置要求

- Linux 服务器（amd64 / arm64，Ubuntu 20.04+ / Debian 11+ / CentOS 8+ 均可）
- [Docker](https://docs.docker.com/engine/install/) >= 24.0
- Docker Compose：支持新版 `docker compose` 或旧版独立 `docker-compose` 1.x

```bash
# 一键安装 Docker（如果未装）
curl -fsSL https://get.docker.com | bash
```

---

## 部署步骤

### 1. 克隆仓库

```bash
git clone https://github.com/jiey616/clawsynapse.git
cd clawsynapse
```

### 2. 配置环境变量

```bash
cp .env.example .env
vim .env
```

**至少填写以下配置：**

```ini
# 角色: pm=项目经理 / executor=执行者
CLAWSYNAPSE_AGENT_ROLE=pm

# Hermes LLM API Key (TokenFlow 默认)
TOKENFLOW_API_KEY=sk-your-tokenflow-key

# Hermes Gateway API Key（可选；留空则 entrypoint 随机生成并持久化到 ~/.hermes/.env）
# 同一容器内的 ClawSynapse 默认通过 127.0.0.1:8642 调用 Gateway
HERMES_GATEWAY_KEY=
```

### 3. 选择部署模式

#### 模式 A：从 Registry 拉取（推荐，速度快）

镜像已自动构建并推送到 GitHub Container Registry 和腾讯云 TCR，直接拉取即可：

```bash
# 编辑 .env，设置镜像地址（二选一）
# 国内推荐腾讯云 TCR:
CLAWSYNAPSE_IMAGE=ccr.ccs.tencentyun.com/jiey616/clawsynapse:v1.0.19
# 国外可用 ghcr.io:
# CLAWSYNAPSE_IMAGE=ghcr.io/jiey616/clawsynapse:v1.0.19

# 拉取镜像
docker compose pull

# 启动
docker compose up -d
```

**可用镜像地址：**

| Registry | 地址 | 适用场景 |
|---|---|---|
| 腾讯云 TCR | `ccr.ccs.tencentyun.com/jiey616/clawsynapse:v1.0.19` | 国内，速度快 |
| ghcr.io | `ghcr.io/jiey616/clawsynapse:v1.0.19` | 国外/科学上网 |

> 镜像支持 `linux/amd64` 和 `linux/arm64` 双架构，pull 时自动匹配。

#### 模式 B：本地构建

```bash
# 留空 CLAWSYNAPSE_IMAGE（或删除该行）
# 然后本地构建
docker compose build
docker compose up -d
```

首次构建约 10-15 分钟，后续利用缓存加速。

### 4. 启动容器

```bash
# 新版 Compose
docker compose up -d

# 旧版 docker-compose
docker-compose up -d
```

### 5. 查看日志

```bash
# 实时日志
docker compose logs -f

# 最近 50 行
docker compose logs --tail 50
```

首次启动时 entrypoint 会自动：
- 创建 `~/.hermes/.env`（注入 LLM API Key，并写入 Gateway API Server 配置 `API_SERVER_ENABLED=true` / `API_SERVER_KEY` 等）
- 注册 TokenFlow 为 Hermes custom_providers（如已配置）
- 写入 `~/.hermes/config.yaml`：`approvals.mode: off`（等效 `--yolo`）、`external_dirs` 按 `CLAWSYNAPSE_AGENT_ROLE` 挂载对应 skills
- 执行 `clawsynapse init --agent-adapter hermes`
- 部署 SKILL.md 到 `~/.hermes/skills/clawsynapse/`
- 后台启动 `hermes gateway run`（Gateway API Server），等待 `/health` 就绪
- 启动 `clawsynapsed start`

### 6. 验证

```bash
# 检查 clawsynapse 版本
docker compose exec clawsynapse clawsynapse version

# 检查 health
docker compose exec clawsynapse clawsynapse health

# 检查 Hermes Gateway API 是否就绪（镜像自带 curl 时；否则用下方 python 一行）
docker compose exec clawsynapse curl -fsS http://127.0.0.1:8642/health
# 备选（无 curl）：docker compose exec clawsynapse python3 -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8642/health', timeout=2)"

# 检查 Hermes 配置（应看到 provider: tokenflow、approvals.mode: off、external_dirs）
docker compose exec clawsynapse cat /root/.hermes/config.yaml
```

---

## 常用操作

```bash
# 停止
docker compose down

# 重启
docker compose restart

# 拉取最新镜像并重启
docker compose pull && docker compose up -d

# 重建镜像并重启（本地构建模式）
docker compose up -d --build

# 进入容器调试
docker compose exec clawsynapse bash
```

---

## 数据持久化

容器使用两个 named volume（Docker 管理，不依赖宿主机路径）：

| Volume | 容器内路径 | 内容 |
|--------|----------|------|
| `clawsynapse-data` | `/root/.clawsynapse/` | clawsynapse 配置、身份密钥、存储 |
| `hermes-data` | `/root/.hermes/` | hermes .env、skills、会话历史、认证 |

---

## Hermes 适配器架构（Gateway API）

自 v1.0.20 起，`agentAdapter: hermes` 不再为每条消息 spawn `hermes chat` 子进程，而是调用容器内常驻的 **Hermes Gateway API**（`hermes gateway run` 进程，监听 `127.0.0.1:8642`）。适配器本身仍是同步的，由消息处理器按 `ctx` 超时控制。

### 双端点分流

| 消息类型 | 网关端点 | 说明 |
|---|---|---|
| `chat.message`（对话） | `POST /v1/responses` | 有状态，通过 `previous_response_id` 自动续话 |
| `task.*` / `todo.*`（任务流） | `POST /v1/runs` → 轮询 `GET /v1/runs/{id}` | 长时运行，轮询到终态，返回 `run_id` |

### 对话自动续话约定（重要）

对话**不会每轮独立**，而是按会话自动续话——除非在 Trustmesh 上新开会话：

- 消息信封的 `SessionKey` 由上游（Trustmesh）透传（`internal/messaging/service.go`：`SessionKey` 为空才随机生成）。
- **同一 Trustmesh 会话内，请保持 `SessionKey` 稳定**，适配器会用它把本轮对话接到上一轮的 `previous_response_id`，保持上下文连贯。
- **新开会话时，请换一个新的 `SessionKey`**，适配器会自然开启一个全新的对话上下文。
- 若上游未传 `SessionKey`，适配器按发送方兜底（`cs-<from>-<nodeID>`），同一发送方仍会自动续话。

> 对接 Trustmesh 时：同一会话复用 `SessionKey`，新开会话换值即可，ClawSynapse 侧无需任何改动。

### 端口关系

| 端口 | 用途 | 暴露 |
|---|---|---|
| `8642` | Hermes Gateway API（内部，clawsynapse 调用） | 仅容器内部 |
| `18080` | ClawSynapse 本地 API（对外） | 映射宿主机 |

### 新增配置项

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `HERMES_GATEWAY_URL` | `http://127.0.0.1:8642/v1` | Gateway base URL（同容器可保持默认） |
| `HERMES_GATEWAY_KEY` | 随机生成并持久化 | Gateway API Server Key（写入 `~/.hermes/.env` 的 `API_SERVER_KEY`） |
| `HERMES_MODEL` | `hermes-agent` | `GET /v1/models` 广告的 agent 名 |

Gateway 启动前，entrypoint 会自动写入 `~/.hermes/.env`（`API_SERVER_ENABLED=true` 等）与 `~/.hermes/config.yaml`（`approvals.mode: off` 等效 `--yolo`、`external_dirs` 按 `CLAWSYNAPSE_AGENT_ROLE` 挂载对应 skills），并等待 `/health` 就绪后再启动 `clawsynapsed`。

---

## 更换 LLM API Key

直接修改 `.env` 文件然后重启容器：

```bash
vim .env
docker compose down
docker compose up -d
```

---

## 故障排查

| 问题 | 排查命令 |
|------|---------|
| 容器启动失败 | `docker compose logs --tail 100` |
| 镜像拉取慢 | 换用腾讯云 TCR 地址 |
| 架构不匹配 | `docker inspect <image> | grep Architecture` |
| 磁盘空间不足 | `docker system prune -a` |

---

## CI/CD：自动构建 Docker 镜像

每次推送 `v*` tag 时，GitHub Actions 自动：
1. 运行测试 + 构建 CLI release 包
2. 用 QEMU + Buildx 构建 `linux/amd64` + `linux/arm64` 双架构镜像
3. 推送到 `ghcr.io` 和腾讯云 TCR（需配置 Secrets）

**所需 GitHub Secrets：**

| Secret 名 | 说明 |
|---|---|
| `GHCR_TOKEN` | **推荐**：GitHub Personal Access Token（classic），勾选 `write:packages` 和 `repo`。当 `GITHUB_TOKEN` 因 `insufficient_scope` 无法推送时使用。若未设置，则回退到 `GITHUB_TOKEN`。 |
| `TCR_REGISTRY` | TCR 地址，如 `ccr.ccs.tencentyun.com` |
| `TCR_NAMESPACE` | TCR 命名空间，如 `jiey616` |
| `TCR_USERNAME` | 腾讯云账号 ID（数字格式） |
| `TCR_PASSWORD` | TCR 登录密码（在控制台设置的固定密码） |

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
```

### 3. 选择部署模式

#### 模式 A：从 Registry 拉取（推荐，速度快）

镜像已自动构建并推送到 GitHub Container Registry 和阿里云 ACR，直接拉取即可：

```bash
# 编辑 .env，设置镜像地址（二选一）
# 国内推荐阿里云:
CLAWSYNAPSE_IMAGE=registry.cn-hangzhou.aliyuncs.com/jiey616/clawsynapse:v1.0.19
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
| 阿里云 ACR | `registry.cn-hangzhou.aliyuncs.com/jiey616/clawsynapse:v1.0.19` | 国内，速度快 |
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
- 创建 `~/.hermes/.env`（从容器环境变量注入 API Key）
- 注册 TokenFlow 为 Hermes custom_providers（如已配置）
- 执行 `clawsynapse init --agent-adapter hermes`
- 部署 SKILL.md 到 `~/.hermes/skills/clawsynapse/`
- 启动 `clawsynapsed start`

### 6. 验证

```bash
# 检查 clawsynapse 版本
docker compose exec clawsynapse clawsynapse version

# 检查 health
docker compose exec clawsynapse clawsynapse health

# 检查 Hermes 配置（应看到 provider: tokenflow）
docker compose exec clawsynapse cat /root/.hermes/config.yaml | grep -A 5 "^model:"
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
| 镜像拉取慢 | 换用阿里云 ACR 地址 |
| 架构不匹配 | `docker inspect <image> | grep Architecture` |
| 磁盘空间不足 | `docker system prune -a` |

---

## CI/CD：自动构建 Docker 镜像

每次推送 `v*` tag 时，GitHub Actions 自动：
1. 运行测试 + 构建 CLI release 包
2. 用 QEMU + Buildx 构建 `linux/amd64` + `linux/arm64` 双架构镜像
3. 推送到 `ghcr.io` 和阿里云 ACR（需配置 Secrets）

**所需 GitHub Secrets：**

| Secret 名 | 说明 |
|---|---|
| `GHCR_TOKEN` | **推荐**：GitHub Personal Access Token（classic），勾选 `write:packages` 和 `repo`。当 `GITHUB_TOKEN` 因 `insufficient_scope` 无法推送时使用。若未设置，则回退到 `GITHUB_TOKEN`。 |
| `ALIYUN_ACR_REGISTRY` | ACR 地址，如 `registry.cn-hangzhou.aliyuncs.com` |
| `ALIYUN_ACR_NAMESPACE` | ACR 命名空间，如 `jiey616` |
| `ALIYUN_ACR_USERNAME` | 阿里云账号 |
| `ALIYUN_ACR_PASSWORD` | 阿里云密码或 RAM 访问密钥 |

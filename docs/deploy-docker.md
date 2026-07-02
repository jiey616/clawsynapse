# ClawSynapse + Hermes Agent — Docker 部署指南

在全新的 Linux 服务器上，通过 Docker 一键部署 ClawSynapse + Hermes Agent。

---

## 前置要求

- Linux 服务器（amd64 / arm64，Ubuntu 20.04+ / Debian 11+ / CentOS 8+ 均可）
- [Docker](https://docs.docker.com/engine/install/) >= 24.0
- [Docker Compose](https://docs.docker.com/compose/install/) >= 2.0（`docker compose` 插件）

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

# Hermes LLM API Key (DeepSeek 推荐)
DEEPSEEK_API_KEY=sk-your-deepseek-key-here
DEEPSEEK_BASE_URL=https://api.deepseek.com
```

> 如需使用其他 LLM 提供商（OpenAI、OpenRouter 等），在 `.env` 中填写对应的 `*_API_KEY` 即可。

### 3. 构建镜像

```bash
docker compose build
```

首次构建约 10-15 分钟（需要下载 Go 依赖、克隆 hermes-agent、安装 Node.js + Playwright 等），后续会利用缓存加速。

### 4. 启动容器

```bash
docker compose up -d
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
- 执行 `clawsynapse init --agent-adapter hermes`
- 部署 SKILL.md 到 `~/.hermes/skills/clawsynapse/`
- 启动 `clawsynapsed start`

### 6. 验证

```bash
# 检查 clawsynapse 版本
docker compose exec clawsynapse clawsynapse version

# 检查 hermes 是否可用
docker compose exec clawsynapse hermes --version

# 检查 health
docker compose exec clawsynapse clawsynapse health

# 检查 SKILL.md 是否已部署
docker compose exec clawsynapse ls -la /root/.hermes/skills/clawsynapse/
```

---

## 常用操作

```bash
# 停止
docker compose down

# 重启
docker compose restart

# 重建镜像并重启
docker compose up -d --build

# 进入容器调试
docker compose exec clawsynapse bash

# 查看 hermes 配置
docker compose exec clawsynapse cat /root/.hermes/.env

# 查看 clawsynapse 配置
docker compose exec clawsynapse cat /root/.clawsynapse/config.yaml
```

---

## 数据持久化

容器使用两个 named volume（Docker 管理，不依赖宿主机路径）：

| Volume | 容器内路径 | 内容 |
|--------|----------|------|
| `clawsynapse-data` | `/root/.clawsynapse/` | clawsynapse 配置、身份密钥、存储 |
| `hermes-data` | `/root/.hermes/` | hermes .env、skills、会话历史、认证 |

```bash
# 查看 volume 位置
docker volume inspect clawsynapse_clawsynapse-data

# 备份 volume
docker run --rm -v clawsynapse_clawsynapse-data:/data -v $(pwd):/backup alpine tar czf /backup/clawsynapse-backup.tar.gz -C /data .

# 恢复 volume
docker run --rm -v clawsynapse_clawsynapse-data:/data -v $(pwd):/backup alpine tar xzf /backup/clawsynapse-backup.tar.gz -C /data
```

---

## 更换 LLM API Key

直接修改 `.env` 文件然后重启容器：

```bash
vim .env                          # 修改 DEEPSEEK_API_KEY
docker compose down               # 停止
docker compose up -d              # 重新启动
```

> 容器内的 `/root/.hermes/.env` 只会在**首次不存在时**自动生成。如果已存在（volume 持久化），需进入容器手动修改：
> ```bash
> docker compose exec clawsynapse vim /root/.hermes/.env
> ```

---

## 切换到其他 LLM 提供商

1. 修改 `.env`，填写对应 `*_API_KEY`
2. 进入容器修改 `~/.hermes/config.yaml` 中的 `model.default`
3. 重启容器

```bash
docker compose exec clawsynapse bash
vim /root/.hermes/config.yaml      # 改 model.default
exit
docker compose restart
```

---

## 故障排查

| 问题 | 排查命令 |
|------|---------|
| 容器启动失败 | `docker compose logs --tail 100` |
| hermes 命令不可用 | `docker compose exec clawsynapse which hermes` |
| API Key 未生效 | `docker compose exec clawsynapse cat /root/.hermes/.env` |
| 端口冲突 | `docker compose port clawsynapse 8080` |
| 磁盘空间不足 | `docker system prune -a`（清理未使用的镜像） |

---

## 架构说明

```
┌─────────────────────────────────────┐
│            Docker Container          │
│                                     │
│  /usr/local/bin/clawsynapsed        │
│       │ (hermes adapter)            │
│       │ spawns                      │
│  ┌────▼────────────────────┐        │
│  │  hermes chat -q "msg"   │        │
│  │  (installed at build)   │        │
│  │  /usr/local/bin/hermes  │        │
│  └─────────────────────────┘        │
│                                     │
│  Data volumes:                      │
│    /root/.clawsynapse/  ← config    │
│    /root/.hermes/       ← env/skills│
└─────────────────────────────────────┘
```

# ClawSynapse 节点部署指南（新机器）

> 通过腾讯云 TCR 预构建镜像在**新机器**上部署 ClawSynapse 节点。
> 无需克隆源码、无需编译 Go。镜像为多架构（`linux/amd64` + `linux/arm64`），同一命令自动适配 x86 / ARM 服务器。
>
> 适用版本：**v1.0.35**（镜像内含 clawsynapsed + hermes 0.20.1/0.21 + aiohttp gateway + 内置技能）

---

## 1. 前置要求

| 项 | 要求 |
| --- | --- |
| 服务器 | Linux（amd64 或 arm64），Ubuntu 20.04+ / Debian 11+ / CentOS 8+ |
| Docker | ≥ 24.0（含 compose 插件） |
| 网络 | 能访问 `ccr.ccs.tencentyun.com`（拉镜像）、能连共享 NATS |
| 端口 | 对外放行 `18080`（节点 API）；如需 kanban Web UI 再放行 `9119` |
| TCR 凭据 | 腾讯云账号 ID + TCR 长期密钥（访问凭证） |

一键装 Docker（未安装时）：

```bash
curl -fsSL https://get.docker.com | bash
```

---

## 2. 登录腾讯云镜像仓库

```bash
docker login ccr.ccs.tencentyun.com -u <腾讯云账号ID>
# 密码 = TCR 长期密钥（在腾讯云容器镜像服务控制台生成）
```

> 登录成功后凭据存在 `~/.docker/config.json`，后续 `docker compose pull` 自动复用。

---

## 3. 准备部署目录

全文统一使用 `/opt/clawsynapse`：

```bash
mkdir -p /opt/clawsynapse/data/clawsynapse /opt/clawsynapse/data/hermes
cd /opt/clawsynapse
```

> ⚠️ **宿主机目录必须先创建**。否则 Docker 会自动以 root 创建，可能导致权限异常。

---

## 4. 创建 `.env`

```bash
cat > /opt/clawsynapse/.env << 'EOF'
# ── 镜像 ──────────────────────────────────────────
# 腾讯云 TCR（唯一维护中的仓库，国内速度快）
# ghcr.io/jiey616/clawsynapse 已停止同步（仅到 v1.0.34），请勿使用
CLAWSYNAPSE_IMAGE=ccr.ccs.tencentyun.com/jiey616/clawsynapse:v1.0.35

# ── 节点身份（首次留空，自动生成密钥对）──────────────
# 身份落在 /root/.clawsynapse，务必用数据卷持久化，否则重建容器即换身份
CLAWSYNAPSE_NODE_ID=
CLAWSYNAPSE_NODE_KEY=

# ── 节点角色 ──────────────────────────────────────
# pm       = 项目经理（加载 tm-task-plan / tm-meeting-host 技能）
# executor = 执行智能体（加载 tm-task-exec / tm-meeting-participant 技能）
# 留空     = 只加载协议技能 clawsynapse
CLAWSYNAPSE_AGENT_ROLE=executor

# ── API 监听（容器内固定 18080）────────────────────
CLAWSYNAPSE_API_LISTEN=0.0.0.0:18080

# ── 共享 NATS（多个节点靠它互相发现）────────────────
# 🔴 变量名是 NATS_SERVERS，没有 CLAWSYNAPSE_ 前缀！写错会导致节点连不上
# 默认值即 175.27.135.91:4222（config.go:17），显式写上便于跨机房切换
NATS_SERVERS=nats://175.27.135.91:4222
# 跨节点共享的另一个可选地址：nats://220.168.146.21:9414

# ── LLM Provider（至少配一个）───────────────────────
TOKENFLOW_API_KEY=
TOKENFLOW_BASE_URL=
HERMES_BASE_URL=
DEEPSEEK_API_KEY=
DEEPSEEK_BASE_URL=https://api.deepseek.com
# 其余 provider 留空即可（显式定义只为避免 compose 报变量未设置的警告）
OPENAI_API_KEY=
OPENROUTER_API_KEY=
GOOGLE_API_KEY=
GEMINI_API_KEY=
NOVITA_API_KEY=
NOVITA_BASE_URL=
OLLAMA_API_KEY=

# ── Hermes 模型配置 ────────────────────────────────
HERMES_PROVIDER=deepseek
HERMES_MODEL=deepseek-v4-flash

# ── Hermes Gateway Key（必填）──────────────────────
# 适配器据此 Bearer 鉴权调用本机 Hermes Gateway（127.0.0.1:8642）
# 生成：openssl rand -hex 32
# entrypoint 会把该值同步写入 Gateway 的 API_SERVER_KEY，两边自动一致
# 🔴 切勿留空 —— 留空时 Gateway 随机生成而 Adapter 读到空值 → 401
HERMES_GATEWAY_KEY=

# ── Hermes Dashboard / Kanban Web UI（可选，v1.0.35+）──
# 设为 1 才启动；不设或设为 0 则完全不拉起 dashboard 进程
HERMES_DASHBOARD_ENABLED=1
# 留空则由 entrypoint 自动生成随机密码并打印到容器日志
HERMES_DASHBOARD_USER=clawsynapse
HERMES_DASHBOARD_PASSWORD=
EOF

# 生成并填入 Gateway Key
sed -i "s|^HERMES_GATEWAY_KEY=$|HERMES_GATEWAY_KEY=$(openssl rand -hex 32)|" /opt/clawsynapse/.env
```

> **Dashboard 凭据说明（v1.0.35 起）**
> - hermes 0.21 的 dashboard 有 auth gate：**非 loopback 绑定必须已注册 auth provider**。
>   只要凭据解析成功，entrypoint 就会绑 `0.0.0.0`，`docker -p 9119:9119` 直接可用。
> - 凭据优先级：env 明文密码 > 卷内 `config.yaml` 的 `dashboard.basic_auth`。
>   卷里已有凭据时不会覆盖，**升级/重启后密码保持不变**。
> - 全新卷且未设 `HERMES_DASHBOARD_PASSWORD` 时，entrypoint 生成随机密码并**打印到容器日志一次**：
>   `docker compose logs clawsynapse 2>&1 | grep -A3 "dashboard: NO credentials"`

---

## 5. 创建 `docker-compose.yml`

```bash
cat > /opt/clawsynapse/docker-compose.yml << 'EOF'
services:
  clawsynapse:
    image: ${CLAWSYNAPSE_IMAGE:-ccr.ccs.tencentyun.com/jiey616/clawsynapse:v1.0.35}
    container_name: clawsynapse
    restart: unless-stopped

    ports:
      - "18080:18080"   # 节点 API（宿主端口可改，如 "18081:18080"）
      - "9119:9119"     # kanban Web UI（未启用 dashboard 可删掉这行）

    environment:
      # 身份与角色
      - CLAWSYNAPSE_NODE_ID=${CLAWSYNAPSE_NODE_ID}
      - CLAWSYNAPSE_NODE_KEY=${CLAWSYNAPSE_NODE_KEY}
      - CLAWSYNAPSE_AGENT_ROLE=${CLAWSYNAPSE_AGENT_ROLE}
      - CLAWSYNAPSE_API_LISTEN=${CLAWSYNAPSE_API_LISTEN}

      # 共享 NATS（注意变量名无 CLAWSYNAPSE_ 前缀）
      - NATS_SERVERS=${NATS_SERVERS}

      # LLM Provider
      - TOKENFLOW_API_KEY=${TOKENFLOW_API_KEY}
      - TOKENFLOW_BASE_URL=${TOKENFLOW_BASE_URL}
      - DEEPSEEK_API_KEY=${DEEPSEEK_API_KEY}
      - DEEPSEEK_BASE_URL=${DEEPSEEK_BASE_URL}
      - OPENROUTER_API_KEY=${OPENROUTER_API_KEY}
      - OPENAI_API_KEY=${OPENAI_API_KEY}
      - GOOGLE_API_KEY=${GOOGLE_API_KEY}
      - GEMINI_API_KEY=${GEMINI_API_KEY}
      - NOVITA_API_KEY=${NOVITA_API_KEY}
      - NOVITA_BASE_URL=${NOVITA_BASE_URL}
      - OLLAMA_API_KEY=${OLLAMA_API_KEY}

      # 模型
      - HERMES_PROVIDER=${HERMES_PROVIDER}
      - HERMES_MODEL=${HERMES_MODEL}
      - HERMES_BASE_URL=${HERMES_BASE_URL}

      # Gateway（必填）
      - HERMES_GATEWAY_KEY=${HERMES_GATEWAY_KEY}

      # Dashboard / Kanban（可选）
      - HERMES_DASHBOARD_ENABLED=${HERMES_DASHBOARD_ENABLED}
      - HERMES_DASHBOARD_USER=${HERMES_DASHBOARD_USER}
      - HERMES_DASHBOARD_PASSWORD=${HERMES_DASHBOARD_PASSWORD}

    volumes:
      # 绑定挂载：路径可控、直接 tar 即可备份
      - /opt/clawsynapse/data/clawsynapse:/root/.clawsynapse
      - /opt/clawsynapse/data/hermes:/root/.hermes

    logging:
      driver: "json-file"
      options:
        max-size: "10m"
        max-file: "3"
EOF
```

> **关于数据卷**：上面用绑定挂载（路径可控、备份方便）。若改用命名卷，把 `volumes:` 段换成
> `- clawsynapse-data:/root/.clawsynapse` / `- hermes-data:/root/.hermes`，并在文件末尾补
> `volumes: { clawsynapse-data: {driver: local}, hermes-data: {driver: local} }`。
> 两种方式**都不要**用 `docker compose down -v`（`-v` 会删卷，节点身份丢失）。

---

## 6. 拉取并启动

```bash
cd /opt/clawsynapse
docker compose pull
docker compose up -d
```

首次启动会自动：生成身份密钥 → `clawsynapse init` → 起 hermes gateway → 起 hermes dashboard（若启用）→ 起 `clawsynapsed`。约 1-2 分钟。

```bash
docker compose logs -f clawsynapse
```

---

## 7. 验证

### 7.1 节点健康

```bash
curl -s http://localhost:18080/v1/health | python3 -m json.tool
```

关注这几个字段：

```jsonc
{
  "data": {
    "adapter": { "healthy": true, "name": "hermes" },   // adapter 必须是 true / hermes
    "nats":    { "status": "CONNECTED", "serverUrl": "nats://175.27.135.91:4222" },
    "self":    { "nodeId": "n1-xxxxxxxx", "did": "did:key:z6Mk...", "version": "v1.0.35" },
    "peersCount": 9                                      // 同 NATS 上的其他节点数
  }
}
```

| 字段 | 期望 | 不对时的排查 |
| --- | --- | --- |
| `adapter.healthy` | `true` | Gateway 起来了但 401 → `HERMES_GATEWAY_KEY` 为空或两边不一致 |
| `nats.status` | `CONNECTED` | 检查 `NATS_SERVERS` 变量名与地址可达性 |
| `peersCount` | ≥ 1（有兄弟节点时） | 所有节点必须连**同一个** NATS |

### 7.2 记录节点身份（接入平台要用）

```bash
curl -s http://localhost:18080/v1/health | python3 -c \
  "import sys,json; d=json.load(sys.stdin)['data']['self']; print('nodeId:',d['nodeId']); print('did:',d['did'])"
```

### 7.3 Kanban Web UI（若启用）

```bash
curl -sI http://localhost:9119/ | head -1    # 期望 302（跳 /login）
```

浏览器打开 `http://<服务器IP>:9119`，用第 4 节设的 `HERMES_DASHBOARD_USER` / `HERMES_DASHBOARD_PASSWORD` 登录。
忘记密码时：`docker compose logs clawsynapse 2>&1 | grep -A3 "dashboard: NO credentials"`。

### 7.4 hermes gateway（容器内部，供适配器调用）

```bash
docker compose exec clawsynapse curl -fsS http://127.0.0.1:8642/health
```

---

## 8. 接入 TrustMesh 平台

节点起得来只是 standalone，**必须在平台侧登记**才能被调度：

1. 从第 7.2 步拿到 `nodeId`（`n1-xxx`）与 `did`（`did:key:z6Mk...`）。
2. 在 TrustMesh 平台添加 agent，绑定该 `nodeId`（同时指定角色：PM / Executor）。
3. 确认平台与节点连的是**同一个 NATS**（`nats.status` 为 `CONNECTED`，且 `peersCount` 增长）。
4. 平台侧发一条测试消息，观察 `docker compose logs -f clawsynapse` 是否收到。

---

## 9. 升级 / 回滚

```bash
cd /opt/clawsynapse
sed -i 's|:v1.0.35|:v1.0.36|' docker-compose.yml   # 改 tag
docker compose pull
docker compose up -d
docker compose logs -f
```

数据目录保留，**节点身份不变**。回滚把 tag 改回旧版本再 `pull && up -d` 即可。

> TCR 个人版有限流，勿短时间连续推多个 `v*` tag。

---

## 10. 备份与迁移

绑定挂载下数据就是宿主机普通目录，直接 `tar`：

```bash
# 备份
cd /opt/clawsynapse
tar czf clawsynapse-data.tar.gz -C data/clawsynapse .
tar czf hermes-data.tar.gz      -C data/hermes .

# 新机器恢复（先按第 3 节建好目录、停掉容器）
tar xzf clawsynapse-data.tar.gz -C data/clawsynapse
tar xzf hermes-data.tar.gz      -C data/hermes
docker compose up -d
```

> 带走 `data/clawsynapse` 才能保持节点身份不变；`data/hermes` 含会话历史与 dashboard 凭据。

---

## 11. 卸载

```bash
cd /opt/clawsynapse
docker compose down                  # 停并删容器（数据保留）
rm -rf /opt/clawsynapse/data         # 彻底删除数据（含身份）；只删容器则省略
```

---

## 12. 常见问题

| 问题 | 处理 |
| --- | --- |
| `docker pull` 报 unauthorized | 重新 `docker login ccr.ccs.tencentyun.com`（账号 ID + 长期密钥） |
| 容器启动后立即退出 | `.env` 含 CRLF 行尾（Windows 编辑过）→ `sed -i 's/\r$//' .env` |
| health 里 `nats.status` 不是 CONNECTED | 🔴 确认变量名是 **`NATS_SERVERS`**（无 `CLAWSYNAPSE_` 前缀）且地址可达 |
| `adapter.healthy` 为 false / 日志 401 | `HERMES_GATEWAY_KEY` 留空或改动后未同步 → 重新生成固定值并 `up -d` |
| 端口 9119 打不开 | ① `HERMES_DASHBOARD_ENABLED` 未设为 1；② 无凭据时 entrypoint fail closed 只绑 `127.0.0.1`（查日志确认是否生成了随机密码）；③ compose 漏了 `9119:9119` 映射 |
| 换机器后节点身份变了 | 身份在 `data/clawsynapse` 目录里，迁移时必须带走 |
| 端口冲突 | 改 compose `ports` 的宿主端口，如 `"18081:18080"` |
| 架构不匹配 | `docker inspect <image> \| grep Architecture`；TCR 镜像是双架构 manifest，pull 时自动匹配 |

---

## 附录 A：端口表

| 端口 | 用途 | 暴露方式 |
| --- | --- | --- |
| `18080` | ClawSynapse 节点 API（`/v1/health` 等） | 映射宿主机，对外 |
| `9119` | Hermes Dashboard / Kanban Web UI（可选） | 映射宿主机，需基础认证 |
| `8642` | Hermes Gateway API（适配器内部调用） | **仅容器内部，不要映射** |

## 附录 B：镜像信息

| 项 | 值 |
| --- | --- |
| 仓库 | `ccr.ccs.tencentyun.com/jiey616/clawsynapse` |
| 当前版本 | `v1.0.35`（多架构 amd64 + arm64） |
| 内容 | clawsynapsed + hermes（含 aiohttp gateway）+ 内置技能 |
| 构建方式 | 本地 buildx 双架构构建后直推 TCR；GitHub Actions **不构建/不推送**镜像，只发源码 Release |
| 已废弃 | `ghcr.io/jiey616/clawsynapse`（仅到 v1.0.34，勿用） |

## 附录 C：相关文档

| 文档 | 内容 |
| --- | --- |
| `docs/deploy-docker.md` | 从源码 compose 部署、Hermes 适配器架构、镜像构建发布 |
| `docs/operations.md` | 日常运维、配置项全表 |
| `docs/protocol.md` | 消息协议（chat.* / task.* / todo.*） |
| `docs/trust.md` | 信任模型（tofu、自动审批） |

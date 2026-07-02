# Linux 服务器部署指南 — ClawSynapse + Hermes Agent

## 前置条件

- Linux 服务器 (amd64 或 arm64)
- root 或 sudo 权限
- 网络可访问 GitHub 和 PyPI

## 步骤总览

```
1. 安装 hermes agent (Python)
2. 配置 hermes API Key + 模型
3. 安装 clawsynapse (二进制 + systemd 服务)
4. 部署 SKILL.md
5. 启动守护进程
6. 验证
```

---

## 1. 安装 Hermes Agent

```bash
# 官方一键安装 (自带 Python 3.11 + uv + 依赖)
curl -fsSL https://hermes-agent.nousresearch.com/install.sh | bash

# 加载环境变量
source ~/.bashrc

# 验证
hermes --version
```

安装完成后 hermes 位于 `~/.hermes/` 目录下。

---

## 2. 配置 Hermes API Key + 模型

```bash
# 编辑 hermes 配置
cat > ~/.hermes/config.yaml << 'EOF'
model:
  default: deepseek-v4-flash
  provider: deepseek
  base_url: https://api.deepseek.com
toolsets:
  - hermes-cli
agent:
  max_turns: 150
  gateway_timeout: 1800
EOF

# 写入 API Key
cat > ~/.hermes/.env << 'EOF'
DEEPSEEK_API_KEY=sk-你的key
DEEPSEEK_BASE_URL=https://api.deepseek.com
EOF

# 快速验证 hermes 能工作
hermes chat -q "hello" -Q
```

> 如果用其他模型 (OpenAI/OpenRouter 等)，参考 `hermes config` 命令修改。

---

## 3. 安装 ClawSynapse

```bash
# 一键安装: CLI + 守护进程 + systemd 服务
curl -fsSL https://raw.githubusercontent.com/jiey616/clawsynapse/main/scripts/install.sh | \
  bash -s -- --agent-adapter hermes --agent-role pm --version v1.0.10
```

这会完成：
- 下载 `clawsynapse` + `clawsynapsed` 二进制到 `/usr/local/bin/`
- 创建 `~/.clawsynapse/config.yaml` (含 `agentAdapter: hermes`, `agentRole: pm`)
- 注册 systemd 服务 `clawsynapsed.service` (默认自启)

### 非交互式参数说明

| 参数 | 说明 | 默认值 |
|------|------|--------|
| `--agent-adapter hermes` | 使用 hermes 适配器 | default |
| `--agent-role pm` | 角色为 PM (加载 tm-task-plan skill) | 空 |
| `--agent-role executor` | 角色为执行者 (加载 tm-task-exec skill) | 空 |
| `--version v1.0.10` | 指定版本 | latest |
| `--no-start` | 安装但不启动 | 默认启动 |
| `--nats-servers URL` | NATS 服务器地址 | nats://220.168.146.21:9414 |

---

## 4. 部署 SKILL.md

install.sh 不会部署 hermes skill，需要手动执行一次 init：

```bash
# init 检测到已有 config 会跳过配置创建，只部署 SKILL.md
clawsynapse init --agent-adapter hermes --agent-role pm

# 验证 SKILL.md 已部署
ls -la ~/.hermes/skills/clawsynapse/SKILL.md
```

---

## 5. 启动守护进程

```bash
# install.sh 默认已自启，如需手动操作：
sudo systemctl start clawsynapsed

# 查看状态
sudo systemctl status clawsynapsed

# 查看日志
sudo journalctl -u clawsynapsed -f
```

---

## 6. 验证

```bash
# clawsynapse 版本
clawsynapse version

# 健康检查
clawsynapse health

# 查看 config
cat ~/.clawsynapse/config.yaml

# 确认 hermes 在 PATH 中
which hermes

# 确认 SKILL.md
cat ~/.hermes/skills/clawsynapse/SKILL.md | head -5
```

---

## 常用运维命令

```bash
# 重启守护进程
sudo systemctl restart clawsynapsed

# 停止
sudo systemctl stop clawsynapsed

# 查看实时日志
sudo journalctl -u clawsynapsed -f --since "1 min ago"

# 修改配置后重启
vim ~/.clawsynapse/config.yaml
sudo systemctl restart clawsynapsed

# 升级 clawsynapse
curl -fsSL https://raw.githubusercontent.com/jiey616/clawsynapse/main/scripts/install.sh | \
  bash -s -- --version v1.0.11

# 升级 hermes
hermes update

# 完全卸载
curl -fsSL https://raw.githubusercontent.com/jiey616/clawsynapse/main/scripts/install.sh | \
  bash -s -- --daemon --uninstall --purge
```

---

## 配置文件参考

### ~/.clawsynapse/config.yaml

```yaml
natsServers:
  - nats://220.168.146.21:9414
localApiAddr: 127.0.0.1:18080
trustMode: tofu
agentAdapter: hermes
agentRole: pm
heartbeatInterval: 15s
announceTtl: 30s
dataDir: ~/.clawsynapse
identityKeyPath: ~/.clawsynapse/identity.key
identityPubPath: ~/.clawsynapse/identity.pub
deliverablePrefixes:
  - chat
  - task
  - todo
transferDir: ~/.clawsynapse/transfers
transferMaxFileSize: 104857600
transferTtl: 24h
logLevel: info
logFormat: json
```

### 关键字段

| 字段 | 说明 |
|------|------|
| `agentAdapter: hermes` | 使用 hermes 适配器 |
| `agentRole: pm` | PM 角色 (加载 tm-task-plan skill) |
| `deliverablePrefixes` | hermes 需要 `todo` 前缀接收 todo 消息 |
| `natsServers` | TrustMesh 消息总线地址 |

---

## 故障排查

### hermes 不在 PATH

```bash
# 检查
which hermes

# 如果找不到，手动加到 PATH
echo 'export PATH="$HOME/.hermes/bin:$PATH"' >> ~/.bashrc
source ~/.bashrc
```

### 守护进程启动失败

```bash
# 查看详细日志
sudo journalctl -u clawsynapsed -n 50 --no-pager

# 手动前台运行看报错
clawsynapsed --config ~/.clawsynapse/config.yaml
```

### hermes chat 报错

```bash
# 检查 API Key
cat ~/.hermes/.env | grep API_KEY

# 测试 hermes
hermes chat -q "test" -Q
```

### SKILL.md 未部署

```bash
# 手动部署
clawsynapse init --agent-adapter hermes --agent-role pm

# 或手动复制
mkdir -p ~/.hermes/skills/clawsynapse/
# 从 GitHub 下载
curl -fsSL https://raw.githubusercontent.com/jiey616/clawsynapse/main/skills/clawsynapse/SKILL.md \
  -o ~/.hermes/skills/clawsynapse/SKILL.md
```

# ─────────────────────────────────────────────
# ClawSynapse + Hermes Agent Docker Image
# 同一个容器内运行 clawsynapse 守护进程 + hermes agent CLI
# ─────────────────────────────────────────────
# Build:  docker build -t clawsynapse:latest .
# Run:    docker run -d --name clawsynapse \
#           -v clawsynapse-data:/root/.clawsynapse \
#           -v hermes-data:/root/.hermes \
#           -e CLAWSYNAPSE_AGENT_ROLE=pm \
#           -e DEEPSEEK_API_KEY=sk-xxx \
#           -p 8080:8080 \
#           clawsynapse:latest
# ─────────────────────────────────────────────

# ── Stage 1: Build clawsynapse ──
# Use the glibc-based (bookworm) Go image instead of alpine. The alpine
# variant on some hosts (especially arm64) can hit a runtime panic in the
# Go compiler during the build, which manifests as an obscure GC stack trace
# and a non-zero build exit code.
FROM --platform=$BUILDPLATFORM golang:1.25 AS builder

ARG TARGETARCH
WORKDIR /build
COPY go.mod go.sum ./
RUN GOPROXY=https://goproxy.cn,direct go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -ldflags="-s -w" -o /build/clawsynapse ./cmd/clawsynapse/ && \
    go build -ldflags="-s -w" -o /build/clawsynapsed ./cmd/clawsynapsed/

# ── Stage 2: Runtime with Hermes ──
FROM python:3.11-slim

LABEL org.opencontainers.image.source=https://github.com/jiey616/clawsynapse

# ── System dependencies (hermes install needs: git, curl, bash, build tools) ──
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl git ca-certificates bash procps \
    build-essential python3-dev python3-venv libffi-dev \
    && rm -rf /var/lib/apt/lists/*

# Install PyYAML so docker-entrypoint can safely rewrite Hermes config.yaml
RUN pip install --no-cache-dir PyYAML

# ── Install Hermes Agent (Chinese mirror) ──
# Mirror script may contain ANSI color codes; strip them before bash execution.
# --skip-setup: skip interactive API key configuration
RUN curl -fsSL https://res1.hermesagent.org.cn/install.sh -o /tmp/install.sh && \
    python3 -c "import re; s=open('/tmp/install.sh').read(); s=re.sub(r'\033\[[0-9;]*m', '', s); open('/tmp/install.sh','w').write(s)" && \
    bash /tmp/install.sh --skip-setup

# Make the hermes symlink (created by install script in ~/.local/bin) discoverable
ENV PATH="/root/.local/bin:${PATH}"
# ── Copy clawsynapse binaries ──
COPY --from=builder /build/clawsynapse /usr/local/bin/clawsynapse
COPY --from=builder /build/clawsynapsed /usr/local/bin/clawsynapsed

# ── Embed SKILL.md (belt & suspenders: also deployed by init --agent-adapter hermes) ──
COPY cmd/clawsynapse/skill_assets/clawsynapse/SKILL.md /usr/local/share/clawsynapse/SKILL.md

# ── Entrypoint ──
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

# Healthcheck
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD clawsynapse version || exit 1

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]

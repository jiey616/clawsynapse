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
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux \
    go build -ldflags="-s -w" -o /build/clawsynapse ./cmd/clawsynapse/ && \
    go build -ldflags="-s -w" -o /build/clawsynapsed ./cmd/clawsynapsed/

# ── Stage 2: Runtime with Hermes ──
FROM python:3.11-slim

LABEL org.opencontainers.image.source=https://github.com/jiey616/clawsynapse

# ── System dependencies (hermes install script needs: git, curl, bash) ──
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl git ca-certificates bash procps \
    && rm -rf /var/lib/apt/lists/*

# ── Install Hermes Agent (official install script) ──
# The script detects "root" user → installs binary to /usr/local/lib/hermes-agent/
# and creates /usr/local/bin/hermes. Data dir: /root/.hermes/
# --non-interactive: skip setup wizard
# --skip-setup: skip interactive API key configuration
RUN curl -fsSL https://hermes-agent.nousresearch.com/install.sh | \
    bash -s -- --non-interactive --skip-setup

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

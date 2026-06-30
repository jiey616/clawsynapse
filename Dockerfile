# ─────────────────────────────────────────────
# ClawSynapse + Hermes Agent Docker Image
# ─────────────────────────────────────────────
# Build:  docker build -t clawsynapse:latest .
# Run:    docker run -d --name cs \
#           -v clawsynapse-data:/root \
#           -e HERMES_API_KEY=<key> \
#           -e CLAWSYNAPSE_NODE_ID=<id> \
#           -e CLAWSYNAPSE_NODE_KEY=<key> \
#           -e CLAWSYNAPSE_AGENT_ROLE=pm \
#           -p 8080:8080 \
#           clawsynapse:latest
# ─────────────────────────────────────────────

# ── Stage 1: Build clawsynapse (Linux amd64) ──
FROM --platform=linux/amd64 golang:1.22-alpine AS builder

RUN apk add --no-cache git

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -o /build/clawsynapse ./cmd/clawsynapse/ && \
    go build -o /build/clawsynapsed ./cmd/clawsynapsed/

# ── Stage 2: Runtime image ──
FROM --platform=linux/amd64 python:3.11-slim

LABEL org.opencontainers.image.source=https://github.com/jiey616/clawsynapse

# ── Install system dependencies ──
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl \
    git \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# ── Install hermes-agent (Python) ──
# hermes is installed as a Python package in a venv
RUN python3 -m venv /opt/hermes-venv && \
    /opt/hermes-venv/bin/pip install --no-cache-dir hermes-agent

# Make hermes CLI available in PATH
ENV PATH="/opt/hermes-venv/bin:${PATH}"

# ── Copy clawsynapse binaries ──
COPY --from=builder /build/clawsynapse /usr/local/bin/clawsynapse
COPY --from=builder /build/clawsynapsed /usr/local/bin/clawsynapsed

# ── Embed SKILL.md (go:embed copy for init deployment) ──
# init --agent-adapter hermes will deploy this to ~/.hermes/skills/clawsynapse/
COPY cmd/clawsynapse/skill_assets/clawsynapse/SKILL.md /usr/local/share/clawsynapse/SKILL.md

# ── Config directory ──
RUN mkdir -p /etc/clawsynapse

# ── Volume for data persistence ──
VOLUME ["/root/.clawsynapse", "/root/.hermes"]

# ── Entrypoint ──
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]

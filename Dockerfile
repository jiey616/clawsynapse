# ─────────────────────────────────────────────
# ClawSynapse + Hermes Agent Docker Image
# 同一个容器内运行 clawsynapse 守护进程 + hermes agent CLI
# ─────────────────────────────────────────────
# Build (CI): the Go binaries are NOT compiled here. The release workflow
# cross-compiles linux/amd64 + linux/arm64 binaries on a native amd64 runner
# (fast) and injects them via a named build context named `prebuilt`:
#
#   docker buildx build --build-context prebuilt=./bin-linux \
#     --platform linux/amd64,linux/arm64 -t clawsynapse:latest .
#
# Why: QEMU-emulated arm64 Go compilation (esp. the modernc.org/sqlite dep)
# exceeded the 6h job timeout. Prebuilt binaries keep docker builds well
# under that.
#
# Build (local): use Dockerfile.local (keeps the in-image builder stage).
# ─────────────────────────────────────────────

# ── Stage: Runtime with Hermes ──
# The `prebuilt` build context must contain:
#   clawsynapse-linux-<arch>  (amd64|arm64)
#   clawsynapsed-linux-<arch>
FROM python:3.11-slim

ARG TARGETARCH

LABEL org.opencontainers.image.source=https://github.com/jiey616/clawsynapse

# Make the hermes symlink (created by install script in ~/.local/bin) discoverable
ENV PATH="/root/.local/bin:${PATH}"

# ── System deps + Hermes install + cleanup, all in ONE layer ──
# build-essential / python3-dev / libffi-dev are ONLY needed to COMPILE hermes's
# native python wheels (cffi, cryptography, ...). Once hermes is installed we
# purge them so they never land in the final image (saves ~300-450MB).
# Build-time pip via China mirror: hermes install.sh + aiohttp pull many wheels
# from files.pythonhosted.org, whose read timeouts kill arm64/QEMU builds
# (v1.0.32 first build attempt died this way). Same rationale as GOPROXY=goproxy.cn.
# CI (overseas runners) can override with --build-arg PIP_INDEX_URL=https://pypi.org/simple
ARG PIP_INDEX_URL=https://pypi.tuna.tsinghua.edu.cn/simple
ARG PIP_DEFAULT_TIMEOUT=120
ENV PIP_INDEX_URL=${PIP_INDEX_URL} \
    PIP_DEFAULT_TIMEOUT=${PIP_DEFAULT_TIMEOUT}
RUN apt-get update && apt-get install -y --no-install-recommends \
      curl git ca-certificates bash procps python3-venv \
      build-essential python3-dev libffi-dev \
 && pip install --no-cache-dir PyYAML \
 && curl -fsSL https://hermes-agent.nousresearch.com/install.sh | bash -s -- --skip-setup \
 && HERMES_BIN="$(command -v hermes 2>/dev/null || true)" \
 && if [ -z "$HERMES_BIN" ]; then \
       HERMES_BIN="$(ls -d /root/.hermes/*/venv/bin/hermes 2>/dev/null | head -n1)"; \
    fi \
 && HERMES_REAL="$(readlink -f "$HERMES_BIN" 2>/dev/null || echo "$HERMES_BIN")" \
 && HERMES_VENV_PY="$(dirname "$HERMES_REAL")/python" \
 && if [ -x "$HERMES_VENV_PY" ]; then \
       echo "[docker] installing aiohttp into hermes venv: $HERMES_VENV_PY"; \
       "$HERMES_VENV_PY" -m pip install --no-cache-dir aiohttp; \
    else \
       echo "[docker] WARN: hermes venv python not found at $HERMES_VENV_PY; installing aiohttp into system python"; \
       pip install --no-cache-dir aiohttp; \
    fi \
 && apt-get purge -y --auto-remove build-essential python3-dev libffi-dev \
 && rm -rf /var/lib/apt/lists/* /root/.cache/pip /root/.cache/uv /root/.cache/node-gyp /tmp/* \
 && find /root/.hermes -type d -name '__pycache__' -prune -exec rm -rf {} + 2>/dev/null \
 && rm -rf /usr/local/lib/hermes-agent/tests /usr/local/lib/hermes-agent/website 2>/dev/null \
 && (command -v hermes >/dev/null 2>&1 || { echo "[docker] ERROR: hermes binary not found after install"; exit 1; })

# ── Prebuild the Hermes web dashboard (web_dist) ──
# hermes dashboard --skip-build serves hermes_cli/web_dist directly (vite outDir is a
# sibling of web/, NOT web/dist). If absent, the dashboard process launches but silently
# binds nothing — observed on arm64/SZJT (9119=000 there while amd64 self-healed via a
# runtime vite build). Build it deterministically for BOTH arches so --skip-build works
# everywhere. node/npm + web/node_modules are already present (hermes install.sh git method).
RUN cd /usr/local/lib/hermes-agent/web \
 && npm run build

# ── Copy prebuilt clawsynapse binaries (from the `prebuilt` build context) ──
COPY --from=prebuilt clawsynapse-linux-${TARGETARCH} /usr/local/bin/clawsynapse
COPY --from=prebuilt clawsynapsed-linux-${TARGETARCH} /usr/local/bin/clawsynapsed

# ── Embed SKILL.md (belt & suspenders: also deployed by init --agent-adapter hermes) ──
COPY cmd/clawsynapse/skill_assets/clawsynapse/SKILL.md /usr/local/share/clawsynapse/SKILL.md

# ── Embed TrustMesh business skills (tm-*) for hermes external_dirs loading ──
# These are copied verbatim into hermes's skills dir by docker-entrypoint.sh so
# they load for the matching agent role (pm / executor). They live under a
# non-volume path here and are materialized into /root/.hermes/skills at runtime.
COPY cmd/clawsynapse/skill_assets/tm-task-plan/SKILL.md /usr/local/share/clawsynapse/skills/tm-task-plan/SKILL.md
COPY cmd/clawsynapse/skill_assets/tm-task-exec/SKILL.md /usr/local/share/clawsynapse/skills/tm-task-exec/SKILL.md
COPY cmd/clawsynapse/skill_assets/tm-meeting-host/SKILL.md /usr/local/share/clawsynapse/skills/tm-meeting-host/SKILL.md
COPY cmd/clawsynapse/skill_assets/tm-meeting-participant/SKILL.md /usr/local/share/clawsynapse/skills/tm-meeting-participant/SKILL.md

# ── Entrypoint ──
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

# Healthcheck
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD clawsynapse version || exit 1

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]

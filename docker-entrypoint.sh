#!/usr/bin/env bash
# docker-entrypoint.sh — ClawSynapse + Hermes container entrypoint
# Handles first-run init and daemon startup
set -e

CLAWSYNAPSE_CONFIG="${CLAWSYNAPSE_CONFIG:-/root/.clawsynapse/config.yaml}"
HERMES_SKILL_DIR="${HERMES_SKILL_DIR:-/root/.hermes/skills/clawsynapse}"
SKILL_SRC="${SKILL_SRC:-/usr/local/share/clawsynapse/SKILL.md}"

# ── Helper: log ──
log() { echo "[entrypoint] $*"; }

# ── Step 1: First-time init (if config doesn't exist) ──
if [ ! -f "$CLAWSYNAPSE_CONFIG" ]; then
    log "First run — initializing clawsynapse with hermes adapter..."

    INIT_ARGS=(--agent-adapter hermes)

    [ -n "$CLAWSYNAPSE_NODE_ID" ]     && INIT_ARGS+=(--node-id "$CLAWSYNAPSE_NODE_ID")
    [ -n "$CLAWSYNAPSE_NODE_KEY" ]   && INIT_ARGS+=(--node-key "$CLAWSYNAPSE_NODE_KEY")
    [ -n "$CLAWSYNAPSE_AGENT_ROLE" ] && INIT_ARGS+=(--agent-role "$CLAWSYNAPSE_AGENT_ROLE")
    [ -n "$CLAWSYNAPSE_API_LISTEN" ] && INIT_ARGS+=(--api-listen "$CLAWSYNAPSE_API_LISTEN")

    clawsynapse init "${INIT_ARGS[@]}"

    # ── Deploy SKILL.md to hermes skills dir (belt & suspenders) ──
    if [ -f "$SKILL_SRC" ]; then
        mkdir -p "$HERMES_SKILL_DIR"
        cp "$SKILL_SRC" "$HERMES_SKILL_DIR/SKILL.md"
        log "Deployed SKILL.md → $HERMES_SKILL_DIR/"
    fi

    log "Init complete."
else
    log "Config found at $CLAWSYNAPSE_CONFIG — skipping init."
fi

# ── Step 2: Ensure SKILL.md is deployed (in case init was done manually) ──
if [ ! -f "$HERMES_SKILL_DIR/SKILL.md" ] && [ -f "$SKILL_SRC" ]; then
    mkdir -p "$HERMES_SKILL_DIR"
    cp "$SKILL_SRC" "$HERMES_SKILL_DIR/SKILL.md"
    log "Deployed SKILL.md → $HERMES_SKILL_DIR/ (post-init fixup)"
fi

# ── Step 3: Override config from environment (runtime overrides) ──
if [ -n "$CLAWSYNAPSE_API_LISTEN" ]; then
    # Patch config.yaml in-place (simple yq-less approach)
    sed -i "s|^apiListen:.*|apiListen: ${CLAWSYNAPSE_API_LISTEN}|" "$CLAWSYNAPSE_CONFIG" 2>/dev/null || true
fi

# ── Step 4: Start clawsynapse daemon ──
log "Starting clawsynapsed..."
exec clawsynapsed start

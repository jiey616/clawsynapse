#!/bin/sh
# 极简 entrypoint：不依赖 hermes agent，直接以 clawsynapse 用户启动 clawsynapsed。
# 适用于 TrustMesh webhook adapter 场景（clawsynapse 仅作为网关/传输层）。
set -eu

STATE_DIR="${DATA_DIR:-/var/lib/clawsynapse}"
TRANSFER_DIR="${TRANSFER_DIR:-/var/lib/trustmesh-transfers}"

mkdir -p "$STATE_DIR" "$TRANSFER_DIR"
chown -R clawsynapse:clawsynapse "$STATE_DIR" "$TRANSFER_DIR"
chmod 755 "$TRANSFER_DIR"

exec su-exec clawsynapse /usr/local/bin/clawsynapsed "$@"

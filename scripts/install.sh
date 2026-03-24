#!/usr/bin/env bash
#
# ClawSynapse installer for CLI and daemon service.
#
# Usage:
#   CLI only:
#     ./scripts/install.sh
#     curl -fsSL https://raw.githubusercontent.com/yuanjun5681/clawsynapse/main/scripts/install.sh | bash -s -- --cli
#
#   CLI + daemon service (default):
#     ./scripts/install.sh --all --node-id node-alpha
#     curl -fsSL https://raw.githubusercontent.com/yuanjun5681/clawsynapse/main/scripts/install.sh | \
#       bash -s -- --node-id node-alpha
#
set -euo pipefail

CLI_BINARY="clawsynapse"
DAEMON_BINARY="clawsynapsed"
DEFAULT_REPO="yuanjun5681/clawsynapse"
DEFAULT_INSTALL_DIR="/usr/local/bin"
FALLBACK_INSTALL_DIR="${HOME}/.local/bin"
DEFAULT_CONFIG_ROOT="${HOME}/.clawsynapse"
DEFAULT_CONFIG_PATH="${DEFAULT_CONFIG_ROOT}/config.yaml"
DEFAULT_DATA_DIR="${DEFAULT_CONFIG_ROOT}"
DEFAULT_TRANSFER_DIR="${DEFAULT_DATA_DIR}/transfers"
DEFAULT_NATS_SERVERS="nats://220.168.146.21:9414"
DEFAULT_LOCAL_API_ADDR="127.0.0.1:18080"
DEFAULT_TRUST_MODE="tofu"
DEFAULT_AGENT_ADAPTER="default"
DEFAULT_DELIVERABLE_PREFIXES="chat,task"
DEFAULT_HEARTBEAT="15s"
DEFAULT_ANNOUNCE_TTL="30s"
DEFAULT_TRANSFER_MAX_FILE_SIZE="104857600"
DEFAULT_TRANSFER_TTL="24h"
DEFAULT_LOG_LEVEL="info"
DEFAULT_LOG_FORMAT="json"
SYSTEMD_UNIT_NAME="clawsynapsed.service"
LAUNCHD_LABEL="io.github.yuanjun5681.clawsynapse.clawsynapsed"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m'

info()  { printf "${GREEN}[info]${NC}  %s\n" "$*"; }
warn()  { printf "${YELLOW}[warn]${NC}  %s\n" "$*"; }
error() { printf "${RED}[error]${NC} %s\n" "$*" >&2; exit 1; }

is_interactive_shell() {
    [ -t 1 ] && [ -r /dev/tty ]
}

usage() {
    cat <<'EOF'
ClawSynapse installer

Usage:
  ./scripts/install.sh [options]
  curl -fsSL https://raw.githubusercontent.com/yuanjun5681/clawsynapse/main/scripts/install.sh | bash -s -- [options]

Install modes:
  (default)            Install both CLI and daemon service
  --cli                Install CLI only
  --daemon             Install daemon binary and register the service
  --all                Install both CLI and daemon service

Common options:
  --install-dir DIR    Install binaries into DIR
  --version VERSION    Install a specific release tag instead of latest
  --repo OWNER/REPO    Override GitHub repo (default: yuanjun5681/clawsynapse)
  --uninstall          Uninstall the selected components
  --purge              With --daemon --uninstall, also remove config/data files
  --no-start           Install the daemon service but do not start it
  -h, --help           Show this help

Daemon bootstrap options:
  --node-id ID
  --nats-servers URLS
  --local-api-addr ADDR
  --trust-mode MODE
  --agent-adapter NAME
  --webhook-url URL
  --deliverable-prefixes CSV
  --config PATH
  --data-dir PATH
  --transfer-dir PATH
  --heartbeat DURATION
  --announce-ttl DURATION
  --transfer-max-file-size BYTES
  --transfer-ttl DURATION
  --log-level LEVEL
  --log-format FORMAT

Examples:
  Install CLI and daemon as a service:
    ./scripts/install.sh --node-id node-alpha --nats-servers nats://127.0.0.1:4222
    curl -fsSL https://raw.githubusercontent.com/yuanjun5681/clawsynapse/main/scripts/install.sh | bash

  Install CLI only:
    ./scripts/install.sh --cli

  Install daemon only from GitHub Release:
    curl -fsSL https://raw.githubusercontent.com/yuanjun5681/clawsynapse/main/scripts/install.sh | \
      bash -s -- --daemon --node-id node-alpha

  Uninstall the daemon service:
    ./scripts/install.sh --daemon --uninstall
EOF
}

trim() {
    local value="$1"
    value="${value#"${value%%[![:space:]]*}"}"
    value="${value%"${value##*[![:space:]]}"}"
    printf '%s' "$value"
}

append_unique_path() {
    local current="$1"
    local candidate="$2"

    candidate="$(trim "$candidate")"
    if [ -z "$candidate" ]; then
        printf '%s' "$current"
        return 0
    fi

    case ":$current:" in
        *":$candidate:"*)
            printf '%s' "$current"
            ;;
        *)
            if [ -n "$current" ]; then
                printf '%s:%s' "$current" "$candidate"
            else
                printf '%s' "$candidate"
            fi
            ;;
    esac
}

build_service_path() {
    local path_value=""
    local candidate

    for candidate in \
        "$INSTALL_DIR" \
        "$HOME/.local/bin" \
        "$HOME/.npm-global/bin" \
        "/opt/homebrew/bin" \
        "/opt/homebrew/sbin" \
        "/usr/local/bin" \
        "/usr/local/sbin" \
        "/usr/bin" \
        "/bin" \
        "/usr/sbin" \
        "/sbin"
    do
        path_value="$(append_unique_path "$path_value" "$candidate")"
    done

    printf '%s\n' "$path_value"
}

expand_path() {
    local path="$1"

    if [ -z "$path" ]; then
        printf '%s' "$path"
        return 0
    fi

    case "$path" in
        "~")
            printf '%s\n' "$HOME"
            ;;
        "~/"*)
            printf '%s\n' "${HOME}/${path#~/}"
            ;;
        /*)
            printf '%s\n' "$path"
            ;;
        *)
            printf '%s\n' "${PWD}/${path}"
            ;;
    esac
}

prompt_value() {
    local label="$1"
    local current="${2:-}"
    local answer

    if [ -n "$current" ]; then
        printf "%s [%s]: " "$label" "$current" >&2
    else
        printf "%s: " "$label" >&2
    fi

    if [ -t 0 ]; then
        IFS= read -r answer || true
    else
        IFS= read -r answer </dev/tty || true
    fi
    answer="$(trim "$answer")"
    if [ -n "$answer" ]; then
        printf '%s' "$answer"
        return 0
    fi
    printf '%s' "$current"
}

prompt_required() {
    local label="$1"
    local current="${2:-}"
    local answer

    while :; do
        answer="$(prompt_value "$label" "$current")"
        if [ -n "$answer" ]; then
            printf '%s' "$answer"
            return 0
        fi
        warn "${label} is required"
    done
}

prompt_choice() {
    local label="$1"
    local current="$2"
    shift 2

    local answer choice
    while :; do
        answer="$(prompt_value "${label} ($(printf '%s/' "$@" | sed 's:/$::'))" "$current")"
        for choice in "$@"; do
            if [ "$answer" = "$choice" ]; then
                printf '%s' "$answer"
                return 0
            fi
        done
        warn "choose one of: $*"
    done
}

maybe_prompt_daemon_config() {
    if [ "$ACTION" != "install" ] || [ "$INSTALL_DAEMON" -ne 1 ]; then
        return 0
    fi
    if ! is_interactive_shell; then
        return 0
    fi
    if [ -n "$NODE_ID" ]; then
        return 0
    fi

    info "interactive daemon configuration"
    info "reading answers from your terminal"
    NODE_ID="$(prompt_required "Node ID" "$NODE_ID")"
    NATS_SERVERS="$(prompt_value "NATS servers (comma-separated)" "$NATS_SERVERS")"
    AGENT_ADAPTER="$(prompt_choice "Agent adapter" "$AGENT_ADAPTER" default openclaw webhook)"
    if [ "$AGENT_ADAPTER" = "webhook" ]; then
        WEBHOOK_URL="$(prompt_required "Webhook URL" "$WEBHOOK_URL")"
    else
        WEBHOOK_URL=""
    fi
    TRUST_MODE="$(prompt_choice "Trust mode" "$TRUST_MODE" open tofu explicit)"
    LOCAL_API_ADDR="$(prompt_value "Local API address" "$LOCAL_API_ADDR")"
    DELIVERABLE_PREFIXES="$(prompt_value "Deliverable prefixes (comma-separated)" "$DELIVERABLE_PREFIXES")"
}

detect_platform() {
    local os arch

    case "$(uname -s)" in
        Darwin) os="darwin" ;;
        Linux)  os="linux" ;;
        *)      error "unsupported operating system: $(uname -s)" ;;
    esac

    case "$(uname -m)" in
        x86_64|amd64)  arch="amd64" ;;
        arm64|aarch64) arch="arm64" ;;
        *)             error "unsupported architecture: $(uname -m)" ;;
    esac

    printf '%s-%s\n' "$os" "$arch"
}

require_sudo() {
    if [ -n "${SUDO:-}" ]; then
        return 0
    fi
    if ! command -v sudo >/dev/null 2>&1; then
        error "this operation requires sudo, but sudo is not available"
    fi
    SUDO="sudo"
}

detect_install_dir() {
    if [ -n "${INSTALL_DIR:-}" ]; then
        INSTALL_DIR="$(expand_path "$INSTALL_DIR")"
        return 0
    fi

    if [ -w "$DEFAULT_INSTALL_DIR" ] || { [ ! -e "$DEFAULT_INSTALL_DIR" ] && [ -w "$(dirname "$DEFAULT_INSTALL_DIR")" ]; }; then
        INSTALL_DIR="$DEFAULT_INSTALL_DIR"
        return 0
    fi

    if command -v sudo >/dev/null 2>&1; then
        INSTALL_DIR="$DEFAULT_INSTALL_DIR"
        require_sudo
        return 0
    fi

    INSTALL_DIR="$FALLBACK_INSTALL_DIR"
}

ensure_install_dir() {
    local dir="$1"
    if [ -w "$(dirname "$dir")" ] || { [ -d "$dir" ] && [ -w "$dir" ]; }; then
        install -d -m 755 "$dir"
        return 0
    fi

    require_sudo
    ${SUDO} install -d -m 755 "$dir"
}

download_to() {
    local url="$1"
    local dst="$2"

    if command -v curl >/dev/null 2>&1; then
        curl -fsSL -o "$dst" "$url" || error "download failed: $url"
        return 0
    fi
    if command -v wget >/dev/null 2>&1; then
        wget -qO "$dst" "$url" || error "download failed: $url"
        return 0
    fi
    error "curl or wget is required"
}

find_local_artifact() {
    local binary="$1"
    local platform="$2"
    local candidate

    for candidate in "dist/${binary}-${platform}" "./dist/${binary}-${platform}"; do
        if [ -f "$candidate" ]; then
            printf '%s\n' "$candidate"
            return 0
        fi
    done

    return 1
}

install_binary() {
    local binary="$1"
    local platform="$2"
    local dest="${INSTALL_DIR}/${binary}"
    local src
    local tmpfile
    local url

    if src="$(find_local_artifact "$binary" "$platform")"; then
        info "installing ${binary} from local artifact: ${src}"
        if [ -n "${SUDO:-}" ]; then
            ${SUDO} install -m 755 "$src" "$dest"
        else
            install -m 755 "$src" "$dest"
        fi
        info "installed: ${dest}"
        return 0
    fi

    tmpfile="$(mktemp)"

    if [ "$VERSION" = "latest" ]; then
        url="https://github.com/${GITHUB_REPO}/releases/latest/download/${binary}-${platform}"
    else
        url="https://github.com/${GITHUB_REPO}/releases/download/${VERSION}/${binary}-${platform}"
    fi

    info "downloading ${binary}: ${url}"
    download_to "$url" "$tmpfile"

    if [ -n "${SUDO:-}" ]; then
        ${SUDO} install -m 755 "$tmpfile" "$dest"
    else
        install -m 755 "$tmpfile" "$dest"
    fi
    rm -f "$tmpfile"
    info "installed: ${dest}"
}

ensure_config_root() {
    install -d -m 700 "$CONFIG_ROOT"
    install -d -m 700 "$DATA_DIR"
    install -d -m 700 "$TRANSFER_DIR"
    install -d -m 700 "${DATA_DIR}/log"
}

yaml_write_list() {
    local csv="$1"
    local item
    local trimmed

    IFS=',' read -r -a items <<<"$csv"
    for item in "${items[@]}"; do
        trimmed="$(trim "$item")"
        [ -n "$trimmed" ] || continue
        printf '  - %s\n' "$trimmed"
    done
}

write_config_if_missing() {
    local tmpfile

    if [ -f "$CONFIG_PATH" ]; then
        info "keeping existing config: ${CONFIG_PATH}"
        return 0
    fi

    if [ -z "$NODE_ID" ]; then
        error "daemon install requires --node-id the first time so ${CONFIG_PATH} can be created"
    fi
    if [ "$AGENT_ADAPTER" = "webhook" ] && [ -z "$WEBHOOK_URL" ]; then
        error "--webhook-url is required when --agent-adapter webhook is used"
    fi

    tmpfile="$(mktemp)"

    {
        printf 'nodeId: %s\n' "$NODE_ID"
        printf 'natsServers:\n'
        yaml_write_list "$NATS_SERVERS"
        printf 'localApiAddr: %s\n' "$LOCAL_API_ADDR"
        printf 'trustMode: %s\n' "$TRUST_MODE"
        printf 'agentAdapter: %s\n' "$AGENT_ADAPTER"
        if [ -n "$WEBHOOK_URL" ]; then
            printf 'webhookUrl: %s\n' "$WEBHOOK_URL"
        fi
        printf 'heartbeatInterval: %s\n' "$HEARTBEAT_INTERVAL"
        printf 'announceTtl: %s\n' "$ANNOUNCE_TTL"
        printf 'dataDir: %s\n' "$DATA_DIR"
        printf 'identityKeyPath: %s\n' "${DATA_DIR}/identity.key"
        printf 'identityPubPath: %s\n' "${DATA_DIR}/identity.pub"
        printf 'deliverablePrefixes:\n'
        yaml_write_list "$DELIVERABLE_PREFIXES"
        printf 'transferDir: %s\n' "$TRANSFER_DIR"
        printf 'transferMaxFileSize: %s\n' "$TRANSFER_MAX_FILE_SIZE"
        printf 'transferTtl: %s\n' "$TRANSFER_TTL"
        printf 'logLevel: %s\n' "$LOG_LEVEL"
        printf 'logFormat: %s\n' "$LOG_FORMAT"
    } >"$tmpfile"

    install -m 600 "$tmpfile" "$CONFIG_PATH"
    rm -f "$tmpfile"
    info "created config: ${CONFIG_PATH}"
}

validate_daemon_config() {
    local daemon_path="${INSTALL_DIR}/${DAEMON_BINARY}"
    info "validating daemon config"
    "$daemon_path" --config "$CONFIG_PATH" --check-config >/dev/null
}

service_manager() {
    case "$(uname -s)" in
        Linux)
            command -v systemctl >/dev/null 2>&1 || error "systemctl is required to install the daemon service on Linux"
            printf '%s\n' "systemd"
            ;;
        Darwin)
            command -v launchctl >/dev/null 2>&1 || error "launchctl is required to install the daemon service on macOS"
            printf '%s\n' "launchd"
            ;;
        *)
            error "unsupported operating system: $(uname -s)"
            ;;
    esac
}

install_systemd_service() {
    local daemon_path="${INSTALL_DIR}/${DAEMON_BINARY}"
    local unit_path="/etc/systemd/system/${SYSTEMD_UNIT_NAME}"
    local service_path
    local tmpfile
    local service_user
    local service_group

    service_user="$(id -un)"
    service_group="$(id -gn)"
    service_path="$(build_service_path)"
    require_sudo

    tmpfile="$(mktemp)"

    cat >"$tmpfile" <<EOF
[Unit]
Description=ClawSynapse Daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${service_user}
Group=${service_group}
Environment=HOME=${HOME}
Environment=PATH=${service_path}
WorkingDirectory=${HOME}
ExecStart=${daemon_path} --config ${CONFIG_PATH}
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF

    ${SUDO} install -m 644 "$tmpfile" "$unit_path"
    rm -f "$tmpfile"
    ${SUDO} systemctl daemon-reload
    if [ "$AUTO_START" -eq 1 ]; then
        ${SUDO} systemctl enable --now "${SYSTEMD_UNIT_NAME}"
        info "systemd service enabled and started: ${SYSTEMD_UNIT_NAME}"
    else
        ${SUDO} systemctl enable "${SYSTEMD_UNIT_NAME}"
        info "systemd service enabled: ${SYSTEMD_UNIT_NAME}"
    fi
}

launchd_domain() {
    local uid
    uid="$(id -u)"
    if launchctl print "gui/${uid}" >/dev/null 2>&1; then
        printf 'gui/%s\n' "$uid"
        return 0
    fi
    printf 'user/%s\n' "$uid"
}

install_launchd_service() {
    local daemon_path="${INSTALL_DIR}/${DAEMON_BINARY}"
    local plist_dir="${HOME}/Library/LaunchAgents"
    local plist_path="${plist_dir}/${LAUNCHD_LABEL}.plist"
    local service_path
    local tmpfile
    local domain

    install -d -m 755 "$plist_dir"
    service_path="$(build_service_path)"
    tmpfile="$(mktemp)"

    cat >"$tmpfile" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "https://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>${LAUNCHD_LABEL}</string>
  <key>ProgramArguments</key>
  <array>
    <string>${daemon_path}</string>
    <string>--config</string>
    <string>${CONFIG_PATH}</string>
  </array>
  <key>WorkingDirectory</key>
  <string>${HOME}</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>${service_path}</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>${DATA_DIR}/log/clawsynapsed.stdout.log</string>
  <key>StandardErrorPath</key>
  <string>${DATA_DIR}/log/clawsynapsed.stderr.log</string>
</dict>
</plist>
EOF

    install -m 644 "$tmpfile" "$plist_path"
    rm -f "$tmpfile"
    domain="$(launchd_domain)"

    launchctl bootout "$domain" "$plist_path" >/dev/null 2>&1 || true
    if [ "$AUTO_START" -eq 1 ]; then
        launchctl bootstrap "$domain" "$plist_path"
        launchctl enable "${domain}/${LAUNCHD_LABEL}" >/dev/null 2>&1 || true
        launchctl kickstart -k "${domain}/${LAUNCHD_LABEL}" >/dev/null 2>&1 || true
        info "launchd service loaded and started: ${LAUNCHD_LABEL}"
    else
        info "launchd plist installed: ${plist_path}"
        info "start it manually with: launchctl bootstrap ${domain} ${plist_path}"
    fi
}

install_daemon_service() {
    case "$(service_manager)" in
        systemd)
            install_systemd_service
            ;;
        launchd)
            install_launchd_service
            ;;
    esac
}

uninstall_systemd_service() {
    local unit_path="/etc/systemd/system/${SYSTEMD_UNIT_NAME}"
    require_sudo
    ${SUDO} systemctl disable --now "${SYSTEMD_UNIT_NAME}" >/dev/null 2>&1 || true
    if [ -f "$unit_path" ]; then
        ${SUDO} rm -f "$unit_path"
    fi
    ${SUDO} systemctl daemon-reload
    info "removed systemd service: ${SYSTEMD_UNIT_NAME}"
}

uninstall_launchd_service() {
    local plist_path="${HOME}/Library/LaunchAgents/${LAUNCHD_LABEL}.plist"
    local domain
    domain="$(launchd_domain)"
    launchctl bootout "$domain" "$plist_path" >/dev/null 2>&1 || true
    rm -f "$plist_path"
    info "removed launchd service: ${LAUNCHD_LABEL}"
}

uninstall_daemon_service() {
    case "$(service_manager)" in
        systemd)
            uninstall_systemd_service
            ;;
        launchd)
            uninstall_launchd_service
            ;;
    esac
}

remove_binary_if_present() {
    local binary="$1"
    local target="${INSTALL_DIR}/${binary}"

    if [ ! -f "$target" ]; then
        warn "binary not found, skipping: ${target}"
        return 0
    fi

    if [ -w "$target" ] || [ -w "$(dirname "$target")" ]; then
        rm -f "$target"
    else
        require_sudo
        ${SUDO} rm -f "$target"
    fi
    info "removed binary: ${target}"
}

purge_daemon_state() {
    rm -rf "$CONFIG_ROOT"
    info "removed daemon state: ${CONFIG_ROOT}"
}

print_post_install_notes() {
    if [ "$INSTALL_CLI" -eq 1 ]; then
        info "CLI command available at: ${INSTALL_DIR}/${CLI_BINARY}"
        if [[ ":${PATH}:" != *":${INSTALL_DIR}:"* ]]; then
            warn "add ${INSTALL_DIR} to PATH to call ${CLI_BINARY} directly"
        fi
    fi

    if [ "$INSTALL_DAEMON" -eq 1 ]; then
        info "daemon config: ${CONFIG_PATH}"
        case "$(service_manager)" in
            systemd)
                info "manage service with: sudo systemctl status ${SYSTEMD_UNIT_NAME}"
                info "view logs with: sudo journalctl -u ${SYSTEMD_UNIT_NAME} -f"
                ;;
            launchd)
                info "manage service with: launchctl print $(launchd_domain)/${LAUNCHD_LABEL}"
                info "view logs in: ${DATA_DIR}/log/"
                ;;
        esac
        if [ "$INSTALL_CLI" -eq 1 ]; then
            info "health check: ${INSTALL_DIR}/${CLI_BINARY} health"
        fi
    fi
}

parse_args() {
    while [ $# -gt 0 ]; do
        case "$1" in
            --cli)
                MODE="cli"
                ;;
            --daemon)
                MODE="daemon"
                ;;
            --all)
                MODE="all"
                ;;
            --install-dir)
                [ $# -ge 2 ] || error "missing value for --install-dir"
                INSTALL_DIR="$2"
                shift
                ;;
            --version)
                [ $# -ge 2 ] || error "missing value for --version"
                VERSION="$2"
                shift
                ;;
            --repo)
                [ $# -ge 2 ] || error "missing value for --repo"
                GITHUB_REPO="$2"
                shift
                ;;
            --uninstall)
                ACTION="uninstall"
                ;;
            --purge)
                PURGE=1
                ;;
            --no-start)
                AUTO_START=0
                ;;
            --node-id)
                [ $# -ge 2 ] || error "missing value for --node-id"
                NODE_ID="$2"
                shift
                ;;
            --nats-servers)
                [ $# -ge 2 ] || error "missing value for --nats-servers"
                NATS_SERVERS="$2"
                shift
                ;;
            --local-api-addr)
                [ $# -ge 2 ] || error "missing value for --local-api-addr"
                LOCAL_API_ADDR="$2"
                shift
                ;;
            --trust-mode)
                [ $# -ge 2 ] || error "missing value for --trust-mode"
                TRUST_MODE="$2"
                shift
                ;;
            --agent-adapter)
                [ $# -ge 2 ] || error "missing value for --agent-adapter"
                AGENT_ADAPTER="$2"
                shift
                ;;
            --webhook-url)
                [ $# -ge 2 ] || error "missing value for --webhook-url"
                WEBHOOK_URL="$2"
                shift
                ;;
            --deliverable-prefixes)
                [ $# -ge 2 ] || error "missing value for --deliverable-prefixes"
                DELIVERABLE_PREFIXES="$2"
                shift
                ;;
            --config)
                [ $# -ge 2 ] || error "missing value for --config"
                CONFIG_PATH="$2"
                shift
                ;;
            --data-dir)
                [ $# -ge 2 ] || error "missing value for --data-dir"
                DATA_DIR="$2"
                shift
                ;;
            --transfer-dir)
                [ $# -ge 2 ] || error "missing value for --transfer-dir"
                TRANSFER_DIR="$2"
                shift
                ;;
            --heartbeat)
                [ $# -ge 2 ] || error "missing value for --heartbeat"
                HEARTBEAT_INTERVAL="$2"
                shift
                ;;
            --announce-ttl)
                [ $# -ge 2 ] || error "missing value for --announce-ttl"
                ANNOUNCE_TTL="$2"
                shift
                ;;
            --transfer-max-file-size)
                [ $# -ge 2 ] || error "missing value for --transfer-max-file-size"
                TRANSFER_MAX_FILE_SIZE="$2"
                shift
                ;;
            --transfer-ttl)
                [ $# -ge 2 ] || error "missing value for --transfer-ttl"
                TRANSFER_TTL="$2"
                shift
                ;;
            --log-level)
                [ $# -ge 2 ] || error "missing value for --log-level"
                LOG_LEVEL="$2"
                shift
                ;;
            --log-format)
                [ $# -ge 2 ] || error "missing value for --log-format"
                LOG_FORMAT="$2"
                shift
                ;;
            -h|--help)
                usage
                exit 0
                ;;
            *)
                error "unknown argument: $1"
                ;;
        esac
        shift
    done
}

normalize_paths() {
    CONFIG_PATH="$(expand_path "$CONFIG_PATH")"
    CONFIG_ROOT="$(dirname "$CONFIG_PATH")"
    DATA_DIR="$(expand_path "$DATA_DIR")"

    if [ -z "$TRANSFER_DIR" ]; then
        TRANSFER_DIR="${DATA_DIR}/transfers"
    fi
    TRANSFER_DIR="$(expand_path "$TRANSFER_DIR")"
}

select_components() {
    case "$MODE" in
        cli)
            INSTALL_CLI=1
            INSTALL_DAEMON=0
            ;;
        daemon)
            INSTALL_CLI=0
            INSTALL_DAEMON=1
            ;;
        all)
            INSTALL_CLI=1
            INSTALL_DAEMON=1
            ;;
        *)
            error "invalid install mode: ${MODE}"
            ;;
    esac
}

install_selected() {
    local platform="$1"

    ensure_install_dir "$INSTALL_DIR"
    if [ "$INSTALL_CLI" -eq 1 ]; then
        install_binary "$CLI_BINARY" "$platform"
    fi
    if [ "$INSTALL_DAEMON" -eq 1 ]; then
        install_binary "$DAEMON_BINARY" "$platform"
        ensure_config_root
        write_config_if_missing
        validate_daemon_config
        install_daemon_service
    fi
}

uninstall_selected() {
    if [ "$INSTALL_DAEMON" -eq 1 ]; then
        uninstall_daemon_service
        remove_binary_if_present "$DAEMON_BINARY"
        if [ "$PURGE" -eq 1 ]; then
            purge_daemon_state
        fi
    fi
    if [ "$INSTALL_CLI" -eq 1 ]; then
        remove_binary_if_present "$CLI_BINARY"
    fi
}

main() {
    MODE="all"
    ACTION="install"
    INSTALL_CLI=0
    INSTALL_DAEMON=0
    AUTO_START=1
    PURGE=0
    SUDO=""
    VERSION="${VERSION:-latest}"
    GITHUB_REPO="${GITHUB_REPO:-$DEFAULT_REPO}"
    INSTALL_DIR="${INSTALL_DIR:-}"
    CONFIG_PATH="${CONFIG_PATH:-$DEFAULT_CONFIG_PATH}"
    DATA_DIR="${DATA_DIR:-$DEFAULT_DATA_DIR}"
    TRANSFER_DIR="${TRANSFER_DIR:-$DEFAULT_TRANSFER_DIR}"
    NODE_ID="${NODE_ID:-}"
    NATS_SERVERS="${NATS_SERVERS:-$DEFAULT_NATS_SERVERS}"
    LOCAL_API_ADDR="${LOCAL_API_ADDR:-$DEFAULT_LOCAL_API_ADDR}"
    TRUST_MODE="${TRUST_MODE:-$DEFAULT_TRUST_MODE}"
    AGENT_ADAPTER="${AGENT_ADAPTER:-$DEFAULT_AGENT_ADAPTER}"
    WEBHOOK_URL="${WEBHOOK_URL:-}"
    DELIVERABLE_PREFIXES="${DELIVERABLE_PREFIXES:-$DEFAULT_DELIVERABLE_PREFIXES}"
    HEARTBEAT_INTERVAL="${HEARTBEAT_INTERVAL:-$DEFAULT_HEARTBEAT}"
    ANNOUNCE_TTL="${ANNOUNCE_TTL:-$DEFAULT_ANNOUNCE_TTL}"
    TRANSFER_MAX_FILE_SIZE="${TRANSFER_MAX_FILE_SIZE:-$DEFAULT_TRANSFER_MAX_FILE_SIZE}"
    TRANSFER_TTL="${TRANSFER_TTL:-$DEFAULT_TRANSFER_TTL}"
    LOG_LEVEL="${LOG_LEVEL:-$DEFAULT_LOG_LEVEL}"
    LOG_FORMAT="${LOG_FORMAT:-$DEFAULT_LOG_FORMAT}"

    parse_args "$@"
    select_components
    maybe_prompt_daemon_config
    normalize_paths
    detect_install_dir

    info "ClawSynapse installer"
    info "install dir: ${INSTALL_DIR}"
    info "platform: $(detect_platform)"

    if [ "$ACTION" = "uninstall" ]; then
        uninstall_selected
        exit 0
    fi

    install_selected "$(detect_platform)"
    print_post_install_notes
}

main "$@"

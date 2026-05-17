#!/bin/bash
# =====================================================
# MusallahBoard Agent Install Script
# =====================================================
# Installs musallahboard-agent as a systemd service on
# a Debian/Ubuntu x86-64 or arm64 machine.
#
# Usage:
#   sudo bash install.sh [AGENT_DIR] [--token TOKEN] [--backend URL]
#
#   AGENT_DIR   Path to the agent source/build directory.
#               Defaults to the directory containing this script.
#   --token     One-time enrollment token. If omitted, enrollment
#               is skipped (run `musallahboard-agent enroll` later).
#   --backend   Backend base URL (e.g. http://localhost:8080).
#               Required when --token is provided.
# =====================================================

set -euo pipefail

# ── Colour helpers ────────────────────────────────────────────────────────────
RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'; BOLD='\033[1m'; NC='\033[0m'
info()    { echo -e "${GREEN}[INFO]${NC}  $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error()   { echo -e "${RED}[ERROR]${NC} $*"; exit 1; }
section() { echo; echo -e "${BOLD}═══ $* ═══${NC}"; }

# ── Constants ─────────────────────────────────────────────────────────────────
SERVICE_USER=admin
BINARY_DEST=/usr/local/bin/musallahboard-agent
CONFIG_DIR=/etc/musallahboard
STATE_DIR=/var/lib/musallahboard
LOG_DIR=/var/log/musallahboard
CONFIG_PATH=${CONFIG_DIR}/agent.toml
SUDOERS_DEST=/etc/sudoers.d/musallahboard-agent
SERVICE_DEST=/etc/systemd/system/musallahboard-agent.service

# ── Arg parsing ───────────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AGENT_DIR=""
ENROLL_TOKEN=""
BACKEND_URL=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --token)    ENROLL_TOKEN="$2"; shift 2 ;;
        --token=*)  ENROLL_TOKEN="${1#--token=}"; shift ;;
        --backend)  BACKEND_URL="$2";  shift 2 ;;
        --backend=*) BACKEND_URL="${1#--backend=}"; shift ;;
        --*)        error "Unknown flag: $1" ;;
        *)
            [[ -n "$AGENT_DIR" ]] && error "Unexpected argument: $1"
            AGENT_DIR="$1"
            shift
            ;;
    esac
done

AGENT_DIR="${AGENT_DIR:-$SCRIPT_DIR/..}"
AGENT_DIR="$(cd "$AGENT_DIR" && pwd)"

# ── Pre-flight ────────────────────────────────────────────────────────────────
[[ $EUID -ne 0 ]] && error "Run as root: sudo bash $0"

[[ -n "$ENROLL_TOKEN" && -z "$BACKEND_URL" ]] && \
    error "--backend is required when --token is provided"

section "Pre-flight"

# Detect architecture and pick the right binary
ARCH="$(uname -m)"
case "$ARCH" in
    x86_64)  ARCH_SUFFIX=amd64 ;;
    aarch64) ARCH_SUFFIX=arm64 ;;
    *)        error "Unsupported architecture: $ARCH" ;;
esac

# Prefer the arch-specific binary; fall back to the plain one (native build).
# Also check the package root for release tarballs where the binary sits next
# to the packaging/ directory rather than under build/.
BINARY=""
for candidate in \
    "${AGENT_DIR}/build/musallahboard-agent-${ARCH_SUFFIX}" \
    "${AGENT_DIR}/build/musallahboard-agent" \
    "${AGENT_DIR}/musallahboard-agent-${ARCH_SUFFIX}" \
    "${AGENT_DIR}/musallahboard-agent"
do
    if [[ -x "$candidate" ]]; then
        BINARY="$candidate"
        break
    fi
done

[[ -z "$BINARY" ]] && error "No binary found under ${AGENT_DIR}. Run 'make build' first."

SUDOERS_SRC="${AGENT_DIR}/packaging/sudoers.d/musallahboard-agent"
SERVICE_SRC="${AGENT_DIR}/packaging/musallahboard-agent.service"

[[ -f "$SUDOERS_SRC" ]] || error "Missing: $SUDOERS_SRC"
[[ -f "$SERVICE_SRC" ]] || error "Missing: $SERVICE_SRC"

info "Agent directory : $AGENT_DIR"
info "Binary          : $BINARY  (arch: $ARCH_SUFFIX)"
info "Service user    : $SERVICE_USER"
if [[ -n "$ENROLL_TOKEN" ]]; then
    info "Enrollment      : yes  (backend: $BACKEND_URL)"
else
    warn "Enrollment      : skipped — run 'musallahboard-agent enroll' manually"
fi

# ── Service user ──────────────────────────────────────────────────────────────
section "Service user: $SERVICE_USER"

if id "$SERVICE_USER" &>/dev/null; then
    info "User '$SERVICE_USER' already exists"
else
    useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
    info "Created system user '$SERVICE_USER'"
fi

# systemd-journal membership lets the agent read the journal (for logs.tail).
if getent group systemd-journal &>/dev/null; then
    usermod -aG systemd-journal "$SERVICE_USER"
    info "Added '$SERVICE_USER' to systemd-journal group"
fi

# ── Directories ───────────────────────────────────────────────────────────────
section "Directories"

mkdir -p "$CONFIG_DIR" "$STATE_DIR" "$LOG_DIR"
# Config dir: owned by the service user so the agent can write its own config
# and key files at enrollment time and read them at runtime. The agent CLI's
# `enroll` subcommand chowns newly-written files to match this directory's
# owner, which means re-enrolling under `sudo` still leaves files owned by
# $SERVICE_USER rather than root.
chown "$SERVICE_USER:$SERVICE_USER" "$CONFIG_DIR"
chmod 0750 "$CONFIG_DIR"
# State and log dirs: agent-owned (service runs as $SERVICE_USER)
chown "$SERVICE_USER:$SERVICE_USER" "$STATE_DIR" "$LOG_DIR"
chmod 0750 "$STATE_DIR" "$LOG_DIR"

info "Created $CONFIG_DIR  ($SERVICE_USER 0750)"
info "Created $STATE_DIR   ($SERVICE_USER 0750)"
info "Created $LOG_DIR     ($SERVICE_USER 0750)"

# ── Binary ────────────────────────────────────────────────────────────────────
section "Binary"

install -o root -g root -m 0755 "$BINARY" "$BINARY_DEST"
info "Installed $BINARY_DEST"

# ── Sudoers ───────────────────────────────────────────────────────────────────
section "Sudoers"

install -o root -g root -m 0440 "$SUDOERS_SRC" "$SUDOERS_DEST"
# Validate syntax — visudo -c exits non-zero if the file is broken
visudo -c -f "$SUDOERS_DEST" || {
    rm -f "$SUDOERS_DEST"
    error "Sudoers file failed validation — removed. Check $SUDOERS_SRC."
}
info "Installed $SUDOERS_DEST"

# ── Systemd service ───────────────────────────────────────────────────────────
section "Systemd service"

install -o root -g root -m 0644 "$SERVICE_SRC" "$SERVICE_DEST"
systemctl daemon-reload
systemctl enable musallahboard-agent.service
info "Service installed and enabled"

# ── Enrollment ────────────────────────────────────────────────────────────────
if [[ -n "$ENROLL_TOKEN" ]]; then
    section "Enrollment"

    if [[ -f "$CONFIG_PATH" ]]; then
        warn "Config already exists at $CONFIG_PATH — skipping enrollment."
        warn "Delete it and re-run with --token to re-enroll."
    else
        # Run enroll as the service user so files end up owned by it directly.
        # (The agent's enroll path also chowns to the parent dir's owner as a
        # belt-and-suspenders measure when run via `sudo`.)
        sudo -u "$SERVICE_USER" "$BINARY_DEST" enroll \
            --token="$ENROLL_TOKEN" \
            --backend="$BACKEND_URL" \
            --config="$CONFIG_PATH"
        info "Device enrolled. Config: $CONFIG_PATH"
    fi
fi

# ── Start the service ─────────────────────────────────────────────────────────
section "Service"

if [[ -f "$CONFIG_PATH" ]]; then
    systemctl restart musallahboard-agent.service
    sleep 1
    systemctl status musallahboard-agent.service --no-pager --lines=5
else
    warn "No config found — service not started."
    warn "Run: musallahboard-agent enroll --token=X --backend=Y"
    warn "Then: systemctl start musallahboard-agent.service"
fi

# ── Summary ───────────────────────────────────────────────────────────────────
cat << EOF

  ╔══════════════════════════════════════════════════════════╗
  ║  musallahboard-agent install complete                    ║
  ╚══════════════════════════════════════════════════════════╝

  Binary    : $BINARY_DEST
  Config    : $CONFIG_PATH
  State     : $STATE_DIR
  Service   : musallahboard-agent.service

  Useful commands:
    sudo systemctl status musallahboard-agent
    sudo journalctl -u musallahboard-agent -f
    sudo musallahboard-agent enroll --token=X --backend=Y

EOF

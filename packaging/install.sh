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
#   --kiosk-user USER
#               Locked-down display user that cage/Chromium runs as. When
#               given, the cage kiosk unit + launcher are installed and the
#               box is switched to boot straight into it (no display manager).
#   --board-url URL
#               Base board URL (no query). The agent appends ?deviceId and
#               writes the result to /etc/musallahboard/kiosk-url. Required
#               when --kiosk-user is given.
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
KIOSK_SERVICE_DEST=/etc/systemd/system/musallahboard-kiosk.service
KIOSK_WATCH_DEST=/etc/systemd/system/musallahboard-kiosk-watch.path
KIOSK_RELOAD_DEST=/etc/systemd/system/musallahboard-kiosk-reload.service
KIOSK_LAUNCHER_DEST=/usr/local/bin/start-kiosk.sh
KIOSK_SHARE_DIR=/usr/local/share/musallahboard
BOARD_URL_PATH=${CONFIG_DIR}/board-url

# ── Arg parsing ───────────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AGENT_DIR=""
ENROLL_TOKEN=""
BACKEND_URL=""
KIOSK_USER=""
BOARD_URL=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --token)    ENROLL_TOKEN="$2"; shift 2 ;;
        --token=*)  ENROLL_TOKEN="${1#--token=}"; shift ;;
        --backend)  BACKEND_URL="$2";  shift 2 ;;
        --backend=*) BACKEND_URL="${1#--backend=}"; shift ;;
        --kiosk-user)   KIOSK_USER="$2"; shift 2 ;;
        --kiosk-user=*) KIOSK_USER="${1#--kiosk-user=}"; shift ;;
        --board-url)    BOARD_URL="$2"; shift 2 ;;
        --board-url=*)  BOARD_URL="${1#--board-url=}"; shift ;;
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

[[ -n "$KIOSK_USER" && -z "$BOARD_URL" ]] && \
    error "--board-url is required when --kiosk-user is provided"
[[ -n "$KIOSK_USER" ]] && ! id "$KIOSK_USER" &>/dev/null && \
    error "Kiosk user '$KIOSK_USER' does not exist (create it before install)"

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
# 0751 (not 0750): the unprivileged kiosk user must traverse this dir to read
# the agent-written board-url / kiosk-url files. 0751 grants traverse-by-path
# without directory listing; agent.key stays 0600 so it is unreadable anyway.
chmod 0751 "$CONFIG_DIR"
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

# ── Kiosk (cage + Chromium) ───────────────────────────────────────────────────
if [[ -n "$KIOSK_USER" ]]; then
    section "Kiosk (cage)"

    command -v cage >/dev/null || error "cage not installed (apt install cage)"

    KIOSK_SERVICE_SRC="${AGENT_DIR}/packaging/musallahboard-kiosk.service"
    KIOSK_WATCH_SRC="${AGENT_DIR}/packaging/musallahboard-kiosk-watch.path"
    KIOSK_RELOAD_SRC="${AGENT_DIR}/packaging/musallahboard-kiosk-reload.service"
    KIOSK_LAUNCHER_SRC="${AGENT_DIR}/packaging/start-kiosk.sh"
    KIOSK_SPLASH_SRC="${AGENT_DIR}/packaging/waiting.html"
    for f in "$KIOSK_SERVICE_SRC" "$KIOSK_WATCH_SRC" "$KIOSK_RELOAD_SRC" \
             "$KIOSK_LAUNCHER_SRC" "$KIOSK_SPLASH_SRC"; do
        [[ -f "$f" ]] || error "Missing: $f"
    done

    # Base board URL — the agent appends ?deviceId and writes kiosk-url. 0644
    # so the kiosk user can read it across the 0751 config dir.
    printf '%s\n' "$BOARD_URL" > "$BOARD_URL_PATH"
    chown "$SERVICE_USER:$SERVICE_USER" "$BOARD_URL_PATH"
    chmod 0644 "$BOARD_URL_PATH"
    info "Board URL    : $BOARD_URL  ($BOARD_URL_PATH)"

    install -o root -g root -m 0755 "$KIOSK_LAUNCHER_SRC" "$KIOSK_LAUNCHER_DEST"
    mkdir -p "$KIOSK_SHARE_DIR"
    install -o root -g root -m 0644 "$KIOSK_SPLASH_SRC" "$KIOSK_SHARE_DIR/waiting.html"

    # Substitute the kiosk user into the unit's User=/ExecStopPost lines.
    sed "s/KIOSK_USER_PLACEHOLDER/${KIOSK_USER}/g" "$KIOSK_SERVICE_SRC" \
        > "$KIOSK_SERVICE_DEST"
    chown root:root "$KIOSK_SERVICE_DEST"
    chmod 0644 "$KIOSK_SERVICE_DEST"

    # Watcher: reloads the kiosk the moment the agent (re)composes kiosk-url,
    # so the splash is replaced by the board on enrollment with no operator
    # action. No user substitution — these run as root in the system manager.
    install -o root -g root -m 0644 "$KIOSK_WATCH_SRC"  "$KIOSK_WATCH_DEST"
    install -o root -g root -m 0644 "$KIOSK_RELOAD_SRC" "$KIOSK_RELOAD_DEST"

    # No display manager: boot to multi-user; the kiosk unit is the session.
    if systemctl list-unit-files | grep -qE '^(lightdm|gdm3?|sddm)\.service'; then
        systemctl disable --now lightdm.service gdm.service gdm3.service sddm.service 2>/dev/null || true
        info "Disabled display manager(s)"
    fi
    systemctl set-default multi-user.target >/dev/null
    systemctl daemon-reload
    systemctl enable musallahboard-kiosk.service
    # The .path watcher must be enabled+started so it is armed before the
    # agent ever writes kiosk-url. The reload .service is pulled in by the
    # path unit on demand — it is not enabled directly.
    systemctl enable --now musallahboard-kiosk-watch.path
    info "Kiosk unit + URL watcher installed and enabled (user: $KIOSK_USER)"
fi

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
    # Kiosk shows the splash immediately and the .path watcher swaps it for
    # the board when the agent writes kiosk-url — safe to start either way.
    if [[ -n "$KIOSK_USER" ]]; then
        systemctl restart musallahboard-kiosk.service
        info "Kiosk service (re)started"
    fi
else
    warn "No config found — agent not started."
    warn "Run: musallahboard-agent enroll --token=X --backend=Y"
    warn "Then: systemctl start musallahboard-agent.service"
    if [[ -n "$KIOSK_USER" ]]; then
        systemctl start musallahboard-kiosk.service || true
        warn "Kiosk started on splash — it will load the board once enrolled."
    fi
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

#!/bin/bash
# =====================================================
# MusallahBoard Agent Install Script
# =====================================================
# Installs musallahboard-agent as a systemd service on
# a Debian/Ubuntu x86-64 or arm64 machine.
#
# SCOPE — this script installs PAYLOAD ONLY: files, accounts and unit
# enablement. It is written to be the .deb's postinst, so it deliberately does
# not do any of the following, all of which are host policy or per-device
# state and belong to the provisioner (packaging/lib/common.sh, the image
# build, or firstboot):
#
#   - switching the default boot target / disabling display managers
#     → packaging/appliance-policy.sh
#                                                    → provisioner or firstboot
#   - the ethernet service port (NetworkManager profiles, ufw rules)
#                                                    → setup.sh --service-port
#   - enrollment                                     → firstboot, or by hand
#   - hostname, SSH, UFW, watchdog, timezone, LUKS  → common.sh / setup.sh
#
# It also assumes nothing about whether systemd is running: every operation
# that needs a live manager is skipped when there is no /run/systemd/system,
# so this works unchanged inside an image-build chroot.
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
#   --kiosk     Install the cage kiosk unit + launcher. The display account is
#               always `musallahkiosk` and is created here if absent — it is
#               not configurable, because the unit that names it ships as a
#               static file. Booting into it is a separate step; see
#               packaging/appliance-policy.sh.
# =====================================================

set -euo pipefail

# ── Colour helpers ────────────────────────────────────────────────────────────
RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'; BOLD='\033[1m'; NC='\033[0m'
info()    { echo -e "${GREEN}[INFO]${NC}  $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error()   { echo -e "${RED}[ERROR]${NC} $*"; exit 1; }
section() { echo; echo -e "${BOLD}═══ $* ═══${NC}"; }

# ── systemd availability ──────────────────────────────────────────────────────
# True only when there is a live system manager to talk to. Inside an
# image-build chroot (mmdebstrap, debootstrap, packer provisioning a mounted
# root) there is not, and `systemctl start|restart|status|enable --now` all
# fail there. `systemctl enable` on its own is fine either way — it only
# writes symlinks — so it is not gated.
systemd_running() { [[ -d /run/systemd/system ]]; }

# ── Constants ─────────────────────────────────────────────────────────────────
# Fixed, unconfigurable system account. It must match User=/Group= in
# musallahboard-agent.service and the first column of the sudoers allow-list;
# keeping all three hardcoded to the same name is what lets those files ship
# verbatim in a .deb rather than being templated at install time.
SERVICE_USER=musallahdaemon
# The display account. Also fixed: it is baked into User= and ExecStopPost in
# musallahboard-kiosk.service, which is what lets that unit ship as a static
# file. Deliberately NOT the same account as $SERVICE_USER — it runs Chromium
# against remote content, so a browser compromise must not reach the device's
# Ed25519 key or the sudo allow-list.
KIOSK_USER=musallahkiosk
BINARY_DEST=/usr/bin/musallahboard-agent
CONFIG_DIR=/etc/musallahboard
STATE_DIR=/var/lib/musallahboard
LOG_DIR=/var/log/musallahboard
CONFIG_PATH=${CONFIG_DIR}/agent.toml
SUDOERS_DEST=/etc/sudoers.d/musallahboard-agent
# /lib, not /etc: /etc/systemd/system belongs to the local administrator and is
# where the enable symlinks land. Package-shipped units go here, so an operator
# can still override any of them with a drop-in or a masking file in /etc.
UNIT_DIR=/lib/systemd/system
SERVICE_DEST=${UNIT_DIR}/musallahboard-agent.service
KIOSK_SERVICE_DEST=${UNIT_DIR}/musallahboard-kiosk.service
KIOSK_WATCH_DEST=${UNIT_DIR}/musallahboard-kiosk-watch.path
KIOSK_RELOAD_DEST=${UNIT_DIR}/musallahboard-kiosk-reload.service
KIOSK_LAUNCHER_DEST=/usr/bin/start-kiosk.sh
KIOSK_SHARE_DIR=/usr/share/musallahboard
# v2 (docs/architecture.md): the root self-updater, the USB import helper and
# the udev rules that feed them.
UPDATE_PATH_DEST=${UNIT_DIR}/musallahboard-agent-update.path
UPDATE_SERVICE_DEST=${UNIT_DIR}/musallahboard-agent-update.service
USB_SERVICE_DEST=${UNIT_DIR}/musallahboard-usb-import@.service
UDEV_DIR=/etc/udev/rules.d
UDEV_RULES=(90-musallahboard-usb.rules 90-musallahboard-rtc.rules)
INBOX_DIR=${STATE_DIR}/inbox
LIB_DIR=/usr/lib/musallahboard
TRUST_PATH=${CONFIG_DIR}/trust.json

# ── Arg parsing ───────────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AGENT_DIR=""
ENROLL_TOKEN=""
BACKEND_URL=""
WANT_KIOSK=no

while [[ $# -gt 0 ]]; do
    case "$1" in
        --token)    ENROLL_TOKEN="$2"; shift 2 ;;
        --token=*)  ENROLL_TOKEN="${1#--token=}"; shift ;;
        --backend)  BACKEND_URL="$2";  shift 2 ;;
        --backend=*) BACKEND_URL="${1#--backend=}"; shift ;;
        --kiosk)        WANT_KIOSK=yes; shift ;;
        # Accepted only to fail loudly: the account is fixed now, and silently
        # ignoring a name here would install a kiosk running as the wrong user.
        --kiosk-user|--kiosk-user=*)
            error "--kiosk-user is gone; the display account is always '$KIOSK_USER'. Use --kiosk." ;;
        # There is no hosted board any more: the kiosk always shows the
        # board the agent serves (docs/architecture.md section 14).
        --board-url|--board-url=*)
            error "--board-url is gone: the kiosk always shows the board served by the agent." ;;
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
UPDATE_PATH_SRC="${AGENT_DIR}/packaging/musallahboard-agent-update.path"
UPDATE_SERVICE_SRC="${AGENT_DIR}/packaging/musallahboard-agent-update.service"
USB_SERVICE_SRC="${AGENT_DIR}/packaging/musallahboard-usb-import@.service"
UDEV_SRC_DIR="${AGENT_DIR}/packaging/udev"

for f in "$SUDOERS_SRC" "$SERVICE_SRC" "$UPDATE_PATH_SRC" "$UPDATE_SERVICE_SRC" "$USB_SERVICE_SRC"; do
    [[ -f "$f" ]] || error "Missing: $f"
done
for r in "${UDEV_RULES[@]}"; do
    [[ -f "$UDEV_SRC_DIR/$r" ]] || error "Missing: $UDEV_SRC_DIR/$r"
done

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

# video membership is what lets telemetry read /dev/vcio, which is root:video
# 0660 on Pi OS. Without it `vcgencmd get_throttled` fails and under-voltage /
# thermal-throttle reporting silently returns nothing on every board — the
# failure mode is missing data, not an error, so it is easy to miss.
if getent group video &>/dev/null; then
    usermod -aG video "$SERVICE_USER"
    info "Added '$SERVICE_USER' to video group (vcgencmd → /dev/vcio)"
fi

# ── Directories ───────────────────────────────────────────────────────────────
section "Directories"

# Only the config dir is created here. $STATE_DIR and $LOG_DIR are declared as
# StateDirectory=/LogsDirectory= in musallahboard-agent.service, so systemd
# creates and chowns them on every start — one less thing for a postinst to get
# wrong, and it works even when this runs in a chroot.
#
# /etc/musallahboard cannot be handled that way: those directories only appear
# once the service has started, and `enroll` has to write agent.toml and
# agent.key before that has ever happened.
mkdir -p "$CONFIG_DIR"
# Owned by the service user so the agent can write its own config and key files
# at enrollment time and read them at runtime. The agent CLI's `enroll`
# subcommand chowns newly-written files to match this directory's owner, which
# means re-enrolling under `sudo` still leaves files owned by $SERVICE_USER
# rather than root.
chown "$SERVICE_USER:$SERVICE_USER" "$CONFIG_DIR"
# 0751 (not 0750): the unprivileged kiosk user must traverse this dir to read
# the agent-written kiosk-url file. 0751 grants traverse-by-path
# without directory listing; agent.key stays 0600 so it is unreadable anyway.
chmod 0751 "$CONFIG_DIR"

info "Created $CONFIG_DIR  ($SERVICE_USER 0751)"

# The state dir is also StateDirectory= in the unit, but the inbox inside it is
# written by root tools (the USB helper, `musallahboard-agent import`) before
# the daemon may ever have started, so both are created here. The owner must
# match User= exactly: systemd recursively chowns a StateDirectory= whose top
# directory has the wrong owner, which would also hand the daemon the
# root-owned agent/last-update.json.
install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0750 "$STATE_DIR"
install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0770 "$INBOX_DIR"
info "Created $INBOX_DIR  ($SERVICE_USER 0770)"
info "$LOG_DIR: created by systemd at first start"

# Where the root self-updater keeps the previous agent binary for rollback.
install -d -o root -g root -m 0755 "$LIB_DIR"
info "Created $LIB_DIR  (root 0755)"

# ── Trust store ───────────────────────────────────────────────────────────────
section "Trust store"

# Root-owned and world-readable: the daemon reads it, only root changes it
# (`musallahboard-agent trust ...`, enrollment, the self-updater). Never
# overwritten: it holds the content keys pinned at enrollment, and release
# keys an admin may have added. An empty store is fine; release keys are also
# compiled into the agent, and content keys arrive with enrollment or
# `trust fetch`.
if [[ -e "$TRUST_PATH" ]]; then
    info "Keeping existing $TRUST_PATH"
else
    printf '{"content":[],"release":[]}\n' > "$TRUST_PATH.tmp"
    chown root:root "$TRUST_PATH.tmp"
    chmod 0644 "$TRUST_PATH.tmp"
    mv -f "$TRUST_PATH.tmp" "$TRUST_PATH"
    info "Created $TRUST_PATH  (root 0644, no keys yet)"
fi

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

# Root self-updater (docs/architecture.md section 12): the .path unit watches
# for agent/staged/ready and starts the oneshot. Only the .path is enabled.
install -o root -g root -m 0644 "$UPDATE_PATH_SRC"    "$UPDATE_PATH_DEST"
install -o root -g root -m 0644 "$UPDATE_SERVICE_SRC" "$UPDATE_SERVICE_DEST"
# USB import helper (section 9.6): a template started by udev, never enabled.
install -o root -g root -m 0644 "$USB_SERVICE_SRC"    "$USB_SERVICE_DEST"

systemd_running && systemctl daemon-reload
systemctl enable musallahboard-agent.service
systemctl enable musallahboard-agent-update.path
info "Service, self-updater and USB import units installed and enabled"

# ── udev rules ────────────────────────────────────────────────────────────────
section "udev rules"

# USB sticks -> musallahboard-usb-import@<dev>.service, and rtc0 writable by
# the agent's group so it can save a corrected clock to the RTC.
mkdir -p "$UDEV_DIR"
for r in "${UDEV_RULES[@]}"; do
    install -o root -g root -m 0644 "$UDEV_SRC_DIR/$r" "$UDEV_DIR/$r"
done
if systemd_running && command -v udevadm >/dev/null; then
    udevadm control --reload || true
    # Apply the rtc0 permissions now rather than at the next boot. Not a
    # block-device trigger: that would import from a stick already plugged in.
    udevadm trigger --subsystem-match=rtc --action=change || true
fi
info "Installed ${UDEV_RULES[*]} in $UDEV_DIR"

# ── Kiosk (cage + Chromium) ───────────────────────────────────────────────────
if [[ "$WANT_KIOSK" == "yes" ]]; then
    section "Kiosk (cage)"

    command -v cage >/dev/null || error "cage not installed (apt install cage)"

    # Display account. Needs a real home: start-kiosk.sh puts Chromium's
    # profile in $HOME/.musallahboard-kiosk, which is mandatory rather than
    # cosmetic — Chrome >=136 silently ignores --remote-debugging-port unless
    # a non-default --user-data-dir is given, and the agent's CDP commands
    # depend on that port.
    #
    # The shell stays /bin/bash rather than nologin: the unit runs with
    # PAMName=login to get a logind seat, and some PAM stacks refuse a session
    # for a nologin shell. The account is locked instead — no password, no SSH
    # (sshd DenyUsers), and a .bashrc that exits on sight.
    if id "$KIOSK_USER" &>/dev/null; then
        info "User '$KIOSK_USER' already exists"
    else
        useradd -m -s /bin/bash "$KIOSK_USER"
        info "Created user '$KIOSK_USER'"
    fi
    passwd -l "$KIOSK_USER" >/dev/null
    # Strip every privilege group. video and render stay: cage needs DRM master
    # and the browser needs the GPU. input stays off — cage gets its devices
    # through the logind seat, not group membership.
    for g in sudo adm dialout cdrom plugdev games users input netdev; do
        gpasswd -d "$KIOSK_USER" "$g" 2>/dev/null || true
    done
    for g in video render; do
        getent group "$g" >/dev/null && usermod -aG "$g" "$KIOSK_USER"
    done
    rm -f "/etc/sudoers.d/$KIOSK_USER"
    cat > "/home/$KIOSK_USER/.bashrc" <<'BASHRC'
readonly PATH
echo "This account is for kiosk use only. Direct shell access is not permitted."
exit
BASHRC
    printf 'export PATH="/home/%s/bin"\n' "$KIOSK_USER" > "/home/$KIOSK_USER/.bash_profile"
    chown "$KIOSK_USER:$KIOSK_USER" \
        "/home/$KIOSK_USER/.bashrc" "/home/$KIOSK_USER/.bash_profile"
    info "Kiosk user   : $KIOSK_USER (locked, no sudo, no shell, video+render)"

    KIOSK_SERVICE_SRC="${AGENT_DIR}/packaging/musallahboard-kiosk.service"
    KIOSK_WATCH_SRC="${AGENT_DIR}/packaging/musallahboard-kiosk-watch.path"
    KIOSK_RELOAD_SRC="${AGENT_DIR}/packaging/musallahboard-kiosk-reload.service"
    KIOSK_LAUNCHER_SRC="${AGENT_DIR}/packaging/start-kiosk.sh"
    KIOSK_SPLASH_SRC="${AGENT_DIR}/packaging/waiting.html"
    for f in "$KIOSK_SERVICE_SRC" "$KIOSK_WATCH_SRC" "$KIOSK_RELOAD_SRC" \
             "$KIOSK_LAUNCHER_SRC" "$KIOSK_SPLASH_SRC"; do
        [[ -f "$f" ]] || error "Missing: $f"
    done

    install -o root -g root -m 0755 "$KIOSK_LAUNCHER_SRC" "$KIOSK_LAUNCHER_DEST"
    mkdir -p "$KIOSK_SHARE_DIR"
    install -o root -g root -m 0644 "$KIOSK_SPLASH_SRC" "$KIOSK_SHARE_DIR/waiting.html"

    # Copied verbatim — no substitution. The unit names $KIOSK_USER directly,
    # which is the whole point of fixing the account name: a .deb ships static
    # files and cannot template them at install time.
    install -o root -g root -m 0644 "$KIOSK_SERVICE_SRC" "$KIOSK_SERVICE_DEST"

    # Watcher: reloads the kiosk the moment the agent (re)composes kiosk-url,
    # so the splash is replaced by the board on enrollment with no operator
    # action. No user substitution — these run as root in the system manager.
    install -o root -g root -m 0644 "$KIOSK_WATCH_SRC"  "$KIOSK_WATCH_DEST"
    install -o root -g root -m 0644 "$KIOSK_RELOAD_SRC" "$KIOSK_RELOAD_DEST"

    # NOTE: switching the default boot target and disabling display managers
    # used to happen here. It is host policy, not payload — a package that
    # repoints your boot target is a package nobody wants — so it moved to
    # packaging/appliance-policy.sh. Installing the kiosk unit does not by
    # itself make the box boot into it.
    systemd_running && systemctl daemon-reload
    systemctl enable musallahboard-kiosk.service
    # The .path watcher must be armed before the agent ever writes kiosk-url.
    # The reload .service is pulled in by the path unit on demand — it is not
    # enabled directly. `enable` alone is enough in a chroot (the unit starts
    # at next boot); on a live system also start it now.
    systemctl enable musallahboard-kiosk-watch.path
    systemd_running && systemctl start musallahboard-kiosk-watch.path
    info "Kiosk unit + URL watcher installed and enabled (user: $KIOSK_USER)"
fi

# ── Enrollment ────────────────────────────────────────────────────────────────
if [[ -n "$ENROLL_TOKEN" ]]; then
    section "Enrollment"

    if [[ -f "$CONFIG_PATH" ]]; then
        warn "Config already exists at $CONFIG_PATH — skipping enrollment."
        warn "Delete it and re-run with --token to re-enroll."
    else
        # Run as root: enrollment also pins the backend's content signing keys
        # in the root-owned trust.json. agent.toml and agent.key still end up
        # owned by $SERVICE_USER, because enroll chowns the files it creates to
        # the owner of the config directory.
        "$BINARY_DEST" enroll \
            --token="$ENROLL_TOKEN" \
            --backend="$BACKEND_URL" \
            --config="$CONFIG_PATH"
        info "Device enrolled. Config: $CONFIG_PATH"
    fi

    # Whatever wrote it, trust.json is root's. And a backend that predates
    # contentSigningKeys in the enroll response leaves no content key, which
    # `trust fetch` fills in (trust on first use over TLS).
    if [[ -f "$TRUST_PATH" ]]; then
        chown root:root "$TRUST_PATH"
        chmod 0644 "$TRUST_PATH"
    fi
    if ! tr -d '[:space:]' < "$TRUST_PATH" 2>/dev/null | grep -qF '"content":[{'; then
        "$BINARY_DEST" trust fetch \
            || warn "No content signing key yet, and 'trust fetch' failed. Run 'sudo musallahboard-agent trust fetch' once the backend is reachable."
    fi
fi

# ── Start the service ─────────────────────────────────────────────────────────
section "Service"

# Everything below needs a live manager. In an image-build chroot the units are
# enabled and that is all that matters — they come up on the built board's
# first boot.
if ! systemd_running; then
    info "No running systemd (chroot/image build) — units enabled, nothing started"
else
    # Started with or without a config. An unenrolled agent no longer exits: it
    # runs a pre-enrollment loop that paints this device's IP address onto the
    # kiosk splash — which is how an operator finds the box to SSH into it —
    # and picks up enrollment on its own, so nothing here has to be re-run.
    systemctl restart musallahboard-agent.service
    sleep 1
    systemctl status musallahboard-agent.service --no-pager --lines=5

    systemctl start musallahboard-agent-update.path

    # Kiosk shows the splash immediately and the .path watcher swaps it for
    # the board when the agent writes kiosk-url — safe to start either way.
    if [[ "$WANT_KIOSK" == "yes" ]]; then
        systemctl restart musallahboard-kiosk.service || true
        info "Kiosk service (re)started"
    fi

    if [[ ! -f "$CONFIG_PATH" ]]; then
        warn "No config found — agent is waiting to be enrolled."
        warn "The splash now shows this device's IP address."
        warn "Run: musallahboard-agent enroll --token=X --backend=Y"
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
  Trust     : $TRUST_PATH
  Updates   : USB stick, upload on the service port, or the release channel
              (docs/architecture.md)

  Useful commands:
    sudo systemctl status musallahboard-agent
    sudo journalctl -u musallahboard-agent -f
    sudo musallahboard-agent enroll --token=X --backend=Y
    sudo musallahboard-agent status
    sudo musallahboard-agent import <file.mbu>

EOF

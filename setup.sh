#!/bin/bash
# =====================================================
# MusallahBoard — Raspberry Pi One-Line Installer
# =====================================================
# Provisions a fresh Raspberry Pi OS (Debian/trixie, arm64) into a
# MusallahBoard kiosk. It needs internet once, here; the board never does
# again.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/lensbridge/agent/main/setup.sh | bash
#
# Or with pre-supplied answers (no prompts):
#   curl -fsSL ... | MB_HOSTNAME=lobby MB_ADMIN_USER=admin \
#       MB_TIMEZONE=America/Toronto \
#       MB_ADMIN_SSH_KEY="ssh-ed25519 AAAA..." \
#       MB_CONTENT_KEY="<base64 content signing public key>" bash
#
# The content signing public key is LensBridge's: the board only accepts
# content and setup files signed by it. The console shows it (and the backend
# publishes it at /api/agent/signing-keys).
#
# After setup the board shows "This board is not set up yet". Plug in the
# setup USB stick exported from the LensBridge console (Devices, the board,
# Setup file or USB kit): the board takes its identity and its content from
# it, and, if its setup file names a backend, connects when it has internet.
#
# The ethernet service port, where a laptop or phone plugged in can upload
# signed update packages, is switched on with
#   bash setup.sh --service-port [--rtc]
# USB sticks work on every board.
#
# Re-running this script is safe.
#
# The pre-built board image (image/build.sh) is made from this script too, in
# two halves, so there is one copy of every step:
#   setup.sh --image      as root, inside a chroot of Raspberry Pi OS Lite:
#                         everything every board shares (packages, accounts,
#                         agent, system files). No per-board answers, no
#                         running system needed.
#   setup.sh --firstboot  run on the board by musallahboard-firstboot.service:
#                         what needs the running system or this board's
#                         answers (SSH lockdown for the admin cloud-init
#                         created, firewall, service port, the content key
#                         from /boot/firmware/musallahboard.txt).
# The curl | bash install above runs both halves' steps, in one go.
# =====================================================

set -euo pipefail

# ── Version and download URLs ─────────────────────────────────────────────────
# MB_VERSION pins a release tag; the default follows the latest release, since
# this script and the units it writes track the current agent.
VERSION="${MB_VERSION:-latest}"
GITHUB_REPO="LensBridge/agent"
if [[ "$VERSION" == "latest" ]]; then
    RELEASE_URL="https://github.com/${GITHUB_REPO}/releases/latest/download"
else
    RELEASE_URL="https://github.com/${GITHUB_REPO}/releases/download/${VERSION}"
fi
# A board is a Raspberry Pi (arm64). A virtual machine (vm/mbvm, for testing)
# may also be amd64; it skips the Pi-only steps.
IS_VM=0
if systemd-detect-virt --vm --quiet 2>/dev/null; then
    IS_VM=1
fi
case "$(uname -m)" in
    aarch64) ARCH=arm64 ;;
    x86_64)  ARCH=amd64 ;;
    *)       ARCH="$(uname -m)" ;;
esac
BINARY_URL="${RELEASE_URL}/musallahboard-agent-${ARCH}"

# ── Colour helpers ────────────────────────────────────────────────────────────
RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'; BOLD='\033[1m'; NC='\033[0m'
# Plain text into the journal (--firstboot) and build logs.
[[ -t 1 ]] || { RED=''; YELLOW=''; GREEN=''; BOLD=''; NC=''; }
info()    { echo -e "${GREEN}[INFO]${NC}  $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error()   { echo -e "${RED}[ERROR]${NC} $*"; exit 1; }
section() { echo; echo -e "${BOLD}=== $* ===${NC}"; }

# What this run does: a normal install (curl | bash), the image half
# (--image) or the first-boot half (--firstboot). See the header.
MODE=install

# --image and --firstboot run as root, where every `sudo` below is just the
# command: running a step as root or through sudo must not be two versions of
# the step. (A normal install refuses root in preflight, so this changes
# nothing there.)
if [[ $EUID -eq 0 ]]; then
    sudo() {
        while [[ "${1-}" == -* ]]; do shift; done
        [[ $# -eq 0 ]] && return 0
        "$@"
    }
fi

# ── Tunables ──────────────────────────────────────────────────────────────────
NTP_SYNC_TIMEOUT_SEC="${NTP_SYNC_TIMEOUT_SEC:-90}"

# ── Non-interactive configuration ─────────────────────────────────────────────
# Every prompt can be answered in advance via environment variable. When piped
# (stdin is the script itself), prompts cannot work — defaults are used.
#
#   MB_HOSTNAME  MB_ADMIN_USER  MB_TIMEZONE  MB_ADMIN_SSH_KEY  MB_CONTENT_KEY
#   MB_ASSUME_YES=1   take the default for anything unset, confirm nothing
#   MB_REBOOT=auto|never|ask
MB_ASSUME_YES="${MB_ASSUME_YES:-0}"
MB_REBOOT="${MB_REBOOT:-ask}"

# Fixed service accounts — not configurable.
KIOSK_USER=musallahkiosk
SERVICE_USER=musallahdaemon

# Paths
BINARY_DEST=/usr/bin/musallahboard-agent
# Where this script is published (for the instructions it prints).
SETUP_URL=https://raw.githubusercontent.com/lensbridge/agent/main/setup.sh
CONFIG_DIR=/etc/musallahboard
STATE_DIR=/var/lib/musallahboard
INBOX_DIR=/var/lib/musallahboard/inbox
LIB_DIR=/usr/lib/musallahboard
TRUST_PATH=/etc/musallahboard/trust.json
# The kiosk always loads the agent's local server (docs/architecture.md, 14).
KIOSK_URL="http://127.0.0.1:8080/"

# The ethernet service port (setup.sh --service-port). Connection names and
# the address are shared with cmd/agent (serviceport_cli.go) and cmd/mbpush —
# change them together.
SERVICE_PORT=0
SERVICE_PORT_RTC=0
SERVICE_CONN=musallahboard-service-port
LAN_CONN=musallahboard-lan
SERVICE_IFACE=eth0
SERVICE_ADDR=10.77.0.1/24
SERVICE_NET=10.77.0.0/24
RTC_OVERLAY="dtoverlay=i2c-rtc,ds3231"
# sshd keeps the first value it reads for each option, and reads
# sshd_config.d in name order: the 00- prefix puts this ahead of cloud-init's
# 50-cloud-init.conf, which says `PasswordAuthentication yes` when Raspberry Pi
# Imager is told to allow passwords. The old name sorted after it.
SSHD_HARDENING=/etc/ssh/sshd_config.d/00-musallahboard-hardening.conf
SSHD_HARDENING_OLD=/etc/ssh/sshd_config.d/kiosk-hardening.conf

# The pre-built image (--image, --firstboot).
# A file on the SD card's boot partition (FAT, so any computer can write it)
# with this board's content key; read and removed at boot.
BOOT_CONFIG=/boot/firmware/musallahboard.txt
BOOT_CONFIG_FAILED=/boot/firmware/musallahboard-failed.txt
# A copy of this script on the image, for musallahboard-firstboot.service.
SETUP_COPY="$LIB_DIR/setup.sh"
FIRSTBOOT_UNIT=/etc/systemd/system/musallahboard-firstboot.service
# Written when the one-time part of --firstboot has finished. Root's, outside
# the daemon's directories.
FIRSTBOOT_DONE="$LIB_DIR/firstboot.done"

# ── Argument parsing ──────────────────────────────────────────────────────────
usage() {
    cat <<'USAGE'
Usage: bash setup.sh [options]

  --hostname=NAME        hostname to set
  --admin-user=NAME      SSH/sudo account
  --timezone=TZ          e.g. America/Toronto
  --admin-ssh-key=KEY    authorized public key for the admin account
  --content-key=B64      LensBridge's content signing public key (base64)
  --reboot=auto|never|ask
  --yes, -y              answer every prompt with its default
  --no-service-port      never serve addresses on eth0: leave the ethernet
                         service port off (it is on by default)
  -h, --help

Service port (after a normal setup, while still online):
  --service-port         provision the ethernet service port, where a laptop
                         or phone plugged in can upload update packages, and
                         turn it on; skips the normal setup
  --rtc                  a DS3231 RTC is fitted: enable it, remove fake-hwclock

Pre-built image (image/build.sh; see docs/architecture.md, section 17):
  --image                as root in a chroot of Raspberry Pi OS Lite: the
                         steps every board shares. MB_AGENT_BINARY=<file>
                         installs that agent instead of downloading one;
                         MB_APP_PACKAGE=<file.mbu> adds the board app.
  --firstboot            run by musallahboard-firstboot.service on the board

Update packages (.mbu) are installed with a USB stick, an upload on the
service port, or: sudo musallahboard-agent import <file.mbu>
USAGE
}

for arg in "$@"; do
    case "$arg" in
        --hostname=*)      export MB_HOSTNAME="${arg#*=}" ;;
        --admin-user=*)    export MB_ADMIN_USER="${arg#*=}" ;;
        --timezone=*)      export MB_TIMEZONE="${arg#*=}" ;;
        --admin-ssh-key=*) export MB_ADMIN_SSH_KEY="${arg#*=}" ;;
        --content-key=*)   export MB_CONTENT_KEY="${arg#*=}" ;;
        --reboot=*)        export MB_REBOOT="${arg#*=}" ;;
        --yes|-y)          export MB_ASSUME_YES=1 ;;
        --service-port)    SERVICE_PORT=1 ;;
        --no-service-port) export MB_SERVICE_PORT=0 ;;
        --rtc)             SERVICE_PORT_RTC=1 ;;
        --image)           MODE=image ;;
        --firstboot)       MODE=firstboot ;;
        -h|--help)         usage; exit 0 ;;
        *) echo "Unknown option: $arg" >&2; usage >&2; exit 1 ;;
    esac
done

if [[ "$MODE" != install && "$SERVICE_PORT" == "1" ]]; then
    echo "--service-port does not go with --image or --firstboot" >&2
    exit 1
fi
# The image is for a Pi, whatever the machine building it is (often a VM).
[[ "$MODE" == image ]] && IS_VM=0

# Building the image, nothing runs: there is no systemd to start, restart or
# reload anything. systemctl itself skips those in a chroot it recognises,
# but not where /proc/1 is hidden (a container building the image), where it
# fails instead. So in --image they are skipped here; what works on files
# (enable, disable, mask, set-default, is-enabled) goes through.
if [[ "$MODE" == image ]]; then
    systemctl() {
        local a
        for a in "$@"; do
            case "$a" in
                -*) continue ;;
                start|stop|restart|reload|try-restart|reload-or-restart|try-reload-or-restart|daemon-reload|daemon-reexec)
                    echo "  (image build: not running systemctl $*)"
                    return 0 ;;
                *) break ;;
            esac
        done
        command systemctl "$@"
    }
fi

if [[ "$SERVICE_PORT" != "1" && "$SERVICE_PORT_RTC" == "1" ]]; then
    echo "--rtc only applies with --service-port" >&2
    exit 1
fi

# ── Helper: read one answer ───────────────────────────────────────────────────
# $1 variable to set   $2 prompt text   $3 default
# _interactive: can we ask the person running this? Under `curl ... | bash`
# stdin is the script itself, so questions go to the terminal, /dev/tty.
_interactive() {
    [[ "$MB_ASSUME_YES" != "1" ]] && { : < /dev/tty; } 2>/dev/null
}

_ask() {
    local _var="$1" _prompt="$2" _default="$3" _preset _reply
    _preset="${!_var-}"

    if [[ -n "$_preset" ]]; then
        printf '  %-34s %s\n' "$_prompt" "$_preset"
        return 0
    fi

    if ! _interactive; then
        printf -v "$_var" '%s' "$_default"
        printf '  %-34s %s (default)\n' "$_prompt" "$_default"
        return 0
    fi

    if [[ -n "$_default" ]]; then
        read -rp "$(printf '  %-34s [%s]: ' "$_prompt" "$_default")" _reply < /dev/tty
    else
        read -rp "$(printf '  %-34s: ' "$_prompt")" _reply < /dev/tty
    fi
    printf -v "$_var" '%s' "${_reply:-$_default}"
    return 0
}

_confirm() {
    local _prompt="$1" _reply
    _interactive || return 0
    read -rp "$_prompt" -n 1 _reply < /dev/tty; echo
    [[ $_reply =~ ^[Yy]$ ]]
}

# valid_content_key: base64 (standard alphabet) of exactly 32 bytes, the format
# `musallahboard-agent trust add` takes. The agent checks it again.
valid_content_key() {
    [[ "$1" =~ ^[A-Za-z0-9+/]{43}=$ ]] && [[ "$(printf '%s' "$1" | base64 -d 2>/dev/null | wc -c)" -eq 32 ]]
}

# ── Pre-flight checks ─────────────────────────────────────────────────────────
preflight() {
    [[ $EUID -eq 0 ]] && error "Do not run as root. Run as a user with sudo access."
    # Not `sudo -v` alone: it prompts even under NOPASSWD when any other
    # matching sudoers entry (e.g. %sudo) needs a password, which is a stock
    # Raspberry Pi OS user, and fails outright with no terminal (curl | bash
    # over ssh, MB_ASSUME_YES). Only ask for a password when one is needed.
    sudo -n true 2>/dev/null || sudo -v || error "This script requires sudo access."

    if [[ "$IS_VM" == "1" ]]; then
        [[ "$ARCH" == "arm64" || "$ARCH" == "amd64" ]] || \
            error "Unsupported architecture in a virtual machine: $(uname -m)"
        warn "Virtual machine: a test board (vm/mbvm), not a Raspberry Pi. Pi-only steps are skipped."
    else
        [[ "$ARCH" == "arm64" ]] || error "This script is for arm64 (Raspberry Pi) only. Got: $(uname -m)"

        # Check for Raspberry Pi OS
        [[ -f /boot/firmware/config.txt ]] || [[ -f /boot/config.txt ]] || \
            error "This does not look like Raspberry Pi OS (no config.txt found)."
    fi

    command -v apt-get >/dev/null || error "apt-get not found — is this Debian/Raspbian?"
    command -v systemctl >/dev/null || error "systemctl not found — systemd required"
}

# ── Banner ────────────────────────────────────────────────────────────────────
banner() {
    cat << 'BANNER'

  +----------------------------------------------+
  |       MusallahBoard Setup                    |
  |       Raspberry Pi (arm64) or test VM        |
  |       Kiosk - Hardened SSH - Wayland/cage    |
  +----------------------------------------------+

BANNER
}

# ── Interactive configuration ─────────────────────────────────────────────────
prompt_config() {
    section "Configuration"

    # HOSTNAME is set by bash to the machine's name; clear it so _ask works
    HOSTNAME="${MB_HOSTNAME-}"
    ADMIN_USER="${MB_ADMIN_USER-}"
    TIMEZONE="${MB_TIMEZONE-}"

    _ask HOSTNAME   "Hostname for this board"          "musallahboard"
    _ask ADMIN_USER "Admin username (SSH/sudo)"        ""
    _ask TIMEZONE   "Timezone"                         "America/Toronto"

    [[ -n "$ADMIN_USER" ]] || \
        error "An admin username is required (the account you will SSH in as). Set MB_ADMIN_USER when not running interactively."
    [[ "$ADMIN_USER" =~ ^[a-z_][a-z0-9_-]{0,31}$ ]] || \
        error "'$ADMIN_USER' is not a valid Linux username (lowercase letters, digits, - and _)."

    # Validate admin user is not a reserved name.
    for _reserved in musallahdaemon "$KIOSK_USER"; do
        [[ "$ADMIN_USER" == "$_reserved" ]] && \
            error "'$_reserved' is reserved for a MusallahBoard service account."
    done

    echo
    SSH_PUB_KEY="${MB_ADMIN_SSH_KEY-}"
    if [[ -n "$SSH_PUB_KEY" ]]; then
        info "SSH key supplied for $ADMIN_USER"
    elif _interactive; then
        echo "Paste the SSH public key for $ADMIN_USER (one line, starting ssh-ed25519 or ssh-rsa):"
        read -rp "> " SSH_PUB_KEY < /dev/tty
        [[ -z "$SSH_PUB_KEY" ]] && error "An SSH public key is required: the board has no password login."
    else
        error "An SSH public key is required: the board has no password login. Set MB_ADMIN_SSH_KEY."
    fi
    [[ "$SSH_PUB_KEY" =~ ^(ssh-|ecdsa-|sk-) ]] || \
        error "That does not look like an SSH public key (it should start with ssh-ed25519, ssh-rsa or ecdsa-)."

    echo
    CONTENT_KEY="${MB_CONTENT_KEY-}"
    if [[ -n "$CONTENT_KEY" ]]; then
        info "Content signing key supplied"
    elif _interactive; then
        echo "Paste LensBridge's content signing public key (base64, from the console):"
        read -rp "> " CONTENT_KEY < /dev/tty
    fi
    CONTENT_KEY="$(tr -d '[:space:]' <<< "$CONTENT_KEY")"
    [[ -n "$CONTENT_KEY" ]] || \
        error "The content signing public key is required: without it the board accepts no content or setup file. Set MB_CONTENT_KEY."
    valid_content_key "$CONTENT_KEY" || \
        error "That is not a content signing public key: it must be the base64 of 32 bytes (44 characters ending in =)."

    echo
    info "Summary"
    printf "  %-18s %s\n" "Hostname:"   "$HOSTNAME"
    printf "  %-18s %s\n" "Admin user:" "$ADMIN_USER  (SSH key auth, passwordless sudo)"
    printf "  %-18s %s\n" "Kiosk user:" "$KIOSK_USER  (auto-login, browser only, no sudo/SSH/shell)"
    printf "  %-18s %s\n" "Kiosk URL:"  "$KIOSK_URL  (served by the agent)"
    printf "  %-18s %s\n" "Timezone:"   "$TIMEZONE"
    printf "  %-18s %s\n" "Content key:" "$CONTENT_KEY"
    echo
    if ! _confirm "Continue? (y/n): "; then
        exit 0
    fi
    return 0
}

# ── Install packages ──────────────────────────────────────────────────────────
install_packages() {
    section "Installing packages"

    export DEBIAN_FRONTEND=noninteractive
    sudo apt-get update -qq

    # cage: single-app Wayland kiosk compositor
    # chromium: deb browser (not snap)
    local pkgs=(cage chromium ufw unattended-upgrades apt-listchanges curl)
    sudo apt-get install -y "${pkgs[@]}"

    # Purge services that add unnecessary network attack surface. Raspberry
    # Pi Connect was installed by earlier versions of this script.
    info "Purging unnecessary network services..."
    sudo apt-get purge -y rpcbind nfs-common rpi-connect rpi-connect-lite 2>/dev/null || true
    sudo apt-mark hold rpcbind nfs-common 2>/dev/null || true
}

# ── Hostname ──────────────────────────────────────────────────────────────────
setup_hostname() {
    section "Hostname"

    echo "$HOSTNAME" | sudo tee /etc/hostname > /dev/null
    if grep -q "127\.0\.1\.1" /etc/hosts; then
        sudo sed -i "s/^\(127\.0\.1\.1\s\+\).*/\1$HOSTNAME/" /etc/hosts
    else
        echo "127.0.1.1	$HOSTNAME" | sudo tee -a /etc/hosts > /dev/null
    fi
    sudo hostnamectl set-hostname "$HOSTNAME"
    info "Hostname set to $HOSTNAME"
}

# ── Clock ─────────────────────────────────────────────────────────────────────
setup_clock() {
    section "Clock"

    if ! timedatectl list-timezones | grep -qx "$TIMEZONE"; then
        error "Unknown timezone '$TIMEZONE' (see: timedatectl list-timezones)"
    fi
    sudo timedatectl set-timezone "$TIMEZONE"
    info "Timezone set to $TIMEZONE"

    sudo systemctl enable --now systemd-timesyncd
    sudo timedatectl set-ntp true

    info "Waiting for NTP sync (up to ${NTP_SYNC_TIMEOUT_SEC}s)..."
    local waited=0
    while [[ $waited -lt $NTP_SYNC_TIMEOUT_SEC ]]; do
        if [[ "$(timedatectl show --property=NTPSynchronized --value)" == "yes" ]]; then
            info "Clock synced: $(date)"
            return 0
        fi
        sleep 2
        waited=$((waited + 2))
    done

    error "Clock did not sync within ${NTP_SYNC_TIMEOUT_SEC}s.
       Prayer times and agent authentication both depend on it.
       Check outbound NTP (udp/123) and rerun."
}

# ── Admin user ────────────────────────────────────────────────────────────────
setup_admin_user() {
    section "Admin user: $ADMIN_USER"

    if ! id "$ADMIN_USER" &>/dev/null; then
        sudo useradd -m -s /bin/bash "$ADMIN_USER"
        info "Created user $ADMIN_USER"
    else
        info "User $ADMIN_USER already exists"
    fi

    sudo usermod -aG sudo "$ADMIN_USER"
    echo "$ADMIN_USER ALL=(ALL) NOPASSWD:ALL" | sudo tee "/etc/sudoers.d/$ADMIN_USER" > /dev/null
    sudo chmod 440 "/etc/sudoers.d/$ADMIN_USER"

    sudo mkdir -p "/home/$ADMIN_USER/.ssh"
    sudo chmod 700 "/home/$ADMIN_USER/.ssh"
    echo "$SSH_PUB_KEY" | sudo tee "/home/$ADMIN_USER/.ssh/authorized_keys" > /dev/null
    sudo chmod 600 "/home/$ADMIN_USER/.ssh/authorized_keys"
    sudo chown -R "$ADMIN_USER:$ADMIN_USER" "/home/$ADMIN_USER/.ssh"
    info "$ADMIN_USER configured with SSH key auth and passwordless sudo"
}

# ── Service user (musallahdaemon) ─────────────────────────────────────────────
setup_service_user() {
    section "Service user: $SERVICE_USER"

    if id "$SERVICE_USER" &>/dev/null; then
        info "User '$SERVICE_USER' already exists"
    else
        sudo useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
        info "Created system user '$SERVICE_USER'"
    fi

    # systemd-journal membership lets the agent read the journal
    if getent group systemd-journal &>/dev/null; then
        sudo usermod -aG systemd-journal "$SERVICE_USER"
        info "Added '$SERVICE_USER' to systemd-journal group"
    fi

    # video membership for vcgencmd (thermal/throttle reporting)
    if getent group video &>/dev/null; then
        sudo usermod -aG video "$SERVICE_USER"
        info "Added '$SERVICE_USER' to video group"
    fi
}

# ── Kiosk user (musallahkiosk) ────────────────────────────────────────────────
setup_kiosk_user() {
    section "Kiosk user: $KIOSK_USER"

    if id "$KIOSK_USER" &>/dev/null; then
        info "User '$KIOSK_USER' already exists"
    else
        sudo useradd -m -s /bin/bash "$KIOSK_USER"
        info "Created user '$KIOSK_USER'"
    fi

    # Lock the account (no password login)
    sudo passwd -l "$KIOSK_USER" >/dev/null

    # Strip privilege groups; keep video and render for cage/GPU
    for g in sudo adm dialout cdrom plugdev games users input netdev; do
        sudo gpasswd -d "$KIOSK_USER" "$g" 2>/dev/null || true
    done
    for g in video render; do
        getent group "$g" >/dev/null && sudo usermod -aG "$g" "$KIOSK_USER"
    done

    # Remove any sudoers file
    sudo rm -f "/etc/sudoers.d/$KIOSK_USER"

    # .bashrc that exits immediately (no shell access)
    sudo tee "/home/$KIOSK_USER/.bashrc" > /dev/null <<'BASHRC'
readonly PATH
echo "This account is for kiosk use only. Direct shell access is not permitted."
exit
BASHRC
    printf 'export PATH="/home/%s/bin"\n' "$KIOSK_USER" | sudo tee "/home/$KIOSK_USER/.bash_profile" > /dev/null
    sudo chown "$KIOSK_USER:$KIOSK_USER" \
        "/home/$KIOSK_USER/.bashrc" "/home/$KIOSK_USER/.bash_profile"

    info "Kiosk user locked down (no sudo, no shell, video+render only)"
}

# ── Display stack (config dir, kiosk.env) ──────────────────────────
setup_display_stack() {
    section "Display stack"

    sudo mkdir -p "$CONFIG_DIR"
    sudo chown "$SERVICE_USER:$SERVICE_USER" "$CONFIG_DIR"
    sudo chmod 0751 "$CONFIG_DIR"

    # Kiosk environment: software-rendering fallback for VMs. An image is for
    # a Pi, wherever it is being built.
    if [[ "$MODE" != image ]] && systemd-detect-virt --quiet 2>/dev/null; then
        sudo tee "$CONFIG_DIR/kiosk.env" > /dev/null << 'EOF'
WLR_NO_HARDWARE_CURSORS=1
WLR_RENDERER=pixman
WLR_DRM_NO_MODIFIERS=1
EOF
        sudo chmod 0644 "$CONFIG_DIR/kiosk.env"
        info "Virtualized host detected — wrote kiosk.env (software-render fallback)"
    else
        sudo rm -f "$CONFIG_DIR/kiosk.env"
        info "Bare-metal host — no kiosk.env needed"
    fi
}

# ── Disable tty1 autologin ────────────────────────────────────────────────────
disable_tty1_autologin() {
    section "Disable tty1 autologin"

    sudo rm -f /etc/systemd/system/getty@tty1.service.d/autologin.conf
    sudo systemctl daemon-reload
    info "tty1 autologin removed"
}

# ── SSH hardening ─────────────────────────────────────────────────────────────
# ADMIN_USER is one name (a normal install) or a space-separated list
# (--firstboot: every admin cloud-init made). With none, nobody is allowed in
# by name, and no account can log in anyway: no passwords, no keys.
harden_ssh() {
    section "SSH hardening"

    local allow=""
    [[ -n "${ADMIN_USER// }" ]] && allow="AllowUsers $ADMIN_USER"
    sudo mkdir -p /etc/ssh/sshd_config.d
    sudo tee "$SSHD_HARDENING" > /dev/null << EOF
PasswordAuthentication no
KbdInteractiveAuthentication no
ChallengeResponseAuthentication no
PubkeyAuthentication yes
PermitRootLogin no
${allow}
DenyUsers $KIOSK_USER $SERVICE_USER
EOF
    sudo rm -f "$SSHD_HARDENING_OLD"

    sudo systemctl reload ssh 2>/dev/null || sudo systemctl reload sshd 2>/dev/null || true
    if [[ -n "$allow" ]]; then
        info "SSH: key-based auth only, for: $ADMIN_USER"
        [[ "$MODE" == install ]] && warn "Verify you can SSH as $ADMIN_USER before closing this session!"
    else
        warn "SSH: key-based auth only, and no admin account to allow: nobody can log in over SSH"
    fi
    return 0
}

# ── logind ────────────────────────────────────────────────────────────────────
setup_logind() {
    section "logind (idle/power key)"

    sudo mkdir -p /etc/systemd/logind.conf.d
    sudo tee /etc/systemd/logind.conf.d/kiosk.conf > /dev/null << 'EOF'
[Login]
IdleAction=ignore
HandlePowerKey=ignore
HandleLidSwitch=ignore
EOF
    info "logind: idle/power/lid actions ignored"
}

# ── Journal ───────────────────────────────────────────────────────────────────
setup_journal() {
    section "Journal (persistent, size-capped)"

    sudo mkdir -p /var/log/journal
    sudo systemd-tmpfiles --create --prefix /var/log/journal

    sudo mkdir -p /etc/systemd/journald.conf.d
    sudo tee /etc/systemd/journald.conf.d/size.conf > /dev/null << 'EOF'
[Journal]
Storage=persistent
SystemMaxUse=100M
RuntimeMaxUse=20M
Compress=yes
EOF

    sudo systemctl restart systemd-journald
    info "Journal: persistent, 100 MB cap"
}

# ── Unattended upgrades ───────────────────────────────────────────────────────
setup_unattended_upgrades() {
    section "Unattended security upgrades"

    sudo tee /etc/apt/apt.conf.d/20auto-upgrades > /dev/null << 'EOF'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
APT::Periodic::AutocleanInterval "7";
EOF
    info "Unattended security upgrades enabled"
}

# ── UFW firewall ──────────────────────────────────────────────────────────────
setup_ufw() {
    section "UFW firewall"

    sudo ufw default deny incoming
    sudo ufw default allow outgoing
    sudo ufw allow ssh
    sudo ufw --force enable
    info "Firewall: deny inbound except SSH"
}

# ── Pi-specific extras ────────────────────────────────────────────────────────
setup_pi_extras() {
    if [[ "$IS_VM" == "1" ]]; then
        info "Virtual machine: no hardware watchdog"
        return
    fi
    section "Hardware watchdog"
    sudo mkdir -p /etc/systemd/system.conf.d
    sudo tee /etc/systemd/system.conf.d/watchdog.conf > /dev/null << 'EOF'
[Manager]
RuntimeWatchdogSec=15
RebootWatchdogSec=2min
EOF
    info "Watchdog: systemd pings /dev/watchdog0 every 15s; reboots if it misses"
}

# ── Download and install agent binary ─────────────────────────────────────────
install_agent_binary() {
    section "Agent binary"

    # A local file (the image build, or testing a build of this checkout).
    if [[ -n "${MB_AGENT_BINARY-}" ]]; then
        [[ -f "$MB_AGENT_BINARY" ]] || error "MB_AGENT_BINARY: no such file: $MB_AGENT_BINARY"
        sudo install -o root -g root -m 0755 "$MB_AGENT_BINARY" "$BINARY_DEST"
        info "Installed $BINARY_DEST from $MB_AGENT_BINARY ($("$BINARY_DEST" version 2>/dev/null || echo 'version unknown'))"
        return 0
    fi

    info "Downloading from $BINARY_URL"
    local tmp_binary="/tmp/musallahboard-agent-$$"

    if ! curl -fsSL "$BINARY_URL" -o "$tmp_binary"; then
        error "Failed to download agent binary from $BINARY_URL"
    fi

    sudo install -o root -g root -m 0755 "$tmp_binary" "$BINARY_DEST"
    rm -f "$tmp_binary"
    info "Installed $BINARY_DEST ($("$BINARY_DEST" version 2>/dev/null || echo 'version unknown'))"
}

# ── System files ──────────────────────────────────────────────────────────────
# The systemd units, udev rules, sudoers allow-list, modules-load list, kiosk
# launcher and the kiosk's local page are embedded in the agent binary
# (packaging/ in the repository), so this script has no copies of them to
# keep in sync. install-system writes them, removes files earlier versions
# installed, reloads systemd and udev, and enables the units.
install_system_files() {
    section "System files"
    sudo "$BINARY_DEST" install-system
    sudo systemctl start musallahboard-agent-update.path
    info "Units, udev rules, sudoers and kiosk launcher installed from the agent binary"
}

# ── State directories and trust store ─────────────────────────────────────────
install_state_dirs() {
    section "State directories and trust store"

    # Owned by the service user, as StateDirectory= in the unit expects: a
    # mismatched owner there makes systemd chown the whole tree recursively.
    sudo install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0750 "$STATE_DIR"
    # Written by root tools (the USB helper, `musallahboard-agent import`) and
    # consumed by the daemon.
    sudo install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0770 "$INBOX_DIR"
    # The self-updater's rollback copy of the previous agent.
    sudo install -d -o root -g root -m 0755 "$LIB_DIR"
    info "$INBOX_DIR ($SERVICE_USER 0770), $LIB_DIR (root 0755)"

    # Root-owned, world-readable, never overwritten: it holds the content keys
    # pinned here, by `trust fetch` or at enrollment, and any release keys an
    # admin added.
    if sudo test -e "$TRUST_PATH"; then
        info "Keeping existing $TRUST_PATH"
    else
        printf '{"content":[],"release":[]}\n' | sudo tee "$TRUST_PATH.tmp" > /dev/null
        sudo chown root:root "$TRUST_PATH.tmp"
        sudo chmod 0644 "$TRUST_PATH.tmp"
        sudo mv -f "$TRUST_PATH.tmp" "$TRUST_PATH"
        info "Created $TRUST_PATH (root 0644, no keys yet)"
    fi
}

# ── Content signing key ───────────────────────────────────────────────────────
# Pinned through the agent's own root-only trust path, which checks the key
# and the file again and leaves an already trusted key alone.
pin_content_key() {
    section "Content signing key"
    sudo "$BINARY_DEST" trust add content "$CONTENT_KEY" "lensbridge (setup.sh)"
}

# ── Apply appliance policy ────────────────────────────────────────────────────
apply_appliance_policy() {
    section "Appliance boot policy"

    # Disable display managers
    if systemctl list-unit-files 2>/dev/null | grep -qE '^(lightdm|gdm3?|sddm)\.service'; then
        local now=--now
        [[ "$MODE" == image ]] && now=
        sudo systemctl disable $now lightdm.service gdm.service gdm3.service sddm.service 2>/dev/null || true
        info "Disabled display manager(s)"
    else
        info "No display manager installed"
    fi

    sudo systemctl set-default multi-user.target >/dev/null
    info "Default boot target: multi-user.target"

    local target
    target="$(systemctl get-default 2>/dev/null || echo unknown)"
    [[ "$target" == "multi-user.target" ]] || \
        error "default target is '$target' after set-default"
}

# ── Summary and reboot ────────────────────────────────────────────────────────
print_summary() {
    cat << EOF

  +----------------------------------------------------------+
  |  Setup complete                                          |
  +----------------------------------------------------------+

  Hostname   : $HOSTNAME
  Admin SSH  : ssh $ADMIN_USER@<ip>
  Kiosk URL  : $KIOSK_URL (served by the agent)

  FINAL STEP - plug in this board's setup USB stick, exported from the
  LensBridge console (Devices, the board, Setup file or USB kit). The board
  takes its identity, content, board app and agent from it; it needs no
  internet. Until then the screen says "This board is not set up yet" and
  shows the board's address (the <ip> to SSH to).

  If its setup file names a backend, the board connects to LensBridge by
  itself whenever it has internet. (Maintainers can still enroll with a
  token: sudo musallahboard-agent enroll --token=<token> --backend=<url>)

  Without internet, update it from a phone (the MusallahBoard Push app)
  or a laptop plugged straight into its ethernet port, at http://10.77.0.1/.
  The service port is on: on any network that hands out addresses the
  board is just a client there. (Never serve addresses from this board:
  sudo musallahboard-agent service-port off)
  To fit a DS3231 clock module as well, re-run this setup with --rtc:
      curl -fsSL $SETUP_URL | bash -s -- --service-port --rtc

  * Boots multi-user -> musallahboard-kiosk.service -> cage -> browser
  * SSH as $ADMIN_USER to manage the system
  * $KIOSK_USER has no SSH, no sudo, no shell
  * No display manager; tty1 is owned by the kiosk unit

EOF

    warn "Verify SSH works as $ADMIN_USER before rebooting!"
    echo

    case "$MB_REBOOT" in
        never)
            info "Not rebooting (MB_REBOOT=never). Reboot when you are ready."
            ;;
        auto)
            info "Rebooting now (MB_REBOOT=auto)."
            sudo reboot
            ;;
        *)
            if _confirm "Reboot now? (y/n): "; then
                sudo reboot
            fi
            ;;
    esac
    return 0
}

# ══ Service port (setup.sh --service-port) ════════════════════════════════════
# Everything below runs only with --service-port, on a board that has already
# been set up. See docs/architecture.md, sections 9.5 and 13.
# Every step is idempotent: re-running it is safe, and is how you add --rtc
# later.

service_port_preflight() {
    [[ $EUID -eq 0 ]] && error "Do not run as root. Run as a user with sudo access."
    # Not `sudo -v`: it prompts even under NOPASSWD when any other matching
    # sudoers entry (e.g. %sudo) needs a password, which breaks ssh 'setup.sh'.
    sudo -n true 2>/dev/null || sudo -v || error "This script requires sudo access."
    command -v nmcli >/dev/null || \
        error "nmcli not found. The service port needs NetworkManager (the Raspberry Pi OS default)."
    [[ -x "$BINARY_DEST" ]] || \
        error "$BINARY_DEST is not installed. Run the normal setup (without --service-port) first."
}

service_port_install_packages() {
    section "Service port: packages"
    # dnsmasq-base: NetworkManager's "shared" mode runs it for DHCP/DNS.
    # util-linux-extra: hwclock, which the agent uses to save the time to the RTC.
    local pkgs=(dnsmasq-base util-linux-extra)
    # Skip apt when they are already there: re-running setup.sh --service-port
    # to add --rtc may happen on a board that has no internet any more.
    if dpkg -s "${pkgs[@]}" &>/dev/null; then
        info "${pkgs[*]} already installed"
        return
    fi
    export DEBIAN_FRONTEND=noninteractive
    sudo apt-get update -qq
    sudo apt-get install -y "${pkgs[@]}"
}

# Prints the name of a saved (not auto-generated) wired profile eth0 could
# use, other than ours; fails if there is none. NetworkManager's automatic
# "Wired connection 1" lives under /run and is not counted: it stops being
# generated once any saved profile for eth0 exists, including ours.
_saved_wired_profile() {
    local name type file iface
    while IFS=: read -r name type file; do
        [[ "$type" == "802-3-ethernet" ]] || continue
        [[ "$name" == "$SERVICE_CONN" || "$name" == "$LAN_CONN" ]] && continue
        [[ -z "$file" || "$file" == /run/* ]] && continue
        iface="$(nmcli -g connection.interface-name connection show "$name" 2>/dev/null || true)"
        [[ -z "$iface" || "$iface" == "$SERVICE_IFACE" ]] || continue
        echo "$name"
        return 0
    done < <(nmcli -t -f NAME,TYPE,FILENAME connection show)
    return 1
}

_nm_has() { nmcli -g NAME connection show | grep -qxF "$1"; }

service_port_profiles() {
    section "Service port: ethernet profiles"

    # Two profiles on eth0, tried in priority order (docs/architecture.md, 13):
    #
    #  1. A DHCP client ($LAN_CONN, default priority). Plugged into a real
    #     network, the board just gets an address there, as it does today.
    #     A laptop runs no DHCP server, so with one plugged in this fails
    #     after ipv4.dhcp-timeout and, with the single retry `service-port on`
    #     gives it, NetworkManager moves on to...
    #  2. the service port ($SERVICE_CONN): shared mode, i.e. a DHCP server,
    #     at the lowest priority NetworkManager has (-999). It only ever
    #     autoconnects after every other eth0 profile has failed.
    #
    # This is what stops a board booted on a building's network from handing
    # out addresses there. The remaining exposure is a network whose DHCP
    # server does not answer within 30 s.
    local lan_props=(
        connection.interface-name "$SERVICE_IFACE"
        connection.autoconnect yes
        connection.autoconnect-priority 0
        ipv4.method auto
        ipv4.dhcp-timeout 30
        ipv6.method auto
    )
    local existing
    if _nm_has "$LAN_CONN"; then
        sudo nmcli connection modify "$LAN_CONN" "${lan_props[@]}"
        info "Updated NetworkManager connection $LAN_CONN (DHCP client, tried first)"
    elif existing="$(_saved_wired_profile)"; then
        info "Keeping your saved wired profile '$existing' as eth0's first choice"
        warn "It keeps NetworkManager's default of 4 attempts, so a laptop may wait a few minutes"
        warn "for an address. Delete it to use $LAN_CONN instead, then re-run this setup."
    else
        sudo nmcli connection add type ethernet con-name "$LAN_CONN" "${lan_props[@]}"
        info "Created NetworkManager connection $LAN_CONN (DHCP client, tried first)"
    fi

    # autoconnect stays off here: `musallahboard-agent service-port on` below
    # turns it on and `service-port off` turns it off, so that switch alone
    # decides whether this board can ever act as a DHCP server.
    local svc_props=(
        connection.interface-name "$SERVICE_IFACE"
        connection.autoconnect no
        connection.autoconnect-priority -999
        ipv4.method shared
        ipv4.addresses "$SERVICE_ADDR"
        ipv6.method disabled
    )
    if _nm_has "$SERVICE_CONN"; then
        sudo nmcli connection modify "$SERVICE_CONN" "${svc_props[@]}"
        info "Updated NetworkManager connection $SERVICE_CONN"
    else
        sudo nmcli connection add type ethernet con-name "$SERVICE_CONN" "${svc_props[@]}"
        info "Created NetworkManager connection $SERVICE_CONN"
    fi
    info "$SERVICE_IFACE: shared, ${SERVICE_ADDR}, IPv6 off — a laptop plugged in gets a 10.77.0.x address"
}

service_port_firewall() {
    section "Service port: firewall"
    # SSH is already allowed on every interface by the normal setup. ufw skips
    # rules that already exist, so these are safe to re-add. The upload server
    # (port 80) is reachable only from the service port's own subnet on eth0,
    # never from Wi-Fi or a building network.
    sudo ufw allow in on "$SERVICE_IFACE" to any port 67 proto udp comment 'musallahboard service port DHCP'
    sudo ufw allow in on "$SERVICE_IFACE" to any port 53 comment 'musallahboard service port DNS'
    sudo ufw allow in on "$SERVICE_IFACE" from "$SERVICE_NET" to any port 80 proto tcp comment 'musallahboard upload server'
    info "Firewall: DHCP, DNS and uploads (80/tcp from $SERVICE_NET) allowed in on $SERVICE_IFACE"
}

service_port_rtc() {
    section "Service port: RTC (DS3231)"
    local config_txt=/boot/firmware/config.txt
    [[ -f "$config_txt" ]] || config_txt=/boot/config.txt

    if grep -qxF "$RTC_OVERLAY" "$config_txt"; then
        info "$RTC_OVERLAY already in $config_txt"
    else
        echo "$RTC_OVERLAY" | sudo tee -a "$config_txt" > /dev/null
        info "Added $RTC_OVERLAY to $config_txt (takes effect after a reboot)"
    fi

    # fake-hwclock restores the time saved at the last shutdown, which is
    # wrong by however long the board was off and would fight the RTC.
    if dpkg -s fake-hwclock &>/dev/null; then
        sudo apt-get purge -y fake-hwclock
        info "Removed fake-hwclock"
    else
        info "fake-hwclock not installed"
    fi

    # musallahboard-rtc.service reads the RTC when udev sees rtc0 (the
    # ds3231 driver is a module, so the kernel's own boot-time read is over by
    # then). Writing it back is the agent's job.
    sudo "$BINARY_DEST" install-system --rtc
    info "RTC read at boot via musallahboard-rtc.service"
    warn "Reboot once with the RTC fitted. The agent saves the time to it whenever it corrects the clock,"
    warn "for example on the next upload from a laptop (mbpush or http://10.77.0.1/)."
}

# Part of the normal setup: everything the service port needs, left OFF, so
# `sudo musallahboard-agent service-port on` works later without this script
# (which a curl | bash install does not leave on the board). The service-port
# profile does not autoconnect until it is switched on, so eth0 behaves as
# before: a DHCP client on whatever network it is plugged into.
prepare_service_port() {
    if ! command -v nmcli >/dev/null; then
        warn "nmcli not found: the ethernet service port needs NetworkManager; skipping it"
        return
    fi
    service_port_install_packages
    service_port_profiles
    service_port_firewall
    info "Service port prepared; it is switched on once the agent is installed"
}

# On by default: eth0 stays a DHCP client on any network that answers, and
# serves a phone or laptop plugged straight in only when none does; the
# agent's service-port guard stops it serving a network. --no-service-port
# (MB_SERVICE_PORT=0) leaves it off for a board that must never serve
# addresses.
enable_service_port() {
    if [[ "${MB_SERVICE_PORT:-1}" == "0" ]]; then
        info "Service port left OFF (--no-service-port). Turn it on later with: sudo musallahboard-agent service-port on"
        return
    fi
    if ! command -v nmcli >/dev/null; then
        return
    fi
    section "Service port: switch on"
    sudo "$BINARY_DEST" service-port on || warn "Could not switch the service port on; run: sudo musallahboard-agent service-port on"
}

service_port_switch_on() {
    section "Service port: switch on"
    # If eth0 is on a network right now, `service-port on` leaves the port
    # stopped (it would serve DHCP there) and says so. It restarts the agent,
    # which then also runs with the units installed above.
    sudo "$BINARY_DEST" service-port on
}

service_port_summary() {
    cat << EOF

  +----------------------------------------------------------+
  |  The service port is ON                                  |
  +----------------------------------------------------------+

  The board keeps showing what it has installed, and syncs from LensBridge
  whenever it can reach it. To update it without internet:

    - USB stick: copy the .mbu files to it (root, or a MusallahBoard/ folder)
      and plug it in. The screen says when it is done.
    - Laptop or phone: plug into the board's ethernet port and open
        http://${SERVICE_ADDR%/*}/
      or run  mbpush <file.mbu>...  (it also shows the board's clock).
      Allow up to a minute after plugging in: the board first tries to get
      an address from the laptop, and only then starts serving one.

  Check it any time:            sudo musallahboard-agent status
  Before joining a network:     sudo musallahboard-agent service-port off

EOF
}

service_port_main() {
    service_port_preflight
    service_port_install_packages
    service_port_profiles
    service_port_firewall
    if [[ "$SERVICE_PORT_RTC" == "1" ]]; then
        service_port_rtc
    fi
    service_port_switch_on
    service_port_summary
}

# ══ Pre-built image, part 1 (setup.sh --image) ════════════════════════════════
# Runs as root inside a chroot of Raspberry Pi OS Lite (image/build.sh). Only
# the steps every board shares; the rest is --firstboot's. A chroot has no
# running systemd: `systemctl enable/mask/set-default` work on the files, and
# start/restart/reload are skipped by systemctl itself ("Running in chroot").

image_preflight() {
    [[ $EUID -eq 0 ]] || error "--image runs as root, inside the image's chroot (image/build.sh)."
    # A booted system has /run/systemd/system (sd_booted). The image's chroot
    # does not: nothing in it was booted.
    [[ ! -d /run/systemd/system ]] || \
        error "--image changes a system for good and is only for an image being built: run it in the image's chroot (image/build.sh), not on a running system."
    [[ "$ARCH" == "arm64" ]] || error "The image is arm64. Got: $(uname -m)"
    [[ -f /boot/firmware/config.txt ]] || \
        error "No /boot/firmware/config.txt: mount the image's boot partition there first."
    command -v apt-get >/dev/null || error "apt-get not found: is this Raspberry Pi OS?"
    [[ -f "${BASH_SOURCE[0]}" ]] || \
        error "--image copies this script into the image for --firstboot: run it from a file, not curl | bash."
}

# The answers a board gets until cloud-init (Raspberry Pi Imager) gives it
# its own: the same defaults a normal install offers.
image_defaults() {
    section "Image defaults"
    HOSTNAME="${MB_HOSTNAME:-musallahboard}"
    TIMEZONE="${MB_TIMEZONE:-America/Toronto}"
    [[ -f "/usr/share/zoneinfo/$TIMEZONE" ]] || error "Unknown timezone '$TIMEZONE'"

    echo "$HOSTNAME" > /etc/hostname
    if grep -q "127\.0\.1\.1" /etc/hosts; then
        sed -i "s/^\(127\.0\.1\.1\s\+\).*/\1$HOSTNAME/" /etc/hosts
    else
        echo "127.0.1.1	$HOSTNAME" >> /etc/hosts
    fi
    ln -sf "/usr/share/zoneinfo/$TIMEZONE" /etc/localtime
    echo "$TIMEZONE" > /etc/timezone
    info "Hostname $HOSTNAME, time zone $TIMEZONE (cloud-init may set others at first boot)"

    # Raspberry Pi OS's first-boot wizard asks for a user name and password
    # on tty8 and switches the screen to it, over the kiosk, on a board with
    # no keyboard. cloud-init masks it when it makes a user; a card flashed
    # without one would show the wizard forever. Admins come from cloud-init.
    systemctl mask userconfig.service >/dev/null 2>&1
    info "Masked userconfig.service (admin accounts come from cloud-init / Raspberry Pi Imager)"
}

# The board app, imported by the agent from its inbox within seconds of the
# first boot (signed with the release key; checked like any other package).
image_app_package() {
    [[ -n "${MB_APP_PACKAGE-}" ]] || { info "No board app in the image: the board fetches it when online, or from a USB kit"; return 0; }
    section "Board app"
    [[ -f "$MB_APP_PACKAGE" ]] || error "MB_APP_PACKAGE: no such file: $MB_APP_PACKAGE"
    local dest
    dest="$INBOX_DIR/$(basename "$MB_APP_PACKAGE")"
    [[ "$dest" == *.mbu ]] || dest="$dest.mbu"
    # image/build.sh has checked its signature (mbpack verify); the agent
    # checks it again before installing it.
    install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0640 "$MB_APP_PACKAGE" "$dest"
    info "Queued $(basename "$dest") in $INBOX_DIR"
}

# musallahboard-firstboot.service: this script's --firstboot, once after the
# first boot (and again whenever a musallahboard.txt is put on the card).
install_firstboot() {
    section "First-boot service"
    install -o root -g root -m 0755 "${BASH_SOURCE[0]}" "$SETUP_COPY"
    cat > "$FIRSTBOOT_UNIT" << EOF
# Written by setup.sh --image. See docs/architecture.md, section 17.
[Unit]
Description=MusallahBoard first boot (SSH, firewall, service port, content key)
# After cloud-init made the admin account (cloud-init-network) and applied
# its config, with NetworkManager up for the service-port profiles. None of
# these is ordered after multi-user.target, so this can be part of it.
After=cloud-init-network.service cloud-config.service NetworkManager.service
Wants=NetworkManager.service
RequiresMountsFor=/boot/firmware
# Once, and again whenever the card carries a new musallahboard.txt.
ConditionPathExists=|!$FIRSTBOOT_DONE
ConditionPathExists=|$BOOT_CONFIG

[Service]
Type=oneshot
ExecStart=$SETUP_COPY --firstboot
StandardOutput=journal+console

[Install]
WantedBy=multi-user.target
EOF
    chmod 0644 "$FIRSTBOOT_UNIT"
    systemctl enable musallahboard-firstboot.service >/dev/null 2>&1
    cat > /boot/firmware/musallahboard-example.txt << 'EOF'
# MusallahBoard: this board's settings, read once at boot.
#
# Copy this file to musallahboard.txt (same folder, the SD card's boot
# partition) and fill it in. The board reads it at its next boot, applies it,
# and deletes it. If something is wrong with it, the board renames it to
# musallahboard-failed.txt and writes the reason at the end.
#
# LensBridge's content signing public key (base64, 44 characters ending in =).
# The console shows it; the backend publishes it at /api/agent/signing-keys.
# Without it the board accepts no setup file and no content.
content_key =

# The ethernet service port, for a laptop or phone plugged straight in:
# on (default) or off. Read at the first boot only.
service_port = on
EOF
    info "Installed $FIRSTBOOT_UNIT and $SETUP_COPY"
    info "Wrote /boot/firmware/musallahboard-example.txt"
}

image_main() {
    image_preflight
    info "Building the MusallahBoard image (setup.sh --image)"

    install_packages
    service_port_install_packages
    image_defaults
    setup_service_user
    setup_kiosk_user
    setup_display_stack
    disable_tty1_autologin
    setup_logind
    setup_journal
    setup_unattended_upgrades
    setup_pi_extras

    install_agent_binary
    install_state_dirs
    install_system_files
    apply_appliance_policy
    image_app_package
    install_firstboot
    echo
    info "Image steps done. The rest happens on the board's first boot (setup.sh --firstboot)."
}

# ══ Pre-built image, part 2 (setup.sh --firstboot) ════════════════════════════
# Run by musallahboard-firstboot.service, as root, on the board. Needs no
# internet. The one-time part runs at the first boot; the content key part
# whenever /boot/firmware/musallahboard.txt is there.

firstboot_preflight() {
    [[ $EUID -eq 0 ]] || error "--firstboot runs as root (musallahboard-firstboot.service)."
    [[ -d /run/systemd/system ]] || error "--firstboot runs on the booted board (musallahboard-firstboot.service)."
    [[ -x "$BINARY_DEST" ]] || error "$BINARY_DEST is not installed: is this a MusallahBoard image?"
}

# Moves a bad musallahboard.txt aside with the reason, so whoever looks at the
# card in a computer sees what went wrong, and stops.
_boot_config_failed() {
    if [[ -f "$BOOT_CONFIG" ]]; then
        { echo; echo "# The board did not apply this file: $*"; } >> "$BOOT_CONFIG"
        mv -f "$BOOT_CONFIG" "$BOOT_CONFIG_FAILED"
        sync
    fi
    error "$BOOT_CONFIG: $*"
}

# read_boot_config: CONTENT_KEY and MB_SERVICE_PORT from musallahboard.txt,
# `key = value` lines. Written on any computer, so CRLF and a BOM are fine.
read_boot_config() {
    CONTENT_KEY=""
    [[ -f "$BOOT_CONFIG" ]] || return 0
    info "Reading $BOOT_CONFIG"
    local line key value n=0
    while IFS= read -r line || [[ -n "$line" ]]; do
        n=$((n + 1))
        line="${line//$'\r'/}"
        [[ $n -eq 1 ]] && line="${line#$'\xef\xbb\xbf'}"
        [[ "$line" =~ ^[[:space:]]*(#|$) ]] && continue
        [[ "$line" == *=* ]] || _boot_config_failed "line $n is not 'name = value': $line"
        key="$(tr -d '[:space:]' <<< "${line%%=*}")"
        value="$(tr -d '[:space:]' <<< "${line#*=}")"
        case "$key" in
            content_key)  CONTENT_KEY="$value" ;;
            service_port)
                case "${value,,}" in
                    on|yes|true|1)  export MB_SERVICE_PORT=1 ;;
                    off|no|false|0) export MB_SERVICE_PORT=0 ;;
                    *) _boot_config_failed "service_port must be on or off, not '$value'" ;;
                esac ;;
            *) _boot_config_failed "unknown setting '$key' on line $n" ;;
        esac
    done < "$BOOT_CONFIG"
    if [[ -n "$CONTENT_KEY" ]] && ! valid_content_key "$CONTENT_KEY"; then
        _boot_config_failed "content_key is not a content signing public key (the base64 of 32 bytes: 44 characters ending in =)"
    fi
    return 0
}

# The admins cloud-init (Raspberry Pi Imager) made: members of sudo with a
# login shell, other than the board's own accounts. Raspberry Pi OS's
# placeholder `pi` has no shell until cloud-init renames it into an admin.
firstboot_admins() {
    local members u shell admins=()
    members="$(getent group sudo | cut -d: -f4 | tr ',' ' ')"
    for u in $members; do
        [[ "$u" == "$KIOSK_USER" || "$u" == "$SERVICE_USER" ]] && continue
        shell="$(getent passwd "$u" | cut -d: -f7)"
        [[ -z "$shell" || "$shell" == */nologin || "$shell" == */false ]] && continue
        admins+=("$u")
    done
    echo "${admins[*]}"
}

firstboot_main() {
    firstboot_preflight
    read_boot_config

    if [[ ! -e "$FIRSTBOOT_DONE" ]]; then
        info "MusallahBoard first boot (setup.sh --firstboot)"
        ADMIN_USER="$(firstboot_admins)"
        harden_ssh
        setup_ufw
        prepare_service_port
        enable_service_port
        install -o root -g root -m 0644 /dev/null "$FIRSTBOOT_DONE"
        date -u +%FT%TZ > "$FIRSTBOOT_DONE"
        info "First boot done"
    fi

    if [[ -n "$CONTENT_KEY" ]]; then
        pin_content_key
    elif [[ -f "$BOOT_CONFIG" ]]; then
        info "$BOOT_CONFIG has no content_key; nothing to pin"
    fi
    if [[ -f "$BOOT_CONFIG" ]]; then
        rm -f "$BOOT_CONFIG" "$BOOT_CONFIG_FAILED"
        sync
        info "Applied and removed $BOOT_CONFIG"
    fi
    if "$BINARY_DEST" trust show 2>/dev/null | grep -q "^  none\. This board cannot verify any content"; then
        warn "No content signing key is pinned: the board accepts no setup file or content until one is."
        warn "Put musallahboard.txt with content_key = ... on the card's boot partition and reboot (see musallahboard-example.txt)."
    fi
    return 0
}

# ── Main ──────────────────────────────────────────────────────────────────────
main() {
    case "$MODE" in
        image)     image_main; return ;;
        firstboot) firstboot_main; return ;;
    esac
    if [[ "$SERVICE_PORT" == "1" ]]; then
        service_port_main
        return
    fi

    preflight
    banner
    prompt_config

    install_packages
    setup_hostname
    setup_clock
    setup_admin_user
    setup_service_user
    setup_kiosk_user
    setup_display_stack
    disable_tty1_autologin
    harden_ssh
    setup_logind
    setup_journal
    setup_unattended_upgrades
    setup_ufw
    prepare_service_port
    setup_pi_extras

    install_agent_binary
    install_state_dirs
    pin_content_key
    install_system_files
    enable_service_port
    apply_appliance_policy

    print_summary
}

main "$@"

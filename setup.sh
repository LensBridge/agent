#!/bin/bash
# =====================================================
# MusallahBoard — Raspberry Pi One-Line Installer
# =====================================================
# Provisions a fresh Raspberry Pi OS (Debian/trixie, arm64) into a fully
# configured MusallahBoard kiosk, ready for enrollment.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/lensbridge/agent/main/setup.sh | bash
#
# Or with pre-supplied answers (no prompts):
#   curl -fsSL ... | MB_HOSTNAME=lobby MB_ADMIN_USER=admin \
#       MB_TIMEZONE=America/Toronto \
#       MB_ADMIN_SSH_KEY="ssh-ed25519 AAAA..." bash
#
# After setup completes, enroll the device:
#   sudo musallahboard-agent enroll --token=<token> --backend=<url>
#
# The kiosk shows a "waiting for enrollment" splash until then; once enrolled
# it shows the board served by the local agent, with no reboot. Every board
# renders from its own disk and syncs content whenever it has internet
# (docs/architecture.md).
#
# Boards without internet: once the board is set up and enrolled, and while
# it still has internet, run
#   bash setup.sh --service-port [--rtc]
# to provision the ethernet service port, where a laptop or phone plugged in
# can upload signed update packages. USB sticks work on every board.
#
# Re-running this script is safe.
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
BINARY_URL="${RELEASE_URL}/musallahboard-agent-arm64"

# ── Colour helpers ────────────────────────────────────────────────────────────
RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'; BOLD='\033[1m'; NC='\033[0m'
info()    { echo -e "${GREEN}[INFO]${NC}  $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error()   { echo -e "${RED}[ERROR]${NC} $*"; exit 1; }
section() { echo; echo -e "${BOLD}=== $* ===${NC}"; }

# ── Tunables ──────────────────────────────────────────────────────────────────
NTP_SYNC_TIMEOUT_SEC="${NTP_SYNC_TIMEOUT_SEC:-90}"

# ── Non-interactive configuration ─────────────────────────────────────────────
# Every prompt can be answered in advance via environment variable. When piped
# (stdin is the script itself), prompts cannot work — defaults are used.
#
#   MB_HOSTNAME  MB_ADMIN_USER  MB_TIMEZONE  MB_ADMIN_SSH_KEY
#   MB_ASSUME_YES=1   take the default for anything unset, confirm nothing
#   MB_REBOOT=auto|never|ask
MB_ASSUME_YES="${MB_ASSUME_YES:-0}"
MB_REBOOT="${MB_REBOOT:-ask}"

# Fixed service accounts — not configurable.
KIOSK_USER=musallahkiosk
SERVICE_USER=musallahdaemon

# Paths
BINARY_DEST=/usr/bin/musallahboard-agent
CONFIG_DIR=/etc/musallahboard
UNIT_DIR=/lib/systemd/system
SUDOERS_DEST=/etc/sudoers.d/musallahboard-agent
KIOSK_SHARE_DIR=/usr/share/musallahboard
STATE_DIR=/var/lib/musallahboard
INBOX_DIR=/var/lib/musallahboard/inbox
LIB_DIR=/usr/lib/musallahboard
TRUST_PATH=/etc/musallahboard/trust.json
UDEV_DIR=/etc/udev/rules.d
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
SSHD_HARDENING=/etc/ssh/sshd_config.d/kiosk-hardening.conf

# ── Argument parsing ──────────────────────────────────────────────────────────
usage() {
    cat <<'USAGE'
Usage: bash setup.sh [options]

  --hostname=NAME        hostname to set
  --admin-user=NAME      SSH/sudo account
  --timezone=TZ          e.g. America/Toronto
  --admin-ssh-key=KEY    authorized public key for the admin account
  --reboot=auto|never|ask
  --yes, -y              answer every prompt with its default
  -h, --help

Service port (after a normal setup and enrollment, while still online):
  --service-port         provision the ethernet service port, where a laptop
                         or phone plugged in can upload update packages, and
                         turn it on; skips the normal setup
  --rtc                  a DS3231 RTC is fitted: enable it, remove fake-hwclock

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
        --reboot=*)        export MB_REBOOT="${arg#*=}" ;;
        --yes|-y)          export MB_ASSUME_YES=1 ;;
        --service-port)    SERVICE_PORT=1 ;;
        --rtc)             SERVICE_PORT_RTC=1 ;;
        -h|--help)         usage; exit 0 ;;
        *) echo "Unknown option: $arg" >&2; usage >&2; exit 1 ;;
    esac
done

if [[ "$SERVICE_PORT" != "1" && "$SERVICE_PORT_RTC" == "1" ]]; then
    echo "--rtc only applies with --service-port" >&2
    exit 1
fi

# ── Helper: read one answer ───────────────────────────────────────────────────
# $1 variable to set   $2 prompt text   $3 default
_ask() {
    local _var="$1" _prompt="$2" _default="$3" _preset _reply
    _preset="${!_var-}"

    if [[ -n "$_preset" ]]; then
        printf '  %-34s %s\n' "$_prompt" "$_preset"
        return 0
    fi

    if [[ "$MB_ASSUME_YES" == "1" ]] || [[ ! -t 0 ]]; then
        printf -v "$_var" '%s' "$_default"
        printf '  %-34s %s (default)\n' "$_prompt" "$_default"
        return 0
    fi

    read -rp "$(printf '  %-34s [%s]: ' "$_prompt" "$_default")" _reply
    printf -v "$_var" '%s' "${_reply:-$_default}"
    return 0
}

_confirm() {
    local _prompt="$1" _reply
    [[ "$MB_ASSUME_YES" == "1" ]] && return 0
    [[ ! -t 0 ]] && return 0
    read -rp "$_prompt" -n 1 _reply; echo
    [[ $_reply =~ ^[Yy]$ ]]
}

# ── Pre-flight checks ─────────────────────────────────────────────────────────
preflight() {
    [[ $EUID -eq 0 ]] && error "Do not run as root. Run as a user with sudo access."
    sudo -v || error "This script requires sudo access."

    [[ "$(uname -m)" == "aarch64" ]] || error "This script is for arm64 (Raspberry Pi) only. Got: $(uname -m)"

    # Check for Raspberry Pi OS
    [[ -f /boot/firmware/config.txt ]] || [[ -f /boot/config.txt ]] || \
        error "This does not look like Raspberry Pi OS (no config.txt found)."

    command -v apt-get >/dev/null || error "apt-get not found — is this Debian/Raspbian?"
    command -v systemctl >/dev/null || error "systemctl not found — systemd required"
}

# ── Banner ────────────────────────────────────────────────────────────────────
banner() {
    cat << 'BANNER'

  +----------------------------------------------+
  |       MusallahBoard Setup                    |
  |       Raspberry Pi (arm64)                   |
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
    _ask ADMIN_USER "Admin username (SSH/sudo)"        "ibra"
    _ask TIMEZONE   "Timezone"                         "America/Toronto"

    # Validate admin user is not a reserved name.
    for _reserved in musallahdaemon "$KIOSK_USER"; do
        [[ "$ADMIN_USER" == "$_reserved" ]] && \
            error "'$_reserved' is reserved for a MusallahBoard service account."
    done

    echo
    SSH_PUB_KEY="${MB_ADMIN_SSH_KEY-}"
    if [[ -n "$SSH_PUB_KEY" ]]; then
        info "SSH key supplied for $ADMIN_USER"
    elif [[ -t 0 ]]; then
        echo "Paste the SSH public key for $ADMIN_USER:"
        read -rp "> " SSH_PUB_KEY
        [[ -z "$SSH_PUB_KEY" ]] && error "SSH public key is required (headless Pi)."
    else
        error "SSH public key is required. Pass MB_ADMIN_SSH_KEY."
    fi

    echo
    info "Summary"
    printf "  %-18s %s\n" "Hostname:"   "$HOSTNAME"
    printf "  %-18s %s\n" "Admin user:" "$ADMIN_USER  (SSH key auth, passwordless sudo)"
    printf "  %-18s %s\n" "Kiosk user:" "$KIOSK_USER  (auto-login, browser only, no sudo/SSH/shell)"
    printf "  %-18s %s\n" "Kiosk URL:"  "$KIOSK_URL  (served by the agent)"
    printf "  %-18s %s\n" "Timezone:"   "$TIMEZONE"
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
    # rpi-connect: Raspberry Pi Connect for remote access
    sudo apt-get install -y \
        cage \
        chromium \
        rpi-connect \
        ufw \
        unattended-upgrades \
        apt-listchanges \
        curl

    # Purge services that add unnecessary network attack surface
    info "Purging unnecessary network services..."
    sudo apt-get purge -y rpcbind nfs-common 2>/dev/null || true
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

    # Kiosk environment: software-rendering fallback for VMs
    if systemd-detect-virt --quiet 2>/dev/null; then
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
harden_ssh() {
    section "SSH hardening"

    sudo mkdir -p /etc/ssh/sshd_config.d
    sudo tee /etc/ssh/sshd_config.d/kiosk-hardening.conf > /dev/null << EOF
PasswordAuthentication no
ChallengeResponseAuthentication no
PubkeyAuthentication yes
PermitRootLogin no
AllowUsers $ADMIN_USER
DenyUsers $KIOSK_USER
EOF

    sudo systemctl reload ssh 2>/dev/null || sudo systemctl reload sshd 2>/dev/null || true
    info "SSH: key-based auth only, $ADMIN_USER access only"
    warn "Verify you can SSH as $ADMIN_USER before closing this session!"
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
    section "Hardware watchdog"
    sudo mkdir -p /etc/systemd/system.conf.d
    sudo tee /etc/systemd/system.conf.d/watchdog.conf > /dev/null << 'EOF'
[Manager]
RuntimeWatchdogSec=15
RebootWatchdogSec=2min
EOF
    info "Watchdog: systemd pings /dev/watchdog0 every 15s; reboots if it misses"

    section "Raspberry Pi Connect"
    sudo loginctl enable-linger "$ADMIN_USER"
    sudo systemctl stop wayvnc.service 2>/dev/null || true
    sudo systemctl mask wayvnc.service 2>/dev/null || true
    info "Linger enabled for $ADMIN_USER; system wayvnc.service masked"
    warn "Post-install: ssh in as $ADMIN_USER and run 'rpi-connect signin'"
}

# ── Download and install agent binary ─────────────────────────────────────────
install_agent_binary() {
    section "Agent binary"

    info "Downloading from $BINARY_URL"
    local tmp_binary="/tmp/musallahboard-agent-$$"

    if ! curl -fsSL "$BINARY_URL" -o "$tmp_binary"; then
        error "Failed to download agent binary from $BINARY_URL"
    fi

    sudo install -o root -g root -m 0755 "$tmp_binary" "$BINARY_DEST"
    rm -f "$tmp_binary"
    info "Installed $BINARY_DEST ($("$BINARY_DEST" version 2>/dev/null || echo 'version unknown'))"
}

# ── Install sudoers allow-list ────────────────────────────────────────────────
install_sudoers() {
    section "Sudoers"

    sudo tee "$SUDOERS_DEST" > /dev/null << 'EOF'
# musallahboard-agent — privileged commands allow-list
musallahdaemon ALL=(root) NOPASSWD: /bin/systemctl restart musallahboard-kiosk.service
musallahdaemon ALL=(root) NOPASSWD: /bin/systemctl reboot
EOF
    sudo chmod 0440 "$SUDOERS_DEST"
    sudo chown root:root "$SUDOERS_DEST"

    # Validate syntax
    if ! sudo visudo -c -f "$SUDOERS_DEST"; then
        sudo rm -f "$SUDOERS_DEST"
        error "Sudoers file failed validation"
    fi
    info "Installed $SUDOERS_DEST"
}

# ── Install systemd units ─────────────────────────────────────────────────────
install_systemd_units() {
    section "Systemd units"

    # KEEP IN SYNC with packaging/*.service, *.path and packaging/udev/. This
    # script is curl|bash'd and has no checkout to copy from, so the units are
    # inlined here without their comments; the packaged files explain them.

    # Agent service. CAP_SYS_TIME sets the clock (and, with the RTC rule, the
    # RTC); CAP_NET_BIND_SERVICE is for the upload server on port 80.
    sudo tee "$UNIT_DIR/musallahboard-agent.service" > /dev/null << 'EOF'
[Unit]
Description=MusallahBoard device agent
Documentation=https://github.com/LensBridge/agent
After=network-online.target
Wants=network-online.target
StartLimitBurst=5
StartLimitIntervalSec=60

[Service]
Type=notify
User=musallahdaemon
Group=musallahdaemon
ExecStart=/usr/bin/musallahboard-agent run
Restart=always
RestartSec=5
WatchdogSec=60s
AmbientCapabilities=CAP_SYS_TIME CAP_NET_BIND_SERVICE
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
StateDirectory=musallahboard
StateDirectoryMode=0750
LogsDirectory=musallahboard
LogsDirectoryMode=0750
ReadWritePaths=/etc/musallahboard /var/lib/musallahboard /var/log/musallahboard
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
LockPersonality=yes
RestrictRealtime=yes

[Install]
WantedBy=multi-user.target
EOF

    # Kiosk service
    sudo tee "$UNIT_DIR/musallahboard-kiosk.service" > /dev/null << 'EOF'
[Unit]
Description=MusallahBoard kiosk (cage + Chromium)
Documentation=https://github.com/LensBridge/agent
After=systemd-user-sessions.service
After=musallahboard-agent.service
Wants=musallahboard-agent.service
After=getty@tty1.service
Conflicts=getty@tty1.service

[Service]
Type=simple
User=musallahkiosk
PAMName=login
TTYPath=/dev/tty1
TTYReset=yes
TTYVHangup=yes
TTYVTDisallocate=yes
StandardInput=tty-fail
StandardOutput=journal
StandardError=journal
UtmpIdentifier=tty1
UtmpMode=user
EnvironmentFile=-/etc/musallahboard/kiosk.env
ExecStart=/usr/bin/start-kiosk.sh
ExecStopPost=-/usr/bin/pkill -9 -u musallahkiosk -f "google-chrome|chromium"
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF

    # Kiosk URL watcher (path unit)
    sudo tee "$UNIT_DIR/musallahboard-kiosk-watch.path" > /dev/null << 'EOF'
[Unit]
Description=Watch the composed kiosk URL and reload the kiosk on change
Documentation=https://github.com/LensBridge/agent

[Path]
PathModified=/etc/musallahboard/kiosk-url
Unit=musallahboard-kiosk-reload.service

[Install]
WantedBy=multi-user.target
EOF

    # Kiosk reload service
    sudo tee "$UNIT_DIR/musallahboard-kiosk-reload.service" > /dev/null << 'EOF'
[Unit]
Description=Reload the MusallahBoard kiosk after the composed URL changed
Documentation=https://github.com/LensBridge/agent
After=musallahboard-kiosk.service

[Service]
Type=oneshot
ExecStart=/bin/systemctl --no-block try-restart musallahboard-kiosk.service
EOF

    # Root self-updater: the daemon stages a verified agent package and
    # creates staged/ready; this re-verifies it as root, swaps the binary and
    # rolls back if the new agent does not come up.
    sudo tee "$UNIT_DIR/musallahboard-agent-update.path" > /dev/null << 'EOF'
[Unit]
Description=Watch for a staged MusallahBoard agent update
Documentation=https://github.com/LensBridge/agent/blob/main/docs/architecture.md

[Path]
PathExists=/var/lib/musallahboard/agent/staged/ready
Unit=musallahboard-agent-update.service

[Install]
WantedBy=multi-user.target
EOF

    sudo tee "$UNIT_DIR/musallahboard-agent-update.service" > /dev/null << 'EOF'
[Unit]
Description=Apply a staged MusallahBoard agent update
Documentation=https://github.com/LensBridge/agent/blob/main/docs/architecture.md
After=musallahboard-agent.service

[Service]
Type=oneshot
ExecStart=/usr/bin/musallahboard-agent selfupdate apply
TimeoutStartSec=5min
ProtectSystem=strict
ReadWritePaths=/usr/bin -/usr/lib/musallahboard /var/lib/musallahboard /etc/musallahboard
ProtectHome=yes
PrivateTmp=yes
NoNewPrivileges=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectHostname=yes
RestrictSUIDSGID=yes
RestrictRealtime=yes
RestrictNamespaces=yes
LockPersonality=yes
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK
UMask=0022
EOF

    # USB import helper, started by udev for each USB filesystem. Root (it
    # mounts), no network, read-only system except the inbox.
    sudo tee "$UNIT_DIR/musallahboard-usb-import@.service" > /dev/null << 'EOF'
[Unit]
Description=Import MusallahBoard packages from USB stick %I
Documentation=https://github.com/LensBridge/agent/blob/main/docs/architecture.md

[Service]
Type=oneshot
ExecStart=/usr/bin/musallahboard-agent usb-import %I
TimeoutStartSec=20min
RuntimeDirectory=musallahboard/usb/%I
RuntimeDirectoryMode=0700
PrivateNetwork=yes
ProtectSystem=strict
ReadWritePaths=/var/lib/musallahboard/inbox
ProtectHome=yes
PrivateTmp=yes
NoNewPrivileges=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
RestrictSUIDSGID=yes
RestrictRealtime=yes
RestrictNamespaces=yes
LockPersonality=yes
RestrictAddressFamilies=AF_UNIX
CapabilityBoundingSet=CAP_SYS_ADMIN CAP_DAC_OVERRIDE CAP_DAC_READ_SEARCH CAP_CHOWN CAP_FOWNER
UMask=0022
EOF

    info "Installed systemd units"
}

# ── Install udev rules ────────────────────────────────────────────────────────
install_udev_rules() {
    section "udev rules"

    # KEEP IN SYNC with packaging/udev/.
    sudo mkdir -p "$UDEV_DIR"
    sudo tee "$UDEV_DIR/90-musallahboard-usb.rules" > /dev/null << 'EOF'
# MusallahBoard: import signed update packages (*.mbu) from a USB stick.
ACTION=="add", SUBSYSTEM=="block", KERNEL=="sd[a-z]*", ENV{ID_BUS}=="usb", ENV{ID_FS_USAGE}=="filesystem", ENV{ID_FS_TYPE}=="vfat|exfat|ext4|ntfs|ntfs3", TAG+="systemd", ENV{SYSTEMD_WANTS}+="musallahboard-usb-import@%k.service"
EOF
    sudo tee "$UDEV_DIR/90-musallahboard-rtc.rules" > /dev/null << 'EOF'
# MusallahBoard: let the agent (group musallahdaemon) write the RTC with hwclock.
SUBSYSTEM=="rtc", KERNEL=="rtc0", GROUP="musallahdaemon", MODE="0660"
EOF
    sudo chmod 0644 "$UDEV_DIR/90-musallahboard-usb.rules" "$UDEV_DIR/90-musallahboard-rtc.rules"

    sudo udevadm control --reload || true
    # Apply the rtc0 permissions now. Not the block subsystem: that would
    # start an import from a stick that happens to be plugged in.
    sudo udevadm trigger --subsystem-match=rtc --action=change || true

    # KEEP IN SYNC with packaging/modules-load.d/. The USB helper's sandbox
    # (ProtectKernelModules=yes) cannot load filesystem drivers, so exFAT and
    # NTFS sticks need them loaded at boot, and now.
    sudo mkdir -p /etc/modules-load.d
    printf '%s\n' \
        "# MusallahBoard: filesystem drivers for USB update sticks (see setup.sh)." \
        exfat ntfs3 | sudo tee /etc/modules-load.d/musallahboard.conf > /dev/null
    sudo chmod 0644 /etc/modules-load.d/musallahboard.conf
    sudo modprobe exfat 2>/dev/null || warn "Could not load exfat now; exFAT sticks work after a reboot"
    sudo modprobe ntfs3 2>/dev/null || warn "Could not load ntfs3 now; NTFS sticks work after a reboot"
    info "USB sticks start musallahboard-usb-import@<dev>.service; rtc0 is writable by $SERVICE_USER"
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
    # pinned at enrollment and any release keys an admin added.
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

# ── Install kiosk launcher script ─────────────────────────────────────────────
install_kiosk_launcher() {
    section "Kiosk launcher"

    sudo tee /usr/bin/start-kiosk.sh > /dev/null << 'LAUNCHER'
#!/bin/bash
set -euo pipefail

KIOSK_URL_FILE="/etc/musallahboard/kiosk-url"
SPLASH="file:///usr/share/musallahboard/waiting.html"

URL=""
if [[ -r "$KIOSK_URL_FILE" ]]; then
    URL="$(tr -d '[:space:]' < "$KIOSK_URL_FILE")"
fi
[[ -z "$URL" ]] && URL="$SPLASH"

pick_browser() {
    local cand
    for cand in google-chrome-stable google-chrome chromium chromium-browser; do
        local path
        path="$(command -v "$cand" 2>/dev/null)" || continue
        case "$(readlink -f "$path")" in
            /snap/*) continue ;;
        esac
        if head -c4 "$path" | grep -q $'\x7fELF' || file -b "$path" 2>/dev/null | grep -qi 'ELF'; then
            echo "$path"; return 0
        fi
        if ! grep -qi 'snap' "$path" 2>/dev/null; then
            echo "$path"; return 0
        fi
    done
    return 1
}

BROWSER="$(pick_browser)" || {
    echo "start-kiosk: no non-snap browser found" >&2
    exit 1
}

PROFILE_DIR="${HOME:-/home/$(id -un)}/.musallahboard-kiosk"
mkdir -p "$PROFILE_DIR"

exec cage -d -- "$BROWSER" \
    --ozone-platform=wayland \
    --disable-dev-shm-usage \
    --kiosk \
    --noerrdialogs \
    --disable-infobars \
    --disable-session-crashed-bubble \
    --disable-restore-session-state \
    --no-first-run \
    --start-fullscreen \
    --disable-translate \
    --disable-features=TranslateUI \
    --disable-pinch \
    --overscroll-history-navigation=0 \
    --check-for-update-interval=31536000 \
    --enable-features=OverlayScrollbar \
    --password-store=basic \
    --user-data-dir="$PROFILE_DIR" \
    --remote-debugging-port=9222 \
    --remote-debugging-address=127.0.0.1 \
    "$URL"
LAUNCHER

    sudo chmod 0755 /usr/bin/start-kiosk.sh
    info "Installed /usr/bin/start-kiosk.sh"
}

# ── Install splash page ───────────────────────────────────────────────────────
install_splash_page() {
    section "Splash page"

    sudo mkdir -p "$KIOSK_SHARE_DIR"

    # KEEP IN SYNC with packaging/waiting.html. This script is curl|bash'd and
    # never has a repo checkout to copy from, so the page is inlined here; the
    # deb/install.sh path installs the packaged file instead. The setNetInfo
    # hook below is a contract with internal/splash — changing its name breaks
    # the IP readout on unenrolled boards.
    sudo tee "$KIOSK_SHARE_DIR/waiting.html" > /dev/null << 'HTML'
<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>MusallahBoard</title>
<style>
  html,body{height:100%;margin:0;cursor:none}
  body{display:flex;flex-direction:column;align-items:center;justify-content:center;
        background:#0b1f17;color:#e8f5ee;font:600 28px/1.4 system-ui,sans-serif}
  .dot{width:10px;height:10px;border-radius:50%;background:#3ddc84;
       margin-top:24px;animation:p 1.2s ease-in-out infinite}
  @keyframes p{0%,100%{opacity:.25}50%{opacity:1}}
  small{margin-top:14px;font:400 15px system-ui,sans-serif;color:#8fb8a4}
  footer{position:fixed;bottom:16px;left:0;right:0;text-align:center;
         font:400 13px system-ui,sans-serif;color:#5c8270}

  /* Network card - hidden until the agent pushes something into it. */
  .net{margin-top:40px;padding:20px 34px;max-width:80vw;text-align:center;
       border:1px solid #1e4535;border-radius:12px;background:#0e2a1f}
  .net[hidden]{display:none}
  .net-label{font:500 13px/1 system-ui,sans-serif;letter-spacing:.09em;
             text-transform:uppercase;color:#6f9d87}
  .net-value{margin-top:12px;color:#3ddc84;word-break:break-all;
             font:700 34px/1.25 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
  .net-value.offline{color:#c98b6b}
  .net-meta{margin-top:10px;font:400 15px system-ui,sans-serif;color:#8fb8a4}
  .net-meta:empty{display:none}
</style>
</head>
<body>
  <div>Waiting for device enrollment...</div>
  <div class="dot"></div>
  <small>This screen clears automatically once the agent is enrolled.</small>

  <div class="net" id="net" hidden>
    <div class="net-label" id="net-label"></div>
    <div class="net-value" id="net-value"></div>
    <div class="net-meta" id="net-meta"></div>
  </div>

  <footer>MusallahBoard v2.0</footer>

  <script>
  // window.MusallahBoard.setNetInfo() is the contract the agent's
  // pre-enrollment loop calls over CDP (Runtime.evaluate) every few seconds -
  // see internal/splash. This page is loaded over file://, so it has an opaque
  // origin and cannot fetch its own address from anywhere; a push from the
  // agent is the only way it can know. Nothing renders until that first push,
  // which doubles as proof the agent is alive.
  (function () {
    var card  = document.getElementById('net');
    var label = document.getElementById('net-label');
    var value = document.getElementById('net-value');
    var meta  = document.getElementById('net-meta');

    function text(v) { return typeof v === 'string' ? v.trim() : ''; }

    window.MusallahBoard = window.MusallahBoard || {};
    window.MusallahBoard.setNetInfo = function (info) {
      info = info || {};
      var ips = Array.isArray(info.ipv4) ? info.ipv4.filter(text) : [];

      if (ips.length) {
        label.textContent = ips.length > 1 ? 'Reachable at' : 'This device is at';
        value.textContent = ips.join('   -   ');
        value.classList.remove('offline');
      } else {
        label.textContent = 'Network';
        value.textContent = 'Not connected';
        value.classList.add('offline');
      }

      // Hostname and SSID are secondary: useful for telling two boards on a
      // cart apart, not worth competing with the address for attention.
      var bits = [];
      if (text(info.hostname)) bits.push(text(info.hostname));
      if (text(info.ssid)) bits.push('Wi-Fi: ' + text(info.ssid));
      meta.textContent = bits.join('   -   ');

      card.hidden = false;
    };
  })();
  </script>
</body>
</html>
HTML

    sudo chmod 0644 "$KIOSK_SHARE_DIR/waiting.html"
    info "Installed $KIOSK_SHARE_DIR/waiting.html"
}

# ── Enable services ───────────────────────────────────────────────────────────
enable_services() {
    section "Enable services"

    sudo systemctl daemon-reload
    sudo systemctl enable musallahboard-agent.service
    sudo systemctl enable musallahboard-kiosk.service
    sudo systemctl enable musallahboard-kiosk-watch.path
    sudo systemctl enable musallahboard-agent-update.path

    # Start the watchers now: one catches enrollment, the other a staged update
    sudo systemctl start musallahboard-kiosk-watch.path
    sudo systemctl start musallahboard-agent-update.path

    info "Services enabled"
}

# ── Apply appliance policy ────────────────────────────────────────────────────
apply_appliance_policy() {
    section "Appliance boot policy"

    # Disable display managers
    if systemctl list-unit-files 2>/dev/null | grep -qE '^(lightdm|gdm3?|sddm)\.service'; then
        sudo systemctl disable --now lightdm.service gdm.service gdm3.service sddm.service 2>/dev/null || true
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
  |  Setup complete - device READY FOR ENROLLMENT            |
  +----------------------------------------------------------+

  Hostname   : $HOSTNAME
  Admin SSH  : ssh $ADMIN_USER@<ip>
  Kiosk URL  : $KIOSK_URL (served by the agent)

  FINAL STEP - enroll this device (once, with a token from the admin UI):

    sudo musallahboard-agent enroll \\
         --token=<one-time-token> --backend=<backend-url>

  Until then the kiosk shows the local "waiting" splash, which prints this
  device's IP address on screen once it has one - that is the <ip> to SSH to.
  On enrollment the kiosk switches to the board served by the agent, which
  starts syncing content and fetches the board app right away.

  A board that will not have internet: after enrolling, while it still has
  internet, run  bash setup.sh --service-port

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
# been set up and enrolled. See docs/architecture.md, sections 9.5 and 13.
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
    sudo test -s "$CONFIG_DIR/agent.toml" || \
        error "This board is not enrolled yet. Enroll it online first:
       sudo musallahboard-agent enroll --token=<token> --backend=<url>"
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
        warn "for an address. Delete it to use $LAN_CONN instead, then re-run setup.sh --service-port."
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

    # The ds3231 driver is a module, so the kernel's own boot-time read of the
    # RTC is over by the time it loads, and udev cannot set the clock itself
    # (systemd-udevd runs without CAP_SYS_TIME). Instead udev starts a oneshot
    # unit when rtc0 appears, and that unit reads the RTC. Writing it back is
    # the agent's job (90-musallahboard-rtc.rules gives it access).
    sudo tee /etc/udev/rules.d/85-musallahboard-rtc.rules > /dev/null << 'EOF'
# MusallahBoard: set the system clock from the RTC when it appears.
ACTION=="add", SUBSYSTEM=="rtc", KERNEL=="rtc0", TAG+="systemd", ENV{SYSTEMD_WANTS}+="musallahboard-rtc.service"
EOF
    sudo tee "$UNIT_DIR/musallahboard-rtc.service" > /dev/null << 'EOF'
[Unit]
Description=Set the system clock from the RTC (MusallahBoard)
Documentation=https://github.com/LensBridge/agent
DefaultDependencies=no
Before=time-set.target
Wants=time-set.target

[Service]
Type=oneshot
ExecStart=/usr/sbin/hwclock --rtc=/dev/rtc0 --hctosys --utc
EOF
    sudo systemctl daemon-reload
    sudo udevadm control --reload
    info "RTC read at boot via musallahboard-rtc.service"
    warn "Reboot once with the RTC fitted. The agent saves the time to it whenever it corrects the clock,"
    warn "for example on the next upload from a laptop (mbpush or http://10.77.0.1/)."
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

# ── Main ──────────────────────────────────────────────────────────────────────
main() {
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
    setup_pi_extras

    install_agent_binary
    install_sudoers
    install_state_dirs
    install_systemd_units
    install_udev_rules
    install_kiosk_launcher
    install_splash_page
    enable_services
    apply_appliance_policy

    print_summary
}

main "$@"

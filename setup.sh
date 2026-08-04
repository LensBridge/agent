#!/bin/bash
# =====================================================
# MusallahBoard — Raspberry Pi One-Line Installer
# =====================================================
# Provisions a fresh Raspberry Pi OS (Debian/trixie, arm64) into a fully
# configured MusallahBoard kiosk, ready for enrollment.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/utmmsa/musallahboard-agent/main/setup.sh | bash
#
# Or with pre-supplied answers (no prompts):
#   curl -fsSL ... | MB_HOSTNAME=lobby MB_ADMIN_USER=admin \
#       MB_BOARD_URL=https://board.example.com MB_TIMEZONE=America/Toronto \
#       MB_ADMIN_SSH_KEY="ssh-ed25519 AAAA..." bash
#
# After setup completes, enroll the device:
#   sudo musallahboard-agent enroll --token=<token> --backend=<url>
#
# The kiosk shows a "waiting for enrollment" splash until then; once enrolled
# the board loads automatically with no reboot.
# =====================================================

set -euo pipefail

# ── Version and download URLs ─────────────────────────────────────────────────
VERSION="${MB_VERSION:-0.1.0}"
GITHUB_REPO="LensBridge/agent"
RELEASE_URL="https://github.com/${GITHUB_REPO}/releases/download/v${VERSION}"
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
#   MB_HOSTNAME  MB_ADMIN_USER  MB_BOARD_URL  MB_TIMEZONE  MB_ADMIN_SSH_KEY
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

# ── Argument parsing ──────────────────────────────────────────────────────────
usage() {
    cat <<'USAGE'
Usage: bash setup.sh [options]

  --board-url=URL        kiosk URL for this board
  --hostname=NAME        hostname to set
  --admin-user=NAME      SSH/sudo account
  --timezone=TZ          e.g. America/Toronto
  --admin-ssh-key=KEY    authorized public key for the admin account
  --reboot=auto|never|ask
  --yes, -y              answer every prompt with its default
  -h, --help
USAGE
}

for arg in "$@"; do
    case "$arg" in
        --board-url=*)     export MB_BOARD_URL="${arg#*=}" ;;
        --hostname=*)      export MB_HOSTNAME="${arg#*=}" ;;
        --admin-user=*)    export MB_ADMIN_USER="${arg#*=}" ;;
        --timezone=*)      export MB_TIMEZONE="${arg#*=}" ;;
        --admin-ssh-key=*) export MB_ADMIN_SSH_KEY="${arg#*=}" ;;
        --reboot=*)        export MB_REBOOT="${arg#*=}" ;;
        --yes|-y)          export MB_ASSUME_YES=1 ;;
        -h|--help)         usage; exit 0 ;;
        *) echo "Unknown option: $arg" >&2; usage >&2; exit 1 ;;
    esac
done

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
    KIOSK_URL="${MB_BOARD_URL-}"
    TIMEZONE="${MB_TIMEZONE-}"

    _ask HOSTNAME   "Hostname for this board"          "musallahboard"
    _ask ADMIN_USER "Admin username (SSH/sudo)"        "ibra"
    _ask KIOSK_URL  "Kiosk URL"                        "https://board.lensbridge.tech"
    _ask TIMEZONE   "Timezone"                         "America/Toronto"

    # Validate admin user is not a reserved name
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
    printf "  %-18s %s\n" "Kiosk URL:"  "$KIOSK_URL"
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

# ── Display stack (config dir, board-url) ─────────────────────────────────────
setup_display_stack() {
    section "Display stack"

    sudo mkdir -p "$CONFIG_DIR"
    sudo chown "$SERVICE_USER:$SERVICE_USER" "$CONFIG_DIR"
    sudo chmod 0751 "$CONFIG_DIR"

    # Software-rendering fallback for VMs
    if systemd-detect-virt --quiet 2>/dev/null; then
        sudo tee "$CONFIG_DIR/kiosk.env" > /dev/null << 'EOF'
WLR_RENDERER=pixman
WLR_NO_HARDWARE_CURSORS=1
WLR_DRM_NO_MODIFIERS=1
EOF
        sudo chmod 0644 "$CONFIG_DIR/kiosk.env"
        info "Virtualized host detected — wrote kiosk.env (software-render fallback)"
    else
        sudo rm -f "$CONFIG_DIR/kiosk.env"
        info "Bare-metal host — keeping hardware GL"
    fi

    # Persist the board base URL
    printf '%s\n' "$KIOSK_URL" | sudo tee "$CONFIG_DIR/board-url" > /dev/null
    sudo chmod 0644 "$CONFIG_DIR/board-url"
    info "Wrote $CONFIG_DIR/board-url ($KIOSK_URL)"
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

    section "WiFi power save"
    if nmcli device status 2>/dev/null | grep -q wifi; then
        WIFI_CONN=$(nmcli -t -f NAME,TYPE connection show --active 2>/dev/null | grep ':wifi$' | cut -d: -f1 | head -1)
        if [[ -n "$WIFI_CONN" ]]; then
            sudo nmcli connection modify "$WIFI_CONN" 802-11-wireless.powersave 2
            sudo iw dev wlan0 set power_save off 2>/dev/null || true
            info "WiFi power save disabled for connection: $WIFI_CONN"
        else
            info "No active WiFi connection — skipping"
        fi
        sudo mkdir -p /etc/NetworkManager/conf.d
        sudo tee /etc/NetworkManager/conf.d/wifi-powersave-off.conf > /dev/null << 'EOF'
[connection]
wifi.powersave = 2
EOF
        sudo systemctl reload NetworkManager 2>/dev/null || true
        info "Global NM policy: wifi.powersave=disable"
    else
        info "No WiFi device detected — skipping"
    fi

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
    info "Installed $BINARY_DEST"
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

    # Agent service
    sudo tee "$UNIT_DIR/musallahboard-agent.service" > /dev/null << 'EOF'
[Unit]
Description=MusallahBoard device agent
Documentation=https://github.com/utmmsa/musallahboard-agent
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
User=musallahdaemon
Group=musallahdaemon
ExecStart=/usr/bin/musallahboard-agent run
Restart=always
RestartSec=5
StartLimitBurst=5
StartLimitIntervalSec=60
WatchdogSec=60s
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
Documentation=https://github.com/utmmsa/musallahboard-agent
After=systemd-user-sessions.service
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
Documentation=https://github.com/utmmsa/musallahboard-agent

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
Documentation=https://github.com/utmmsa/musallahboard-agent
After=musallahboard-kiosk.service

[Service]
Type=oneshot
ExecStart=/bin/systemctl --no-block try-restart musallahboard-kiosk.service
EOF

    info "Installed systemd units"
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
</style>
</head>
<body>
  <div>Waiting for device enrollment...</div>
  <div class="dot"></div>
  <small>This screen clears automatically once the agent is enrolled.</small>
  <footer>MusallahBoard v2.0</footer>
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

    # Start the watcher now so it catches enrollment
    sudo systemctl start musallahboard-kiosk-watch.path

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
  Kiosk URL  : $KIOSK_URL

  FINAL STEP - enroll this device (once, with a token from the admin UI):

    sudo musallahboard-agent enroll \\
         --token=<one-time-token> --backend=<backend-url>

  Until then the kiosk shows the local "waiting" splash. On enrollment the
  agent composes <board-url>?deviceId=<uuid> and the board loads automatically.

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

# ── Main ──────────────────────────────────────────────────────────────────────
main() {
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
    install_systemd_units
    install_kiosk_launcher
    install_splash_page
    enable_services
    apply_appliance_policy

    print_summary
}

main "$@"

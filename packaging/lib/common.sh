# shellcheck shell=bash
# =====================================================
# MusallahBoard setup — shared library
# =====================================================
# Sourced by the thin per-platform wrapper:
#   - setup.sh         (Raspberry Pi, arm64)
#
# The wrapper sets a handful of variables and defines two hooks, then calls
# `musallahboard_setup_main`. Everything else — config prompts, users, SSH
# hardening, journal, firewall — lives here.
#
# x86-64 hosts are provisioned by the Packer image in ../../image, not by a
# wrapper here. The platform contract below stays generic so a second wrapper
# remains cheap to add.
#
# Contract the wrapper MUST satisfy before calling musallahboard_setup_main:
#
#   PLATFORM_LABEL      Human label, e.g. "Raspberry Pi (arm64)"
#   PLATFORM_ARCH       "arm64" | "amd64"  — used only for the install.sh hint
#   SSH_KEY_REQUIRED    "yes" | "no"       — Pi mandates a key; VM may skip
#   REPO_ROOT           Absolute path to the agent repo (has packaging/)
#
#   platform_install_packages   (function) installs the browser + platform
#                               packages and purges unwanted services.
#   platform_after_hardening    (function, optional) Pi-only extras: watchdog,
#                               WiFi power-save, Raspberry Pi Connect. Define
#                               an empty function if there is nothing to do.
#
# This file is sourced, never executed. It assumes `set -euo pipefail` was
# already set by the wrapper.
# =====================================================

# ── Colour helpers ────────────────────────────────────────────────────────────
RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'; BOLD='\033[1m'; NC='\033[0m'
info()    { echo -e "${GREEN}[INFO]${NC}  $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error()   { echo -e "${RED}[ERROR]${NC} $*"; exit 1; }
section() { echo; echo -e "${BOLD}═══ $* ═══${NC}"; }

# ── Tunables ──────────────────────────────────────────────────────────────────
# How long to wait for the first NTP sync before giving up. Generous: a cold Pi
# on campus WiFi can take a while to associate before it can reach a time server.
NTP_SYNC_TIMEOUT_SEC="${NTP_SYNC_TIMEOUT_SEC:-90}"

# ── Pre-flight ────────────────────────────────────────────────────────────────
_preflight() {
    [[ $EUID -eq 0 ]] && error "Do not run as root. Run as a user with sudo access."
    sudo -v || error "This script requires sudo access."

    : "${PLATFORM_LABEL:?wrapper must set PLATFORM_LABEL}"
    : "${PLATFORM_ARCH:?wrapper must set PLATFORM_ARCH}"
    : "${SSH_KEY_REQUIRED:?wrapper must set SSH_KEY_REQUIRED}"
    : "${REPO_ROOT:?wrapper must set REPO_ROOT}"
    declare -F platform_install_packages >/dev/null \
        || error "wrapper must define platform_install_packages()"
    declare -F platform_after_hardening >/dev/null \
        || platform_after_hardening() { :; }
}

_banner() {
    cat << BANNER

  ╔══════════════════════════════════════════════╗
  ║       MusallahBoard Setup                     ║
  ║       ${PLATFORM_LABEL}
  ║       Kiosk • Hardened SSH • Wayland/cage     ║
  ╚══════════════════════════════════════════════╝

BANNER
}

# ── Interactive configuration ─────────────────────────────────────────────────
_prompt_config() {
    section "Configuration"

    read -rp "Hostname for this board          [musallahboard]: " _h
    HOSTNAME="${_h:-musallahboard}"

    read -rp "Admin username (SSH/sudo)        [ibra]: " _a
    ADMIN_USER="${_a:-ibra}"

    read -rp "Kiosk username (display account) [musallah]: " _k
    KIOSK_USER="${_k:-musallah}"

    read -rp "Kiosk URL                        [https://board.lensbridge.tech]: " _u
    KIOSK_URL="${_u:-https://board.lensbridge.tech}"

    read -rp "Timezone                         [America/Toronto]: " _tz
    TIMEZONE="${_tz:-America/Toronto}"

    echo
    if [[ "$SSH_KEY_REQUIRED" == "yes" ]]; then
        echo "Paste the SSH public key for $ADMIN_USER:"
        read -rp "> " SSH_PUB_KEY
        [[ -z "$SSH_PUB_KEY" ]] && error "SSH public key is required on this platform."
    else
        echo "Paste the SSH public key for $ADMIN_USER (blank to skip SSH hardening):"
        read -rp "> " SSH_PUB_KEY
    fi

    echo
    info "Summary"
    printf "  %-18s %s\n" "Platform:"   "$PLATFORM_LABEL"
    printf "  %-18s %s\n" "Hostname:"   "$HOSTNAME"
    printf "  %-18s %s\n" "Admin user:" "$ADMIN_USER  (SSH key auth, passwordless sudo)"
    printf "  %-18s %s\n" "Kiosk user:" "$KIOSK_USER  (auto-login, browser only, no sudo/SSH/shell)"
    printf "  %-18s %s\n" "Kiosk URL:"  "$KIOSK_URL"
    printf "  %-18s %s\n" "Timezone:"   "$TIMEZONE"
    [[ -z "$SSH_PUB_KEY" ]] && warn "  No SSH key — password auth stays enabled; harden after testing."
    echo
    read -rp "Continue? (y/n): " -n 1 REPLY; echo
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        exit 0
    fi
    # Explicit: a trailing `cond && action` would make this function return the
    # condition's (failed) status, which `set -e` turns into a silent
    # whole-script abort in the caller. Always return success here.
    return 0
}

# ── Hostname ──────────────────────────────────────────────────────────────────
_setup_hostname() {
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
# A Pi has no battery-backed RTC: after a power cut it boots with whatever time
# it last knew. Two things break on a wrong clock, and neither says so plainly:
# prayer times are computed against local midnight, and the backend refuses an
# agent handshake whose signature timestamp is more than five minutes out. So
# set the zone and refuse to finish setup until NTP has actually synced.
_setup_clock() {
    section "Clock"

    if ! timedatectl list-timezones | grep -qx "$TIMEZONE"; then
        error "Unknown timezone '$TIMEZONE' (see: timedatectl list-timezones)"
    fi
    sudo timedatectl set-timezone "$TIMEZONE"
    info "Timezone set to $TIMEZONE"

    sudo systemctl enable --now systemd-timesyncd
    sudo timedatectl set-ntp true

    info "Waiting for NTP sync (up to ${NTP_SYNC_TIMEOUT_SEC}s)…"
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
       Prayer times and agent authentication both depend on it, so setup stops here.
       Check outbound NTP (udp/123) and rerun, or fix the clock by hand:
         sudo timedatectl set-ntp true
         timedatectl status"
}

# ── Admin user ────────────────────────────────────────────────────────────────
_setup_admin_user() {
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

    if [[ -n "$SSH_PUB_KEY" ]]; then
        sudo mkdir -p "/home/$ADMIN_USER/.ssh"
        sudo chmod 700 "/home/$ADMIN_USER/.ssh"
        echo "$SSH_PUB_KEY" | sudo tee "/home/$ADMIN_USER/.ssh/authorized_keys" > /dev/null
        sudo chmod 600 "/home/$ADMIN_USER/.ssh/authorized_keys"
        sudo chown -R "$ADMIN_USER:$ADMIN_USER" "/home/$ADMIN_USER/.ssh"
        info "$ADMIN_USER configured with SSH key auth and passwordless sudo"
    else
        warn "No SSH key provided — skipping authorized_keys for $ADMIN_USER"
    fi
}

# ── Kiosk user ────────────────────────────────────────────────────────────────
_setup_kiosk_user() {
    section "Kiosk user: $KIOSK_USER"

    if ! id "$KIOSK_USER" &>/dev/null; then
        sudo useradd -m -s /bin/bash "$KIOSK_USER"
        info "Created user $KIOSK_USER"
    fi

    # Locked password — no password-based login path
    sudo passwd -l "$KIOSK_USER"

    # Strip privilege groups (keep audio/video for browser multimedia)
    for group in sudo adm dialout cdrom plugdev games users input netdev; do
        sudo gpasswd -d "$KIOSK_USER" "$group" 2>/dev/null || true
    done
    sudo rm -f "/etc/sudoers.d/$KIOSK_USER"

    # Shell exits immediately — belt-and-suspenders since SSH denies this user.
    sudo tee "/home/$KIOSK_USER/.bashrc" > /dev/null << 'EOF'
readonly PATH
echo "This account is for kiosk use only. Direct shell access is not permitted."
exit
EOF

    sudo tee "/home/$KIOSK_USER/.bash_profile" > /dev/null << EOF
export PATH="/home/$KIOSK_USER/bin"
EOF

    sudo chown "$KIOSK_USER:$KIOSK_USER" \
        "/home/$KIOSK_USER/.bashrc" \
        "/home/$KIOSK_USER/.bash_profile"

    info "$KIOSK_USER locked down (password locked, no sudo, no SSH, no shell)"
}

# ── Kiosk display stack ───────────────────────────────────────────────────────
_setup_display_stack() {
    section "Kiosk display stack"

    # cage (the single-app Wayland kiosk compositor), the kiosk systemd unit,
    # the launcher and the splash are all installed by packaging/install.sh —
    # the separate agent step. That step disables any display manager and sets
    # the boot target to multi-user; the cage unit *is* the graphical session.
    # Until install.sh runs there is no kiosk.
    command -v cage >/dev/null \
        && info "cage present" \
        || warn "cage not yet installed — packaging/install.sh installs it"

    sudo mkdir -p /etc/musallahboard

    # Software-rendering fallback: a virtual GPU can't scan out GBM/DMA-BUF
    # buffers, so wlroots must fall back to pixman or cage shows a blank
    # screen. Detect virtualization rather than hardcode per platform — this
    # covers a Pi in a VM and bare-metal x86 alike. The kiosk unit reads this
    # via EnvironmentFile=-/etc/musallahboard/kiosk.env; on real hardware the
    # file is absent and GL acceleration is kept.
    if systemd-detect-virt --quiet 2>/dev/null; then
        sudo tee /etc/musallahboard/kiosk.env > /dev/null << 'EOF'
WLR_RENDERER=pixman
WLR_NO_HARDWARE_CURSORS=1
WLR_DRM_NO_MODIFIERS=1
EOF
        sudo chmod 0644 /etc/musallahboard/kiosk.env
        info "Virtualized host detected — wrote kiosk.env (software-render fallback)"
    else
        sudo rm -f /etc/musallahboard/kiosk.env
        info "Bare-metal host — keeping hardware GL (no kiosk.env)"
    fi

    # Persist the board base URL now, unconditionally. install.sh --board-url
    # also writes this, but doing it here means the value survives even if the
    # operator enrolls the agent before running install.sh — otherwise the
    # agent composes an empty kiosk-url and the kiosk waits forever. The agent
    # owns appending ?deviceId=<uuid> → /etc/musallahboard/kiosk-url.
    printf '%s\n' "$KIOSK_URL" | sudo tee /etc/musallahboard/board-url > /dev/null
    sudo chmod 0644 /etc/musallahboard/board-url
    info "Wrote /etc/musallahboard/board-url ($KIOSK_URL)"
}

# ── Disable tty1 autologin ────────────────────────────────────────────────────
_disable_tty1_autologin() {
    section "Disable tty1 autologin"

    # Prevents physical-access privilege escalation via VT switch to a root
    # shell. The kiosk unit takes tty1 (Conflicts=getty@tty1.service).
    sudo rm -f /etc/systemd/system/getty@tty1.service.d/autologin.conf
    sudo systemctl daemon-reload
    info "tty1 autologin removed"
}

# ── SSH hardening ─────────────────────────────────────────────────────────────
_harden_ssh() {
    section "SSH hardening"

    sudo mkdir -p /etc/ssh/sshd_config.d
    sudo tee /etc/ssh/sshd_config.d/kiosk-hardening.conf > /dev/null << EOF
# Password auth disabled only once a key is installed for the admin user.
PasswordAuthentication $([ -n "$SSH_PUB_KEY" ] && echo no || echo yes)
ChallengeResponseAuthentication no
PubkeyAuthentication yes

# No root login
PermitRootLogin no

# Only the admin user may SSH in; deny kiosk user explicitly
AllowUsers $ADMIN_USER
DenyUsers $KIOSK_USER
EOF

    sudo systemctl reload ssh 2>/dev/null || sudo systemctl reload sshd 2>/dev/null || true

    if [[ -n "$SSH_PUB_KEY" ]]; then
        info "SSH: key-based auth only, $ADMIN_USER access only"
        warn "Verify you can SSH as $ADMIN_USER before closing this session!"
    else
        warn "SSH: password auth left enabled (no key provided) — harden after testing"
    fi
}

# ── logind: ignore idle/power on a display-only device ────────────────────────
_setup_logind() {
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

# ── Persistent, size-capped journal ───────────────────────────────────────────
_setup_journal() {
    section "Journal (persistent, size-capped)"

    # Persistent storage means logs survive reboots — critical for post-mortem
    # diagnosis. Without this journald keeps logs in RAM only.
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
    info "Journal: persistent, 100 MB cap — 'sudo journalctl -b -1' shows previous boot"
}

# ── Unattended security upgrades ──────────────────────────────────────────────
_setup_unattended_upgrades() {
    section "Unattended security upgrades"

    sudo tee /etc/apt/apt.conf.d/20auto-upgrades > /dev/null << 'EOF'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
APT::Periodic::AutocleanInterval "7";
EOF
    info "Unattended security upgrades enabled"
}

# ── UFW firewall ──────────────────────────────────────────────────────────────
_setup_ufw() {
    section "UFW firewall"

    sudo ufw default deny incoming
    sudo ufw default allow outgoing
    sudo ufw allow ssh
    sudo ufw --force enable
    info "Firewall: deny inbound except SSH"
}

# ── Install agent + kiosk (everything except enrollment) ──────────────────────
# Runs packaging/install.sh WITHOUT --token/--backend: the binary, the cage
# kiosk unit and the systemd service all get installed and enabled, the box is
# switched to boot into the kiosk, and the splash shows — the device is left
# fully provisioned and *ready for enrollment*. The only remaining manual step
# is `musallahboard-agent enroll`, which the operator runs once with a
# one-time token from the admin UI.
_install_agent() {
    section "Agent + kiosk install"

    # Locate the built binary. Match on existence, NOT the +x bit — binaries
    # copied from Windows/scp routinely lose execute permission; install.sh
    # re-installs them 0755 anyway. Prefer the arch-specific build.
    local cand AGENT_BINARY=""
    for cand in \
        "$REPO_ROOT/build/musallahboard-agent-$PLATFORM_ARCH" \
        "$REPO_ROOT/build/musallahboard-agent" \
        "$REPO_ROOT/musallahboard-agent-$PLATFORM_ARCH" \
        "$REPO_ROOT/musallahboard-agent"
    do
        if [[ -f "$cand" ]]; then
            AGENT_BINARY="$cand"
            chmod +x "$cand" 2>/dev/null || true
            break
        fi
    done

    if [[ -z "$AGENT_BINARY" ]]; then
        AGENT_INSTALLED="no"
        warn "No agent binary found under $REPO_ROOT (looked in build/ and repo root)."
        warn "Build it, then run install.sh yourself:"
        warn "  make build-$PLATFORM_ARCH"
        warn "  sudo bash $REPO_ROOT/packaging/install.sh \"$REPO_ROOT\" \\"
        warn "       --kiosk-user=$KIOSK_USER --board-url=$KIOSK_URL"
        return 0
    fi

    info "Agent binary : $AGENT_BINARY"
    sudo bash "$REPO_ROOT/packaging/install.sh" "$REPO_ROOT" \
        --kiosk-user="$KIOSK_USER" \
        --board-url="$KIOSK_URL"
    AGENT_INSTALLED="yes"
    info "Agent + kiosk installed and enabled (enrollment still pending)"
}

# ── Summary + reboot ──────────────────────────────────────────────────────────
_print_summary() {
    cat << EOF

  ╔══════════════════════════════════════════════════════════╗
  ║  Setup complete — device READY FOR ENROLLMENT            ║
  ╚══════════════════════════════════════════════════════════╝

  Hostname   : $HOSTNAME
  Admin SSH  : ssh $ADMIN_USER@<ip>
  Kiosk URL  : $KIOSK_URL
               Change later: sudo nano /etc/musallahboard/board-url
               then:         sudo systemctl restart musallahboard-agent

EOF

    if [[ "${AGENT_INSTALLED:-no}" == "yes" ]]; then
        cat << EOF
  FINAL STEP — enroll this device (once, with a token from the admin UI):

    sudo musallahboard-agent enroll \\
         --token=<one-time-token> --backend=<backend-url>

  Until then the kiosk shows the local "waiting" splash. On enrollment the
  agent composes <board-url>?deviceId=<uuid> and the board loads automatically
  — no reboot needed.

  ✓  Boots multi-user → musallahboard-kiosk.service → cage → browser
  ✓  SSH as $ADMIN_USER to manage the system
  ✗  $KIOSK_USER has no SSH, no sudo, no shell
  ✗  No display manager; tty1 is owned by the kiosk unit

EOF
    else
        cat << EOF
  Host is prepared but the agent/kiosk are NOT installed (no binary found).
  Build + install, then enroll — see the warnings above.

EOF
    fi

    [[ -n "$SSH_PUB_KEY" ]] && warn "Verify SSH works as $ADMIN_USER before rebooting!"
    echo
    read -rp "Reboot now? (y/n): " -n 1 REBOOT_REPLY; echo
    if [[ $REBOOT_REPLY =~ ^[Yy]$ ]]; then
        sudo reboot
    fi
    return 0
}

# ── Orchestration ─────────────────────────────────────────────────────────────
musallahboard_setup_main() {
    _preflight
    _banner
    _prompt_config

    section "Installing packages"
    sudo apt-get update -qq
    platform_install_packages          # wrapper-provided: browser + platform pkgs

    _setup_hostname
    _setup_clock
    _setup_admin_user
    _setup_kiosk_user
    _setup_display_stack
    _disable_tty1_autologin
    _harden_ssh
    _setup_logind
    _setup_journal
    _setup_unattended_upgrades
    _setup_ufw

    platform_after_hardening           # wrapper-provided: Pi-only extras (or noop)

    _install_agent                     # runs install.sh (no enrollment)
    _print_summary
}

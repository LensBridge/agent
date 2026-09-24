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
#   platform_finalize           (function, optional) runs LAST, after the agent
#                               is installed. Anything that changes how the
#                               board boots belongs here, so that a failure
#                               earlier on leaves a machine that still boots.
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

# ── Non-interactive configuration ─────────────────────────────────────────────
# Every prompt below can be answered in advance through an environment
# variable. That is what makes this usable from the one-line installer, where
# the script arrives down a pipe and stdin is the script itself rather than a
# person — a bare `read` there consumes the rest of the script instead of an
# answer, and does it silently.
#
#   MB_HOSTNAME  MB_ADMIN_USER  MB_TIMEZONE
#   MB_ADMIN_SSH_KEY
#   MB_BOARD_URL      optional, never asked for: the hosted board the kiosk
#                     falls back to until the first app release is installed
#   MB_ASSUME_YES=1   take the default for anything unset, confirm nothing
#   MB_REBOOT=auto|never|ask
#
# Anything left unset is still asked for interactively, so `bash setup.sh` by
# hand behaves exactly as it always has.
#
# MB_KIOSK_USER is gone: the display account is fixed at `musallahkiosk`
# because musallahboard-kiosk.service names it in User= and ExecStopPost, and
# a .deb ships that unit as a static file.
MB_ASSUME_YES="${MB_ASSUME_YES:-0}"
MB_REBOOT="${MB_REBOOT:-ask}"

# Fixed service accounts, both created by packaging/install.sh. Mirrored here
# only so prompts, the SSH deny-list and the summary can name them.
KIOSK_USER=musallahkiosk
SERVICE_USER=musallahdaemon

# Read one answer: the pre-supplied value if there is one, otherwise a prompt,
# otherwise the default. Never prompts when there is no terminal on stdin —
# that case is not a user who wants defaults, it is a user who would never see
# the question.
#
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

# Yes/no confirmation that defaults to yes under MB_ASSUME_YES and refuses to
# guess when there is no terminal.
_confirm() {
    local _prompt="$1" _reply
    [[ "$MB_ASSUME_YES" == "1" ]] && return 0
    [[ ! -t 0 ]] && return 0
    read -rp "$_prompt" -n 1 _reply; echo
    [[ $_reply =~ ^[Yy]$ ]]
}

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
    declare -F platform_finalize >/dev/null \
        || platform_finalize() { :; }
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

    # Seeded from the MB_* overrides so the same code path serves an interactive
    # run and a piped one. HOSTNAME is assigned unconditionally on purpose: bash
    # sets it to this machine's own name, which _ask would otherwise mistake for
    # an answer somebody supplied and never ask the question.
    HOSTNAME="${MB_HOSTNAME-}"
    ADMIN_USER="${MB_ADMIN_USER-}"
    TIMEZONE="${MB_TIMEZONE-}"
    # The kiosk shows the board served by the local agent
    # (docs/architecture.md section 14), so there is no URL to ask for.
    # MB_BOARD_URL, if given, is only the hosted fallback shown until the
    # first signed app release is installed.
    KIOSK_URL="http://127.0.0.1:8080/"
    BOARD_URL="${MB_BOARD_URL-}"

    _ask HOSTNAME   "Hostname for this board"          "musallahboard"
    _ask ADMIN_USER "Admin username (SSH/sudo)"        "ibra"
    _ask TIMEZONE   "Timezone"                         "America/Toronto"

    # Three accounts, three trust levels, no overlap:
    #   $ADMIN_USER     human login, passwordless sudo
    #   musallahkiosk   runs Chromium against remote content; least trusted
    #   musallahdaemon  holds the Ed25519 device key + the sudo allow-list
    #
    # Only the first is a choice. The other two are fixed because the systemd
    # units and the sudoers allow-list name them literally — that is what lets
    # those files ship verbatim in a .deb instead of being templated at install
    # time. packaging/install.sh creates both; nothing here does.
    #
    # Aliasing any pair collapses a boundary: naming the human account
    # `musallahdaemon` would hand the daemon NOPASSWD:ALL, and `musallahkiosk`
    # would put the device key behind a browser exploit.
    for _reserved in musallahdaemon "$KIOSK_USER"; do
        [[ "$ADMIN_USER" == "$_reserved" ]] && \
            error "'$_reserved' is reserved for a MusallahBoard service account."
    done

    echo
    SSH_PUB_KEY="${MB_ADMIN_SSH_KEY-}"
    if [[ -n "$SSH_PUB_KEY" ]]; then
        info "SSH key supplied for $ADMIN_USER"
    elif [[ -t 0 ]]; then
        if [[ "$SSH_KEY_REQUIRED" == "yes" ]]; then
            echo "Paste the SSH public key for $ADMIN_USER:"
            read -rp "> " SSH_PUB_KEY
            [[ -z "$SSH_PUB_KEY" ]] && error "SSH public key is required on this platform."
        else
            echo "Paste the SSH public key for $ADMIN_USER (blank to skip SSH hardening):"
            read -rp "> " SSH_PUB_KEY
        fi
    elif [[ "$SSH_KEY_REQUIRED" == "yes" ]]; then
        # No key, no terminal to ask on, and this platform mandates one. Failing
        # here beats handing back a headless Pi whose only admin account cannot
        # be logged into.
        error "SSH public key is required on this platform. Pass MB_ADMIN_SSH_KEY."
    fi

    echo
    info "Summary"
    printf "  %-18s %s\n" "Platform:"   "$PLATFORM_LABEL"
    printf "  %-18s %s\n" "Hostname:"   "$HOSTNAME"
    printf "  %-18s %s\n" "Admin user:" "$ADMIN_USER  (SSH key auth, passwordless sudo)"
    printf "  %-18s %s\n" "Kiosk user:" "$KIOSK_USER  (auto-login, browser only, no sudo/SSH/shell)"
    printf "  %-18s %s\n" "Kiosk URL:"  "$KIOSK_URL  (served by the agent)"
    [[ -n "$BOARD_URL" ]] && \
        printf "  %-18s %s\n" "Hosted fallback:" "$BOARD_URL  (until the first app release is installed)"
    printf "  %-18s %s\n" "Timezone:"   "$TIMEZONE"
    [[ -z "$SSH_PUB_KEY" ]] && warn "  No SSH key — password auth stays enabled; harden after testing."
    echo
    if ! _confirm "Continue? (y/n): "; then
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
# Deliberately absent. Both $KIOSK_USER and $SERVICE_USER are created and
# locked down by packaging/install.sh, which is the code that will become the
# .deb's postinst — keeping account creation in one place stops the host
# provisioner and the package from drifting into two different lockdowns.
# _harden_ssh still names $KIOSK_USER in DenyUsers; that is just a config
# string and does not require the account to exist yet.

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

    # board-url is optional since v2. Once enrolled, the agent writes
    # kiosk-url itself, pointing at its own local server; board-url only
    # decides what the kiosk shows until the first signed app release is
    # installed (the hosted site, instead of a "board app not installed"
    # page). Written only when given, and an existing one is left alone.
    if [[ -n "$BOARD_URL" ]]; then
        printf '%s\n' "$BOARD_URL" | sudo tee /etc/musallahboard/board-url > /dev/null
        sudo chmod 0644 /etc/musallahboard/board-url
        info "Wrote /etc/musallahboard/board-url ($BOARD_URL, hosted fallback until the first app release)"
    fi
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
        warn "  sudo bash $REPO_ROOT/packaging/install.sh \"$REPO_ROOT\" --kiosk"
        warn "  sudo bash $REPO_ROOT/packaging/appliance-policy.sh"
        return 0
    fi

    info "Agent binary : $AGENT_BINARY"
    # install.sh is payload only: files, accounts, unit enablement. It does not
    # touch the boot target — that is appliance-policy.sh, below.
    sudo bash "$REPO_ROOT/packaging/install.sh" "$REPO_ROOT" --kiosk
    sudo bash "$REPO_ROOT/packaging/appliance-policy.sh"
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
  Kiosk URL  : $KIOSK_URL (served by the agent)

EOF

    if [[ "${AGENT_INSTALLED:-no}" == "yes" ]]; then
        cat << EOF
  FINAL STEP — enroll this device (once, with a token from the admin UI):

    sudo musallahboard-agent enroll \\
         --token=<one-time-token> --backend=<backend-url>

  Until then the kiosk shows the local "waiting" splash. On enrollment the
  kiosk switches to the board served by the agent, which starts syncing
  content right away, with no reboot needed.

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

    # When the encryption has been armed the reboot is not a formality — it is
    # the step that does the conversion — so say so rather than offering the
    # same bare y/n as always.
    if [[ "${LUKS_ARMED:-no}" == "yes" ]]; then
        echo
        warn "The next boot converts the root filesystem. It will sit on the"
        warn "console saying DO NOT POWER OFF for several minutes."
    fi

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
    # No _setup_kiosk_user — packaging/install.sh creates and locks down both
    # service accounts. See the "Kiosk user" note further up this file.
    _setup_display_stack
    _disable_tty1_autologin
    _harden_ssh
    _setup_logind
    _setup_journal
    _setup_unattended_upgrades
    _setup_ufw

    platform_after_hardening           # wrapper-provided: Pi-only extras (or noop)

    _install_agent                     # runs install.sh (no enrollment)

    # Last, and deliberately so. platform_finalize is where the boot files get
    # pointed at an encrypted root; doing it before the agent install would mean
    # a board that had committed to the conversion and then failed to provision.
    platform_finalize

    _print_summary
}

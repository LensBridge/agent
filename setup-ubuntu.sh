#!/bin/bash
# =====================================================
# MusallahBoard Ubuntu 26.04 VM Setup Script
# =====================================================
# Configures a fresh Ubuntu 26.04 (x86-64) installation to run
# the MusallahBoard kiosk + agent service for end-to-end testing.
# Mirrors the Pi production setup but adapted for VMs:
#   - labwc Wayland compositor + lightdm autologin
#   - Chromium in kiosk mode
#   - musallahboard-agent systemd service (if binary provided)
#   - Hardened SSH (key-based auth, admin user only)
#   - Locked-down kiosk user (no sudo, no SSH, no shell)
#   - Persistent journal, idle prevention
#   - UFW firewall and unattended security upgrades
#
# Intentionally omits Pi-only features:
#   - No Raspberry Pi Connect (use SSH instead)
#   - No hardware watchdog (VM guest, not bare metal)
#   - No WiFi power-save tuning (virtual NIC)
#
# Run as a user with sudo access:
#   bash setup-ubuntu.sh
# =====================================================

set -euo pipefail

# ── Colour helpers ────────────────────────────────────────────────────────────
RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'; BOLD='\033[1m'; NC='\033[0m'
info()    { echo -e "${GREEN}[INFO]${NC}  $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error()   { echo -e "${RED}[ERROR]${NC} $*"; exit 1; }
section() { echo; echo -e "${BOLD}═══ $* ═══${NC}"; }

# ── Pre-flight checks ─────────────────────────────────────────────────────────
[[ $EUID -eq 0 ]] && error "Do not run as root. Run as a user with sudo access."
sudo -v || error "This script requires sudo access."

# ── Locate the agent repo root ────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ── Banner ────────────────────────────────────────────────────────────────────
cat << 'BANNER'

  ╔══════════════════════════════════════════════╗
  ║   MusallahBoard Ubuntu VM Setup Script       ║
  ║   Kiosk • Agent • Hardened SSH • labwc       ║
  ╚══════════════════════════════════════════════╝

BANNER

# ── Interactive configuration ─────────────────────────────────────────────────
section "Configuration"

read -rp "Hostname for this board          [musallahboard]: " _h
HOSTNAME="${_h:-musallahboard}"

read -rp "Admin username (SSH/sudo)        [ibra]: " _a
ADMIN_USER="${_a:-ibra}"

read -rp "Kiosk username (display account) [musallah]: " _k
KIOSK_USER="${_k:-musallah}"

read -rp "Kiosk URL                        [https://board.lensbridge.tech]: " _u
KIOSK_URL="${_u:-https://board.lensbridge.tech}"

echo
echo "Paste the SSH public key for $ADMIN_USER (leave blank to skip SSH hardening):"
read -rp "> " SSH_PUB_KEY

echo
echo "musallahboard-agent enrollment (leave blank to skip):"
read -rp "  Backend URL   (e.g. http://192.168.1.10:8080): " BACKEND_URL
read -rp "  Enroll token  (one-time token from backend):   " ENROLL_TOKEN

# Locate agent binary (amd64 — VM build)
AGENT_BINARY=""
for candidate in \
    "${SCRIPT_DIR}/build/musallahboard-agent-amd64" \
    "${SCRIPT_DIR}/build/musallahboard-agent" \
    "${SCRIPT_DIR}/musallahboard-agent-amd64" \
    "${SCRIPT_DIR}/musallahboard-agent"
do
    if [[ -x "$candidate" ]]; then
        AGENT_BINARY="$candidate"
        break
    fi
done

echo
info "Summary"
printf "  %-22s %s\n" "Hostname:"     "$HOSTNAME"
printf "  %-22s %s\n" "Admin user:"   "$ADMIN_USER  (SSH key-based auth, passwordless sudo)"
printf "  %-22s %s\n" "Kiosk user:"   "$KIOSK_USER  (auto-login, Chromium only, no sudo/SSH/shell)"
printf "  %-22s %s\n" "Kiosk URL:"    "$KIOSK_URL"
if [[ -n "$AGENT_BINARY" ]]; then
    printf "  %-22s %s\n" "Agent binary:" "$AGENT_BINARY"
elif [[ -n "$ENROLL_TOKEN" ]]; then
    warn "  Agent binary not found — build with 'make build-amd64' and re-run."
fi
if [[ -n "$ENROLL_TOKEN" ]]; then
    printf "  %-22s %s\n" "Agent backend:" "$BACKEND_URL"
    printf "  %-22s %s\n" "Agent enroll:"  "yes"
else
    printf "  %-22s %s\n" "Agent enroll:"  "skipped — run 'musallahboard-agent enroll' manually"
fi
echo
read -rp "Continue? (y/n): " -n 1 REPLY; echo
[[ ! $REPLY =~ ^[Yy]$ ]] && exit 0

# ── Packages ──────────────────────────────────────────────────────────────────
section "Installing packages"

# Enable universe for labwc, wtype, and other kiosk tools
sudo add-apt-repository -y universe
sudo apt-get update -qq
sudo apt-get install -y \
    labwc \
    wtype \
    chromium \
    chromium-sandbox \
    lightdm \
    lightdm-gtk-greeter \
    ufw \
    unattended-upgrades \
    apt-listchanges

# Purge avahi/cups if present — reduces network attack surface on a kiosk
sudo apt-get purge -y avahi-daemon cups 2>/dev/null || true
sudo apt-mark hold avahi-daemon cups 2>/dev/null || true

# ── Hostname ──────────────────────────────────────────────────────────────────
section "Hostname"

echo "$HOSTNAME" | sudo tee /etc/hostname > /dev/null
if grep -q "127\.0\.1\.1" /etc/hosts; then
    sudo sed -i "s/^\(127\.0\.1\.1\s\+\).*/\1$HOSTNAME/" /etc/hosts
else
    echo "127.0.1.1	$HOSTNAME" | sudo tee -a /etc/hosts > /dev/null
fi
sudo hostnamectl set-hostname "$HOSTNAME"
info "Hostname set to $HOSTNAME"

# ── Admin user ────────────────────────────────────────────────────────────────
section "Admin user: $ADMIN_USER"

if ! id "$ADMIN_USER" &>/dev/null; then
    sudo useradd -m -s /bin/bash "$ADMIN_USER"
    info "Created user $ADMIN_USER"
else
    info "User $ADMIN_USER already exists, updating SSH key..."
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
    warn "No SSH key provided — skipping SSH key setup for $ADMIN_USER"
fi

# ── Kiosk user ────────────────────────────────────────────────────────────────
section "Kiosk user: $KIOSK_USER"

if ! id "$KIOSK_USER" &>/dev/null; then
    sudo useradd -m -s /bin/bash "$KIOSK_USER"
    info "Created user $KIOSK_USER"
fi

# Locked password — no password-based login path
sudo passwd -l "$KIOSK_USER"

# Strip privilege groups (keep audio/video for Chromium multimedia)
for group in sudo adm dialout cdrom plugdev games users input netdev; do
    sudo gpasswd -d "$KIOSK_USER" "$group" 2>/dev/null || true
done
sudo rm -f "/etc/sudoers.d/$KIOSK_USER"

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

# URL file — edit as $ADMIN_USER to change what the board displays
echo "$KIOSK_URL" | sudo tee "/home/$KIOSK_USER/url.txt" > /dev/null
sudo chown "$KIOSK_USER:$KIOSK_USER" "/home/$KIOSK_USER/url.txt"
sudo chmod 644 "/home/$KIOSK_USER/url.txt"

info "$KIOSK_USER locked down (password locked, no sudo, no SSH)"

# ── Kiosk launcher ────────────────────────────────────────────────────────────
section "Kiosk launcher"

sudo mkdir -p "/home/$KIOSK_USER/bin"

sudo tee "/home/$KIOSK_USER/bin/start-kiosk.sh" > /dev/null << SCRIPT
#!/bin/bash

URL_FILE="/home/$KIOSK_USER/url.txt"
DEFAULT_URL="$KIOSK_URL"

if [[ -f "\$URL_FILE" ]]; then
    URL=\$(tr -d '[:space:]' < "\$URL_FILE")
fi
[[ -z "\$URL" ]] && URL="\$DEFAULT_URL"

exec chromium \\
    --ozone-platform=wayland \\
    --disable-dev-shm-usage \\
    --kiosk \\
    --noerrdialogs \\
    --disable-infobars \\
    --disable-session-crashed-bubble \\
    --disable-restore-session-state \\
    --no-first-run \\
    --start-fullscreen \\
    --disable-translate \\
    --disable-features=TranslateUI \\
    --disable-pinch \\
    --overscroll-history-navigation=0 \\
    --check-for-update-interval=31536000 \\
    --enable-features=OverlayScrollbar \\
    --remote-debugging-port=9222 \\
    --remote-debugging-address=127.0.0.1 \\
    "\$URL"
SCRIPT

sudo chmod +x "/home/$KIOSK_USER/bin/start-kiosk.sh"
sudo chown -R "$KIOSK_USER:$KIOSK_USER" "/home/$KIOSK_USER/bin"

info "Launcher installed at /home/$KIOSK_USER/bin/start-kiosk.sh"

# ── labwc Wayland compositor config ───────────────────────────────────────────
section "labwc kiosk config"

sudo mkdir -p "/home/$KIOSK_USER/.config/labwc"

# Simpler autostart than Pi — no cursor-hide workaround needed on x86 VMs
# (Pi has virtual pointer devices from vc4-hdmi that cause a phantom cursor)
sudo tee "/home/$KIOSK_USER/.config/labwc/autostart" > /dev/null << AUTOSTART
#!/bin/sh
/home/${KIOSK_USER}/bin/start-kiosk.sh &
AUTOSTART

sudo chmod +x "/home/$KIOSK_USER/.config/labwc/autostart"

sudo tee "/home/$KIOSK_USER/.config/labwc/rc.xml" > /dev/null << 'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<labwc_config>
  <core>
    <decoration>server</decoration>
  </core>

  <focus>
    <followMouse>no</followMouse>
  </focus>
</labwc_config>
EOF

sudo tee "/home/$KIOSK_USER/.config/labwc/environment" > /dev/null << 'EOF'
XDG_CURRENT_DESKTOP=labwc
XKB_DEFAULT_MODEL=pc105
XKB_DEFAULT_LAYOUT=us
EOF

sudo chown -R "$KIOSK_USER:$KIOSK_USER" "/home/$KIOSK_USER/.config"

info "labwc config installed"

# ── lightdm autologin ─────────────────────────────────────────────────────────
section "lightdm autologin"

# Set lightdm as the default display manager
echo "/usr/sbin/lightdm" | sudo tee /etc/X11/default-display-manager > /dev/null
sudo DEBIAN_FRONTEND=noninteractive dpkg-reconfigure lightdm 2>/dev/null || true

sudo mkdir -p /etc/lightdm/lightdm.conf.d
sudo tee /etc/lightdm/lightdm.conf.d/90-kiosk.conf > /dev/null << EOF
[Seat:*]
autologin-user=$KIOSK_USER
autologin-user-timeout=0
autologin-session=labwc
EOF

if [[ -f /etc/lightdm/lightdm.conf ]]; then
    sudo sed -i \
        -e "s/^#*autologin-user=.*/autologin-user=$KIOSK_USER/" \
        -e "s/^#*autologin-session=.*/autologin-session=labwc/" \
        /etc/lightdm/lightdm.conf
fi

info "lightdm will auto-login $KIOSK_USER into the labwc session"

# ── Remove tty1 autologin ─────────────────────────────────────────────────────
section "Disable tty1 autologin"

sudo rm -f /etc/systemd/system/getty@tty1.service.d/autologin.conf
sudo systemctl daemon-reload

info "tty1 autologin removed"

# ── SSH hardening ─────────────────────────────────────────────────────────────
section "SSH hardening"

sudo mkdir -p /etc/ssh/sshd_config.d
sudo tee /etc/ssh/sshd_config.d/kiosk-hardening.conf > /dev/null << EOF
# Disable password authentication entirely — SSH key only
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

# ── Systemd: logind — prevent idle actions on a display-only device ───────────
section "logind (idle/power key)"

sudo mkdir -p /etc/systemd/logind.conf.d
sudo tee /etc/systemd/logind.conf.d/kiosk.conf > /dev/null << 'EOF'
[Login]
IdleAction=ignore
HandlePowerKey=ignore
HandleLidSwitch=ignore
EOF

# ── Systemd: persistent journal with size cap ─────────────────────────────────
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
info "Journal: persistent storage, 100 MB cap"

# ── Unattended security upgrades ──────────────────────────────────────────────
section "Unattended security upgrades"

sudo tee /etc/apt/apt.conf.d/20auto-upgrades > /dev/null << 'EOF'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
APT::Periodic::AutocleanInterval "7";
EOF

# ── UFW firewall ──────────────────────────────────────────────────────────────
section "UFW firewall"

sudo ufw default deny incoming
sudo ufw default allow outgoing
sudo ufw allow ssh
sudo ufw --force enable

info "Firewall: deny inbound except SSH"

# ── musallahboard-agent ────────────────────────────────────────────────────────
section "musallahboard-agent"

if [[ -z "$AGENT_BINARY" ]]; then
    warn "No agent binary found — skipping agent install."
    warn "Build with 'make build-amd64', then re-run or install manually:"
    warn "  sudo bash packaging/install.sh [--token=X --backend=Y]"
else
    INSTALL_SCRIPT="${SCRIPT_DIR}/packaging/install.sh"
    [[ -f "$INSTALL_SCRIPT" ]] || error "Missing install script: $INSTALL_SCRIPT"

    INSTALL_ARGS=("$SCRIPT_DIR")
    [[ -n "$ENROLL_TOKEN" && -n "$BACKEND_URL" ]] && \
        INSTALL_ARGS+=(--token="$ENROLL_TOKEN" --backend="$BACKEND_URL")

    sudo bash "$INSTALL_SCRIPT" "${INSTALL_ARGS[@]}"
    info "musallahboard-agent installed"
fi

# ── Summary ───────────────────────────────────────────────────────────────────
cat << EOF

  ╔══════════════════════════════════════════════════════════╗
  ║  Setup complete!                                         ║
  ╚══════════════════════════════════════════════════════════╝

  Hostname   : $HOSTNAME
  Admin SSH  : ssh $ADMIN_USER@<ip>
  Kiosk URL  : $KIOSK_URL
               Change: sudo nano /home/$KIOSK_USER/url.txt

  After reboot:
  ✓  $KIOSK_USER auto-logs in → lightdm → labwc → Chromium kiosk
  ✓  SSH as $ADMIN_USER to manage the system
  ✓  musallahboard-agent runs as 'admin' user
  ✗  $KIOSK_USER has no SSH, no sudo, no shell
  ✗  tty1 autologin is disabled

  Useful commands:
    sudo systemctl status musallahboard-agent
    sudo journalctl -u musallahboard-agent -f
    sudo musallahboard-agent enroll --token=X --backend=Y

EOF

[[ -n "$SSH_PUB_KEY" ]] && warn "Verify SSH works as $ADMIN_USER before rebooting!"
echo
read -rp "Reboot now? (y/n): " -n 1 REBOOT_REPLY; echo
[[ $REBOOT_REPLY =~ ^[Yy]$ ]] && sudo reboot

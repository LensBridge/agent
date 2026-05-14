#!/bin/bash
# =====================================================
# MusallahBoard Raspberry Pi Setup Script
# =====================================================
# Configures a fresh Pi OS (Debian/trixie) installation to run
# the MusallahBoard kiosk with:
#   - labwc Wayland compositor with HideCursor action (0.8.4+)
#   - Chromium in kiosk mode
#   - Raspberry Pi Connect for remote screen + shell access
#   - Hardened SSH (key-based auth, admin user only)
#   - Locked-down kiosk user (no sudo, no SSH, no shell)
#   - Hardware watchdog, persistent journal, idle prevention
#   - WiFi power save disabled (prevents overnight DHCP lease drop)
#   - UFW firewall and unattended security upgrades
#
# Run as a user with sudo access (e.g. the default 'pi' user):
#   bash setup.sh
# =====================================================

set -euo pipefail

# ── Colour helpers ────────────────────────────────────────────────────────────
RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'; BOLD='\033[1m'; NC='\033[0m'
info()  { echo -e "${GREEN}[INFO]${NC}  $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error() { echo -e "${RED}[ERROR]${NC} $*"; exit 1; }
section() { echo; echo -e "${BOLD}═══ $* ═══${NC}"; }

# ── Pre-flight checks ─────────────────────────────────────────────────────────
[[ $EUID -eq 0 ]] && error "Do not run as root. Run as a user with sudo access (e.g. 'pi')."
sudo -v || error "This script requires sudo access."

# ── Banner ────────────────────────────────────────────────────────────────────
cat << 'BANNER'

  ╔══════════════════════════════════════════════╗
  ║       MusallahBoard Pi Setup Script          ║
  ║       Kiosk • Hardened SSH • Wayland/cage    ║
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
echo "Paste the SSH public key for $ADMIN_USER:"
read -rp "> " SSH_PUB_KEY
[[ -z "$SSH_PUB_KEY" ]] && error "SSH public key is required."

echo
info "Summary"
printf "  %-18s %s\n" "Hostname:"   "$HOSTNAME"
printf "  %-18s %s\n" "Admin user:" "$ADMIN_USER  (SSH key-based auth, passwordless sudo)"
printf "  %-18s %s\n" "Kiosk user:" "$KIOSK_USER  (auto-login, Chromium only, no sudo/SSH/shell)"
printf "  %-18s %s\n" "Kiosk URL:"  "$KIOSK_URL"
echo
read -rp "Continue? (y/n): " -n 1 REPLY; echo
[[ ! $REPLY =~ ^[Yy]$ ]] && exit 0

# ── Packages ──────────────────────────────────────────────────────────────────
section "Installing packages"

sudo apt-get update -qq
sudo apt-get install -y \
    labwc \
    wtype \
    chromium \
    rpi-connect \
    ufw \
    unattended-upgrades \
    apt-listchanges

# Purge services that add unnecessary network attack surface.
# Use purge+hold to prevent them coming back as a dependency pull-in.
# NOTE: Do NOT run apt autoremove after this — it may remove rpd-wayland-core
#       and other display stack packages on Pi OS desktop images.
info "Purging unnecessary network services..."
sudo apt-get purge -y rpcbind nfs-common 2>/dev/null || true
sudo apt-mark hold rpcbind nfs-common

# ── Hostname ──────────────────────────────────────────────────────────────────
section "Hostname"

echo "$HOSTNAME" | sudo tee /etc/hostname > /dev/null
if grep -q "127\.0\.1\.1" /etc/hosts; then
    sudo sed -i "s/^\(127\.0\.1\.1\s\+\).*/\1$HOSTNAME/" /etc/hosts
else
    echo "127.0.1.1	$HOSTNAME" | sudo tee -a /etc/hosts > /dev/null
fi
info "Hostname set to $HOSTNAME (takes effect after reboot)"

# ── Admin user ────────────────────────────────────────────────────────────────
section "Admin user: $ADMIN_USER"

if ! id "$ADMIN_USER" &>/dev/null; then
    sudo useradd -m -s /bin/bash "$ADMIN_USER"
    info "Created user $ADMIN_USER"
else
    info "User $ADMIN_USER already exists, updating SSH key..."
fi

sudo mkdir -p "/home/$ADMIN_USER/.ssh"
sudo chmod 700 "/home/$ADMIN_USER/.ssh"
echo "$SSH_PUB_KEY" | sudo tee "/home/$ADMIN_USER/.ssh/authorized_keys" > /dev/null
sudo chmod 600 "/home/$ADMIN_USER/.ssh/authorized_keys"
sudo chown -R "$ADMIN_USER:$ADMIN_USER" "/home/$ADMIN_USER/.ssh"

sudo usermod -aG sudo "$ADMIN_USER"
echo "$ADMIN_USER ALL=(ALL) NOPASSWD:ALL" | sudo tee "/etc/sudoers.d/$ADMIN_USER" > /dev/null
sudo chmod 440 "/etc/sudoers.d/$ADMIN_USER"

info "$ADMIN_USER configured with SSH key auth and passwordless sudo"

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

# Shell exits immediately — belt-and-suspenders since SSH denies this user anyway
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

# URL file — edit this as $ADMIN_USER to change what the board displays
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

# labwc (0.8.4+) has a native HideCursor action. We bind it to Alt+Super+H in
# rc.xml, then trigger it from autostart via wtype. This is the only reliable
# way to hide the cursor caused by the Pi's vc4-hdmi virtual pointer devices.
sudo mkdir -p "/home/$KIOSK_USER/.config/labwc"

sudo tee "/home/$KIOSK_USER/.config/labwc/autostart" > /dev/null << 'EOF'
#!/bin/sh

# Hide cursor on startup via labwc HideCursor action.
# wtype sends the keybind defined in rc.xml; sleep gives labwc time to be ready.
sleep 0.5
wtype -M alt -M logo h -m alt -m logo

# Launch kiosk browser
/home/KIOSK_USER_PLACEHOLDER/bin/start-kiosk.sh &
EOF

# Substitute the actual kiosk username into the autostart script
sudo sed -i "s|KIOSK_USER_PLACEHOLDER|$KIOSK_USER|g" \
    "/home/$KIOSK_USER/.config/labwc/autostart"
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

  <keyboard>
    <!-- Triggered from autostart via wtype to hide cursor on boot -->
    <keybind key="A-W-h">
      <action name="HideCursor" />
      <action name="WarpCursor" x="-1" y="-1" />
    </keybind>
  </keyboard>
</labwc_config>
EOF

sudo tee "/home/$KIOSK_USER/.config/labwc/environment" > /dev/null << 'EOF'
XDG_CURRENT_DESKTOP=labwc
XKB_DEFAULT_MODEL=pc105
XKB_DEFAULT_LAYOUT=us
EOF

sudo chown -R "$KIOSK_USER:$KIOSK_USER" "/home/$KIOSK_USER/.config"

info "labwc config installed (HideCursor on startup via wtype keybind)"

# ── lightdm autologin ─────────────────────────────────────────────────────────
section "lightdm autologin"

# Use a high-priority conf.d file (90-) so it survives rpd-common postinst hooks
# which reset autologin-user in /etc/lightdm/lightdm.conf.
sudo mkdir -p /etc/lightdm/lightdm.conf.d
sudo tee /etc/lightdm/lightdm.conf.d/90-kiosk.conf > /dev/null << EOF
[Seat:*]
autologin-user=$KIOSK_USER
autologin-user-timeout=0
autologin-session=labwc
EOF

# Also patch main lightdm.conf in case it wins precedence on this image
if [[ -f /etc/lightdm/lightdm.conf ]]; then
    sudo sed -i \
        -e "s/^autologin-user=.*/autologin-user=$KIOSK_USER/" \
        -e "s/^autologin-session=.*/autologin-session=labwc/" \
        /etc/lightdm/lightdm.conf
fi

info "lightdm will auto-login $KIOSK_USER into the labwc session"

# ── Remove tty1 autologin ─────────────────────────────────────────────────────
section "Disable tty1 autologin"

# Prevents physical-access privilege escalation via VT switching to a root shell
sudo rm -f /etc/systemd/system/getty@tty1.service.d/autologin.conf
sudo systemctl daemon-reload

info "tty1 autologin removed"

# ── SSH hardening ─────────────────────────────────────────────────────────────
section "SSH hardening"

sudo mkdir -p /etc/ssh/sshd_config.d
sudo tee /etc/ssh/sshd_config.d/kiosk-hardening.conf > /dev/null << EOF
# Disable password authentication entirely — SSH key only
PasswordAuthentication no
ChallengeResponseAuthentication no
PubkeyAuthentication yes

# No root login
PermitRootLogin no

# Only the admin user may SSH in; deny kiosk user explicitly
AllowUsers $ADMIN_USER
DenyUsers $KIOSK_USER
EOF

sudo systemctl reload ssh 2>/dev/null || sudo systemctl reload sshd 2>/dev/null || true

info "SSH: key-based auth only, $ADMIN_USER access only"
warn "Verify you can SSH as $ADMIN_USER before closing this session!"

# ── Raspberry Pi Connect ──────────────────────────────────────────────────────
section "Raspberry Pi Connect"

# rpi-connect runs as a *user* systemd service under $ADMIN_USER and uses
# `sudo -u $KIOSK_USER` internally to attach wayvnc to the labwc Wayland
# session. Without linger, the service dies the moment $ADMIN_USER's last
# session ends — so remote access would only work while you happen to be
# SSHed in. Enable linger so it persists 24/7.
sudo loginctl enable-linger "$ADMIN_USER"

# The packaged system-level wayvnc.service runs as user 'vnc' and cannot
# attach to $KIOSK_USER's Wayland session — it loops in a failed-restart
# state forever, eating CPU. rpi-connect runs its own wayvnc, so mask the
# system one to silence the loop.
sudo systemctl stop wayvnc.service 2>/dev/null || true
sudo systemctl mask wayvnc.service

info "Linger enabled for $ADMIN_USER; system wayvnc.service masked"
warn "Post-install: ssh in as $ADMIN_USER and run 'rpi-connect signin'"
warn "             to link this Pi to your Raspberry Pi account."

# ── Systemd: hardware watchdog ────────────────────────────────────────────────
section "Hardware watchdog"

sudo mkdir -p /etc/systemd/system.conf.d
sudo tee /etc/systemd/system.conf.d/watchdog.conf > /dev/null << 'EOF'
[Manager]
RuntimeWatchdogSec=15
RebootWatchdogSec=2min
EOF

info "Watchdog: systemd pings /dev/watchdog0 every 15s; reboots if it misses"

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

# Persistent storage means logs survive reboots — critical for post-mortem diagnosis.
# Without this, journald keeps logs in RAM only and they're gone after every reboot.
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
info "Journal: persistent storage, 100 MB cap — 'sudo journalctl -b -1' shows previous boot"

# ── Unattended security upgrades ──────────────────────────────────────────────
section "Unattended security upgrades"

sudo tee /etc/apt/apt.conf.d/20auto-upgrades > /dev/null << 'EOF'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
APT::Periodic::AutocleanInterval "7";
EOF

# ── WiFi power save ───────────────────────────────────────────────────────────
section "WiFi power save"

# WiFi power save causes the chip to sleep during low-traffic periods. On a 24/7
# kiosk this causes the DHCP renewal packet to be missed, the lease to expire,
# and the Pi to become unreachable — even though the router still shows it as
# "connected" at the MAC layer. Disable it for any wifi connection NM manages.
if nmcli device status | grep -q wifi; then
    WIFI_CONN=$(nmcli -t -f NAME,TYPE connection show --active | grep ':wifi$' | cut -d: -f1 | head -1)
    if [[ -n "$WIFI_CONN" ]]; then
        sudo nmcli connection modify "$WIFI_CONN" 802-11-wireless.powersave 2
        sudo iw dev wlan0 set power_save off 2>/dev/null || true
        info "WiFi power save disabled for connection: $WIFI_CONN"
    else
        info "No active WiFi connection found — skipping (Ethernet-only setup)"
    fi

    # Drop-in conf so any future NM-managed wifi connection also has power save off
    sudo mkdir -p /etc/NetworkManager/conf.d
    sudo tee /etc/NetworkManager/conf.d/wifi-powersave-off.conf > /dev/null << 'EOF'
[connection]
# Disable power save on all WiFi connections (2 = disable, 3 = enable)
wifi.powersave = 2
EOF
    sudo systemctl reload NetworkManager
    info "Global NM policy: wifi.powersave=disable (covers future connections too)"
else
    info "No WiFi device detected — skipping (Ethernet-only setup)"
fi

# ── UFW firewall ──────────────────────────────────────────────────────────────
section "UFW firewall"

sudo ufw default deny incoming
sudo ufw default allow outgoing
sudo ufw allow ssh
sudo ufw --force enable

info "Firewall: deny inbound except SSH"

# ── Summary ───────────────────────────────────────────────────────────────────
cat << EOF

  ╔══════════════════════════════════════════════════════════╗
  ║  Setup complete!                                         ║
  ╚══════════════════════════════════════════════════════════╝

  Hostname   : $HOSTNAME  (active after reboot)
  Admin SSH  : ssh $ADMIN_USER@<ip>
  Kiosk URL  : $KIOSK_URL
               Change: sudo nano /home/$KIOSK_USER/url.txt
  Pi Connect : ssh in as $ADMIN_USER, run 'rpi-connect signin', open the URL
               printed, sign in. Then visit https://connect.raspberrypi.com
               for remote screen + shell access.

  After reboot:
  ✓  $KIOSK_USER auto-logs in → cage → Chromium kiosk
  ✓  SSH as $ADMIN_USER (key only) to manage the system
  ✓  rpi-connect persists across SSH logouts (linger enabled)
  ✗  $KIOSK_USER has no SSH, no sudo, no shell
  ✗  tty1 autologin is disabled (no VT escalation path)

EOF

warn "Verify SSH works as $ADMIN_USER before rebooting!"
echo
read -rp "Reboot now? (y/n): " -n 1 REBOOT_REPLY; echo
[[ $REBOOT_REPLY =~ ^[Yy]$ ]] && sudo reboot

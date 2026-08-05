#!/bin/bash
# =====================================================
# MusallahBoard Uninstall Script
# =====================================================
# Removes MusallahBoard agent, kiosk, and configuration installed by setup.sh.
# Does NOT remove packages (cage, chromium, etc.) or undo hostname/timezone.
#
# Usage:
#   sudo bash uninstall.sh
# =====================================================

set -euo pipefail

RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'; BOLD='\033[1m'; NC='\033[0m'
info()    { echo -e "${GREEN}[INFO]${NC}  $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error()   { echo -e "${RED}[ERROR]${NC} $*"; exit 1; }
section() { echo; echo -e "${BOLD}=== $* ===${NC}"; }

[[ $EUID -ne 0 ]] && error "Run as root: sudo bash $0"

cat << 'BANNER'

  +----------------------------------------------+
  |       MusallahBoard Uninstall                |
  +----------------------------------------------+

BANNER

# ── Stop and disable services ─────────────────────────────────────────────────
section "Stopping services"

systemctl stop musallahboard-agent.service 2>/dev/null || true
systemctl stop musallahboard-kiosk.service 2>/dev/null || true
systemctl stop musallahboard-kiosk-watch.path 2>/dev/null || true
systemctl stop musallahboard-kiosk-reload.service 2>/dev/null || true

systemctl disable musallahboard-agent.service 2>/dev/null || true
systemctl disable musallahboard-kiosk.service 2>/dev/null || true
systemctl disable musallahboard-kiosk-watch.path 2>/dev/null || true

info "Services stopped and disabled"

# ── Remove systemd units ──────────────────────────────────────────────────────
section "Removing systemd units"

rm -f /lib/systemd/system/musallahboard-agent.service
rm -f /lib/systemd/system/musallahboard-kiosk.service
rm -f /lib/systemd/system/musallahboard-kiosk-watch.path
rm -f /lib/systemd/system/musallahboard-kiosk-reload.service

systemctl daemon-reload
info "Systemd units removed"

# ── Remove binary and scripts ─────────────────────────────────────────────────
section "Removing binaries and scripts"

rm -f /usr/bin/musallahboard-agent
rm -f /usr/bin/start-kiosk.sh
info "Binaries removed"

# ── Remove sudoers ────────────────────────────────────────────────────────────
section "Removing sudoers"

rm -f /etc/sudoers.d/musallahboard-agent
info "Sudoers removed"

# ── Remove config and state ───────────────────────────────────────────────────
section "Removing configuration and state"

rm -rf /etc/musallahboard
rm -rf /var/lib/musallahboard
rm -rf /var/log/musallahboard
rm -rf /usr/share/musallahboard
info "Configuration and state removed"

# ── Remove service users ──────────────────────────────────────────────────────
section "Removing service users"

if id musallahkiosk &>/dev/null; then
    # Kill any processes owned by the user first
    pkill -9 -u musallahkiosk 2>/dev/null || true
    userdel -r musallahkiosk 2>/dev/null || userdel musallahkiosk 2>/dev/null || true
    info "Removed user musallahkiosk"
fi

if id musallahdaemon &>/dev/null; then
    userdel musallahdaemon 2>/dev/null || true
    info "Removed user musallahdaemon"
fi

# ── Remove MusallahBoard-specific system configs ──────────────────────────────
section "Removing system configurations"

rm -f /etc/systemd/system.conf.d/watchdog.conf
rm -f /etc/systemd/logind.conf.d/kiosk.conf
rm -f /etc/ssh/sshd_config.d/kiosk-hardening.conf
rm -f /etc/NetworkManager/conf.d/wifi-powersave-off.conf

# Reload affected services
systemctl daemon-reload
systemctl reload ssh 2>/dev/null || systemctl reload sshd 2>/dev/null || true
systemctl reload NetworkManager 2>/dev/null || true

info "System configurations removed"

# ── Unmask wayvnc ─────────────────────────────────────────────────────────────
section "Restoring masked services"

systemctl unmask wayvnc.service 2>/dev/null || true
info "wayvnc.service unmasked"

# ── Restore boot target (optional) ────────────────────────────────────────────
section "Boot target"

current_target=$(systemctl get-default)
if [[ "$current_target" == "multi-user.target" ]]; then
    warn "Boot target is multi-user.target (set by setup.sh)"
    warn "To restore graphical boot: sudo systemctl set-default graphical.target"
fi

# ── Summary ───────────────────────────────────────────────────────────────────
cat << 'EOF'

  +----------------------------------------------------------+
  |  Uninstall complete                                      |
  +----------------------------------------------------------+

  Removed:
    - musallahboard-agent binary and service
    - musallahboard-kiosk service and launcher
    - Service users (musallahdaemon, musallahkiosk)
    - Configuration (/etc/musallahboard)
    - State (/var/lib/musallahboard, /var/log/musallahboard)
    - SSH hardening config
    - Watchdog, logind, WiFi power-save configs

  NOT removed (do manually if needed):
    - Packages (cage, chromium, rpi-connect, ufw, etc.)
    - Admin user and SSH key
    - Hostname and timezone
    - UFW rules
    - Journal config
    - Unattended upgrades config

  You may want to reboot to fully clear state.

EOF

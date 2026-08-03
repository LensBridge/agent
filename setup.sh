#!/bin/bash
# =====================================================
# MusallahBoard — Raspberry Pi (arm64) setup
# =====================================================
# Thin platform wrapper. All shared logic lives in
# packaging/lib/common.sh; this file only declares what is specific to
# Raspberry Pi OS (Debian/trixie) bare metal:
#   - cage + deb Chromium + Raspberry Pi Connect packages
#   - purge of unnecessary network services
#   - hardware watchdog, WiFi power-save fix, rpi-connect/linger/wayvnc
#
# Run as a user with sudo access (e.g. the default 'pi' user):
#   bash setup.sh
#
# This prepares the host only. Install the agent + kiosk afterwards with
# packaging/install.sh (the command is printed at the end).
# =====================================================

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ── Platform contract for common.sh ───────────────────────────────────────────
PLATFORM_LABEL="Raspberry Pi (arm64)"
PLATFORM_ARCH="arm64"
SSH_KEY_REQUIRED="yes"          # a headless Pi must not be left password-auth
export REPO_ROOT PLATFORM_LABEL PLATFORM_ARCH SSH_KEY_REQUIRED

# ── Packages: cage kiosk stack + Pi Connect ───────────────────────────────────
platform_install_packages() {
    # cage is a single-app Wayland kiosk compositor — it replaces the old
    # labwc + lightdm + autostart stack. No display manager is installed.
    # On the Pi deb `chromium` is the correct browser (not a snap).
    sudo apt-get install -y \
        cage \
        chromium \
        rpi-connect \
        ufw \
        unattended-upgrades \
        apt-listchanges

    # Purge services that add unnecessary network attack surface. Use
    # purge+hold so they don't return as a dependency pull-in.
    # NOTE: do NOT run `apt autoremove` after this — it may remove
    # rpd-wayland-core and other display-stack packages on Pi OS images.
    info "Purging unnecessary network services..."
    sudo apt-get purge -y rpcbind nfs-common 2>/dev/null || true
    sudo apt-mark hold rpcbind nfs-common
}

# ── Pi-only hardening: watchdog, WiFi power-save, Pi Connect ──────────────────
platform_after_hardening() {
    section "Hardware watchdog"
    sudo mkdir -p /etc/systemd/system.conf.d
    sudo tee /etc/systemd/system.conf.d/watchdog.conf > /dev/null << 'EOF'
[Manager]
RuntimeWatchdogSec=15
RebootWatchdogSec=2min
EOF
    info "Watchdog: systemd pings /dev/watchdog0 every 15s; reboots if it misses"

    section "WiFi power save"
    # WiFi power save lets the chip sleep during low traffic. On a 24/7 kiosk
    # the DHCP renewal packet gets missed, the lease expires, and the Pi
    # becomes unreachable even though the router still shows it associated.
    if nmcli device status | grep -q wifi; then
        WIFI_CONN=$(nmcli -t -f NAME,TYPE connection show --active | grep ':wifi$' | cut -d: -f1 | head -1)
        if [[ -n "$WIFI_CONN" ]]; then
            sudo nmcli connection modify "$WIFI_CONN" 802-11-wireless.powersave 2
            sudo iw dev wlan0 set power_save off 2>/dev/null || true
            info "WiFi power save disabled for connection: $WIFI_CONN"
        else
            info "No active WiFi connection — skipping (Ethernet-only setup)"
        fi
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

    section "Raspberry Pi Connect"
    # rpi-connect runs as a *user* systemd service under $ADMIN_USER and uses
    # `sudo -u $KIOSK_USER` internally to attach wayvnc to the kiosk user's
    # Wayland session (provided by cage). Without linger the service dies the
    # moment $ADMIN_USER's last session ends, so remote access would only work
    # while you happen to be SSHed in. Enable linger so it persists 24/7.
    sudo loginctl enable-linger "$ADMIN_USER"

    # The packaged system-level wayvnc.service runs as user 'vnc' and cannot
    # attach to $KIOSK_USER's Wayland session — it loops in a failed-restart
    # state forever, eating CPU. rpi-connect runs its own wayvnc, so mask the
    # system one to silence the loop.
    sudo systemctl stop wayvnc.service 2>/dev/null || true
    sudo systemctl mask wayvnc.service
    info "Linger enabled for $ADMIN_USER; system wayvnc.service masked"
    warn "Post-install: ssh in as $ADMIN_USER and run 'rpi-connect signin'"
    warn "             then visit https://connect.raspberrypi.com"
}

# shellcheck source=packaging/lib/common.sh
source "$REPO_ROOT/packaging/lib/common.sh"
musallahboard_setup_main

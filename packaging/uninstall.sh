#!/bin/bash
# =====================================================
# MusallahBoard Agent Uninstall Script
# =====================================================
# Stops and removes musallahboard-agent from a host.
#
# Usage:
#   sudo bash uninstall.sh              # keep device identity (config + key)
#   sudo bash uninstall.sh --purge      # also delete /etc/musallahboard
#                                       # (irreversible — the Ed25519 key is gone)
#
# What is removed in the default mode:
#   - /lib/systemd/system/musallahboard-*.{service,path} (stopped, disabled),
#     including the self-updater and the USB import template
#   - /etc/udev/rules.d/90-musallahboard-{usb,rtc}.rules
#   - /etc/sudoers.d/musallahboard-agent
#   - /usr/bin/musallahboard-agent, /usr/lib/musallahboard (rollback binary)
#   - /var/lib/musallahboard (content, app, inbox, state)
#   - /var/log/musallahboard
#   - the service port, if set up: the musallahboard-service-port and
#     musallahboard-lan NetworkManager connections, its ufw rules, and the
#     RTC boot-time clock unit
#
# What is preserved (unless --purge):
#   - /etc/musallahboard/agent.toml      (device id, backend url)
#   - /etc/musallahboard/agent.key       (Ed25519 private key)
#   - /etc/musallahboard/trust.json      (pinned signing keys)
#   - The service user account ('musallahdaemon')
#
# Re-installing later (without --purge) keeps the device's existing enrollment.
# =====================================================

set -euo pipefail

RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'; BOLD='\033[1m'; NC='\033[0m'
info()    { echo -e "${GREEN}[INFO]${NC}  $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error()   { echo -e "${RED}[ERROR]${NC} $*"; exit 1; }
section() { echo; echo -e "${BOLD}═══ $* ═══${NC}"; }

PURGE=no
while [[ $# -gt 0 ]]; do
    case "$1" in
        --purge)   PURGE=yes; shift ;;
        -h|--help) sed -n '2,31p' "$0"; exit 0 ;;
        *)         error "Unknown flag: $1" ;;
    esac
done

[[ $EUID -ne 0 ]] && error "Run as root: sudo bash $0"

section "Stopping service"
if systemctl list-unit-files musallahboard-agent.service &>/dev/null; then
    systemctl stop    musallahboard-agent.service 2>/dev/null || true
    systemctl disable musallahboard-agent.service 2>/dev/null || true
    info "Service stopped and disabled"
else
    warn "Service unit not installed — nothing to stop"
fi

section "Removing files"

if systemctl list-unit-files musallahboard-kiosk-watch.path &>/dev/null; then
    systemctl disable --now musallahboard-kiosk-watch.path 2>/dev/null || true
fi
# The self-updater first: it must not fire while the files below disappear.
systemctl disable --now musallahboard-agent-update.path 2>/dev/null || true
systemctl stop musallahboard-agent-update.service 2>/dev/null || true
systemctl stop 'musallahboard-usb-import@*.service' 2>/dev/null || true
# udev before the units, so a stick plugged in now starts nothing.
rm -f /etc/udev/rules.d/90-musallahboard-usb.rules
rm -f /etc/udev/rules.d/90-musallahboard-rtc.rules
udevadm control --reload 2>/dev/null || true
rm -f /lib/systemd/system/musallahboard-agent.service
rm -f /lib/systemd/system/musallahboard-kiosk.service
rm -f /lib/systemd/system/musallahboard-kiosk-watch.path
rm -f /lib/systemd/system/musallahboard-kiosk-reload.service
rm -f /lib/systemd/system/musallahboard-agent-update.path
rm -f /lib/systemd/system/musallahboard-agent-update.service
rm -f '/lib/systemd/system/musallahboard-usb-import@.service'
# Pre-/lib layout — harmless if absent, and leaving one behind would shadow a
# reinstall's unit, since /etc/systemd/system takes precedence over /lib.
rm -f /etc/systemd/system/musallahboard-agent.service
rm -f /etc/systemd/system/musallahboard-kiosk.service
rm -f /etc/systemd/system/musallahboard-kiosk-watch.path
rm -f /etc/systemd/system/musallahboard-kiosk-reload.service
rm -f /etc/sudoers.d/musallahboard-agent
rm -f /usr/bin/musallahboard-agent
rm -f /usr/bin/.musallahboard-agent.new
rm -rf /usr/lib/musallahboard
rm -f /usr/bin/start-kiosk.sh
rm -rf /usr/share/musallahboard
info "Removed binary, units (agent, self-updater, USB import, kiosk watcher), udev rules, launcher, splash, sudoers"

# State / logs: never preserved — they don't contain identity, just runtime crud.
# Includes installed content and app releases.
rm -rf /var/lib/musallahboard
rm -rf /var/log/musallahboard
info "Removed /var/lib/musallahboard and /var/log/musallahboard"

# The service port (setup.sh --service-port). Left behind, the
# service-port connection would keep eth0 able to act as a DHCP server with no
# agent to switch it off.
section "Removing the service port"
if command -v nmcli &>/dev/null; then
    for conn in musallahboard-service-port musallahboard-lan; do
        if nmcli -g NAME connection show 2>/dev/null | grep -qxF "$conn"; then
            if nmcli connection delete "$conn" >/dev/null; then
                info "Deleted NetworkManager connection $conn"
            else
                warn "Could not delete NetworkManager connection $conn"
            fi
        fi
    done
fi
if command -v ufw &>/dev/null; then
    ufw delete allow in on eth0 to any port 67 proto udp >/dev/null 2>&1 || true
    ufw delete allow in on eth0 to any port 53 >/dev/null 2>&1 || true
    ufw delete allow in on eth0 from 10.77.0.0/24 to any port 80 proto tcp >/dev/null 2>&1 || true
    info "Removed the service port's DHCP/DNS/upload firewall rules (if present)"
fi
if [[ -e /etc/udev/rules.d/85-musallahboard-rtc.rules || -e /lib/systemd/system/musallahboard-rtc.service ]]; then
    rm -f /etc/udev/rules.d/85-musallahboard-rtc.rules
    rm -f /lib/systemd/system/musallahboard-rtc.service
    udevadm control --reload 2>/dev/null || true
    # The dtoverlay line in config.txt stays (the RTC is still fitted, and
    # harmless), and fake-hwclock is not reinstalled.
    info "Removed the RTC boot-time clock unit"
fi
if [[ "$PURGE" == "yes" ]]; then
    warn "--purge: deleting device identity at /etc/musallahboard"
    warn "         (the Ed25519 private key will be lost — re-enrollment required)"
    rm -rf /etc/musallahboard
    info "Removed /etc/musallahboard"
else
    if [[ -d /etc/musallahboard ]]; then
        info "Preserved /etc/musallahboard (device identity)"
        info "  Use 'uninstall.sh --purge' if you want it gone."
    fi
fi

systemctl daemon-reload

echo
info "musallahboard-agent uninstalled"

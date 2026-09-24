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
#   - /lib/systemd/system/musallahboard-*.{service,path} (stopped, disabled)
#   - /etc/sudoers.d/musallahboard-agent
#   - /usr/bin/musallahboard-agent
#   - /var/lib/musallahboard
#   - /var/log/musallahboard
#   - offline mode, if set up: the musallahboard-service-port and
#     musallahboard-lan NetworkManager connections, the service port's ufw
#     rules, the RTC boot-time clock unit, and the 'mbpush' push account
#     (user, sudoers rule, sshd settings)
#
# What is preserved (unless --purge):
#   - /etc/musallahboard/agent.toml      (device id, backend url)
#   - /etc/musallahboard/agent.key       (Ed25519 private key)
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
        -h|--help) sed -n '2,25p' "$0"; exit 0 ;;
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
rm -f /lib/systemd/system/musallahboard-agent.service
rm -f /lib/systemd/system/musallahboard-kiosk.service
rm -f /lib/systemd/system/musallahboard-kiosk-watch.path
rm -f /lib/systemd/system/musallahboard-kiosk-reload.service
# Pre-/lib layout — harmless if absent, and leaving one behind would shadow a
# reinstall's unit, since /etc/systemd/system takes precedence over /lib.
rm -f /etc/systemd/system/musallahboard-agent.service
rm -f /etc/systemd/system/musallahboard-kiosk.service
rm -f /etc/systemd/system/musallahboard-kiosk-watch.path
rm -f /etc/systemd/system/musallahboard-kiosk-reload.service
rm -f /etc/sudoers.d/musallahboard-agent
rm -f /usr/bin/musallahboard-agent
rm -f /usr/bin/start-kiosk.sh
rm -rf /usr/share/musallahboard
info "Removed binary, units (agent + kiosk watcher), launcher, splash, sudoers"

# State / logs: never preserved — they don't contain identity, just runtime crud.
# Includes offline mode's installed bundles (/var/lib/musallahboard/offline).
rm -rf /var/lib/musallahboard
rm -rf /var/log/musallahboard
info "Removed /var/lib/musallahboard and /var/log/musallahboard"

# Offline mode (setup.sh --offline). Left behind, the service-port connection
# would keep eth0 able to act as a DHCP server with no agent to switch it off.
section "Removing offline mode"
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
    info "Removed the service port's DHCP/DNS firewall rules (if present)"
fi
if [[ -e /etc/udev/rules.d/85-musallahboard-rtc.rules || -e /lib/systemd/system/musallahboard-rtc.service ]]; then
    rm -f /etc/udev/rules.d/85-musallahboard-rtc.rules
    rm -f /lib/systemd/system/musallahboard-rtc.service
    udevadm control --reload 2>/dev/null || true
    # The dtoverlay line in config.txt stays (the RTC is still fitted, and
    # harmless), and fake-hwclock is not reinstalled.
    info "Removed the RTC boot-time clock unit"
fi
# The push account. Its only permission was running the agent's gate, which
# is gone now, but a login nobody uses should not outlive its purpose.
if id mbpush &>/dev/null || [[ -e /etc/sudoers.d/musallahboard-push || -e /etc/ssh/sshd_config.d/99-musallahboard-push.conf ]]; then
    rm -f /etc/sudoers.d/musallahboard-push
    rm -f /etc/ssh/sshd_config.d/99-musallahboard-push.conf
    if [[ -f /etc/ssh/sshd_config.d/kiosk-hardening.conf ]]; then
        sed -i -E 's/^(AllowUsers\b.*)[[:space:]]mbpush([[:space:]]|$)/\1\2/' /etc/ssh/sshd_config.d/kiosk-hardening.conf
    fi
    if /usr/sbin/sshd -t 2>/dev/null; then
        systemctl reload ssh 2>/dev/null || systemctl reload sshd 2>/dev/null || true
    else
        warn "sshd's configuration does not validate after removing the push account; check /etc/ssh before logging out"
    fi
    if id mbpush &>/dev/null; then
        userdel --remove mbpush 2>/dev/null || userdel mbpush || true
    fi
    info "Removed the push account (mbpush), its sudo rule and sshd settings"
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

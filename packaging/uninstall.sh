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
#   - /etc/systemd/system/musallahboard-agent.service (stopped, disabled)
#   - /etc/sudoers.d/musallahboard-agent
#   - /usr/local/bin/musallahboard-agent
#   - /var/lib/musallahboard
#   - /var/log/musallahboard
#
# What is preserved (unless --purge):
#   - /etc/musallahboard/agent.toml      (device id, backend url)
#   - /etc/musallahboard/agent.key       (Ed25519 private key)
#   - The service user account ('admin' or as configured)
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

rm -f /etc/systemd/system/musallahboard-agent.service
rm -f /etc/sudoers.d/musallahboard-agent
rm -f /usr/local/bin/musallahboard-agent
info "Removed binary, unit, sudoers"

# State / logs: never preserved — they don't contain identity, just runtime crud
rm -rf /var/lib/musallahboard
rm -rf /var/log/musallahboard
info "Removed /var/lib/musallahboard and /var/log/musallahboard"

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

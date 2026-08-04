#!/bin/bash
# =====================================================
# MusallahBoard appliance policy
# =====================================================
# Turns a general-purpose Debian box into a board: no display manager, boots
# straight to multi-user.target where musallahboard-kiosk.service IS the
# graphical session.
#
# This is deliberately NOT part of packaging/install.sh, and will never be part
# of the .deb. Installing a package must not repoint the machine's default boot
# target or disable its display manager — that is the local administrator's
# decision, and a package that makes it for them is a package that breaks
# somebody's desktop. install.sh installs and enables the kiosk unit; this
# script is what makes the box actually boot into it.
#
# Callers:
#   packaging/lib/common.sh   (setup.sh — Raspberry Pi bare metal)
#   image/scripts/*           (Packer / image builds)
#
# Safe to re-run. Works with or without a live systemd: `set-default` and
# `disable` only rewrite symlinks, so they succeed in an image-build chroot.
#
# Usage:
#   sudo bash appliance-policy.sh
# =====================================================

set -euo pipefail

RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'; BOLD='\033[1m'; NC='\033[0m'
info()    { echo -e "${GREEN}[INFO]${NC}  $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error()   { echo -e "${RED}[ERROR]${NC} $*"; exit 1; }
section() { echo; echo -e "${BOLD}═══ $* ═══${NC}"; }

[[ $EUID -ne 0 ]] && error "Run as root: sudo bash $0"

# `--now` needs a running manager; in a chroot we only rewrite the symlinks.
systemd_running() { [[ -d /run/systemd/system ]]; }

section "Appliance boot policy"

# The kiosk unit takes /dev/tty1 and owns the seat. A display manager competing
# for the same VT is the classic cause of a board that shows a login prompt, or
# a black screen, instead of the board.
if systemctl list-unit-files 2>/dev/null | grep -qE '^(lightdm|gdm3?|sddm)\.service'; then
    if systemd_running; then
        systemctl disable --now lightdm.service gdm.service gdm3.service sddm.service 2>/dev/null || true
    else
        systemctl disable lightdm.service gdm.service gdm3.service sddm.service 2>/dev/null || true
    fi
    info "Disabled display manager(s)"
else
    info "No display manager installed — nothing to disable"
fi

systemctl set-default multi-user.target >/dev/null
info "Default boot target: multi-user.target"

target="$(systemctl get-default 2>/dev/null || echo unknown)"
[[ "$target" == "multi-user.target" ]] \
    || error "default target is '$target' after set-default — refusing to claim success"

echo
info "Appliance policy applied. The kiosk unit is the graphical session."

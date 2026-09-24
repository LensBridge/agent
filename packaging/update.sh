#!/bin/bash
# =====================================================
# MusallahBoard Agent Update Script
# =====================================================
# Replaces the agent binary in place and restarts the service. Preserves
# /etc/musallahboard (device identity and trust.json) and the systemd unit /
# sudoers config — use install.sh instead if you also need to update those.
#
# This is the manual, admin-over-SSH path. Boards normally update themselves
# from signed agent packages (.mbu) instead: online through the release
# channel, offline from a USB stick or an upload on the service port. Those go
# through the root self-updater, which verifies signatures and rolls back a
# release that does not come up. This script does neither: it trusts the
# binary you give it, and it does not touch the version high-water mark, so it
# can also downgrade.
#
# Usage:
#   sudo bash update.sh                 # uses ./musallahboard-agent next to script
#   sudo bash update.sh /path/to/bin    # uses the binary at the given path
#
# Typical remote-update flow from a dev box:
#   scp build/musallahboard-agent-arm64 \
#       packaging/update.sh             \
#       admin@pi:/tmp/
#   ssh admin@pi 'sudo bash /tmp/update.sh /tmp/musallahboard-agent-arm64'
# =====================================================

set -euo pipefail

RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'; NC='\033[0m'
info()  { echo -e "${GREEN}[INFO]${NC}  $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error() { echo -e "${RED}[ERROR]${NC} $*"; exit 1; }

[[ $EUID -ne 0 ]] && error "Run as root: sudo bash $0"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BINARY_DEST=/usr/bin/musallahboard-agent
SERVICE=musallahboard-agent.service
LIB_DIR=/usr/lib/musallahboard
PREV_DEST=${LIB_DIR}/agent.prev

BINARY_SRC="${1:-}"
if [[ -z "$BINARY_SRC" ]]; then
    # Look for a sibling binary; prefer arch-suffixed
    for cand in \
        "$SCRIPT_DIR/musallahboard-agent-$(uname -m)" \
        "$SCRIPT_DIR/musallahboard-agent-arm64" \
        "$SCRIPT_DIR/musallahboard-agent-amd64" \
        "$SCRIPT_DIR/musallahboard-agent"
    do
        if [[ -x "$cand" ]]; then BINARY_SRC="$cand"; break; fi
    done
fi
[[ -z "$BINARY_SRC" ]] && error "No binary found. Pass one as the first argument."
[[ -x "$BINARY_SRC" ]] || error "Not executable: $BINARY_SRC"

# Sanity: refuse to install a binary that segfaults on `version`
if ! "$BINARY_SRC" version >/dev/null 2>&1; then
    error "Binary at $BINARY_SRC failed 'version' smoke test — refusing to install."
fi
NEW_VERSION="$("$BINARY_SRC" version 2>&1 || true)"
OLD_VERSION=""
[[ -x "$BINARY_DEST" ]] && OLD_VERSION="$($BINARY_DEST version 2>&1 || true)"

info "Replacing $BINARY_DEST"
info "  old: ${OLD_VERSION:-<none>}"
info "  new: $NEW_VERSION"

# Keep the running binary where the self-updater keeps its rollback copy, so
# a bad manual update can be undone the same way:
#   sudo install -m 0755 /usr/lib/musallahboard/agent.prev /usr/bin/musallahboard-agent
if [[ -x "$BINARY_DEST" ]]; then
    install -d -o root -g root -m 0755 "$LIB_DIR"
    install -o root -g root -m 0755 "$BINARY_DEST" "$PREV_DEST"
    info "Saved the previous binary as $PREV_DEST"
fi

# Atomic replace: install(1) handles temp file + rename.
install -o root -g root -m 0755 "$BINARY_SRC" "$BINARY_DEST"

if systemctl list-unit-files "$SERVICE" &>/dev/null; then
    info "Restarting $SERVICE"
    systemctl restart "$SERVICE"
    sleep 1
    systemctl --no-pager --lines=5 status "$SERVICE" || true
else
    warn "$SERVICE not installed — binary replaced but no service to restart."
    warn "Run install.sh to fully install."
fi

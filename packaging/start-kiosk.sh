#!/bin/bash
# =====================================================
# MusallahBoard kiosk launcher
# =====================================================
# Run by musallahboard-kiosk.service as the locked-down kiosk user. cage is a
# single-application Wayland kiosk compositor: it owns the seat (DRM + input)
# and runs exactly one client fullscreen, then exits when that client exits —
# at which point systemd (Restart=always) bounces us. This replaces the old
# lightdm + labwc autologin stack: one systemd unit == the whole display.
#
# The agent writes the URL to KIOSK_URL_FILE once the device is enrolled:
# its own local server, http://127.0.0.1:8080/ (or, on a v1 board with a
# board-url and no app release yet, the hosted site). Until then the file is
# absent and we show the local "waiting for enrollment" splash; the kiosk
# .path watcher restarts us when the agent writes it.
#
# Browser: we must NOT use a snap. Snap Chromium runs in its own snapd cgroup
# scope, so it survives `systemctl restart`
# and the agent's kiosk.restart never actually cycles it. On the Pi, deb
# `chromium` is the right binary; on the Ubuntu test VM it's `google-chrome-
# stable` (a real deb). We pick the first real one and explicitly reject the
# leftover /usr/bin/chromium-browser snap stub.
set -euo pipefail

KIOSK_URL_FILE="/etc/musallahboard/kiosk-url"
SPLASH="file:///usr/share/musallahboard/waiting.html"

URL=""
if [[ -r "$KIOSK_URL_FILE" ]]; then
    URL="$(tr -d '[:space:]' < "$KIOSK_URL_FILE")"
fi
[[ -z "$URL" ]] && URL="$SPLASH"

# Resolve a real (non-snap) browser. A snap shim is a tiny shell script that
# execs /snap/bin or BAMF_DESKTOP_FILE_HINT; a real browser is an ELF. Treat
# anything under /snap or that greps as a snap wrapper as disqualified.
pick_browser() {
    local cand
    for cand in google-chrome-stable google-chrome chromium chromium-browser; do
        local path
        path="$(command -v "$cand" 2>/dev/null)" || continue
        # Reject snap shims / wrappers.
        case "$(readlink -f "$path")" in
            /snap/*) continue ;;
        esac
        if head -c4 "$path" | grep -q $'\x7fELF' || file -b "$path" 2>/dev/null | grep -qi 'ELF'; then
            echo "$path"; return 0
        fi
        # Some distro browsers are launcher scripts that are NOT snap shims
        # (e.g. Debian's /usr/bin/chromium). Accept a script only if it does
        # not reference snap at all.
        if ! grep -qi 'snap' "$path" 2>/dev/null; then
            echo "$path"; return 0
        fi
    done
    return 1
}

BROWSER="$(pick_browser)" || {
    echo "start-kiosk: no non-snap browser found (need deb chromium or google-chrome-stable)" >&2
    exit 1
}

# Chrome/Chromium >=136 SILENTLY DISABLES --remote-debugging-port unless a
# non-default --user-data-dir is also given (token-theft mitigation). The
# agent's CDP commands depend on :9222, so a dedicated profile dir is
# mandatory, not optional. Keep it in the kiosk user's home.
PROFILE_DIR="${HOME:-/home/$(id -un)}/.musallahboard-kiosk"
mkdir -p "$PROFILE_DIR"

# cage -d: don't draw client-side decorations (we want a bare fullscreen page).
# VT switching is already disabled by default (cage needs -s to ALLOW it).
# --remote-debugging-port=9222 on localhost is the channel the agent's CDP
# commands (chrome.reload, chrome.screenshot, config.refresh) attach to —
# do not remove it.
exec cage -d -- "$BROWSER" \
    --ozone-platform=wayland \
    --disable-dev-shm-usage \
    --kiosk \
    --noerrdialogs \
    --disable-infobars \
    --disable-session-crashed-bubble \
    --disable-restore-session-state \
    --no-first-run \
    --start-fullscreen \
    --disable-translate \
    --disable-features=TranslateUI \
    --disable-pinch \
    --overscroll-history-navigation=0 \
    --check-for-update-interval=31536000 \
    --enable-features=OverlayScrollbar \
    --user-data-dir="$PROFILE_DIR" \
    --remote-debugging-port=9222 \
    --remote-debugging-address=127.0.0.1 \
    "$URL"

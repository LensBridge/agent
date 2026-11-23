#!/bin/bash
# =====================================================
# MusallahBoard — pre-built Raspberry Pi image
# =====================================================
# Turns Raspberry Pi OS Lite (trixie, arm64) into a MusallahBoard image: flash
# it, and the board boots to "This board is not set up yet", ready for its
# setup USB stick. docs/architecture.md, section 17, is the design.
#
# The work on the image is setup.sh's, not this script's: this one mounts the
# image and runs `setup.sh --image` inside it, so a board flashed from the
# image and one installed with curl | bash run the same steps. What depends on
# the board (hostname, admin and SSH key, Wi-Fi, time zone) is cloud-init's,
# from Raspberry Pi Imager; the content signing key comes from a
# musallahboard.txt on the boot partition. Both are applied at first boot
# (setup.sh --firstboot).
#
# Usage (as root, on Linux):
#   sudo image/build.sh --agent=build/musallahboard-agent-arm64 \
#       --app=musallahboard-app-2.1.0.mbu --mbpack=build/mbpack
#   sudo image/build.sh --agent-version=1.1.5 --app-latest
#
# Needs: losetup, mount, parted, e2fsck, resize2fs, zerofree, xz, curl,
# sha256sum, chroot, mtools (when the kernel cannot mount vfat). On a host
# that is not arm64, qemu-user-static registered with binfmt_misc (the F
# flag), e.g. `apt install qemu-user-static binfmt-support`.
# =====================================================

set -euo pipefail

# ── Base image ────────────────────────────────────────────────────────────────
# Pinned, so a build is the same build until someone moves it on purpose.
# Newer releases: https://downloads.raspberrypi.com/raspios_lite_arm64/images/
BASE_URL="${MB_BASE_URL:-https://downloads.raspberrypi.com/raspios_lite_arm64/images/raspios_lite_arm64-2026-10-06/2026-10-06-raspios-trixie-arm64-lite.img.xz}"
BASE_SHA256="${MB_BASE_SHA256:-483db18a48da399b5b7022ffae9a07bc6d99daab30e593aedac09b69ff422843}"

AGENT_RELEASES=https://github.com/LensBridge/agent/releases
APP_CHANNEL=https://github.com/LensBridge/MusallahBoard/releases/latest/download/app-channel.json

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/.." && pwd)"
SETUP_SH="$REPO/setup.sh"

# ── Options ───────────────────────────────────────────────────────────────────
AGENT=""
AGENT_VERSION=""
APP=""
APP_LATEST=0
MBPACK=""
BASE_FILE=""
GROW=1536M
WORK="$REPO/build/image-work"
OUT=""
KEEP_IMG=0

usage() {
    cat <<'USAGE'
Usage: sudo image/build.sh [options]

Agent (one of):
  --agent=FILE           an arm64 agent binary (a release build: it must carry
                         the release keys to install the board app)
  --agent-version=V      download that agent release's arm64 binary

Board app (optional; without it the board fetches it when online):
  --app=FILE.mbu         a signed board app package
  --app-latest           download the current one from the app channel

  --mbpack=FILE          verify --app with this mbpack before using it
  --base=FILE            a Raspberry Pi OS Lite .img.xz or .img already on
                         disk (default: download the pinned one and check it)
  --grow=SIZE            room added to the root filesystem (default 1536M)
  --work=DIR             scratch directory (default build/image-work)
  --out=FILE             output (default build/musallahboard-<agent>-arm64.img.xz)
  --keep-img             also keep the uncompressed .img
  -h, --help
USAGE
}

for arg in "$@"; do
    case "$arg" in
        --agent=*)         AGENT="${arg#*=}" ;;
        --agent-version=*) AGENT_VERSION="${arg#*=}" ;;
        --app=*)           APP="${arg#*=}" ;;
        --app-latest)      APP_LATEST=1 ;;
        --mbpack=*)        MBPACK="${arg#*=}" ;;
        --base=*)          BASE_FILE="${arg#*=}" ;;
        --grow=*)          GROW="${arg#*=}" ;;
        --work=*)          WORK="${arg#*=}" ;;
        --out=*)           OUT="${arg#*=}" ;;
        --keep-img)        KEEP_IMG=1 ;;
        -h|--help)         usage; exit 0 ;;
        *) echo "Unknown option: $arg" >&2; usage >&2; exit 1 ;;
    esac
done

info()    { echo "[INFO]  $*"; }
warn()    { echo "[WARN]  $*" >&2; }
die()     { echo "[ERROR] $*" >&2; exit 1; }
section() { echo; echo "=== $* ==="; }

# ── Cleanup ───────────────────────────────────────────────────────────────────
# Everything mounted or attached is recorded here and undone in reverse on any
# exit, so a failed build never leaves a loop device or a mount behind.
MOUNTS=()
LOOPS=()
ROOT=""
BOOT_FALLBACK=""
cleanup() {
    local i
    for ((i = ${#MOUNTS[@]} - 1; i >= 0; i--)); do
        umount "${MOUNTS[$i]}" 2>/dev/null || umount -l "${MOUNTS[$i]}" 2>/dev/null || true
    done
    MOUNTS=()
    for ((i = ${#LOOPS[@]} - 1; i >= 0; i--)); do
        losetup -d "${LOOPS[$i]}" 2>/dev/null || true
    done
    LOOPS=()
}
trap cleanup EXIT

mount_on() { mount "$@"; MOUNTS+=("${!#}"); }

# ── Preflight ─────────────────────────────────────────────────────────────────
preflight() {
    [[ $EUID -eq 0 ]] || die "Run as root (it mounts the image): sudo $0 $*"
    local t
    for t in losetup mount umount parted e2fsck resize2fs zerofree xz curl sha256sum chroot truncate; do
        command -v "$t" >/dev/null || die "$t not found"
    done
    [[ -f "$SETUP_SH" ]] || die "$SETUP_SH not found"
    [[ -n "$AGENT" || -n "$AGENT_VERSION" ]] || die "Give the agent: --agent=FILE or --agent-version=V"
    [[ -z "$AGENT" || -z "$AGENT_VERSION" ]] || die "--agent and --agent-version are exclusive"
    [[ -z "$APP" || "$APP_LATEST" == 0 ]] || die "--app and --app-latest are exclusive"
    if [[ "$(uname -m)" != aarch64 ]]; then
        grep -qs '^enabled' /proc/sys/fs/binfmt_misc/qemu-aarch64 || \
            die "This host is $(uname -m): register qemu-user-static for aarch64 with binfmt_misc (apt install qemu-user-static binfmt-support)"
        grep -qs '^flags:.*F' /proc/sys/fs/binfmt_misc/qemu-aarch64 || \
            die "The aarch64 binfmt handler needs the F flag to work inside a chroot"
    fi
    mkdir -p "$WORK"
}

# ── Inputs ────────────────────────────────────────────────────────────────────
# fetch URL FILE [SHA256]
fetch() {
    local url="$1" out="$2" sha="${3-}"
    if [[ -f "$out" && -n "$sha" ]] && echo "$sha  $out" | sha256sum -c --status; then
        info "Using cached $(basename "$out")"
        return
    fi
    info "Downloading $url"
    curl -fL --retry 3 --retry-delay 5 -o "$out.part" "$url"
    if [[ -n "$sha" ]]; then
        echo "$sha  $out.part" | sha256sum -c --status || die "Checksum mismatch for $url"
    fi
    mv -f "$out.part" "$out"
}

inputs() {
    section "Inputs"
    if [[ -n "$AGENT_VERSION" ]]; then
        AGENT="$WORK/musallahboard-agent-$AGENT_VERSION-arm64"
        fetch "$AGENT_RELEASES/download/$AGENT_VERSION/musallahboard-agent-arm64" "$AGENT"
        chmod 0755 "$AGENT"
    fi
    [[ -f "$AGENT" ]] || die "No such agent binary: $AGENT"
    # ELF header: class 64-bit (byte 4 = 2), machine aarch64 (bytes 18-19 = b7 00).
    [[ "$(od -An -tx1 -j4 -N1 "$AGENT" | tr -d ' ')" == 02 && \
       "$(od -An -tx1 -j18 -N2 "$AGENT" | tr -d ' ')" == b700 ]] || \
        die "$AGENT is not an arm64 Linux binary"
    AGENT_VER="$("$AGENT" version | awk '{print $NF}')"
    info "Agent $AGENT_VER ($AGENT)"
    [[ "$AGENT_VER" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || \
        warn "Agent version '$AGENT_VER' is not a release: a dev build trusts no release key and cannot install the board app"

    if [[ "$APP_LATEST" == 1 ]]; then
        local channel url sha
        channel="$(curl -fsSL --retry 3 "$APP_CHANNEL")"
        url="$(sed -n 's/.*"url" *: *"\([^"]*\)".*/\1/p' <<< "$channel")"
        sha="$(sed -n 's/.*"sha256" *: *"\([0-9a-f]*\)".*/\1/p' <<< "$channel")"
        [[ -n "$url" && -n "$sha" ]] || die "Could not read the app channel $APP_CHANNEL"
        APP="$WORK/$(basename "$url")"
        fetch "$url" "$APP" "$sha"
    fi
    if [[ -n "$APP" ]]; then
        [[ -f "$APP" ]] || die "No such app package: $APP"
        if [[ -n "$MBPACK" ]]; then
            "$MBPACK" verify "$APP" || die "$APP does not verify"
        else
            info "Not verifying the app here (no --mbpack); the board verifies it before installing"
        fi
        info "Board app $(basename "$APP")"
    fi

    if [[ -z "$BASE_FILE" ]]; then
        BASE_FILE="$WORK/$(basename "$BASE_URL")"
        fetch "$BASE_URL" "$BASE_FILE" "$BASE_SHA256"
    fi
    [[ -f "$BASE_FILE" ]] || die "No such base image: $BASE_FILE"
    info "Base image $(basename "$BASE_FILE")"

    [[ -n "$OUT" ]] || OUT="$REPO/build/musallahboard-$AGENT_VER-arm64.img.xz"
    mkdir -p "$(dirname "$OUT")"
    IMG="${OUT%.xz}"
    [[ "$IMG" == *.img ]] || IMG="$IMG.img"
}

# ── Partitions ────────────────────────────────────────────────────────────────
# part N: "START SIZE" in bytes, from the image's partition table.
part() {
    parted -sm "$IMG" unit B print | awk -F: -v n="$1" '$1 == n { sub(/B$/, "", $2); sub(/B$/, "", $4); print $2, $4 }'
}

prepare_image() {
    section "Image"
    info "Unpacking into $IMG"
    if [[ "$BASE_FILE" == *.xz ]]; then
        xz -dc -T0 "$BASE_FILE" > "$IMG"
    else
        cp --sparse=always "$BASE_FILE" "$IMG"
    fi

    # Room for the kiosk's packages. Raspberry Pi OS grows the root partition
    # to the whole card at first boot (rpi-resize), so this is only what the
    # build needs, not what the board gets.
    truncate -s "+$GROW" "$IMG"
    parted -s "$IMG" resizepart 2 100%

    local start size
    read -r start size < <(part 2)
    local loop
    loop="$(losetup -f --show -o "$start" --sizelimit "$size" "$IMG")"
    LOOPS+=("$loop")
    e2fsck -pf "$loop" >/dev/null || [[ $? -le 1 ]] || die "e2fsck failed on the root filesystem"
    resize2fs "$loop" >/dev/null
    ROOT_LOOP="$loop"
    info "Root filesystem grown by $GROW"
}

mount_image() {
    section "Mounting"
    ROOT="$WORK/root"
    mkdir -p "$ROOT"
    mount_on "$ROOT_LOOP" "$ROOT"

    # The boot partition: mounted if this kernel has vfat, otherwise copied out
    # with mtools and back in afterwards (containers often lack vfat).
    local start size loop
    read -r start size < <(part 1)
    loop="$(losetup -f --show -o "$start" --sizelimit "$size" "$IMG")"
    LOOPS+=("$loop")
    BOOT_OFFSET="$start"
    if mount -t vfat "$loop" "$ROOT/boot/firmware" 2>/dev/null; then
        MOUNTS+=("$ROOT/boot/firmware")
        info "Boot partition mounted"
    else
        command -v mcopy >/dev/null || die "This kernel cannot mount vfat, and mtools is not installed"
        BOOT_FALLBACK="$WORK/bootfs"
        rm -rf "$BOOT_FALLBACK"
        mkdir -p "$BOOT_FALLBACK"
        mcopy -s -p -m -i "$IMG@@$BOOT_OFFSET" ::/ "$BOOT_FALLBACK/"
        # mcopy -s ::/ copies into a directory of that name on some versions.
        if [[ ! -f "$BOOT_FALLBACK/config.txt" && -d "$BOOT_FALLBACK/firmware" ]]; then
            BOOT_FALLBACK="$BOOT_FALLBACK/firmware"
        fi
        [[ -f "$BOOT_FALLBACK/config.txt" ]] || die "Could not copy the boot partition out with mtools"
        (cd "$BOOT_FALLBACK" && find . -type f -exec sha256sum {} + | LC_ALL=C sort) > "$WORK/bootfs.before"
        mount_on --bind "$BOOT_FALLBACK" "$ROOT/boot/firmware"
        info "Boot partition copied out with mtools (this kernel cannot mount vfat)"
    fi
    [[ -f "$ROOT/boot/firmware/config.txt" ]] || die "No config.txt on the boot partition: is this Raspberry Pi OS?"

    mount_on -t proc proc "$ROOT/proc"
    mount_on -t sysfs sysfs "$ROOT/sys"
    mount_on --bind /dev "$ROOT/dev"
    mount_on --bind /dev/pts "$ROOT/dev/pts"
    mount_on -t tmpfs tmpfs "$ROOT/run"
    mount_on -t tmpfs tmpfs "$ROOT/tmp"

    # Name resolution for apt, and no services started by package scripts:
    # both undone before the image is closed.
    if [[ -e "$ROOT/etc/resolv.conf" || -L "$ROOT/etc/resolv.conf" ]]; then
        mv "$ROOT/etc/resolv.conf" "$WORK/resolv.conf.image"
    fi
    cp -L /etc/resolv.conf "$ROOT/etc/resolv.conf"
    printf '#!/bin/sh\nexit 101\n' > "$ROOT/usr/sbin/policy-rc.d"
    chmod 0755 "$ROOT/usr/sbin/policy-rc.d"

    chroot "$ROOT" /bin/true || die "Cannot run the image's programs here (arm64 emulation?)"
}

# in_root CMD...: run in the image, with a clean environment. The host's
# proxy settings go through, so apt in the image works behind a proxy (or an
# apt-cacher-ng: http_proxy=http://<host>:3142). Run with `sudo -E` (or set
# them after sudo) for them to reach this script.
in_root() {
    local proxy=() v
    for v in http_proxy https_proxy no_proxy HTTP_PROXY HTTPS_PROXY NO_PROXY; do
        [[ -n "${!v-}" ]] && proxy+=("$v=${!v}")
    done
    chroot "$ROOT" /usr/bin/env -i \
        PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
        HOME=/root TERM=dumb LANG=C.UTF-8 DEBIAN_FRONTEND=noninteractive \
        "${proxy[@]}" \
        "$@"
}

# ── setup.sh --image ──────────────────────────────────────────────────────────
provision() {
    section "setup.sh --image"
    local stage="$ROOT/tmp/mbimage"
    mkdir -p "$stage"
    install -m 0755 "$SETUP_SH" "$stage/setup.sh"
    install -m 0755 "$AGENT" "$stage/musallahboard-agent"
    local app_env=()
    if [[ -n "$APP" ]]; then
        install -m 0644 "$APP" "$stage/$(basename "$APP")"
        app_env=("MB_APP_PACKAGE=/tmp/mbimage/$(basename "$APP")")
    fi
    in_root MB_AGENT_BINARY=/tmp/mbimage/musallahboard-agent "${app_env[@]}" \
        bash /tmp/mbimage/setup.sh --image
}

# ── Checks ────────────────────────────────────────────────────────────────────
# What a flashed card must have. Any failure fails the build: an image that
# boots to a broken kiosk is worse than no image.
FAILED=0
check() {
    local what="$1"; shift
    if "$@" >/dev/null 2>&1; then
        echo "  ok    $what"
    else
        echo "  FAIL  $what"
        FAILED=1
    fi
}
enabled()  { [[ "$(in_root systemctl is-enabled "$1" 2>/dev/null)" == enabled ]]; }
masked()   { [[ "$(in_root systemctl is-enabled "$1" 2>/dev/null)" == masked ]]; }
has_pkg()  { in_root dpkg-query -W -f='${Status}' "$1" 2>/dev/null | grep -q 'install ok installed'; }
no_glob()  { ! compgen -G "$1" >/dev/null; }
user_has() { in_root id -nG "$1" | tr ' ' '\n' | grep -qx "$2"; }

verify_image() {
    section "Checks"
    check "agent $AGENT_VER installed" \
        bash -c "[[ \"\$(chroot '$ROOT' /usr/bin/musallahboard-agent version)\" == *' $AGENT_VER' ]]"
    local u
    for u in musallahboard-agent.service musallahboard-kiosk.service \
             musallahboard-agent-update.path musallahboard-firstboot.service; do
        check "$u enabled" enabled "$u"
    done
    check "userconfig.service masked (no wizard over the kiosk)" masked userconfig.service
    check "boots to multi-user.target" bash -c "[[ \"\$(chroot '$ROOT' systemctl get-default)\" == multi-user.target ]]"
    for u in cage chromium ufw unattended-upgrades dnsmasq-base util-linux-extra; do
        check "package $u" has_pkg "$u"
    done
    check "account musallahdaemon" in_root id musallahdaemon
    check "account musallahkiosk" in_root id musallahkiosk
    check "musallahkiosk has no sudo" bash -c "! chroot '$ROOT' id -nG musallahkiosk | tr ' ' '\n' | grep -qx sudo"
    check "musallahkiosk has video" user_has musallahkiosk video
    check "trust.json root 0644" bash -c "[[ \"\$(stat -c '%U %a' '$ROOT/etc/musallahboard/trust.json')\" == 'root 644' ]]"
    check "no content key pinned (each board gets its own)" \
        bash -c "chroot '$ROOT' /usr/bin/musallahboard-agent trust show | grep -q '^  none\\.'"
    check "unprovisioned: no agent.toml" test ! -e "$ROOT/etc/musallahboard/agent.toml"
    check "no device key" test ! -e "$ROOT/etc/musallahboard/agent.key"
    check "kiosk launcher" test -x "$ROOT/usr/bin/start-kiosk.sh"
    check "first-boot script" test -x "$ROOT/usr/lib/musallahboard/setup.sh"
    check "first boot not yet done" test ! -e "$ROOT/usr/lib/musallahboard/firstboot.done"
    check "musallahboard-example.txt on the boot partition" test -f "$ROOT/boot/firmware/musallahboard-example.txt"
    check "no SSH host keys (made at first boot)" no_glob "$ROOT/etc/ssh/ssh_host_*"
    check "machine-id uninitialized (first boot detection)" \
        bash -c "[[ \"\$(cat '$ROOT/etc/machine-id')\" == uninitialized ]]"
    check "root filesystem still grows at first boot" grep -qw resize "$ROOT/boot/firmware/cmdline.txt"
    if [[ -n "$APP" ]]; then
        check "board app queued in the inbox" test -f "$ROOT/var/lib/musallahboard/inbox/$(basename "$APP")"
    fi
    [[ "$FAILED" == 0 ]] || die "The image failed its checks"
}

# ── Close ─────────────────────────────────────────────────────────────────────
scrub() {
    section "Cleaning up the image"
    in_root apt-get clean
    rm -rf "$ROOT"/var/lib/apt/lists/* "$ROOT"/var/cache/debconf/*-old "$ROOT"/var/lib/dpkg/*-old
    rm -f "$ROOT/usr/sbin/policy-rc.d" "$ROOT"/etc/ssh/ssh_host_* "$ROOT/root/.bash_history"
    rm -f "$ROOT/etc/resolv.conf"
    if [[ -e "$WORK/resolv.conf.image" || -L "$WORK/resolv.conf.image" ]]; then
        mv "$WORK/resolv.conf.image" "$ROOT/etc/resolv.conf"
    fi
    # First-boot detection (ConditionFirstBoot) depends on it.
    echo uninitialized > "$ROOT/etc/machine-id"
    rm -f "$ROOT/var/lib/dbus/machine-id"
    # Logs of the build, not of the board.
    find "$ROOT/var/log" -type f \( -name '*.log' -o -name '*.gz' -o -name '*.[0-9]' \) -delete
    find "$ROOT/var/log" -type f -exec truncate -s 0 {} +
    rm -rf "$ROOT/var/log/journal/"*
    info "apt lists and caches, build logs, host keys and resolv.conf removed"
}

close_image() {
    section "Closing"
    if [[ -n "$BOOT_FALLBACK" ]]; then
        # Write back what the build changed on the boot partition.
        (cd "$BOOT_FALLBACK" && find . -type f -exec sha256sum {} + | LC_ALL=C sort) > "$WORK/bootfs.after"
        local f
        while read -r f; do
            f="${f#./}"
            [[ "$f" == */* ]] && mmd -D s -i "$IMG@@$BOOT_OFFSET" "::/$(dirname "$f")" 2>/dev/null || true
            mcopy -o -p -m -i "$IMG@@$BOOT_OFFSET" "$BOOT_FALLBACK/$f" "::/$f"
            info "boot: wrote $f"
        done < <(LC_ALL=C comm -13 "$WORK/bootfs.before" "$WORK/bootfs.after" | awk '{print $2}')
        while read -r f; do
            f="${f#./}"
            [[ -e "$BOOT_FALLBACK/$f" ]] && continue
            mdel -i "$IMG@@$BOOT_OFFSET" "::/$f"
            info "boot: removed $f"
        done < <(LC_ALL=C comm -23 "$WORK/bootfs.before" "$WORK/bootfs.after" | awk '{print $2}')
    fi
    sync
    cleanup

    # Zero the free blocks, so they compress to nothing.
    local start size loop
    read -r start size < <(part 2)
    loop="$(losetup -f --show -o "$start" --sizelimit "$size" "$IMG")"
    LOOPS+=("$loop")
    e2fsck -pf "$loop" >/dev/null || [[ $? -le 1 ]] || die "e2fsck failed after the build"
    zerofree "$loop"
    cleanup

    section "Compressing"
    rm -f "$OUT"
    if [[ "$KEEP_IMG" == 1 ]]; then
        xz -T0 -6 -k -c "$IMG" > "$OUT"
    else
        xz -T0 -6 -c "$IMG" > "$OUT"
        rm -f "$IMG"
    fi
    (cd "$(dirname "$OUT")" && sha256sum "$(basename "$OUT")" > "$(basename "$OUT").sha256")
}

main() {
    preflight "$@"
    inputs
    prepare_image
    mount_image
    provision
    scrub
    verify_image
    close_image
    echo
    info "Wrote $OUT ($(du -h "$OUT" | cut -f1)) and $(basename "$OUT").sha256"
    info "Agent $AGENT_VER${APP:+, board app $(basename "$APP")}, on $(basename "$BASE_FILE")"
    info "Flash it with Raspberry Pi Imager (see the README, 'Flash a pre-built image')."
}

main "$@"

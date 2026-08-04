#!/bin/bash
# =====================================================
# packaging/luks/install.sh — OTP-keyed root encryption, on a running board
# =====================================================
# Turns a booted Raspberry Pi OS install into one that encrypts its own root
# filesystem on the next boot, keyed to a secret burned into this specific SoC's
# one-time-programmable memory.
#
# Nothing is encrypted HERE. This script installs the tooling, proves the
# initramfs can actually do the job, and only then points the boot files at the
# encrypted device. The conversion itself happens on the next boot, in the
# initramfs, where the root filesystem is not mounted. See initramfs-local-top.
#
#   --no-encrypt     install the tooling but leave the boot files alone. The
#                    hook is inert. Safe, and re-runnable.
#   --dry-run-otp    rehearse the entire conversion on this board against a file
#                    that stands in for OTP. BURNS NOTHING. The resulting board
#                    is encrypted but NOT protected — see the warning it prints.
#   --yes            skip the confirmation prompt (fleet provisioning).
#
# Called from setup.sh's platform_after_hardening, or by hand.
# =====================================================
set -euo pipefail

SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

MB_ENCRYPT=1
DRY_RUN_OTP=0
ASSUME_YES=0

MAPPER_NAME="musallahroot"
MAPPER="/dev/mapper/${MAPPER_NAME}"
FW=/boot/firmware
CMDLINE="${FW}/cmdline.txt"
CONFIG="${FW}/config.txt"
DRY_RUN_KEY=/etc/musallahboard/otp-dry-run.key

for arg in "$@"; do
    case "$arg" in
        --encrypt)      MB_ENCRYPT=1 ;;
        --no-encrypt)   MB_ENCRYPT=0 ;;
        --dry-run-otp)  DRY_RUN_OTP=1 ;;
        --yes|-y)       ASSUME_YES=1 ;;
        *) echo "FATAL: unknown argument: $arg"; exit 1 ;;
    esac
done

die() { echo "FATAL: $*" >&2; exit 1; }

# ── Preflight ────────────────────────────────────────────────────────────────
[ "$(id -u)" -eq 0 ] || die "must run as root"
[ -d "$SRC" ] || die "$SRC missing"

# Uploaded from a Windows checkout by way of a release tarball. The three
# payload files are extensionless, so they rely on .gitattributes rather than
# the *.sh rule — strip CRs anyway rather than trust that. A CR in the shebang
# of the local-top script makes the initramfs look for "/bin/sh\r".
sed -i 's/\r$//' "${SRC}/musallahboard-otp-key" "${SRC}/initramfs-hook" "${SRC}/initramfs-local-top"

[ "$(uname -m)" = "aarch64" ] || die "this is a 64-bit arm64 image only (got $(uname -m))"
[ -f "$CMDLINE" ] || die "${CMDLINE} missing — is this Raspberry Pi OS?"
[ -f "$CONFIG" ]  || die "${CONFIG} missing — is this Raspberry Pi OS?"

# ── Which SoC ────────────────────────────────────────────────────────────────
# The device tree is the authority. /proc/cpuinfo's Revision field would work
# too but needs a lookup table that has to be maintained; `compatible` names the
# chip directly.
COMPAT="$(tr -d '\0' < /proc/device-tree/compatible 2>/dev/null || true)"
MODEL="$(tr -d '\0' < /proc/device-tree/model 2>/dev/null || echo unknown)"

echo "═══ board ═══"
echo "  ${MODEL}"

# ── Cipher ───────────────────────────────────────────────────────────────────
# BCM2711 — the Pi 4B's SoC — does not implement the ARMv8 cryptography
# extensions. There is no AES instruction on a Cortex-A72, so aes-xts-plain64
# there runs entirely in software and competes with Chromium for the same four
# cores. BCM2712 (Cortex-A76, Pi 5) does have them and AES is the right choice.
#
# Adiantum is what exists for this case: a length-preserving construction built
# on XChaCha12 and NH, designed for exactly the CPUs that have no AES
# acceleration, and what Android uses on low-end hardware for the same reason.
#
# This is not our opinion. It is the same split Raspberry Pi's own rpi-idp-luks2
# layer encodes.
#
# Unlike the image build, which has to decide this months before the board sees
# it, here the SoC is sitting right there and can simply be asked.
case "$COMPAT" in
    *bcm2712*)
        LUKS_CIPHER="aes-xts-plain64"
        LUKS_KEYSIZE=512
        CIPHER_WHY="BCM2712 has the ARMv8 crypto extensions"
        ;;
    *bcm2711*)
        LUKS_CIPHER="xchacha12,aes-adiantum-plain64"
        LUKS_KEYSIZE=256
        CIPHER_WHY="BCM2711 has no AES instruction — software AES would cost the CPU"
        ;;
    *)
        # Refusing rather than defaulting. Defaulting to AES on an unknown SoC
        # silently ships a slow board; defaulting to Adiantum on one that has
        # AES silently ships a slower-than-necessary one. Both are the kind of
        # thing nobody notices for a year.
        echo "FATAL: unrecognised SoC."
        echo "       device-tree compatible: ${COMPAT:-(none)}"
        echo "       Supported: Raspberry Pi 4 / CM4 (BCM2711) and Pi 5 / CM5 (BCM2712)."
        echo "       Add the SoC to the cipher table in this script rather than"
        echo "       picking a cipher for it by hand."
        exit 1
        ;;
esac

# ── Where is root, and is there anything to do? ──────────────────────────────
ROOT_SRC="$(findmnt -no SOURCE / )"
ROOT_FSTYPE="$(findmnt -no FSTYPE / )"

if [ "$ROOT_SRC" = "$MAPPER" ]; then
    echo "═══ already encrypted ═══"
    echo "  / is ${MAPPER}. Nothing to do."
    exit 0
fi

[ "$ROOT_FSTYPE" = "ext4" ] \
    || die "root filesystem is ${ROOT_FSTYPE}, and the conversion is ext4-only (it uses resize2fs)"

# PARTUUID, not UUID or LABEL. It lives in the partition table, which the
# conversion never touches, so it resolves identically before and after. A
# filesystem UUID moves inside the container the moment the board is encrypted
# and would resolve exactly once — see the note in initramfs-local-top.
ROOT_PARTUUID="$(blkid -s PARTUUID -o value "$ROOT_SRC" 2>/dev/null || true)"
[ -n "$ROOT_PARTUUID" ] \
    || die "${ROOT_SRC} has no PARTUUID — cannot describe it in a way that survives encryption"

echo "═══ root filesystem ═══"
echo "  device   : ${ROOT_SRC}"
echo "  PARTUUID : ${ROOT_PARTUUID}"
echo "  size     : $(( $(blockdev --getsize64 "$ROOT_SRC") / 1024 / 1024 )) MiB"

# ── Packages ─────────────────────────────────────────────────────────────────
# rpi-eeprom ships /usr/bin/rpi-otp-private-key and raspi-utils-core ships
# /usr/bin/vcmailbox. Between them they are the entire path to the OTP secret
# the disk key is derived from.
#
# Most documentation, including Raspberry Pi's own, still says vcmailbox comes
# from libraspberrypi-bin. That package does not exist for arm64 trixie at all —
# the utilities were split into raspi-utils-* — and naming it here is not a
# harmless extra: apt fails with "no installation candidate".
#
# DEBIAN_FRONTEND is not decoration: installing initramfs-tools on a system that
# does not already have it opens a debconf prompt about initramfs.conf, and this
# script is routinely run down a pipe where there is nobody to answer it.
echo "═══ packages ═══"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq --no-install-recommends \
    cryptsetup \
    cryptsetup-bin \
    fdisk \
    e2fsprogs \
    initramfs-tools \
    rpi-eeprom \
    raspi-utils-core

for t in cryptsetup sfdisk resize2fs tune2fs e2fsck blkid vcmailbox rpi-otp-private-key; do
    command -v "$t" >/dev/null || die "${t} missing after package install"
    echo "  $(command -v "$t")"
done
echo "  $(cryptsetup --version)"

# ── Key derivation ───────────────────────────────────────────────────────────
echo "═══ installing key derivation ═══"
install -o root -g root -m 0755 "${SRC}/musallahboard-otp-key" \
    /usr/local/sbin/musallahboard-otp-key
sh -n /usr/local/sbin/musallahboard-otp-key
echo "  /usr/local/sbin/musallahboard-otp-key"

install -d -m 0755 /etc/musallahboard
cat > /etc/musallahboard/luks.conf <<EOF
# Written by packaging/luks/install.sh on this board. Read by the initramfs
# local-top script when it converts this board's root filesystem, and by
# nothing afterwards — the LUKS2 header records its own cipher, so an already
# converted board does not consult this file to unlock.
#
# ${CIPHER_WHY}
MB_LUKS_CIPHER=${LUKS_CIPHER}
MB_LUKS_KEYSIZE=${LUKS_KEYSIZE}
EOF
chmod 0644 /etc/musallahboard/luks.conf

echo "═══ cipher ═══"
echo "  ${LUKS_CIPHER} / ${LUKS_KEYSIZE}-bit"
echo "  ${CIPHER_WHY}"

# ── OTP, or a stand-in for it ────────────────────────────────────────────────
rm -f "$DRY_RUN_KEY"
if [ "$DRY_RUN_OTP" = "1" ]; then
    echo "═══ REHEARSAL KEY ═══"
    # Generated here rather than on the board, because --ensure would otherwise
    # have to write it into a read-only initramfs.
    od -An -N32 -tx1 /dev/urandom | tr -d ' \n' > "$DRY_RUN_KEY"
    chmod 0600 "$DRY_RUN_KEY"
    echo "  ${DRY_RUN_KEY} — this board's OTP will NOT be touched"
else
    # Report the OTP state without changing it. A board whose OTP is already
    # programmed is fine — it is either a re-run or a board that has been
    # through this before, and the same key derives from it either way.
    echo "═══ OTP ═══"
    if OTP_STATE="$(musallahboard-otp-key --status 2>/dev/null)"; then
        echo "  ${OTP_STATE} — the existing device key will be reused"
    else
        echo "  blank — a device key will be generated and burned on the next boot"
    fi
fi

# ── Initramfs integration ────────────────────────────────────────────────────
echo "═══ installing initramfs integration ═══"
install -d -m 0755 /etc/initramfs-tools/hooks /etc/initramfs-tools/scripts/local-top
install -o root -g root -m 0755 "${SRC}/initramfs-hook" \
    /etc/initramfs-tools/hooks/musallahboard-luks
install -o root -g root -m 0755 "${SRC}/initramfs-local-top" \
    /etc/initramfs-tools/scripts/local-top/musallahboard-luks
sh -n /etc/initramfs-tools/hooks/musallahboard-luks
sh -n /etc/initramfs-tools/scripts/local-top/musallahboard-luks
echo "  hook + local-top installed, syntax OK"

# ── auto_initramfs ───────────────────────────────────────────────────────────
# Without this the firmware loads the kernel directly and the local-top script
# never runs — which, once the boot files point at the mapper, is a board that
# does not boot.
#
# auto_initramfs rather than a hardcoded `initramfs initramfs8 followkernel`
# line: it makes the firmware pick up whatever update-initramfs regenerated
# after a kernel upgrade. The hardcoded form goes stale the first time apt
# installs a new kernel.
#
# config.txt is sectioned ([pi4], [pi5], [all], ...) and a bare append lands in
# whichever section happens to be last — which on some templates is a
# model-specific one, so the setting silently applies to one board and not the
# other. Ours goes into an explicit [all] block.
echo "═══ auto_initramfs ═══"
if grep -qE '^[[:space:]]*auto_initramfs=1' "$CONFIG"; then
    echo "  already set"
else
    cp -a "$CONFIG" "${CONFIG}.mb-orig"
    cat >> "$CONFIG" <<'EOF'

[all]
# ── MusallahBoard ──
# The root filesystem is unlocked by a script in the initramfs. Without this
# the firmware boots the kernel directly and that script never runs.
auto_initramfs=1
EOF
    echo "  added (original saved as ${CONFIG}.mb-orig)"
fi

# ── Which kernels ────────────────────────────────────────────────────────────
KERNELS=()
for d in /lib/modules/*/; do
    v="$(basename "$d")"
    # Skip module trees with no kernel image to match; update-initramfs fails on
    # those and would take the whole run down with it.
    [ -d "${d}kernel" ] || continue
    KERNELS+=("$v")
done
[ "${#KERNELS[@]}" -gt 0 ] || die "no kernel module trees under /lib/modules"

# ── Can this kernel actually do the chosen cipher? ───────────────────────────
# Checked BEFORE building anything, because it is the cheapest possible place to
# find out, and because the failure it prevents is the most expensive one
# available: a kernel with no Adiantum produces an initramfs that looks
# complete, and the Pi 4 that boots it reaches the reencryption, BURNS ITS OTP,
# and only then discovers it cannot create the container. That burn is
# permanent.
#
# Checked against modules.builtin as well as the module tree, because a cipher
# compiled into the kernel is legitimately absent from both /lib/modules and the
# initramfs. Treating that as a failure would block a perfectly good board.
#
# Each entry is a set of acceptable names separated by |. Module naming moves
# between kernel versions and arm64 ships accelerated variants under their own
# names, so a single spelling is not something to bet a fleet's boot path on.
case "$LUKS_CIPHER" in
    aes-xts-plain64)
        CRYPTO_REQ=(
            "xts"
            "aes|aes_generic|aes-ce-blk|aes-neon-blk|aes-neon-bs|aes-arm64"
        )
        ;;
    xchacha12,aes-adiantum-plain64)
        CRYPTO_REQ=(
            "adiantum"
            "nhpoly1305|nhpoly1305-neon"
            "chacha|chacha_generic|chacha-neon|chacha-neon-arm64|libchacha"
            "poly1305|poly1305_generic|poly1305-neon"
            "aes|aes_generic|aes-ce-blk|aes-neon-blk|aes-neon-bs|aes-arm64"
        )
        ;;
esac

MODWANT=()
FAILED=0

echo "═══ kernel support for ${LUKS_CIPHER} ═══"
for v in "${KERNELS[@]}"; do
    BUILTIN_LIST="/lib/modules/${v}/modules.builtin"
    for req in "${CRYPTO_REQ[@]}"; do
        found=""
        kind=""
        IFS='|' read -ra alts <<< "$req"
        for m in "${alts[@]}"; do
            if grep -qE "/${m}\.ko" "$BUILTIN_LIST" 2>/dev/null; then
                found="$m"; kind="builtin"; break
            fi
            if find "/lib/modules/${v}" -name "${m}.ko*" -print -quit 2>/dev/null | grep -q .; then
                found="$m"; kind="module"; break
            fi
        done

        case "$kind" in
            builtin)
                printf '  \033[0;32mok\033[0m    %s (built into %s)\n' "$found" "$v"
                ;;
            module)
                # Loadable: must be pulled into the initramfs, because the root
                # filesystem it would otherwise load from is the thing being
                # encrypted.
                MODWANT+=("/${found}.ko")
                printf '  \033[0;32mok\033[0m    %s (module in %s)\n' "$found" "$v"
                ;;
            *)
                printf '  \033[0;31mFAIL\033[0m  none of {%s} in %s\n' "${req//|/, }" "$v"
                FAILED=1
                ;;
        esac
    done
done
[ "$FAILED" -eq 0 ] || {
    echo "FATAL: this kernel cannot do ${LUKS_CIPHER}."
    echo "       For this SoC that cipher is not a preference — see the cipher"
    echo "       note in this script. Do not switch to aes-xts-plain64 to get"
    echo "       past this without reading it first."
    exit 1
}

# ── Build ────────────────────────────────────────────────────────────────────
echo "═══ building initramfs ═══"
STARTED_AT="$(date +%s)"
for v in "${KERNELS[@]}"; do
    echo "  ── ${v} ──"
    update-initramfs -u -k "$v" 2>&1 | sed 's/^/    /' \
        || update-initramfs -c -k "$v" 2>&1 | sed 's/^/    /' \
        || die "update-initramfs failed for ${v}"
done

# ── Assert the initramfs actually contains what it needs ─────────────────────
# The hook exits non-zero on a missing tool, which fails update-initramfs — but
# only if the hook ran at all. This proves the result, not the intent. A board
# that boots an initramfs without cryptsetup skips encryption silently and
# reports success, which is the worst failure mode available here.
echo "═══ verifying initramfs contents ═══"

WANT=(
    sbin/cryptsetup
    sfdisk
    musallahboard-otp-key
    rpi-otp-private-key
    vcmailbox
    sha256sum
    resize2fs
    blkid
    scripts/local-top/musallahboard-luks
    musallahboard/luks.conf
)
WANT+=("${MODWANT[@]}")
[ "$DRY_RUN_OTP" = "1" ] && WANT+=(musallahboard/otp-dry-run.key)

for v in "${KERNELS[@]}"; do
    IMG="/boot/initrd.img-${v}"
    [ -f "$IMG" ] || { echo "  FAIL: ${IMG} not produced"; FAILED=1; continue; }

    CONTENTS="$(lsinitramfs "$IMG" 2>/dev/null)"
    for want in "${WANT[@]}"; do
        if printf '%s\n' "$CONTENTS" | grep -q -- "$want"; then
            printf '  \033[0;32mok\033[0m    %s in initrd.img-%s\n' "$want" "$v"
        else
            printf '  \033[0;31mFAIL\033[0m  %s MISSING from initrd.img-%s\n' "$want" "$v"
            FAILED=1
        fi
    done
done
[ "$FAILED" -eq 0 ] || {
    echo "FATAL: the initramfs is missing required components."
    echo "       A missing .ko here means the module exists in /lib/modules but"
    echo "       did not get pulled into the initramfs — which for a crypto"
    echo "       module means the board could not create or open its own root."
    exit 1
}

# ── Will the firmware actually load it? ──────────────────────────────────────
# This is the check that stands between a provisioned board and an unbootable
# one. With auto_initramfs=1 the firmware derives the initramfs filename from
# the kernel it loads: kernel8.img wants initramfs8, kernel_2712.img wants
# initramfs_2712, in the same directory. So the question is answerable exactly,
# by name, without knowing anything about which distribution hook was supposed
# to have done the copying.
echo "═══ boot partition ═══"
firmware_initramfs_for() {
    # kernel8.img -> initramfs8 ; kernel_2712.img -> initramfs_2712
    _base="$(basename "$1")"
    _suffix="${_base#kernel}"
    _suffix="${_suffix%.img}"
    printf '%s/initramfs%s' "$FW" "$_suffix"
}

# Match a kernel image to the module tree it belongs to, so the right initrd
# gets copied if we end up having to do it ourselves.
initrd_for_suffix() {
    case "$1" in
        *2712*) _pat="2712" ;;
        *)      _pat="v8" ;;
    esac
    for _v in "${KERNELS[@]}"; do
        case "$_v" in
            *"$_pat"*) [ -f "/boot/initrd.img-${_v}" ] && { printf '/boot/initrd.img-%s' "$_v"; return 0; } ;;
        esac
    done
    return 1
}

MISSING_FW=0
shopt -s nullglob
KERNEL_IMGS=("${FW}"/kernel*.img)
shopt -u nullglob
[ "${#KERNEL_IMGS[@]}" -gt 0 ] || die "no kernel*.img in ${FW} — cannot tell what the firmware will load"

for kimg in "${KERNEL_IMGS[@]}"; do
    want_fw="$(firmware_initramfs_for "$kimg")"
    if [ -f "$want_fw" ] && [ "$(stat -c %Y "$want_fw")" -ge "$STARTED_AT" ]; then
        printf '  \033[0;32mok\033[0m    %s -> %s (fresh)\n' "$(basename "$kimg")" "$(basename "$want_fw")"
        continue
    fi

    # Nothing copied it, or what is there is stale. Do it here, and install a
    # post-update hook so the next kernel upgrade does not silently undo it.
    suffix="$(basename "$want_fw")"
    if src_initrd="$(initrd_for_suffix "$suffix")"; then
        cp -f "$src_initrd" "$want_fw"
        printf '  \033[0;33mcopied\033[0m %s -> %s\n' "$(basename "$src_initrd")" "$(basename "$want_fw")"
    else
        printf '  \033[0;31mFAIL\033[0m  no initrd matches %s\n' "$(basename "$want_fw")"
        MISSING_FW=1
    fi
done

if [ "$MISSING_FW" = "0" ] && [ ! -f /etc/initramfs/post-update.d/zz-musallahboard ]; then
    # Only installed when the above had to intervene is tempting, but a hook
    # that is idempotent and correct costs nothing and covers the case where
    # this board's distribution hook disappears in a later upgrade.
    install -d -m 0755 /etc/initramfs/post-update.d
    cat > /etc/initramfs/post-update.d/zz-musallahboard <<'EOF'
#!/bin/sh
# Copy a regenerated initramfs to the boot partition under the name
# auto_initramfs=1 expects. Raspberry Pi OS normally does this itself; this runs
# after whatever did, writes the same bytes, and exists so that a board whose
# root filesystem is encrypted cannot be left with a stale initramfs it needs to
# boot. Idempotent by construction.
set -e
version="$1"
initrd="$2"
[ -n "$version" ] && [ -r "$initrd" ] || exit 0

case "$version" in
    *2712*) suffix="_2712" ;;
    *v8*)   suffix="8" ;;
    *)      exit 0 ;;
esac

target="/boot/firmware/initramfs${suffix}"
[ -d /boot/firmware ] || exit 0
cp -f "$initrd" "$target"
sync
EOF
    chmod 0755 /etc/initramfs/post-update.d/zz-musallahboard
    echo "  installed /etc/initramfs/post-update.d/zz-musallahboard"
fi

[ "$MISSING_FW" -eq 0 ] || {
    echo "FATAL: the firmware would not find an initramfs to load."
    echo "       Refusing to point the boot files at an encrypted device that"
    echo "       nothing would be able to unlock. Nothing has been changed."
    exit 1
}

sync

# ── Everything below this line changes how the board boots ───────────────────
if [ "$MB_ENCRYPT" != "1" ]; then
    echo
    echo "═══ --no-encrypt ═══"
    echo "  Tooling installed and verified; boot files untouched."
    echo "  This board will boot exactly as it does now."
    echo "  Re-run without --no-encrypt to arm the conversion."
    exit 0
fi

# ── Confirmation ─────────────────────────────────────────────────────────────
# Read from /dev/tty, not stdin. The documented way to run the installer is
# `curl … | sudo bash`, where stdin is the script itself — a `read` there
# consumes the rest of the script rather than an answer from a person.
confirm() {
    [ "$ASSUME_YES" = "1" ] && return 0

    if [ ! -r /dev/tty ]; then
        echo "FATAL: no terminal to ask for confirmation on."
        echo "       Re-run with --yes if you mean it."
        exit 1
    fi

    echo
    echo "  ╔══════════════════════════════════════════════════════════════╗"
    if [ "$DRY_RUN_OTP" = "1" ]; then
        echo "  ║  REHEARSAL — nothing permanent happens                       ║"
        echo "  ╚══════════════════════════════════════════════════════════════╝"
        echo
        echo "  The next boot will encrypt this board's root filesystem using a"
        echo "  key from a FILE, not from this SoC's OTP. The OTP is not touched."
        echo
        echo "  The resulting board is NOT protected: the key sits in an"
        echo "  unencrypted initramfs on the FAT boot partition. Reflash it"
        echo "  before it goes anywhere near a wall."
    else
        echo "  ║  THIS IS PERMANENT                                           ║"
        echo "  ╚══════════════════════════════════════════════════════════════╝"
        echo
        echo "  On the next boot this board will:"
        echo
        echo "    1. Burn a random 256-bit key into its OTP memory."
        echo "       Those bits go from 0 to 1 and NEVER back. There is no undo,"
        echo "       no escrow, and no second attempt."
        echo "    2. Encrypt its root filesystem in place with that key."
        echo
        echo "  Afterwards:"
        echo "    • This card will only be readable by THIS board. Not by a"
        echo "      laptop, not by a card reader, not by another Pi."
        echo "    • If this board dies, its card is unrecoverable. Reflash and"
        echo "      re-enroll; nothing on it cannot be rebuilt from the backend."
        echo
        echo "  If you have not rehearsed this yet, answer no and re-run with"
        echo "  --dry-run-otp first. It exercises all of the above and burns"
        echo "  nothing."
    fi
    echo
    printf '  Type YES to continue: '
    read -r answer < /dev/tty
    [ "$answer" = "YES" ] || { echo "  Aborted. Nothing has been changed."; exit 1; }
}
confirm

# ── Point the boot files at the encrypted device ─────────────────────────────
# Done here, from a running system, where the result can be read back and
# checked — not from inside an initramfs on a board nobody is looking at. The
# originals go next to them: cmdline.txt lives on the FAT boot partition, so a
# board that will not boot is recoverable by editing one file on any machine
# with a card reader.
echo "═══ boot configuration ═══"

cp -a "$CMDLINE" "${CMDLINE}.mb-orig"
cp -a /etc/fstab /etc/fstab.mb-orig

# cmdline.txt must remain exactly one line: the kernel silently ignores
# everything after the first newline, so an editor that appends one drops every
# argument that follows it.
CUR="$(tr -d '\n' < "$CMDLINE")"

# Never leave a stale flag behind, and never leave two root= arguments — the
# kernel takes the last one, which would make a re-run's behaviour depend on
# argument order.
CUR="$(printf '%s\n' "$CUR" \
    | sed -e 's| *root=[^ ]*||g' \
          -e 's| *mb_encrypt=[^ ]*||g' \
          -e 's| *mb_rootdev=[^ ]*||g' \
          -e 's| *mb_otp_dryrun=[^ ]*||g' \
          -e 's|  *| |g' \
          -e 's|^ *||' -e 's| *$||')"

CUR="root=${MAPPER} ${CUR} mb_encrypt=1 mb_rootdev=PARTUUID=${ROOT_PARTUUID}"
[ "$DRY_RUN_OTP" = "1" ] && CUR="${CUR} mb_otp_dryrun=1"

# consoleblank=0 — the kernel blanks the framebuffer console after ten minutes.
# cage owns the display so this is mostly invisible, until cage crashes and the
# board shows a black screen instead of the console that would explain why.
case " $CUR " in
    *" consoleblank="*) ;;
    *) CUR="${CUR} consoleblank=0" ;;
esac

printf '%s\n' "$CUR" > "$CMDLINE"
echo "  ${CMDLINE}:"
sed 's/^/    /' "$CMDLINE"

# fstab: the root entry has to follow root= to the mapper. Matched on the mount
# point rather than on the device string, because what is in field 1 today
# depends on how this card was flashed.
awk -v mapper="$MAPPER" -v OFS='\t' '
    /^[[:space:]]*#/ { print; next }
    $2 == "/"        { $1 = mapper; print; next }
                     { print }
' /etc/fstab > /etc/fstab.mb-new
mv /etc/fstab.mb-new /etc/fstab

grep -qE "^${MAPPER}[[:space:]]" /etc/fstab \
    || die "fstab rewrite did not take — restore /etc/fstab.mb-orig and investigate"
echo "  /etc/fstab:"
grep -vE '^[[:space:]]*#' /etc/fstab | grep -vE '^[[:space:]]*$' | sed 's/^/    /'

sync

cat <<EOF

═══ armed ═══

  The next boot converts this board.

  cipher     : ${LUKS_CIPHER} (${LUKS_KEYSIZE}-bit)
  device     : ${ROOT_SRC} (PARTUUID=${ROOT_PARTUUID})
  becomes    : ${MAPPER}
$(if [ "$DRY_RUN_OTP" = "1" ]; then
echo "  key source : REHEARSAL FILE — the OTP is not touched"
else
echo "  key source : this SoC's OTP (burned on the next boot, permanently)"
fi)

  It will sit on the console saying DO NOT POWER OFF for a few minutes. If
  power is lost anyway, the boot after that resumes where it stopped.

  If this board does not come back up, put the card in any machine, open
  cmdline.txt on the FAT partition and replace its contents with
  cmdline.txt.mb-orig. That reverses everything this script changed about
  how the board boots.
EOF

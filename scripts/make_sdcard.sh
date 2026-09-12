#!/bin/bash
# ============================================================================
# make_sdcard.sh - assemble a dd-able sdcard.img (no root, no loop device)
# ============================================================================
# Inputs:
#   --boot <dir>      boot file directory (layout is auto-detected, see below)
#   --rootfs <file>   rootfs.ext4 (Buildroot or Debian output)
#   --out <file>      output image (default: sdcard.img)
#   --boot-size-mb N  FAT boot partition size in MB (default: 64)
#
# Two boot layouts are supported and detected from --boot:
#
#   FSBL layout (release layout) - marker: BOOT.BIN
#     BOOT.BIN        FSBL + FPGA bitstream + U-Boot (built with bootgen)
#     uImage          Linux kernel                         [required]
#     boot.scr        U-Boot script                        [required]
#     The FAT partition of a release image holds exactly these three files.
#     u-boot.img and zybo-audio.dtb belong to the SPL layout only; they are
#     accepted (and copied) when present, but they are not required here.
#
#   SPL layout - marker: boot.bin or spl.bin
#     boot.bin/spl.bin  U-Boot SPL, loaded directly by the BootROM [required]
#     u-boot.img        second stage U-Boot                        [required]
#     uImage            Linux kernel                               [required]
#     boot.scr          U-Boot script                              [required]
#     zybo-audio.dtb    device tree loaded from FAT                [required]
#
# Optional files copied when present in --boot: system.bit, uEnv.txt.
#
# Dependencies: parted, mkfs.vfat, mcopy (mtools), dd
#   (looked up in PATH, or shipped under ../.tools/bin)
# ============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
BOOT_DIR=""
ROOTFS=""
OUT="sdcard.img"
BOOT_MB=64

while [ $# -gt 0 ]; do
    case "$1" in
        --boot)         BOOT_DIR="$2"; shift 2 ;;
        --rootfs)       ROOTFS="$2"; shift 2 ;;
        --out)          OUT="$2"; shift 2 ;;
        --boot-size-mb) BOOT_MB="$2"; shift 2 ;;
        -h|--help)      awk 'NR==1{next} /^#/{print; next} {exit}' "$0"; exit 0 ;;
        *) echo "unknown argument: $1"; exit 1 ;;
    esac
done

die()  { echo "[ERROR] $*" >&2; exit 1; }
warn() { echo "[WARN]  $*" >&2; }
info() { echo "[SDCARD] $*"; }

find_tool() {
    local name="$1"
    if command -v "$name" >/dev/null 2>&1; then command -v "$name"; return; fi
    for d in "${SCRIPT_DIR}/../../.tools/bin" "${SCRIPT_DIR}/../.tools/bin" "${SCRIPT_DIR}/.tools/bin"; do
        [ -x "$d/$name" ] && { echo "$d/$name"; return; }
    done
    die "missing tool: $name (install parted/dosfstools/mtools, or place it in ../.tools/bin)"
}

PARTED=$(find_tool parted)
MKFS_VFAT=$(find_tool mkfs.vfat)
MCOPY=$(find_tool mcopy)
DD=$(find_tool dd)

[ -n "$BOOT_DIR" ] && [ -d "$BOOT_DIR" ] || die "missing --boot <dir>"
[ -n "$ROOTFS" ] && [ -f "$ROOTFS" ] || die "missing --rootfs <rootfs.ext4>"

# --- detect the boot layout -------------------------------------------------
# FSBL layout: BOOT.BIN (FSBL + bitstream + U-Boot from bootgen).
# SPL layout:  boot.bin / spl.bin (U-Boot SPL loaded directly by the BootROM).
# boot.bin / spl.bin win when both markers are present.
BOOTIMG=""
BOOT_FORM=""
if   [ -f "$BOOT_DIR/boot.bin" ]; then BOOTIMG="$BOOT_DIR/boot.bin"; BOOT_FORM="SPL"
elif [ -f "$BOOT_DIR/spl.bin" ];  then BOOTIMG="$BOOT_DIR/spl.bin";  BOOT_FORM="SPL"
elif [ -f "$BOOT_DIR/BOOT.BIN" ]; then BOOTIMG="$BOOT_DIR/BOOT.BIN"; BOOT_FORM="FSBL"
else
    die "cannot detect the boot layout in $BOOT_DIR: expected BOOT.BIN (FSBL layout) or boot.bin/spl.bin (SPL layout)"
fi
info "detected boot layout: $BOOT_FORM (bootloader: $(basename "$BOOTIMG"))"

# uImage + boot.scr are required by both layouts.
for f in uImage boot.scr; do
    [ -f "$BOOT_DIR/$f" ] || die "$BOOT_DIR/$f is missing: the $BOOT_FORM layout always needs uImage and boot.scr"
done

# SPL layout additionally needs the second stage U-Boot and the device tree.
if [ "$BOOT_FORM" = "SPL" ]; then
    for f in u-boot.img zybo-audio.dtb; do
        [ -f "$BOOT_DIR/$f" ] || die "$BOOT_DIR/$f is missing: the SPL layout (bootloader: $(basename "$BOOTIMG")) needs u-boot.img and zybo-audio.dtb on the FAT partition"
    done
else
    for f in u-boot.img zybo-audio.dtb; do
        [ -f "$BOOT_DIR/$f" ] && warn "FSBL layout: optional file $f found in $BOOT_DIR and will be copied to the FAT partition"
    done
fi

# Explicit file manifest: only recognised boot files reach the FAT partition.
FAT_FILES=("$BOOTIMG" "$BOOT_DIR/uImage" "$BOOT_DIR/boot.scr")
for f in u-boot.img zybo-audio.dtb system.bit uEnv.txt; do
    [ -f "$BOOT_DIR/$f" ] && FAT_FILES+=("$BOOT_DIR/$f")
done

# --- layout: p1 FAT32 at sector 2048, size BOOT_MB; p2 ext4 right after -----
BOOT_START=2048
ROOT_START=$(( BOOT_START + BOOT_MB * 2048 ))
ROOT_MB=$(( $(stat -c%s "$ROOTFS") / 1048576 + 32 ))
TOTAL_MB=$(( BOOT_MB + ROOT_MB + 1 ))
info "boot=${BOOT_MB}MB @sector${BOOT_START}, root=${ROOT_MB}MB @sector${ROOT_START}, total=${TOTAL_MB}MB -> $OUT"

rm -f "$OUT"
"$DD" if=/dev/zero of="$OUT" bs=1M count="$TOTAL_MB" status=none

"$PARTED" -s "$OUT" mklabel msdos \
    mkpart primary fat32 "${BOOT_START}s" "$((ROOT_START - 1))s" set 1 boot on \
    mkpart primary ext4 "${ROOT_START}s" 100%

# FAT32 boot partition: build a standalone boot.vfat first, then dd it into p1.
# (mkfs.vfat --offset is not used: it grows the host file unpredictably and
#  mcopy does not recognise the medium.)
BOOTV=$(mktemp -t bootvfat.XXXXXX.img)
trap 'rm -f "$BOOTV"' EXIT
"$DD" if=/dev/zero of="$BOOTV" bs=1M count="$BOOT_MB" status=none
"$MKFS_VFAT" -F 32 -n BOOT "$BOOTV" >/dev/null
for f in "${FAT_FILES[@]}"; do
    "$MCOPY" -o -i "$BOOTV" "$f" ::/
    info "  copying $(basename "$f")"
done
"$DD" if="$BOOTV" of="$OUT" bs=512 seek="$BOOT_START" conv=notrunc status=none
info "boot partition written at sector $BOOT_START"

# rootfs image written to the second partition
"$DD" if="$ROOTFS" of="$OUT" bs=512 seek="$ROOT_START" conv=notrunc status=none
info "rootfs written at sector $ROOT_START"

info "done: $OUT ($(stat -c%s "$OUT" | awk '{printf "%.0f", $1/1048576}') MB)"
info "burn with: sudo dd if=$OUT of=/dev/sdX bs=4M status=progress conv=fsync"

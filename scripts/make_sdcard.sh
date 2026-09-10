#!/bin/bash
# ============================================================================
# make_sdcard.sh — 用三份产物组装可 dd 的 sdcard.img（不需要 root、不需要 loop）
# ============================================================================
# 输入：
#   --boot <dir>      boot 文件目录（BOOT.BIN/boot.bin、u-boot.img、uImage、
#                     zybo-audio.dtb、boot.scr，可选 system.bit/uEnv.txt）
#   --rootfs <file>   rootfs.ext4（Buildroot 或 Debian 产物）
#   --out <file>      输出镜像（默认 sdcard.img）
#   --boot-size-mb N  boot 分区大小（默认 64）
# 依赖：parted、mkfs.vfat、mcopy(mtools)、dd
#       （可在 PATH，或 ../.tools/bin/ 下自备解包二进制）
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
        -h|--help)      sed -n '2,18p' "$0"; exit 0 ;;
        *) echo "未知参数: $1"; exit 1 ;;
    esac
done

die()  { echo "[ERROR] $*" >&2; exit 1; }
info() { echo "[SDCARD] $*"; }

find_tool() {
    local name="$1"
    if command -v "$name" >/dev/null 2>&1; then command -v "$name"; return; fi
    for d in "${SCRIPT_DIR}/../../.tools/bin" "${SCRIPT_DIR}/../.tools/bin" "${SCRIPT_DIR}/.tools/bin" "${HOME}/Works/.tools/bin"; do
        [ -x "$d/$name" ] && { echo "$d/$name"; return; }
    done
    die "缺少工具: $name（安装 parted/dosfstools/mtools，或放到 ../.tools/bin）"
}

PARTED=$(find_tool parted)
MKFS_VFAT=$(find_tool mkfs.vfat)
MCOPY=$(find_tool mcopy)
DD=$(find_tool dd)

[ -n "$BOOT_DIR" ] && [ -d "$BOOT_DIR" ] || die "缺少 --boot <dir>"
[ -n "$ROOTFS" ] && [ -f "$ROOTFS" ] || die "缺少 --rootfs <rootfs.ext4>"

BOOTBIN=""
for c in "$BOOT_DIR/BOOT.BIN" "$BOOT_DIR/boot.bin"; do [ -f "$c" ] && BOOTBIN="$c" && break; done
[ -n "$BOOTBIN" ] || die "$BOOT_DIR 下找不到 BOOT.BIN / boot.bin"
for f in u-boot.img uImage zybo-audio.dtb; do
    [ -f "$BOOT_DIR/$f" ] || die "$BOOT_DIR 下缺少 $f"
done

# 布局：p1 FAT32 起始扇区 2048，大小 BOOT_MB；p2 ext4 紧随其后
BOOT_START=2048
ROOT_START=$(( BOOT_START + BOOT_MB * 2048 ))
ROOT_MB=$(( $(stat -c%s "$ROOTFS") / 1048576 + 32 ))
TOTAL_MB=$(( BOOT_MB + ROOT_MB + 1 ))
info "boot=${BOOT_MB}MB @扇区${BOOT_START}, root=${ROOT_MB}MB @扇区${ROOT_START}, total=${TOTAL_MB}MB → $OUT"

rm -f "$OUT"
"$DD" if=/dev/zero of="$OUT" bs=1M count="$TOTAL_MB" status=none

"$PARTED" -s "$OUT" mklabel msdos \
    mkpart primary fat32 "${BOOT_START}s" "$((ROOT_START - 1))s" set 1 boot on \
    mkpart primary ext4 "${ROOT_START}s" 100%

# FAT32 boot 分区：先做独立 boot.vfat，再整块 dd 进 p1
# （不用 mkfs.vfat --offset，那样会在宿主文件里乱扩文件、mcopy 也认不出介质）
BOOTV=$(mktemp -t bootvfat.XXXXXX.img)
trap 'rm -f "$BOOTV"' EXIT
"$DD" if=/dev/zero of="$BOOTV" bs=1M count="$BOOT_MB" status=none
"$MKFS_VFAT" -F 32 -n BOOT "$BOOTV" >/dev/null
for f in "$BOOT_DIR"/*; do
    [ -f "$f" ] || continue
    "$MCOPY" -o -i "$BOOTV" "$f" ::/
    info "  拷贝 $(basename "$f")"
done
"$DD" if="$BOOTV" of="$OUT" bs=512 seek="$BOOT_START" conv=notrunc status=none
info "boot 分区已写入扇区 $BOOT_START"

# rootfs 写入第二分区
"$DD" if="$ROOTFS" of="$OUT" bs=512 seek="$ROOT_START" conv=notrunc status=none
info "rootfs 已写入扇区 $ROOT_START"

info "完成：$OUT（$(stat -c%s "$OUT" | awk '{printf "%.0f", $1/1048576}') MB）"
info "烧录：sudo dd if=$OUT of=/dev/sdX bs=4M status=progress conv=fsync"

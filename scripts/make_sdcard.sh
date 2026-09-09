#!/bin/bash
# ============================================================================
# make_sdcard.sh — 用三份产物组装可 dd 的 sdcard.img
# ============================================================================
# 输入：
#   --boot <dir>      boot 文件目录（BOOT.BIN/boot.bin、u-boot.img、uImage、
#                     zybo-audio.dtb、uEnv.txt，可选 system.bit）
#   --rootfs <file>   rootfs.ext4（来自 zybo-debian CI 或 Buildroot）
#   --out <file>      输出镜像（默认 sdcard.img）
#   --boot-size-mb N  boot 分区大小（默认 64）
# 说明：不使用 loop 设备（CI/容器可用）：sgdisk 分区 + mkfs.vfat --offset + dd 写入 rootfs。
# ============================================================================
set -euo pipefail

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
        -h|--help)      sed -n '2,20p' "$0"; exit 0 ;;
        *) echo "未知参数: $1"; exit 1 ;;
    esac
done

die() { echo "[ERROR] $*" >&2; exit 1; }
info() { echo "[SDCARD] $*"; }

[ -n "$BOOT_DIR" ] && [ -d "$BOOT_DIR" ] || die "缺少 --boot <dir>"
[ -n "$ROOTFS" ] && [ -f "$ROOTFS" ] || die "缺少 --rootfs <rootfs.ext4>"

# boot 文件检查（BOOT.BIN 允许大写或小写）
BOOTBIN=""
for c in "$BOOT_DIR/BOOT.BIN" "$BOOT_DIR/boot.bin"; do [ -f "$c" ] && BOOTBIN="$c" && break; done
[ -n "$BOOTBIN" ] || die "$BOOT_DIR 下找不到 BOOT.BIN / boot.bin"
for f in u-boot.img uImage zybo-audio.dtb uEnv.txt; do
    [ -f "$BOOT_DIR/$f" ] || die "$BOOT_DIR 下缺少 $f"
done

ROOT_MB=$(( $(stat -c%s "$ROOTFS") / 1048576 + 32 ))
TOTAL_MB=$(( BOOT_MB + ROOT_MB ))
info "boot=${BOOT_MB}MB root=${ROOT_MB}MB total=${TOTAL_MB}MB → $OUT"

dd if=/dev/zero of="$OUT" bs=1M count="$TOTAL_MB" status=none

sgdisk --clear \
    --new=1:2048:+${BOOT_MB}M --typecode=1:0x0c --change-name=1:BOOT \
    --new=2:0:+${ROOT_MB}M    --typecode=2:0x83 --change-name=2:rootfs \
    "$OUT" >/dev/null

# FAT32 boot 分区（偏移 = 起始扇区 2048 × 512）
mkfs.vfat -F 32 -n BOOT --offset $((2048 * 512)) "$OUT" >/dev/null
for f in "$BOOT_DIR"/*; do
    [ -f "$f" ] || continue
    mcopy -o -i "$OUT@@$((2048 * 512))" "$f" ::/
    info "  拷贝 $(basename "$f")"
done

# rootfs 分区
START2=$(sgdisk -i 2 "$OUT" | awk '/First sector/ {print $3}')
[ -n "$START2" ] || die "无法获取第二分区起始扇区"
dd if="$ROOTFS" of="$OUT" bs=512 seek="$START2" conv=notrunc status=none
info "rootfs 已写入扇区 $START2"

info "完成：$OUT"
info "烧录：sudo dd if=$OUT of=/dev/sdX bs=4M status=progress conv=fsync"

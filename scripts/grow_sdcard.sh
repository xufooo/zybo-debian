#!/bin/bash
# ============================================================================
# grow_sdcard.sh — 把已烧好的 SD 卡根分区扩到整张卡
# ============================================================================
# 背景：sdcard.img 是按"内容 + 少量余量"做的（约 1GB），烧到 8/16/32GB 卡上
#       后面全是空的。这里把根分区和其上的 ext4 一起撑满整张卡。
#       （镜像本身保持小体积 = 便于传输；扩容是烧写后的一次性动作。）
# 用法：sudo ./grow_sdcard.sh /dev/sdX [分区号，默认 2]
# 注意：设备必须未被挂载；会自动跑 e2fsck（必要，否则 resize2fs 拒绝）
# ============================================================================
set -euo pipefail

DEV="${1:?用法: $0 /dev/sdX [分区号，默认 2]}"
PART="${2:-2}"
[ -b "$DEV" ] || { echo "[ERROR] $DEV 不是块设备" >&2; exit 1; }

# mmcblk0 → /dev/mmcblk0p2；sdc → /dev/sdc2
case "$DEV" in
    *mmcblk*|*nvme*) PARTPATH="${DEV}p${PART}" ;;
    *)               PARTPATH="${DEV}${PART}"  ;;
esac
[ -b "$PARTPATH" ] || { echo "[ERROR] 找不到分区 $PARTPATH" >&2; exit 1; }

if mount | grep -q "^${DEV}"; then
    echo "[ERROR] $DEV 还有分区挂载着，先 umount" >&2; exit 1
fi

echo "[GROW] 分区表（改前）"
parted -s "$DEV" unit s print

echo "[GROW] 扩展 ${PARTPATH} 到卡尾"
parted -s "$DEV" resizepart "$PART" 100%
partprobe "$DEV"; sleep 2

echo "[GROW] e2fsck（resize2fs 的前置要求）"
e2fsck -f -y "$PARTPATH"

echo "[GROW] resize2fs"
resize2fs "$PARTPATH"

echo "[GROW] 结果"
dumpe2fs -h "$PARTPATH" 2>/dev/null | grep -E '^Block count|^Block size|^Free blocks'
lsblk -o NAME,SIZE,FSTYPE,LABEL "$DEV"

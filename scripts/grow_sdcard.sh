#!/bin/bash
# ============================================================================
# grow_sdcard.sh — grow the root partition of a flashed SD card to the full card
# ============================================================================
# Background: sdcard.img is built for "content + a little slack" (about 1GB), so
#       flashing it onto an 8/16/32GB card leaves most of the card empty. This
#       script grows the root partition and its ext4 filesystem to fill the card.
#       (Keeping the image small makes it easy to transfer; growing is a
#       one-time action after flashing.)
# Usage: sudo ./grow_sdcard.sh /dev/sdX [partition number, default 2]
# Note: the device must not be mounted; e2fsck is run automatically (required,
#       otherwise resize2fs refuses to run)
# ============================================================================
set -euo pipefail

DEV="${1:?usage: $0 /dev/sdX [partition number, default 2]}"
PART="${2:-2}"
[ -b "$DEV" ] || { echo "[ERROR] $DEV is not a block device" >&2; exit 1; }

# mmcblk0 -> /dev/mmcblk0p2 ; sdc -> /dev/sdc2
case "$DEV" in
    *mmcblk*|*nvme*) PARTPATH="${DEV}p${PART}" ;;
    *)               PARTPATH="${DEV}${PART}"  ;;
esac
[ -b "$PARTPATH" ] || { echo "[ERROR] partition $PARTPATH not found" >&2; exit 1; }

if mount | grep -q "^${DEV}"; then
    echo "[ERROR] $DEV still has mounted partitions; unmount them first" >&2; exit 1
fi

echo "[GROW] partition table (before)"
parted -s "$DEV" unit s print

echo "[GROW] extend ${PARTPATH} to the end of the card"
parted -s "$DEV" resizepart "$PART" 100%
partprobe "$DEV"; sleep 2

echo "[GROW] e2fsck (prerequisite for resize2fs)"
e2fsck -f -y "$PARTPATH"

echo "[GROW] resize2fs"
resize2fs "$PARTPATH"

echo "[GROW] result"
dumpe2fs -h "$PARTPATH" 2>/dev/null | grep -E '^Block count|^Block size|^Free blocks'
lsblk -o NAME,SIZE,FSTYPE,LABEL "$DEV"

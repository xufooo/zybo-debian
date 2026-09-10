#!/bin/sh
# ============================================================================
# customize01-base.sh — mmdebstrap customize hook
# ============================================================================
# 重要：mmdebstrap 的 hook 在**宿主**上执行，chroot 目录通过 $1 传入。
# 因此所有路径都必须加 "$TARGET" 前缀；需要真正进入 chroot 的命令用 chroot "$TARGET"。
# ============================================================================
set -e

TARGET="${1:?usage: mmdebstrap hook requires the chroot directory as \$1}"

# ── /etc/fstab（必须与 U-Boot 的 root= 一致：SD 卡第二分区）────────────────
mkdir -p "$TARGET/etc"
cat > "$TARGET/etc/fstab" <<'EOF'
# <file system>  <mount point>  <type>     <options>             <dump> <pass>
/dev/mmcblk0p2   /              ext4       defaults,noatime      0      1
proc             /proc          proc       defaults              0      0
sysfs            /sys           sysfs      defaults              0      0
devtmpfs         /dev           devtmpfs   mode=0755,nosuid      0      0
tmpfs            /tmp           tmpfs      defaults,nosuid,nodev 0      0
EOF

# ── 主机名 ────────────────────────────────────────────────────────────────
echo zybo > "$TARGET/etc/hostname"
cat > "$TARGET/etc/hosts" <<'EOF'
127.0.0.1   localhost
127.0.1.1   zybo
::1         localhost ip6-localhost ip6-loopback
EOF

# ── ALSA 默认设备：软件重采样到 48kHz（MCLK 固定 12.288MHz）────────────────
# 格式必须 S32_LE：axi-i2s 在 DMA 模式下只暴露 S32_LE（上板验证）
cat > "$TARGET/etc/asound.conf" <<'EOF'
pcm.!default {
    type plug
    slave {
        pcm "hw:0,0"
        rate 48000
        format S32_LE
        channels 2
    }
}

ctl.!default {
    type hw
    card 0
}
EOF

# ── root 密码（开发用；首次登录后请修改）─────────────────────────────────
chroot "$TARGET" chpasswd <<'EOF'
root:zybo
EOF

# ── 启用服务（串口 getty 由 systemd-getty-generator 依 console= 自动生成）──
mkdir -p "$TARGET/etc/systemd/system/multi-user.target.wants"
ln -sf /usr/lib/systemd/system/ssh.service            "$TARGET/etc/systemd/system/multi-user.target.wants/ssh.service"
ln -sf /usr/lib/systemd/system/shairport-sync.service "$TARGET/etc/systemd/system/multi-user.target.wants/shairport-sync.service"
ln -sf /usr/lib/systemd/system/mpd.socket             "$TARGET/etc/systemd/system/multi-user.target.wants/mpd.socket"
chroot "$TARGET" systemctl set-default multi-user.target 2>/dev/null || true

echo "[hook] base configuration applied to $TARGET"

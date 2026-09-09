#!/bin/sh
# ============================================================================
# 01-base.sh — mmdebstrap hook（在 chroot 内以 root 执行）
# 写 fstab / hostname / asound.conf，启用音频与 SSH 服务
# ============================================================================
set -e

# ── /etc/fstab（必须与 U-Boot 的 root= 一致：SD 卡第二分区）────────────────
cat > /etc/fstab <<'EOF'
# <file system>  <mount point>  <type>     <options>             <dump> <pass>
/dev/mmcblk0p2   /              ext4       defaults,noatime      0      1
proc             /proc          proc       defaults              0      0
sysfs            /sys           sysfs      defaults              0      0
devtmpfs         /dev           devtmpfs   mode=0755,nosuid      0      0
tmpfs            /tmp           tmpfs      defaults,nosuid,nodev 0      0
EOF

# ── 主机名 ────────────────────────────────────────────────────────────────
echo zybo > /etc/hostname
cat > /etc/hosts <<'EOF'
127.0.0.1   localhost
127.0.1.1   zybo
::1         localhost ip6-localhost ip6-loopback
EOF

# ── ALSA 默认设备：软件重采样到 48kHz（MCLK 固定 12.288MHz）────────────────
cat > /etc/asound.conf <<'EOF'
pcm.!default {
    type plug
    slave {
        pcm "hw:0,0"
        rate 48000
        format S24_LE
        channels 2
    }
}

ctl.!default {
    type hw
    card 0
}
EOF

# ── root 密码（开发用；首次登录后请修改）─────────────────────────────────
echo 'root:zybo' | chpasswd

# ── 启用服务（串口 getty 由 systemd-getty-generator 依 console= 自动生成）──
# chroot 里 systemctl enable 可能不生效，直接用符号链接，结果可验证
mkdir -p /etc/systemd/system/multi-user.target.wants
ln -sf /usr/lib/systemd/system/ssh.service            /etc/systemd/system/multi-user.target.wants/ssh.service
ln -sf /usr/lib/systemd/system/shairport-sync.service /etc/systemd/system/multi-user.target.wants/shairport-sync.service
ln -sf /usr/lib/systemd/system/mpd.socket             /etc/systemd/system/multi-user.target.wants/mpd.socket
systemctl set-default multi-user.target 2>/dev/null || true

echo "[hook] base configuration done"
